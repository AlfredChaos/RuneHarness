// view.go —— OPS DECK 布局：
//
//	┌ topbar ── ▚ RUNEHARNESS │ MODEL … │ CWD … │ BRANCH … │ SESS … │ · live · clock
//	├ feed (viewport)                    ┆ dock: TASKS / AGENTS / SESSION
//	├ statusline ── spinner model activity elapsed …… key hints
//	├ [perm band]   有待确认时插入
//	└ input box     多行，随内容增高（1~5 行）
//
// 宽度 < dockMinW 时 dock 隐藏，TODO 降级为状态行上方的一行摘要条。
package tui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

var (
	cFg     = lipgloss.Color("#c7d0de")
	cDim    = lipgloss.Color("#5d6a7e")
	cFaint  = lipgloss.Color("#39424f")
	cCyan   = lipgloss.Color("#56c2d6")
	cGreen  = lipgloss.Color("#7ee787")
	cAmber  = lipgloss.Color("#e3b341")
	cRed    = lipgloss.Color("#f47067")
	cViolet = lipgloss.Color("#d2a8ff")
	cBlue   = lipgloss.Color("#6cb6ff")
	cLine   = lipgloss.Color("#28303f")
	cPanel  = lipgloss.Color("#10141d")
	cOnCyan = lipgloss.Color("#04121a")

	youStyle        = lipgloss.NewStyle().Foreground(cBlue).Bold(true)
	agentStyle      = lipgloss.NewStyle().Foreground(cGreen).Bold(true)
	thinkStyle      = lipgloss.NewStyle().Foreground(cViolet)
	thinkBody       = lipgloss.NewStyle().Foreground(cDim).Italic(true)
	hookStyle       = lipgloss.NewStyle().Foreground(cAmber)
	errStyle        = lipgloss.NewStyle().Foreground(cRed)
	okStyle         = lipgloss.NewStyle().Foreground(cGreen)
	verdictBadStyle = lipgloss.NewStyle().Foreground(cRed)
	dimStyle        = lipgloss.NewStyle().Foreground(cDim)
	faintStyle      = lipgloss.NewStyle().Foreground(cFaint)
	fgStyle         = lipgloss.NewStyle().Foreground(cFg)
	iconStyle       = lipgloss.NewStyle().Foreground(cCyan)
	toolNameStyle   = lipgloss.NewStyle().Foreground(cFg).Bold(true)
	subStyle        = lipgloss.NewStyle().Foreground(cViolet)
	subChip         = lipgloss.NewStyle().Foreground(cViolet).Faint(true)
	spinnerStyle    = lipgloss.NewStyle().Foreground(cCyan)
	segKey          = lipgloss.NewStyle().Foreground(cFaint)
	segVal          = lipgloss.NewStyle().Foreground(cFg)
	inputBorder     = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cLine)
	permBorder      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cAmber)
	permStyle       = lipgloss.NewStyle().Foreground(cAmber).Bold(true)
	todoDone        = lipgloss.NewStyle().Foreground(cGreen)
	todoActive      = lipgloss.NewStyle().Foreground(cAmber)
)

const (
	dockW    = 30  // dock 列宽（含分隔列）
	dockMinW = 100 // 宽度低于此值隐藏 dock（对应 demo 的 media query 降级）
)

