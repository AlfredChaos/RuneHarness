// Package tui 提供基于 Bubble Tea 的终端界面（OPS DECK 布局：
// 顶部会话条 + 对话流 + 右侧 dock + 状态行 + 权限带 + 多行输入框）。
package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"runeharness/internal/agent"
	"runeharness/internal/compact"
	"runeharness/internal/memory"
	"runeharness/internal/scope"
	"runeharness/internal/session"
	"runeharness/internal/skill"
	"runeharness/internal/subagent"
	"runeharness/internal/todo"
)

// ToolCallMsg 由 Agent.OnToolCall 回调经 Program.Send 注入，展示一次工具调用。
type ToolCallMsg struct {
	Call agent.ToolCall
}

// ToolResultMsg 由 Agent.OnToolResult 回调经 Program.Send 注入，
// 给工具行补结果摘要（ok/✘ + 耗时 + 行数）。
type ToolResultMsg struct {
	Res agent.ToolResult
}

// PartialMsg 是流式输出的累积快照，经 Program.Send 注入。
type PartialMsg = agent.Partial

// PermRequestMsg 由权限 hook 的 Ask 回调经 Program.Send 注入。
// Seq/Total 是本批 Ask 中的编号与总数（每批从 1 重新计数）；
// Depth 是发起请求的代理深度（0=主代理，>0=子代理）；
// Reply 是容量为 1 的通道：TUI 答复后写入，agent goroutine 按序读取。
type PermRequestMsg struct {
	Call   agent.ToolCall
	Reason string
	Seq    int
	Total  int
	Depth  int
	Reply  chan bool
}

// SubagentMsg 由 task 工具的 OnEvent 回调经 Program.Send 注入，
// 展示子代理生命周期（spawn / 内部工具调用 / done）。
type SubagentMsg struct{ Event subagent.Event }

// InjectMsg 由 Agent.OnInject 回调经 Program.Send 注入，
// 展示 hook 注入的消息（todo nag、Stop 续跑提示等）。
type InjectMsg struct{ Content string }

// TodoMsg 由 todo.Manager.OnChange 回调经 Program.Send 注入，
// 携带当前完整 TODO 列表（写入成功的快照）。
type TodoMsg struct{ Items []todo.Item }

// MemoryMsg 由 memory.OnChange 回调经 Program.Send 注入，
// 在后台 dream 落库后给对话区补一行提示。
type MemoryMsg struct{ Stats memory.Stats }

// dreamDoneMsg 是 /dream 手动消化的完成信号（内部）。
type dreamDoneMsg struct {
	stats memory.Stats
	err   error
}

// clockTickMsg 驱动顶栏时钟与空闲时的重绘。
type clockTickMsg time.Time

// Info 是装配进顶栏与 SESSION 卡的会话元数据，由 main.go 收集。
type Info struct {
	Model   string
	Cwd     string
	Branch  string // git 分支，可为空
	Session string // 会话短 ID
	Tools   int    // 注册的工具数
}

// todoPinMode 是 /todo 命令对任务卡片的显隐覆盖。
// 默认 auto 跟随自动规则；手动覆盖在当前对话轮次内有效，
// 用户提交新输入（submit）时复位为 auto——保证"有未完成项就挂出"
// 这条默认规则在每一轮都成立。
type todoPinMode int

const (
	todoPinAuto todoPinMode = iota // 自动：存在未完成项时显示
	todoPinShow                    // /todo 钉住显示
	todoPinHide                    // /todo 强制隐藏
)

// subTrack 追踪一个子代理实例在 AGENTS 卡里的状态。
type subTrack struct {
	seq   int
	depth int
	typ   string
	start time.Time
	tool  string // 最近一次内部工具名
	done  bool
	err   bool
	dur   time.Duration
}

// turnDoneMsg 是一轮 Agent.Run 的完成信号。
type turnDoneMsg struct {
	history []agent.Message
	err     error
}

// compactDoneMsg 是 /compact 的完成信号；history 总是当前有效视图
// （失败时可能已完成卸载），一律采用。
type compactDoneMsg struct {
	history []agent.Message
	pre     int
	err     error
}

// manualCompactor 是 /compact 需要的压缩能力（compact.Compactor 满足）。
type manualCompactor interface {
	CompactWith(ctx context.Context, history []agent.Message, extra string) ([]agent.Message, error)
}

