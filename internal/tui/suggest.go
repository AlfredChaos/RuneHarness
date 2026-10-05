// suggest.go —— 输入框 "/" 前缀触发的下拉建议（参照 claude-code 的
// typeahead 交互）：
//
//	/             → 系统命令 + 技能统一列表，继续输入按名称模糊过滤
//	/resume <q>   → 切换为会话建议，q 按 title/id 子串过滤
//
// 交互：↑/↓ 移动选中，Tab 只补全进输入框，Enter 补全并执行
// （带参项如 /resume 只补全到 "/resume "），Esc 关闭本次下拉。
package tui

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"runeharness/internal/agent"
	"runeharness/internal/scope"
	"runeharness/internal/session"
	"runeharness/internal/skill"
)

// suggKind 区分下拉项来源，也决定渲染颜色与 Enter 行为。
type suggKind int

const (
	suggCmd     suggKind = iota // 系统命令
	suggSkill                   // 技能（提交时展开为指令消息）
	suggSession                 // /resume 的会话条目
	suggFile                    // @ 文件/目录提及
)

// slashCmd 是系统斜杠命令的注册条目。
type slashCmd struct {
	name    string   // 主名（不带 /）
	aliases []string // 触发别名，不出现在下拉中
	desc    string
	hasArgs bool // Enter 只补全 "/name " 而非执行（如 /resume 等参数位选择器）
}

// slashCommands 是系统命令注册表；"/" 空查询时按此顺序排在技能之前。
var slashCommands = []slashCmd{
	{name: "exit", aliases: []string{"quit"}, desc: "exit the process"},
	{name: "resume", desc: "resume a previous session", hasArgs: true},
	{name: "todo", desc: "toggle the task list"},
}

// suggestion 是下拉框中的一行。
type suggestion struct {
	id    string // 列表重建时保持选中态的键
	kind  suggKind
	label string // 主列：/name、会话标题或文件路径
	desc  string // 右侧描述列
	cmd   *slashCmd
	skill *skill.Meta
	sess  *session.Session
	file  string // suggFile：相对 workspace 的路径
	isDir bool   // suggFile：目录项，补全为 "@dir/" 继续下钻
}

// suggKey 是下拉状态的缓存键：@ 建议跟随光标，键里必须带光标位置——
// 同一文本光标移出 token（如 left 键）也应令下拉失效重算。
func (m *Model) suggKey() string {
	return m.input.Value() + "\x00" + strconv.Itoa(m.cursorOffset())
}

// updateSuggestions 按当前输入重算下拉；值与光标均未变直接返回。
// 输入一变选中项归零（CC 对命令建议同款策略：过滤结果里首个即最佳匹配）；
// Esc 关闭的实现也落在同一处——dismiss 把 suggFor 设为当前键并清空列表，
// 值不变就不再触发，值一变即恢复。
func (m *Model) updateSuggestions() {
	k := m.suggKey()
	if k == m.suggFor {
		return
	}
	m.suggFor = k
	m.suggs, m.suggNote = m.computeSuggestions(m.input.Value(), m.cursorOffset())
	m.suggSel = 0
	m.layout()
}

// computeSuggestions 是纯查询函数：返回下拉项与空态提示（互不共存）。
// "/" 开头的输入走命令/技能/会话分支；其余输入检查光标处的 @ token
// 出文件建议。
func (m *Model) computeSuggestions(v string, cursor int) ([]suggestion, string) {
	if strings.HasPrefix(v, "/") && !strings.ContainsRune(v, '\n') {
		rest := v[1:]
		if i := strings.IndexByte(rest, ' '); i >= 0 {
			// 命令名后已带空格：仅 /resume 继续出参数位建议（会话搜索）。
			if rest[:i] == "resume" {
				list := m.sessionSuggestions(rest[i+1:])
				if len(list) == 0 {
					return nil, "no matching sessions"
				}
				return list, ""
			}
			return nil, ""
		}
		list := m.commandSuggestions(rest)
		if len(list) == 0 && rest != "" {
			return nil, "no matching commands"
		}
		return list, ""
	}
	if tok, ok := atToken(v, cursor); ok {
		// 文件分支无匹配时不出下拉（同 CC），免得占位行拦 Enter。
		return m.fileSuggestions(tok.query), ""
	}
	return nil, ""
}

