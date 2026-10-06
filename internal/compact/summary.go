package compact

import (
	"context"
	"errors"
	"fmt"
	"strings"

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

// summarize 对中段做一次滚动增量摘要：输入 = 旧摘要 + 新进入中段的消息，
// 输出替换旧摘要。成功时写 boundary 与 summary 两行，返回
// system + 首部 + 摘要 + 尾部 的新视图。
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

	input, err := c.renderInput(oldSummary, events)
	if err != nil {
		return nil, err
	}
	resp, err := c.llm.Chat(ctx, []agent.Message{
		{Role: agent.RoleSystem, Content: systemPrompt(extra)},
		{Role: agent.RoleUser, Content: input},
	}, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errSummaryCall, err)
	}
	text, ok := parseSummary(resp.Message.Content)
	if !ok {
		return nil, errUnparsable
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
	return next, nil
}

// renderInput 把旧摘要与新事件渲染成摘要调用的输入。超过窗口可承受的量时
// 先缩短工具预览，仍超出就从头部砍掉一半事件，最多砍两次（CC group-halving）。
func (c *Compactor) renderInput(oldSummary string, events []agent.Message) (string, error) {
	limit := c.cfg.Window - reserveSummary - TextTokens(systemPrompt("")) - 1_000
	toolChars := renderToolChars
	for attempt := 0; attempt < 4; attempt++ {
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
		if TextTokens(b.String()) <= limit {
			return b.String(), nil
		}
		if attempt == 0 {
			toolChars = renderShortChars
			continue
		}
		events = events[len(events)/2:]
	}
	return "", fmt.Errorf("%w: summary input does not fit the context window", errSummaryCall)
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