const (
	inputMinH = 1 // 输入框最小行高（内容行数）
	inputMaxH = 5 // 输入框最大行高，超出后内部滚动
)

// Model 是 Bubble Tea 的程序状态。
type Model struct {
	agent   *agent.Agent
	info    Info
	history []agent.Message

	// scope/store 承载会话持久化：submit/resume 的 ctx 由 scope 注入，
	// TUI 侧追加进 history 的消息（user 输入、续跑 notice）经 store 落库；
	// store 为 nil（测试）时跳过记录。
	sc    scope.Scope
	store session.Store
	mem   *memory.Memory // nil 表示记忆层未启用（/dream 与 /memory 降级提示）

	viewport   viewport.Model
	input      textarea.Model
	spinner    spinner.Model
	transcript string
	pending    string        // 流式输出中的未提交块（纯文本渲染）
	partial    agent.Partial // pending 的原始内容，提交时按 Markdown 重渲染
	mdStyle    string        // glamour 主题，启动时按终端背景确定
	cancel     context.CancelFunc

	permQueue []PermRequestMsg // 待答复的权限确认队列
	todos     []todo.Item      // 当前 TODO 列表快照（来自 TodoMsg）
	todoPin   todoPinMode      // /todo 对任务卡片的手动显隐覆盖

	// "/" 下拉建议：suggs 是当前列表，suggSel 选中项；suggFor 记录列表
	// 对应的输入值——值不变不重算（也是 Esc 关闭的抑制位），suggNote
	// 是列表为空时的占位提示（如 "no matching sessions"）。
	skills   []skill.Meta
	suggs    []suggestion
	suggSel  int
	suggFor  string
	suggNote string

	// @ 文件建议的 workspace 文件清单缓存（TTL 见 fileListTTL）。
	fileIndex   []string
	fileIndexAt time.Time

	// subSeq/subIDs 给子代理编全局序号：事件只带深度，同深度的两次 spawn
	// 若都显示 subN(depth) 会误读成同一实例。同步执行保证同深度不并发。
	subSeq int
	subIDs map[int]int
	subs   map[int]*subTrack // depth → AGENTS 卡状态

	busy      bool
	busySince time.Time // 本轮开始时间，状态栏显示耗时
	activity  string    // busy 时正在等待/进行的事项（streaming / tool 名 / subN）
	// autoResumed 标记本轮是否已因撞步数上限自动续跑过一次；
	// 每次用户提交（submit）复位。
	autoResumed bool
	ready       bool

	// SESSION 卡指标：msgs 在 turnDone 时取 len(history)；
	// ctxTokens 是发送形态的 token 估算（与压缩阈值同一口径，不含 Thinking）。
	turns     int
	msgs      int
	ctxTokens int
	started   time.Time

	width  int
	height int
}

// New 创建界面；init 是初始对话历史（新会话=system prompt 一条，
// resume=LoadHistory 回放的全量），sc/st 承载会话 scope 与落库通道。
// skills 是启动扫描到的技能目录，供 "/" 下拉列出并发起技能调用。
// mem 是记忆层编排器（nil 表示未启用），供 /dream 与 /memory 命令调用。
func New(a *agent.Agent, info Info, init []agent.Message, sc scope.Scope, st session.Store, skills []skill.Meta, mem *memory.Memory) *Model {
	ta := textarea.New()
	ta.Prompt = "❯ "
	ta.Placeholder = "Message…"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(inputMinH)
	// Enter 留给提交；换行改绑 ctrl+j
	ta.KeyMap.InsertNewline.SetKeys("ctrl+j")
	ta.Focus()

	return &Model{
		agent:   a,
		info:    info,
		history: init,
		sc:      sc,
		store:   st,
		mem:     mem,
		skills:  skills,
		input:   ta,
		mdStyle: detectMarkdownStyle(),
		spinner: spinner.New(spinner.WithSpinner(spinner.Dot), spinner.WithStyle(spinnerStyle)),
		subIDs:  map[int]int{},
		subs:    map[int]*subTrack{},
		started: time.Now(),
	}
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, tickClock())
}