// commandSuggestions 对系统命令与技能做统一过滤排序：
// 精确名 > 精确别名 > 名前缀 > 别名前缀 > 名子串 > 描述子串；
// 同级内命令在技能之前，各按名称排序（空查询时保注册/扫描序）。
func (m *Model) commandSuggestions(query string) []suggestion {
	q := strings.ToLower(query)
	rank := func(name string, aliases []string, desc string) int {
		ln := strings.ToLower(name)
		switch {
		case q == "" || ln == q:
			return 0
		}
		for _, a := range aliases {
			if strings.ToLower(a) == q {
				return 1
			}
		}
		if strings.HasPrefix(ln, q) {
			return 2
		}
		for _, a := range aliases {
			if strings.HasPrefix(strings.ToLower(a), q) {
				return 3
			}
		}
		if strings.Contains(ln, q) {
			return 4
		}
		for _, a := range aliases {
			if strings.Contains(strings.ToLower(a), q) {
				return 4
			}
		}
		if strings.Contains(strings.ToLower(desc), q) {
			return 5
		}
		return -1
	}

	type cand struct {
		suggestion
		rank, ord int
	}
	var cands []cand
	for i := range slashCommands {
		c := &slashCommands[i]
		if r := rank(c.name, c.aliases, c.desc); r >= 0 {
			cands = append(cands, cand{
				suggestion{id: "cmd:" + c.name, kind: suggCmd,
					label: "/" + c.name, desc: c.desc, cmd: c},
				r, i})
		}
	}
	for i := range m.skills {
		s := &m.skills[i]
		if r := rank(s.Name, nil, s.Description); r >= 0 {
			cands = append(cands, cand{
				suggestion{id: "skill:" + s.Name, kind: suggSkill,
					label: "/" + s.Name, desc: s.Description, skill: s},
				r, i})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].rank != cands[j].rank {
			return cands[i].rank < cands[j].rank
		}
		if cands[i].kind != cands[j].kind {
			return cands[i].kind < cands[j].kind
		}
		// 空查询保持来源顺序（注册表/扫描序），否则按名称排序。
		if q == "" {
			return cands[i].ord < cands[j].ord
		}
		return cands[i].label < cands[j].label
	})
	out := make([]suggestion, len(cands))
	for i := range cands {
		out[i] = cands[i].suggestion
	}
	return out
}

