package compact

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"runeharness/internal/agent"
	"runeharness/internal/session"
)

// 摘要失败的三类（plan §4.8），都计入熔断。
var (
	errSummaryCall     = errors.New("summary_call_error")
	errUnparsable      = errors.New("summary_unparsable")
	errIneffective     = errors.New("compaction_ineffective")
	errNothingToShrink = fmt.Errorf("%w: middle segment is empty", errIneffective)
)

// 渲染摘要输入时单条内容的上限：工具结果只给预览，摘要不需要全文。
const (
	renderToolChars  = 2_000
	renderShortChars = 300
	renderArgsChars  = 500
)

// summarize 对中段做滚动增量摘要：输入 = 旧摘要 + 新进入中段的消息，
// 输出替换旧摘要。中段超出摘要调用可容纳的量时按时间切块，逐块滚动
// （上一块的产出作为下一块的 EXISTING SUMMARY），不再静默丢头部事件。
// 成功时写 boundary 与 summary 两行，返回 system + 首部 + 摘要 + 尾部 的新视图。
func (c *Compactor) summarize(ctx context.Context, view []agent.Message, seg segments, reason, extra string) ([]agent.Message, error) {
	var oldSummary string
	var events []agent.Message
	for _, j := range seg.middle {
		if view[j].Kind == agent.KindSummary {
			oldSummary = view[j].Content
			continue
		}
		events = append(events, view[j])
	}
	if len(events) == 0 {
		return nil, errNothingToShrink
	}

	started := time.Now()
	var chunks int
	text := oldSummary
	for _, chunk := range c.splitEvents(events) {
		input, err := c.renderInput(text, chunk)
		if err != nil {
			return nil, err
		}
		resp, err := c.llm.Chat(ctx, []agent.Message{
			{Role: agent.RoleSystem, Content: c.systemPrompt(extra)},
			{Role: agent.RoleUser, Content: input},
		}, nil, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errSummaryCall, err)
		}
		s, ok := parseSummary(resp.Message.Content)
		if !ok {
			return nil, errUnparsable
		}
		text = s
		chunks++
	}
	if c.cfg.Reattach != nil {
		if r := c.cfg.Reattach(); r != "" {
			text += "\n\n" + r
		}
	}
	summary := agent.Message{Role: agent.RoleUser, Kind: agent.KindSummary, Content: summaryHeader + text + summaryFooter}

	next := pick(view, seg.system)
	next = append(next, pick(view, seg.head)...)
	next = append(next, summary)
	tailStart := len(next)
	next = append(next, pick(view, seg.tail)...)
	for i := range next {
		next[i].Usage = nil // 锚点对应改写前的视图，作废
	}
	pre, post := Estimate(view, c.cfg.Overhead), Estimate(next, c.cfg.Overhead)
	if post >= c.cfg.CompactLine() {
		return nil, fmt.Errorf("%w: %d tokens after compaction, line is %d", errIneffective, post, c.cfg.CompactLine())
	}
	midTok := Rough(events)
	tailTok := Rough(pick(view, seg.tail))

	// 摘要行先于 boundary 落库：进程若死在两者之间，留下的只是一条不被
	// 引用的孤儿摘要（fold 无 boundary 时把它当普通行展示），而不是悬空
	// boundary 把中段静默丢掉。
	id, err := c.store.Append(ctx, summary)
	if err != nil {
		return nil, fmt.Errorf("record summary: %w", err)
	}
	meta := session.BoundaryMeta{Reason: reason, PreTokens: pre, PostTokens: post,
		CoversTo: maxID(view), SummaryID: id}
	for _, j := range seg.head {
		meta.HeadIDs = append(meta.HeadIDs, view[j].ID)
	}
	if len(seg.tail) > 0 && view[seg.tail[0]].ID > 0 {
		meta.CoversTo = view[seg.tail[0]].ID - 1
	}
	row, err := session.ControlRow(agent.KindBoundary, meta)
	if err != nil {
		return nil, err
	}
	if _, err := c.store.Append(ctx, row); err != nil {
		return nil, fmt.Errorf("record boundary: %w", err)
	}
	next[tailStart-1].ID = id
	slog.Info("context summarized", "session", sessionOf(ctx),
		"head_tokens", Rough(pick(view, seg.head)), "mid_tokens", midTok,
		"tail_tokens", tailTok, "chunks", chunks,
		"duration_ms", time.Since(started).Milliseconds())
	return next, nil
}

// inputLimit 是摘要调用输入的 token 上限：窗口扣掉摘要输出预留、system 提示与余量。
func (c *Compactor) inputLimit() int {
	return c.cfg.Window - reserveSummary - TextTokens(c.systemPrompt("")) - 1_000
}