func tickClock() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return clockTickMsg(t) })
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		return m, nil
	case tea.KeyMsg:
		return m.onKey(msg)
	case clockTickMsg:
		// 时钟与状态行耗时每秒刷新；不转发给 viewport（无滚动语义）。
		return m, tickClock()
	case ToolCallMsg:
		// 带 tool_calls 的 assistant 消息的正文此前只活在 pending 里，
		// 会被下一步流式输出覆盖而从未落盘——工具开始执行时把它提交进
		// 对话区，避免"边想边干"的叙述文本丢失。
		if m.pending != "" {
			m.appendBlock(m.renderPartialRows(m.partial, m.mdStyle))
			m.pending, m.partial = "", agent.Partial{}
		}
		m.activity = "ran " + msg.Call.Name
		m.appendLine(m.rowTool(msg.Call))
		return m, nil
	case ToolResultMsg:
		m.appendLine(m.rowToolResult(msg.Res))
		return m, nil
	case PartialMsg:
		m.activity = "streaming model output"
		// 流式阶段按纯文本显示：未闭合的 Markdown/mermaid 渲染出来会反复跳变，
		// 且每个分片都整段重渲染开销大；块提交时再按 Markdown 渲染。
		m.partial = agent.Partial(msg)
		m.pending = m.renderPartialRows(m.partial, "")
		m.refreshViewport()
		return m, nil
	case InjectMsg:
		m.appendLine(m.rowHook(msg.Content))
		return m, nil
	case SubagentMsg:
		m.trackSubagent(msg.Event)
		m.appendLine(m.rowSub(msg.Event))
		return m, nil
	case TodoMsg:
		m.todos = msg.Items
		if m.ready && !m.dockOn() {
			m.layout() // 窄屏降级条占据高度，需重排
		}
		return m, nil
	case PermRequestMsg:
		m.permQueue = append(m.permQueue, msg)
		if len(m.permQueue) == 1 && m.ready {
			m.layout() // 确认框出现，viewport 收缩让位
		}
		return m, nil
	case turnDoneMsg:
		m.busy = false
		m.cancel = nil
		m.pending, m.partial = "", agent.Partial{}
		m.activity = ""
		if len(msg.history) > 0 {
			m.msgs = len(msg.history)
			m.ctxTokens = compact.Estimate(msg.history, 0)
		}
		var capErr *agent.MaxStepsError
		var intErr *agent.InterruptedError
		if errors.As(msg.err, &intErr) && len(msg.history) > 0 {
			// 中断：未执行的 tool_calls 已补占位结果，保留部分进度。
			// 不自动续跑——中断是用户主动发起的。
			m.history = msg.history
			m.appendLine(m.rowNote("⏹", "interrupted — partial progress kept"))
		} else if errors.As(msg.err, &capErr) && len(msg.history) > 0 {
			// 步数上限：保留部分进度。首次撞墙自动注入提示续跑一轮；
			// 再次撞墙则按普通错误抛出，由用户手动 "continue" 再续。
			m.history = msg.history
			if !m.autoResumed {
				m.appendLine(m.rowNote("⏰", fmt.Sprintf(
					"tool step budget %d reached — resuming with a notice", capErr.Steps)))
				return m, m.resume(capErr.Steps)
			}
			m.appendBlock(m.rowNote("✘", "error: "+msg.err.Error()+
				" — send 'continue' to extend manually"))
		} else if msg.err != nil {
			m.appendBlock(m.rowNote("✘", "error: "+msg.err.Error()))
		} else {
			m.history = msg.history
			last := msg.history[len(msg.history)-1]
			now := time.Now()
			if last.Thinking != "" {
				m.appendBlock(m.rowThink(last.Thinking, now))
			}
			if last.Content != "" {
				m.appendBlock(m.rowAgent(renderMarkdown(last.Content, m.mdStyle, m.feedW()-tagW-gutterW), now))
			} else if last.Thinking == "" {
				// 正文可能已随上一条带 tool_calls 的消息提交过；
				// 尾部空消息不再渲染裸标签，只留一条可诊断的痕迹
				m.appendLine(m.rowNote("·", "empty final response"))
			}
		}
		return m, nil
	case MemoryMsg:
		st := msg.Stats
		m.appendLine(m.rowNote("✎", fmt.Sprintf("memory updated: %d ops, %d merged",
			st.OpsApplied, st.TidyMerged)))
		return m, nil
	case dreamDoneMsg:
		m.busy, m.cancel, m.activity = false, nil, ""
		switch {
		case msg.err != nil:
			m.appendLine(m.rowNote("✘", "dream failed: "+msg.err.Error()))
		case msg.stats.Skipped != "":
			m.appendLine(m.rowNote("·", "dream skipped: "+msg.stats.Skipped))
		default:
			m.appendLine(m.rowNote("✦", fmt.Sprintf(
				"dreamed: %d sessions, %d rows → %d ops, %d merged",
				msg.stats.Sessions, msg.stats.Rows, msg.stats.OpsApplied, msg.stats.TidyMerged)))
		}
		m.layout()
		return m, nil
	case compactDoneMsg:
		m.busy, m.cancel, m.activity = false, nil, ""
		if len(msg.history) > 0 {
			m.history = msg.history
			m.msgs = len(msg.history)
			m.ctxTokens = compact.Estimate(msg.history, 0)
		}
		if msg.err != nil {
			m.appendLine(m.rowNote("✘", "compact failed: "+msg.err.Error()))
		} else {
			m.appendLine(m.rowNote("⇲", fmt.Sprintf("context compacted · ≈%.1fk → ≈%.1fk tokens",
				float64(msg.pre)/1000, float64(m.ctxTokens)/1000)))
		}
		m.layout()
		return m, nil
	case spinner.TickMsg:
		if !m.busy {
			return m, nil
		}
		var c tea.Cmd
		m.spinner, c = m.spinner.Update(msg)
		return m, c
	default:
		// 鼠标滚轮等其余消息交给 viewport（含 busy 时的滚动）
		var c tea.Cmd
		m.viewport, c = m.viewport.Update(msg)
		return m, c
	}
}