// listResumable 返回可恢复的会话：本 workspace 的 main 会话，排除当前会话。
// store 为 nil（测试）时返回空。
func (m *Model) listResumable() ([]session.Session, error) {
	if m.store == nil {
		return nil, nil
	}
	ctx := scope.WithScope(context.Background(), m.sc)
	all, err := m.store.ListSessions(ctx, 0)
	if err != nil {
		return nil, err
	}
	var out []session.Session
	for _, s := range all {
		if s.Kind != session.KindMain || s.ID == m.sc.SessionID || s.Workspace != m.sc.Workspace {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// sessionSuggestions 把可恢复会话按 title/id 子串过滤为下拉项，
// 顺序保持存储的 updated_at 倒序，上限 50 条。
func (m *Model) sessionSuggestions(query string) []suggestion {
	list, err := m.listResumable()
	if err != nil {
		return nil
	}
	q := strings.ToLower(strings.TrimSpace(query))
	var out []suggestion
	for i := range list {
		s := &list[i]
		if q != "" && !strings.Contains(
			strings.ToLower(s.Title+" "+s.ID), q) {
			continue
		}
		label := s.Title
		if label == "" {
			label = "(untitled)"
		}
		out = append(out, suggestion{
			id: "sess:" + s.ID, kind: suggSession,
			label: label,
			desc:  shortID(s.ID) + " · " + fmtAgo(s.UpdatedAt),
			sess:  s,
		})
		if len(out) >= 50 {
			break
		}
	}
	return out
}

// applySuggestion 响应下拉项确认：execute=false 只补全进输入框；
// execute=true 时无参命令/技能直接提交，带参命令同样只补全（
// /resume 补全后下拉随即切换为会话列表），会话项直接恢复。
func (m *Model) applySuggestion(execute bool) tea.Cmd {
	if m.suggSel >= len(m.suggs) {
		return nil
	}
	s := m.suggs[m.suggSel]
	m.suggs, m.suggNote = nil, ""
	switch s.kind {
	case suggFile:
		// 文件项不分 Tab/Enter：都只做补全，不提交。
		return m.applyFileCompletion(s)
	case suggSession:
		if !execute {
			m.setInput("/resume " + s.sess.ID)
			return nil
		}
		m.input.Reset()
		m.suggFor, m.suggSel = "", 0
		m.fitInput()
		return m.resumeSession(*s.sess)
	case suggSkill:
		if execute {
			// 走 submit 单一路径：清输入、回显、进 runSlash 分发。
			m.input.SetValue("/" + s.skill.Name)
			m.suggFor = ""
			return m.submit()
		}
		m.setInput("/" + s.skill.Name + " ")
	default:
		if execute && !s.cmd.hasArgs {
			m.input.SetValue("/" + s.cmd.name)
			m.suggFor = ""
			return m.submit()
		}
		m.setInput("/" + s.cmd.name + " ")
	}
	return nil
}

// setInput 改写输入框并把光标移到末尾，随后重算建议与行高。
func (m *Model) setInput(v string) {
	m.input.SetValue(v)
	m.input.CursorEnd()
	m.suggFor = ""
	m.updateSuggestions()
	m.fitInput()
}

// runResume 处理 /resume 提交：无参数时回填前缀展开会话下拉；
// 有参数时按 id 全等或唯一前缀解析后直接恢复。
func (m *Model) runResume(arg string) tea.Cmd {
	if arg == "" {
		m.setInput("/resume ")
		return nil
	}
	list, err := m.listResumable()
	if err != nil {
		m.appendLine(m.rowNote("✘", "resume failed: "+err.Error()))
		return nil
	}
	var match *session.Session
	n := 0
	for i := range list {
		if list[i].ID == arg {
			match, n = &list[i], 1
			break
		}
		if strings.HasPrefix(list[i].ID, arg) {
			match, n = &list[i], n+1
		}
	}
	switch {
	case n == 1:
		return m.resumeSession(*match)
	case n > 1:
		m.appendLine(m.rowNote("✘", fmt.Sprintf("ambiguous session %q — %d matches, pick from the list", arg, n)))
	default:
		m.appendLine(m.rowNote("✘", "no session matching "+arg+" (type /resume to browse)"))
	}
	return nil
}

// resumeSession 把当前对话切换到目标会话：回放历史进 transcript，
// 换绑 scope 使后续追加写入该会话（与 --resume 语义一致，审计链不断）。
func (m *Model) resumeSession(s session.Session) tea.Cmd {
	ctx := scope.WithScope(context.Background(), m.sc)
	hist, err := m.store.LoadHistory(ctx, s.ID)
	if err != nil {
		m.appendLine(m.rowNote("✘", "resume failed: "+err.Error()))
		return nil
	}
	m.history = hist
	m.sc.SessionID = s.ID
	m.info.Session = shortID(s.ID)
	m.turns, m.msgs, m.ctxChars = 0, len(hist), 0
	for _, h := range hist {
		m.ctxChars += len(h.Content) + len(h.Thinking)
		if h.Role == agent.RoleUser {
			m.turns++
		}
	}
	// 对话轮次、子代理卡片、流式缓冲都属于旧会话现场，一并复位；
	// TODO 列表与权限队列是进程内状态，原样保留。
	m.autoResumed, m.todoPin = false, todoPinAuto
	m.subs = map[int]*subTrack{}
	m.subIDs = map[int]int{}
	m.subSeq = 0
	m.pending, m.partial = "", agent.Partial{}

	title := s.Title
	if title == "" {
		title = "(untitled)"
	}
	m.transcript = ""
	m.appendBlock(m.rowNote("⤺", fmt.Sprintf("resumed %s · %s · %d messages",
		shortID(s.ID), title, len(hist))))
	m.replayHistory(hist)
	m.layout()
	return nil
}

// replayHistory 把回放的会话历史渲染进 transcript：user/assistant 正文
// 照常渲染（thinking 置灰），工具调用与结果折叠为单行摘要。
// 注入类消息（<notice>/<reminder>）按 hook 行渲染，与真实用户输入区分。
func (m *Model) replayHistory(hist []agent.Message) {
	now := time.Now()
	for _, msg := range hist {
		switch msg.Role {
		case agent.RoleUser:
			switch {
			case strings.HasPrefix(msg.Content, "<notice>") || strings.HasPrefix(msg.Content, "<reminder>"):
				m.appendBlock(m.rowHook(msg.Content))
			default:
				// 附件内容块不回放全文，折成 "+N files" 标记
				text, n := splitAttachments(msg.Content)
				if n > 0 {
					text += faintStyle.Render(fmt.Sprintf("  [+%d attached]", n))
				}
				m.appendBlock(m.rowUser(text))
			}
		case agent.RoleAssistant:
			if msg.Thinking != "" {
				m.appendBlock(m.rowThink(msg.Thinking, now))
			}
			if msg.Content != "" {
				m.appendBlock(m.rowAgent(renderMarkdown(msg.Content, m.mdStyle, m.feedW()-tagW-gutterW), now))
			}
			for _, tc := range msg.ToolCalls {
				m.appendLine(m.rowTool(tc))
			}
		case agent.RoleTool:
			st := dimStyle
			if msg.IsError {
				st = errStyle
			}
			m.appendLine(m.row(dimStyle.Render("⎿")+" ", iconW,
				st.Render(summarize(msg.Content)), m.feedW(), now))
		}
	}
}

// findSkill 按精确名查技能；系统命令与技能同名时命令优先（调用方先匹配命令）。
func (m *Model) findSkill(name string) *skill.Meta {
	for i := range m.skills {
		if m.skills[i].Name == name {
			return &m.skills[i]
		}
	}
	return nil
}

// skillPromptText 把 /技能 [args] 展开为发给模型的用户消息：SKILL.md
// 内容由 TUI 进程本地读入并内联（同 CC 的 prompt command 展开）——
// 不能用"给绝对路径让模型 read_file"的方式，技能目录在 workspace 之外，
// read_file 的 resolvePath 会按逃逸拒绝。
func skillPromptText(s skill.Meta, args string) string {
	p := fmt.Sprintf("调用技能 %q 处理本请求。其 SKILL.md 完整内容如下：\n\n<skill path=%q>\n%s\n</skill>",
		s.Name, s.Path, readSkillFile(s.Path))
	if args != "" {
		p += "\n\n" + args
	}
	return p
}

// readSkillFile 读技能定义文件，封顶 32KB；读不到内联错误说明。
func readSkillFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "[error reading SKILL.md: " + err.Error() + "]"
	}
	if len(data) > attachFileCap {
		data = append(data[:attachFileCap], []byte("\n...[truncated]")...)
	}
	return string(data)
}

// shortID 取会话 id 的短前缀用于展示。
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// fmtAgo 把会话更新时间压成紧凑相对时间：now/5m/3h/9d/2025-08-01。
func fmtAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return t.Local().Format("2006-01-02")
	}
}

