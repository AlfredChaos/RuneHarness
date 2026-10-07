// feed.go —— 对话流行构建器：OPS DECK 的 transcript 由一行行
// "时间戳 gutter + 角色标签 + 正文" 组成，折行续行对齐到正文列。
// 行在 append 时即按当前 feed 宽度硬换行落盘（与 viewport 一样不做
// 事后重排——resize 后旧行保持原宽度）。
package tui

import (
	"fmt"
	"strings"
	"time"

	"runeharness/internal/agent"
	"runeharness/internal/bgtask"
	"runeharness/internal/cron"
	"runeharness/internal/subagent"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

const (
	gutterW = 9 // "15:04:05" + 1 空格
	tagW    = 6 // 定宽角色标签（5 字符）+ 1 空格
	iconW   = 2 // ⚙ / ⎿ / ⤷ / ⚠ + 1 空格
)

// tag 渲染定宽 5 字符的角色标签。
func tag(s string, st lipgloss.Style) string {
	return st.Render(fmt.Sprintf("%-5s", s))
}

// row 渲染一条 transcript 行：dim 时间戳 + left（图标或标签，可空）+ 正文。
// leftW 是 left 的可打印宽度；正文按 w-pw 折行，续行缩进 pw 列。
func (m *Model) row(left string, leftW int, body string, w int, t time.Time) string {
	head := dimStyle.Render(t.Format("15:04:05")) + " " + left
	pw := gutterW + leftW
	pad := strings.Repeat(" ", pw)
	var b strings.Builder
	first := true
	for _, ln := range strings.Split(body, "\n") {
		for _, sub := range strings.Split(hardWrap(ln, w-pw), "\n") {
			if !first {
				b.WriteString("\n")
				b.WriteString(pad)
			}
			first = false
			b.WriteString(sub)
		}
	}
	return head + b.String()
}

func (m *Model) rowUser(body string) string {
	return m.row(tag("you", youStyle)+" ", tagW, body, m.feedW(), time.Now())
}

func (m *Model) rowAgent(body string, t time.Time) string {
	return m.row(tag("agent", agentStyle)+" ", tagW, body, m.feedW(), t)
}

func (m *Model) rowThink(body string, t time.Time) string {
	// 逐行渲染：整段 Render 会把每行补空格对齐到最宽行宽，
	// 空格尾巴被 row 的硬换行折成多余空行，段间距随最长行放大。
	lines := strings.Split(body, "\n")
	for i, ln := range lines {
		lines[i] = thinkBody.Render(ln)
	}
	return m.row(tag("think", thinkStyle)+" ", tagW, strings.Join(lines, "\n"), m.feedW(), t)
}

func (m *Model) rowHook(body string) string {
	return m.row(tag("hook", hookStyle)+" ", tagW, hookStyle.Render("⏰ "+body), m.feedW(), time.Now())
}

func (m *Model) rowTool(c agent.ToolCall) string {
	return m.row(iconStyle.Render("⚙")+" ", iconW,
		toolNameStyle.Render(c.Name)+" "+dimStyle.Render(string(c.Arguments)),
		m.feedW(), time.Now())
}

// rowToolResult 渲染工具结果摘要行：⎿ ok · 0.2s · N lines / ✘ 失败原因。
func (m *Model) rowToolResult(r agent.ToolResult) string {
	var body string
	switch {
	case r.Blocked:
		body = verdictBadStyle.Render("✘ blocked") + dimStyle.Render(" · "+truncateRunes(firstLine(r.Output), 60))
	case r.IsError:
		body = verdictBadStyle.Render("✘ "+fmtDur(r.Dur)) + errStyle.Render(" · "+truncateRunes(firstLine(r.Output), 60))
	default:
		body = okStyle.Render("✔ "+fmtDur(r.Dur)) + dimStyle.Render(" · "+summarize(r.Output))
	}
	return m.row(dimStyle.Render("⎿")+" ", iconW, body, m.feedW(), time.Now())
}

func (m *Model) rowVerdict(c agent.ToolCall, ok bool, depth int) string {
	verdict := verdictBadStyle.Render("✘ denied")
	if ok {
		verdict = okStyle.Render("✔ allowed")
	}
	who := ""
	if depth > 0 {
		who = fmt.Sprintf(" · sub%d", m.subLabel(depth))
	}
	return m.row(hookStyle.Render("⚠")+" ", iconW,
		toolNameStyle.Render(c.Name)+" "+dimStyle.Render(string(c.Arguments))+" → "+verdict+dimStyle.Render(who),
		m.feedW(), time.Now())
}

// rowSub 渲染一条子代理事件：⤷ 前缀 + 深度缩进（缩进放在正文里，保证折行对齐）。
func (m *Model) rowSub(ev subagent.Event) string {
	indent := strings.Repeat("  ", ev.Depth)
	var body string
	switch ev.Kind {
	case subagent.EventSpawn:
		desc, _, _ := strings.Cut(ev.Desc, "\n")
		body = indent + fmt.Sprintf("sub%d %s %s", m.subLabel(ev.Depth),
			subChip.Render("["+string(ev.Type)+"]"), dimStyle.Render(truncateRunes(desc, 80)))
	case subagent.EventToolCall:
		body = indent + fmt.Sprintf("⚙ %s %s", ev.Call.Name,
			dimStyle.Render(string(ev.Call.Arguments)))
	default: // EventDone
		if ev.Err != nil {
			body = indent + fmt.Sprintf("sub%d failed: %s", m.subLabel(ev.Depth), ev.Err)
		} else {
			body = indent + fmt.Sprintf("sub%d done · %s", m.subLabel(ev.Depth),
				dimStyle.Render(truncateRunes(firstLine(ev.Result), 80)))
		}
	}
	return m.row(subStyle.Render("⤷")+" ", iconW, subStyle.Render(body), m.feedW(), time.Now())
}

// rowBg 渲染一条后台任务事件行：◐ 图标 + 终态/停滞摘要。
func (m *Model) rowBg(ev bgtask.Event) string {
	t := ev.Task
	var body string
	if ev.Stalled {
		body = fmt.Sprintf("◐ %s stalled — interactive prompt? · %s",
			t.ID, dimStyle.Render(truncateRunes(t.Description, 60)))
	} else {
		state := fmt.Sprintf("%s · %s", t.Status,
			t.EndedAt.Sub(t.StartedAt).Round(time.Second))
		if t.Status == bgtask.StatusCompleted || t.Status == bgtask.StatusFailed {
			state = fmt.Sprintf("%s exit %d · %s", t.Status, t.ExitCode,
				t.EndedAt.Sub(t.StartedAt).Round(time.Second))
		}
		body = fmt.Sprintf("◐ %s %s · %s", t.ID, state,
			dimStyle.Render(truncateRunes(t.Description, 60)))
	}
	return m.row(subStyle.Render("⤴")+" ", iconW, body, m.feedW(), time.Now())
}

// rowCron 渲染一条定时任务事件行：⏰ 图标 + 触发摘要（漏跑汇总单独标注）。
func (m *Model) rowCron(ev cron.Event) string {
	var body string
	if ev.Missed {
		body = "⏰ missed one-shot schedule found — asking before running"
	} else {
		desc := cron.Humanize(ev.Task.Cron)
		if ev.Task.TZ != "" {
			desc += " " + ev.Task.TZ
		}
		body = fmt.Sprintf("⏰ %s fired · %s · %s", ev.Task.ID,
			desc, dimStyle.Render(truncateRunes(firstLine(ev.Task.Prompt), 60)))
	}
	return m.row("", 0, hookStyle.Render(body), m.feedW(), time.Now())
}

// rowNote 渲染无标签的系统行（中断、步数续跑、空答复等），图标进正文。
func (m *Model) rowNote(icon, body string) string {
	return m.row("", 0, dimStyle.Render(icon+" "+body), m.feedW(), time.Now())
}

// renderPartialRows 渲染一次流式快照为带 gutter 的行（thinking + 正文）。
// 时间戳取本轮开始时刻，避免逐分片刷新时时间戳抖动。
// mdStyle 为空时正文按纯文本（流式阶段），否则按 Markdown 渲染。
func (m *Model) renderPartialRows(p agent.Partial, mdStyle string) string {
	t := m.busySince
	if t.IsZero() {
		t = time.Now()
	}
	var s string
	if p.Thinking != "" {
		s = m.rowThink(p.Thinking, t)
	}
	if p.Content != "" {
		if s != "" {
			s += "\n\n"
		}
		body := renderText(p.Content, m.feedW()-tagW-gutterW)
		if mdStyle != "" {
			body = renderMarkdown(p.Content, mdStyle, m.feedW()-tagW-gutterW)
		}
		s += m.rowAgent(body, t)
	}
	return s
}

// summarize 生成结果摘要：多行给行数，单行给截断的原文。
func summarize(s string) string {
	n := strings.Count(s, "\n") + 1
	if n > 1 {
		return fmt.Sprintf("%d lines", n)
	}
	return truncateRunes(strings.TrimSpace(s), 40)
}

// firstLine 返回首个非空行。
func firstLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return ""
}

// fmtDur 压缩耗时显示：0.2s / 3.8s / 21s / 2m03s。
func fmtDur(d time.Duration) string {
	switch {
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	default:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
}

// truncateRunes 把 s 截断到 max 个字符（单行摘要用）。
func truncateRunes(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

// renderText 按终端宽度软换行渲染正文。
func renderText(s string, width int) string {
	return renderStyled(lipgloss.NewStyle(), s, width)
}

// renderStyled 用指定样式渲染并按终端宽度软换行。
func renderStyled(style lipgloss.Style, s string, width int) string {
	if width < 20 {
		width = 80
	}
	return style.Width(width - 2).Render(s)
}

// hardWrap 按终端宽度对单行/多行文本做字符级硬换行（ANSI 感知）。
// 词换行折不断无空格长串（JSON 参数、URL），这类行必须走硬换行。
func hardWrap(s string, width int) string {
	if width < 20 {
		width = 80
	}
	return ansi.Hardwrap(s, width-2, true)
}