func (m *Model) onKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC, tea.KeyCtrlD:
		if m.cancel != nil {
			m.cancel()
		}
		return m, tea.Quit
	}

	// 滚动键任何时候都生效（up/down 在下拉激活时被建议导航接管，见下）
	switch msg.String() {
	case "pgup", "pgdown", "ctrl+u", "ctrl+f":
		var c tea.Cmd
		m.viewport, c = m.viewport.Update(msg)
		return m, c
	}

	// 权限确认优先于 busy 拦截：逐条答复队列中的请求。
	if len(m.permQueue) > 0 {
		switch msg.String() {
		case "y", "Y":
			m.answerPerm(true)
		case "n", "N", "esc":
			m.answerPerm(false)
		}
		return m, nil
	}

	if m.busy {
		// Esc 中断当前轮：cancel 后 agent 在下一个检查点收 ctx.Err，
		// 未执行的 tool_calls 补占位结果、返回 InterruptedError。
		if msg.String() == "esc" && m.cancel != nil {
			m.cancel()
			m.activity = "interrupting"
			m.layout()
		}
		return m, nil
	}

	// "/" 下拉激活时接管导航键：↑↓ 移动选中、Tab 补全、Enter 应用、
	// Esc 关闭本次下拉；其余键照常进入输入框，列表随输入实时重算。
	if len(m.suggs) > 0 {
		switch msg.String() {
		case "up":
			m.suggSel = (m.suggSel - 1 + len(m.suggs)) % len(m.suggs)
			return m, nil
		case "down":
			m.suggSel = (m.suggSel + 1) % len(m.suggs)
			return m, nil
		case "tab":
			return m, m.applySuggestion(false)
		case "esc":
			m.suggs, m.suggNote = nil, ""
			m.suggFor = m.suggKey()
			m.layout()
			return m, nil
		case "enter":
			return m, m.applySuggestion(true)
		}
	} else if m.suggNote != "" && msg.String() == "esc" {
		// 空态提示（无匹配项）也可 Esc 关闭；Enter 不拦，照常提交。
		m.suggNote = ""
		m.suggFor = m.suggKey()
		m.layout()
		return m, nil
	}

	// 下拉关闭时 up/down 照旧滚动对话区
	if k := msg.String(); k == "up" || k == "down" {
		var c tea.Cmd
		m.viewport, c = m.viewport.Update(msg)
		return m, c
	}
	if msg.Type == tea.KeyEnter {
		return m, m.submit()
	}
	var c tea.Cmd
	m.input, c = m.input.Update(msg)
	m.fitInput()
	m.updateSuggestions()
	return m, c
}