// ── 下拉渲染 ──

const suggMaxVisible = 6 // 下拉可视行数，超出滚动

// suggRows 是下拉占用的屏幕行数（含空态提示行）。
func (m *Model) suggRows() int {
	if n := len(m.suggs); n > 0 {
		return min(n, suggMaxVisible)
	}
	if m.suggNote != "" {
		return 1
	}
	return 0
}

func (m *Model) suggView() string {
	if len(m.suggs) == 0 {
		return "  " + faintStyle.Render(m.suggNote)
	}
	n := min(len(m.suggs), suggMaxVisible)
	// 选中项保持在可视窗口中部（与 CC 相同的窗口滚动策略）。
	start := max(0, min(m.suggSel-suggMaxVisible/2, len(m.suggs)-n))
	lw := 0
	for _, s := range m.suggs {
		lw = max(lw, ansi.StringWidth(s.label))
	}
	lw = min(lw+2, max(m.width*2/5, 10))
	rows := make([]string, 0, n)
	for i := start; i < start+n; i++ {
		rows = append(rows, m.suggRow(m.suggs[i], i == m.suggSel, lw))
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// suggRow 渲染一行建议：▸ 标记选中；命令/技能/会话分别以
// 默认色/紫/青着色标签列，描述列置灰，选中项整行提亮。
func (m *Model) suggRow(s suggestion, sel bool, lw int) string {
	marker, lst, dst := "  ", fgStyle, faintStyle
	switch s.kind {
	case suggSkill:
		lst = subStyle
	case suggSession:
		lst = iconStyle
	case suggFile:
		lst = fgStyle
	}
	if sel {
		marker, dst = "▸ ", fgStyle
		lst = lipgloss.NewStyle().Foreground(cCyan).Bold(true)
	}
	label := ansi.Truncate(s.label, lw-2, "…")
	// 文件路径把目录部分压灰，只突出基名
	if s.kind == suggFile {
		if i := strings.LastIndexByte(label, '/'); i >= 0 {
			label = faintStyle.Render(label[:i+1]) + lst.Render(label[i+1:])
		} else {
			label = lst.Render(label)
		}
	} else {
		label = lst.Render(label)
	}
	pad := max(lw-ansi.StringWidth(ansi.Strip(label)), 1)
	row := "  " + marker + label + strings.Repeat(" ", pad)
	if s.desc != "" {
		row += dst.Render(ansi.Truncate(strings.Join(strings.Fields(s.desc), " "), m.width, "…"))
	}
	return ansi.Truncate(row, m.width-1, "…")
}