func (m *Model) View() string {
	if !m.ready {
		return "initializing…"
	}
	var mainRow string
	if m.dockOn() {
		mainRow = lipgloss.JoinHorizontal(lipgloss.Top,
			m.viewport.View(),
			lipgloss.NewStyle().Foreground(cLine).Width(1).Render("│"),
			m.dockView())
	} else {
		mainRow = m.viewport.View()
	}
	parts := []string{m.topbar(), mainRow, m.statusLine()}
	if !m.dockOn() && m.todoVisible() {
		parts = append(parts, m.todoStrip())
	}
	if len(m.permQueue) > 0 {
		parts = append(parts, m.permView())
	}
	if m.suggRows() > 0 {
		parts = append(parts, m.suggView())
	}
	parts = append(parts, m.inputView())
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// ── top bar ──

func (m *Model) topbar() string {
	logo := lipgloss.NewStyle().Background(cCyan).Foreground(cOnCyan).Bold(true).Render("▚ RUNEHARNESS")
	sep := faintStyle.Render(" │ ")
	seg := func(k, v string) string {
		return segKey.Render(k) + " " + segVal.Render(v)
	}
	left := "  " + logo + sep +
		seg("MODEL", m.info.Model) + sep +
		seg("CWD", m.info.Cwd) + sep +
		seg("BRANCH", orDash(m.info.Branch)) + sep +
		seg("SESS", m.info.Session)
	right := lipgloss.NewStyle().Foreground(cGreen).Render("●") +
		dimStyle.Render(" live") + faintStyle.Render(" · ") +
		lipgloss.NewStyle().Foreground(cCyan).Render(time.Now().Format("15:04:05"))
	gap := m.width - ansi.StringWidth(left) - ansi.StringWidth(right)
	if gap < 1 {
		gap = 1
	}
	return lipgloss.NewStyle().Background(cPanel).Width(m.width).MaxWidth(m.width).
		Render(left + strings.Repeat(" ", gap) + right)
}

// ── dock ──

func (m *Model) dockOn() bool { return m.width >= dockMinW }

// feedW 返回对话流宽度；dock 显示时让出 dockW + 1 分隔列。
func (m *Model) feedW() int {
	if m.dockOn() {
		return m.width - dockW - 1
	}
	return m.width
}

func (m *Model) dockView() string {
	var cards []string
	if m.todoVisible() {
		cards = append(cards, m.tasksCard())
	}
	cards = append(cards, m.agentsCard(), m.sessionCard())
	return lipgloss.NewStyle().Width(dockW).Height(m.viewport.Height).
		MaxHeight(m.viewport.Height).PaddingLeft(1).
		Render(lipgloss.JoinVertical(lipgloss.Left, cards...))
}

// card 渲染一张 dock 卡片：细边框 + 标题行（右侧徽标）+ 内容行。
func (m *Model) card(title, badge string, body []string) string {
	head := lipgloss.NewStyle().Foreground(cDim).Bold(true).Render(title) +
		"  " + lipgloss.NewStyle().Foreground(cCyan).Render(badge)
	content := lipgloss.JoinVertical(lipgloss.Left, append([]string{head}, body...)...)
	return lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(cLine).
		Padding(0, 1).Width(dockW - 5).Render(content)
}

func (m *Model) tasksCard() string {
	done := 0
	for _, t := range m.todos {
		if t.Status == "completed" {
			done++
		}
	}
	const barW = 20
	fill := 0
	if len(m.todos) > 0 {
		fill = done * barW / len(m.todos)
	}
	bar := okStyle.Render(strings.Repeat("━", fill)) + faintStyle.Render(strings.Repeat("─", barW-fill))
	lines := []string{bar}
	for _, t := range m.todos {
		icon, st := "☐", fgStyle
		switch t.Status {
		case "completed":
			icon, st = "☑", lipgloss.NewStyle().Foreground(cFaint).Strikethrough(true)
		case "in_progress":
			icon, st = "▶", todoActive
		}
		lines = append(lines, " "+st.Render(icon+" "+truncateRunes(t.Content, dockW-12)))
	}
	return m.card("▣ TASKS", fmt.Sprintf("%d/%d", done, len(m.todos)), lines)
}

func (m *Model) agentsCard() string {
	state := "idle"
	if m.busy {
		state = "run"
	}
	dot := lipgloss.NewStyle().Foreground(cFaint).Render("◌")
	if m.busy {
		dot = okStyle.Render("●")
	}
	lines := []string{dot + " " + fgStyle.Render("main") +
		faintStyle.Render(fmt.Sprintf("  d0 · %s · turn %d", state, m.turns))}
	subs := make([]*subTrack, 0, len(m.subs))
	for _, s := range m.subs {
		subs = append(subs, s)
	}
	sort.Slice(subs, func(i, j int) bool { return subs[i].seq < subs[j].seq })
	for _, s := range subs {
		var meta string
		if s.done {
			meta = fmt.Sprintf("done %s", fmtDur(s.dur))
		} else {
			meta = s.tool
			if meta == "" {
				meta = "starting"
			}
		}
		d := okStyle.Render("●")
		if s.done {
			d = lipgloss.NewStyle().Foreground(cFaint).Render("◌")
		}
		lines = append(lines, d+" "+subStyle.Render(fmt.Sprintf("sub%d", s.seq))+
			faintStyle.Render(fmt.Sprintf("  d%d · %s", s.depth, meta)))
	}
	return m.card("▣ AGENTS", fmt.Sprintf("%d", len(subs)+1), lines)
}

func (m *Model) sessionCard() string {
	kv := func(k, v string, st lipgloss.Style) string {
		w := dockW - 8
		dots := w - ansi.StringWidth(k) - ansi.StringWidth(v)
		if dots < 1 {
			dots = 1
		}
		return segKey.Render(k) + faintStyle.Render(strings.Repeat("·", dots)) + st.Render(v)
	}
	permN := fmt.Sprintf("%d pending", len(m.permQueue))
	permSt := lipgloss.NewStyle().Foreground(cGreen)
	if len(m.permQueue) > 0 {
		permSt = lipgloss.NewStyle().Foreground(cAmber)
	}
	return m.card("▣ SESSION", "", []string{
		kv("ctx", fmt.Sprintf("≈%.1fk", float64(m.ctxChars)/4000), lipgloss.NewStyle().Foreground(cCyan)),
		kv("msgs", fmt.Sprintf("%d", m.msgs), fgStyle),
		kv("tools", fmt.Sprintf("%d", m.info.Tools), fgStyle),
		kv("perm", permN, permSt),
		kv("breaker", "armed", lipgloss.NewStyle().Foreground(cGreen)),
		kv("up", fmtDur(time.Since(m.started)), fgStyle),
	})
}

// ── status line ──

func (m *Model) statusLine() string {
	hints := faintStyle.Render("enter") + dimStyle.Render(" send") +
		faintStyle.Render("  ^j") + dimStyle.Render(" newline") +
		faintStyle.Render("  pgup/dn") + dimStyle.Render(" scroll") +
		faintStyle.Render("  /") + dimStyle.Render(" commands") +
		faintStyle.Render("  @") + dimStyle.Render(" files")
	var left string
	if len(m.permQueue) > 0 {
		left = " " + m.spinner.View() + " " + segVal.Render(m.info.Model) +
			dimStyle.Render(" · waiting for approval…")
	} else if m.busy {
		what := m.activity
		if what == "" {
			what = "working"
		}
		left = " " + m.spinner.View() + " " + segVal.Render(m.info.Model) + dimStyle.Render(" · "+what) +
			hookStyle.Render(fmt.Sprintf(" · %ds", int(time.Since(m.busySince).Seconds())))
	} else {
		left = " " + segVal.Render(m.info.Model) + dimStyle.Render(" · ready")
	}
	gap := m.width - ansi.StringWidth(left) - ansi.StringWidth(hints) - 1
	if gap < 1 {
		return lipgloss.NewStyle().Width(m.width).MaxWidth(m.width).Render(left)
	}
	return left + strings.Repeat(" ", gap) + hints
}

// ── permission band ──

const permBoxHeight = 5 // 上下边框 + 标题/命令/按键三行

func (m *Model) permView() string {
	req := m.permQueue[0]
	who := ""
	if req.Depth > 0 {
		who = fmt.Sprintf("sub%d · ", m.subLabel(req.Depth))
	}
	head := permStyle.Render(fmt.Sprintf("⚠ APPROVAL %d/%d", req.Seq, req.Total)) +
		dimStyle.Render("  "+who+req.Reason)
	body := fmt.Sprintf("%s %s", toolNameStyle.Render(req.Call.Name),
		fgStyle.Render(string(req.Call.Arguments)))
	if m.width > 7 {
		body = ansi.Truncate(body, m.width-6, "…")
	}
	hint := lipgloss.NewStyle().Foreground(cAmber).Render("[y]") + dimStyle.Render(" allow   ") +
		lipgloss.NewStyle().Foreground(cAmber).Render("[n]") + dimStyle.Render(" deny   ") +
		lipgloss.NewStyle().Foreground(cAmber).Render("[esc]") + dimStyle.Render(" deny")
	return permBorder.Width(m.width - 2).Render(
		lipgloss.JoinVertical(lipgloss.Left, head, body, hint))
}

// ── todo strip（dock 隐藏时的降级形态）──

func (m *Model) todoStrip() string {
	done, cur := 0, ""
	for _, t := range m.todos {
		if t.Status == "completed" {
			done++
		} else if cur == "" && t.Status == "in_progress" {
			cur = t.Content
		}
	}
	line := lipgloss.NewStyle().Foreground(cDim).Bold(true).Render("▣ TASKS") +
		lipgloss.NewStyle().Foreground(cCyan).Render(fmt.Sprintf(" %d/%d", done, len(m.todos)))
	if cur != "" {
		line += faintStyle.Render(" · ") + todoActive.Render("▶ "+truncateRunes(cur, m.width-24))
	}
	return "  " + line
}

// ── input ──

func (m *Model) inputView() string {
	return inputBorder.Width(m.width - 2).Render(m.input.View())
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