// submit 处理一次回车：斜杠命令走命令分发，其余作为用户消息异步发起 agent 循环。
func (m *Model) submit() tea.Cmd {
	input := strings.TrimSpace(m.input.Value())
	if input == "" {
		return nil
	}
	m.input.Reset()
	m.suggs, m.suggNote, m.suggFor = nil, "", ""
	m.fitInput()
	if strings.HasPrefix(input, "/") {
		return m.runSlash(input)
	}
	return m.sendUserText(input, input)
}

// sendUserText 把一条用户消息送进 agent 循环：display 渲染进对话区，
// content 经 UserPromptSubmit hook 后作为实际发送内容——两者分离使
// /技能 调用可以把斜杠形式展开为指令文本而对话区保留原始输入。
func (m *Model) sendUserText(display, content string) tea.Cmd {
	m.turns++
	m.pending, m.partial = "", agent.Partial{}
	// 对话区展示用户实际输入；hook 改写只作用于发给模型的内容。
	m.appendBlock(m.rowUser(display))

	// @path 引用展开为附件内容块随消息发出；附件在对话区折成一行清单。
	content, attached := m.expandMentions(content)
	if len(attached) > 0 {
		m.appendLine(m.rowNote("⚲", "attached "+strings.Join(attached, " · ")))
	}

	ctx, cancel := context.WithCancel(scope.WithScope(context.Background(), m.sc))
	replace, blocked := m.agent.Hooks.TriggerUserPromptSubmit(ctx, content)
	if blocked != "" {
		cancel()
		m.appendLine(m.rowNote("✘", "input blocked: "+blocked))
		return nil
	}
	if replace != "" {
		content = replace
	}
	m.history = append(m.history, agent.Message{Role: agent.RoleUser, Content: content})
	if err := m.record(ctx, &m.history[len(m.history)-1]); err != nil {
		cancel()
		m.appendLine(m.rowNote("✘", "session append failed: "+err.Error()))
		return nil
	}
	m.todoPin = todoPinAuto // 新一轮输入复位手动显隐，回到自动规则
	m.autoResumed = false   // 新一轮输入恢复一次自动续跑额度
	m.layout()
	m.busy = true
	m.busySince = time.Now()
	m.activity = "waiting for model"
	m.cancel = cancel
	history := m.history
	run := func() tea.Msg {
		next, err := m.agent.Run(ctx, history)
		return turnDoneMsg{history: next, err: err}
	}
	return tea.Batch(m.spinner.Tick, run)
}

// resume 在撞步数上限后自动续跑：把上限事实注入为 user 消息，让模型自行
// 决定继续剩余调用，还是改用批量/更少步骤的路径。新一次 Run 拥有全新的
// 步数预算；history 已含部分进度，模型在既有上下文上继续。
func (m *Model) resume(steps int) tea.Cmd {
	m.autoResumed = true
	notice := fmt.Sprintf("<notice>The tool-call budget of %d steps was just exhausted. "+
		"A fresh budget is granted now — continue the pending work, or replan with "+
		"fewer, batched tool calls.</notice>", steps)
	ctx, cancel := context.WithCancel(scope.WithScope(context.Background(), m.sc))
	m.history = append(m.history, agent.Message{Role: agent.RoleUser, Kind: agent.KindInject, Content: notice})
	m.appendLine(m.rowHook(notice))
	if err := m.record(ctx, &m.history[len(m.history)-1]); err != nil {
		cancel()
		m.appendLine(m.rowNote("✘", "session append failed: "+err.Error()))
		return nil
	}
	m.busy = true
	m.busySince = time.Now()
	m.activity = "waiting for model"
	m.cancel = cancel
	history := m.history
	return tea.Batch(m.spinner.Tick, func() tea.Msg {
		next, err := m.agent.Run(ctx, history)
		return turnDoneMsg{history: next, err: err}
	})
}