// renderInput 把旧摘要与一批新事件渲染成摘要调用的输入。超出上限时先把
// 工具结果预览缩到最短；仍超出（旧摘要本身已接近上限、或单条事件过大）
// 就对成品文本做尾部裁剪——裁剪只在兜底路径发生，正常输入不丢内容。
func (c *Compactor) renderInput(oldSummary string, events []agent.Message) (string, error) {
	limit := c.inputLimit()
	for _, toolChars := range []int{renderToolChars, renderShortChars} {
		var b strings.Builder
		b.WriteString("EXISTING SUMMARY:\n")
		if oldSummary == "" {
			b.WriteString("(none)\n")
		} else {
			b.WriteString(oldSummary + "\n")
		}
		b.WriteString("\nNEW EVENTS:\n")
		for _, m := range events {
			renderEvent(&b, m, toolChars)
		}
		if s := b.String(); TextTokens(s) <= limit {
			return s, nil
		} else if toolChars == renderShortChars {
			return clipToTokens(s, limit), nil
		}
	}
	return "", fmt.Errorf("%w: summary input does not fit the context window", errSummaryCall)
}

// splitEvents 把中段事件按输入预算切成有序块：每块连同 EXISTING SUMMARY
//（按 summaryBudget 预留）渲染后不超过 inputLimit。单条超限事件独占一块，
// 由 renderInput 的兜底裁剪处理。
func (c *Compactor) splitEvents(events []agent.Message) [][]agent.Message {
	// 事件按摘要格式渲染后的 token 合计加上 EXISTING SUMMARY 段的开销。
	budget := c.inputLimit() - summaryBudget - 64
	if budget < 1 {
		budget = 1
	}
	var out [][]agent.Message
	var cur []agent.Message
	acc := 0
	for _, m := range events {
		var b strings.Builder
		renderEvent(&b, m, renderToolChars)
		tok := TextTokens(b.String())
		if len(cur) > 0 && acc+tok > budget {
			out = append(out, cur)
			cur, acc = nil, 0
		}
		cur = append(cur, m)
		acc += tok
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// clipToTokens 按 token 预算裁剪文本：保住头部，尾部之外替换为省略标记。
// 只服务 renderInput 的兜底路径。
func clipToTokens(s string, budget int) string {
	if TextTokens(s) <= budget {
		return s
	}
	r := []rune(s)
	cut := len(r)
	for cut > 0 && TextTokens(string(r[:cut])) > budget {
		// 二分之外的最朴素步进：rune 数按预算比例缩放后线性收缩。
		step := max(len(r)/64, 8)
		cut -= step
	}
	if cut < 0 {
		cut = 0
	}
	return string(r[:cut]) + "\n…[truncated to fit window]"
}

func renderEvent(b *strings.Builder, m agent.Message, toolChars int) {
	switch m.Role {
	case agent.RoleUser:
		label := "user"
		if m.Kind == agent.KindInject {
			label = "harness notice"
		}
		fmt.Fprintf(b, "[%s]\n%s\n", label, m.Content)
	case agent.RoleAssistant:
		if m.Content != "" {
			fmt.Fprintf(b, "[assistant]\n%s\n", m.Content)
		}
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(b, "[tool call %s %s] %s\n", tc.Name, tc.ID, clip(string(tc.Arguments), renderArgsChars))
		}
	case agent.RoleTool:
		label := "tool result " + m.ToolCallID
		if m.IsError {
			label += " (error)"
		}
		fmt.Fprintf(b, "[%s]\n%s\n", label, clip(m.Content, toolChars))
	}
	// 附件分片进摘要的形态按 kind 区分：image 只留占位（base64 绝不进摘要
	// 提示词），document 留裁剪后的文字摘录——否则 PDF 一问就被折叠成
	// "这里有个文件"，后续追问失去内容。clip 预算与工具结果共用 toolChars。
	for _, p := range m.Parts {
		name := p.Filename
		if name == "" {
			name = p.MediaType
		}
		fmt.Fprintf(b, "[%s: %s]\n", p.Kind, name)
		if p.Kind == agent.PartDocument && p.Text != "" {
			fmt.Fprintf(b, "%s\n", clip(p.Text, toolChars))
		}
	}
}

// clip 保留头尾，中间以省略标记代替。
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return cutRunes(s, n/2) + "\n…[clipped]…\n" + cutRunesFromEnd(s, n/2)
}

// parseSummary 取 "SUMMARY:" 之后的正文；缺段或为空时 ok 为 false。
func parseSummary(out string) (string, bool) {
	i := strings.Index(out, "SUMMARY:")
	if i < 0 {
		return "", false
	}
	text := strings.TrimSpace(out[i+len("SUMMARY:"):])
	return text, text != ""
}

// maxID 返回视图中最大的行 id。
func maxID(view []agent.Message) int64 {
	var m int64
	for _, v := range view {
		m = max(m, v.ID)
	}
	return m
}