// record 把 TUI 侧追加进 history 的消息落库；store 为 nil 时不记录，
// 失败即由调用方终止本轮（与 loop 的 fail-the-turn 语义一致）。
// 分配的行 id 写回 msg：压缩决策按行 id 引用消息。
func (m *Model) record(ctx context.Context, msg *agent.Message) error {
	if m.store == nil {
		return nil
	}
	id, err := m.store.Append(ctx, *msg)
	msg.ID = id
	return err
}

// runSlash 执行 / 开头的输入：系统命令表优先，未命中再按技能名精确
// 匹配（展开为技能调用消息），都不命中报 unknown command。
func (m *Model) runSlash(input string) tea.Cmd {
	name, arg, _ := strings.Cut(strings.TrimPrefix(input, "/"), " ")
	arg = strings.TrimSpace(arg)
	switch name {
	case "exit", "quit":
		if m.cancel != nil {
			m.cancel()
		}
		return tea.Quit
	case "todo":
		// 显隐开关：翻转任务卡片的可见性，不回显到对话区。
		// 列表为空时无卡可切，提示即可。
		if len(m.todos) == 0 {
			m.appendLine(m.rowNote("✘", "no todos yet"))
			return nil
		}
		if m.todoVisible() {
			m.todoPin = todoPinHide
		} else {
			m.todoPin = todoPinShow
		}
		m.layout()
		return nil
	case "resume":
		return m.runResume(arg)
	case "compact":
		return m.runCompact(arg)
	case "dream":
		return m.runDream()
	case "memory":
		return m.runMemory()
	default:
		if s := m.findSkill(name); s != nil {
			return m.sendUserText(input, skillPromptText(*s, arg))
		}
		m.appendLine(m.rowNote("✘", "unknown command: "+input))
		return nil
	}
}

// runCompact 执行 /compact [指令]：轮间直接调压缩器（reason=manual），
// 熔断断开时也执行，作为人工试探（plan §4.8）。
func (m *Model) runCompact(extra string) tea.Cmd {
	cm, ok := m.agent.Compactor.(manualCompactor)
	if !ok {
		m.appendLine(m.rowNote("✘", "compaction is not configured"))
		return nil
	}
	ctx, cancel := context.WithCancel(scope.WithScope(context.Background(), m.sc))
	m.busy, m.busySince, m.activity, m.cancel = true, time.Now(), "compacting", cancel
	m.layout()
	history, pre := m.history, compact.Estimate(m.history, 0)
	return tea.Batch(m.spinner.Tick, func() tea.Msg {
		next, err := cm.CompactWith(ctx, history, extra)
		return compactDoneMsg{history: next, pre: pre, err: err}
	})
}

// runDream 执行 /dream：轮间手动触发记忆消化（manual 只过锁）。消化在
// 后台 goroutine 跑，完成经 dreamDoneMsg 回报。
func (m *Model) runDream() tea.Cmd {
	if m.mem == nil {
		m.appendLine(m.rowNote("✘", "memory is not enabled"))
		return nil
	}
	if m.busy {
		m.appendLine(m.rowNote("·", "busy — try /dream again when idle"))
		return nil
	}
	ctx, cancel := context.WithCancel(scope.WithScope(context.Background(), m.sc))
	m.busy, m.busySince, m.activity, m.cancel = true, time.Now(), "dreaming", cancel
	m.layout()
	return tea.Batch(m.spinner.Tick, func() tea.Msg {
		st, err := m.mem.Dream(ctx, true)
		return dreamDoneMsg{stats: st, err: err}
	})
}

// runMemory 执行 /memory：把当前空间的记忆索引打进对话区（本地查询，不阻塞）。
func (m *Model) runMemory() tea.Cmd {
	if m.mem == nil {
		m.appendLine(m.rowNote("✘", "memory is not enabled"))
		return nil
	}
	ctx := scope.WithScope(context.Background(), m.sc)
	txt, err := m.mem.IndexText(ctx)
	if err != nil {
		m.appendLine(m.rowNote("✘", "memory: "+err.Error()))
		return nil
	}
	m.appendBlock(dimStyle.Render(txt))
	return nil
}

// trackSubagent 按子代理事件更新状态栏 activity 与 AGENTS 卡状态
// （子代理运行时主代理处于阻塞等待，状态栏只显示 "subN running"
// 能直接看出卡点在哪一层）。EventSpawn 时分配全局序号，
// 使"subN"指代实例而非深度。
func (m *Model) trackSubagent(ev subagent.Event) {
	switch ev.Kind {
	case subagent.EventSpawn:
		m.subSeq++
		m.subIDs[ev.Depth] = m.subSeq
		m.subs[ev.Depth] = &subTrack{
			seq: m.subSeq, depth: ev.Depth, typ: string(ev.Type), start: time.Now(),
		}
		m.activity = fmt.Sprintf("sub%d running", m.subSeq)
	case subagent.EventToolCall:
		m.activity = fmt.Sprintf("sub%d · %s", m.subLabel(ev.Depth), ev.Call.Name)
		if s, ok := m.subs[ev.Depth]; ok {
			s.tool = ev.Call.Name
		}
	default: // EventDone
		m.activity = "waiting for model"
		if s, ok := m.subs[ev.Depth]; ok {
			s.done = true
			s.err = ev.Err != nil
			s.dur = time.Since(s.start)
		}
	}
}

// subLabel 返回深度 depth 上当前子代理的序号；无记录时退回深度值。
func (m *Model) subLabel(depth int) int {
	if s, ok := m.subIDs[depth]; ok {
		return s
	}
	return depth
}

// answerPerm 答复队首的确认请求并出队，结果追加到对话区留痕。
func (m *Model) answerPerm(ok bool) {
	req := m.permQueue[0]
	m.permQueue = m.permQueue[1:]
	req.Reply <- ok
	m.appendLine(m.rowVerdict(req.Call, ok, req.Depth))
	if len(m.permQueue) == 0 {
		m.layout() // 确认框消失，viewport 恢复高度
	}
}

func (m *Model) resize(w, h int) {
	m.width, m.height = w, h
	m.viewport.Width = m.feedW()
	m.layout()
	m.input.SetWidth(w - 4)
	m.ready = true
}

// layout 按当前是否有待确认的权限请求、可见的 TODO 摘要条重算 viewport 高度。
// dock 显示时 TODO 在右栏，不占对话区高度。
func (m *Model) layout() {
	h := m.height - 1 - 1     // topbar + statusLine
	h -= m.input.Height() + 2 // 输入框内容行 + 上下边框
	if !m.dockOn() && m.todoVisible() {
		h-- // 窄屏 TODO 摘要条
	}
	if len(m.permQueue) > 0 {
		h -= permBoxHeight
	}
	h -= m.suggRows() // "/" 下拉建议
	m.viewport.Height = max(h, 1)
}

// fitInput 按输入内容行数调整输入框高度（1~5 行，超出滚动）。
func (m *Model) fitInput() {
	w := m.input.Width()
	if w < 10 {
		w = 40
	}
	n := 0
	for _, ln := range strings.Split(m.input.Value(), "\n") {
		n += max(1, (ansi.StringWidth(ln)+w-1)/w)
	}
	n = min(max(n, inputMinH), inputMaxH)
	if n != m.input.Height() {
		m.input.SetHeight(n)
		m.layout()
	}
}

// todoVisible 报告任务卡片/摘要条是否可见：手动覆盖（pinShow/pinHide）优先，
// 否则跟随自动规则——列表非空且存在未完成项时可见。
func (m *Model) todoVisible() bool {
	if len(m.todos) == 0 {
		return false
	}
	switch m.todoPin {
	case todoPinShow:
		return true
	case todoPinHide:
		return false
	}
	for _, t := range m.todos {
		if t.Status != "completed" {
			return true
		}
	}
	return false
}

// appendBlock 追加一个对话块（与前文空一行分隔）并刷新滚动区。
func (m *Model) appendBlock(s string) { m.append(s, "\n\n") }

// appendLine 追加一行（工具调用等紧凑内容）并刷新滚动区。
func (m *Model) appendLine(s string) { m.append(s, "\n") }

func (m *Model) append(s, sep string) {
	if m.transcript != "" {
		m.transcript += sep
	}
	m.transcript += s
	m.refreshViewport()
}

// refreshViewport 渲染 transcript + 流式中的 pending 块。
func (m *Model) refreshViewport() {
	content := m.transcript
	if m.pending != "" {
		if content != "" {
			content += "\n\n"
		}
		content += m.pending
	}
	m.viewport.SetContent(content)
	m.viewport.GotoBottom()
}
