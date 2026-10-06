package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"runeharness/internal/agent"
	"runeharness/internal/breaker"
	"runeharness/internal/compact"
	"runeharness/internal/config"
	"runeharness/internal/llm"
	"runeharness/internal/permission"
	"runeharness/internal/prompt"
	"runeharness/internal/scope"
	"runeharness/internal/session"
	"runeharness/internal/session/sqlite"
	"runeharness/internal/skill"
	"runeharness/internal/subagent"
	"runeharness/internal/todo"
	"runeharness/internal/tools"
	"runeharness/internal/tui"
)

const maxToolSteps = 50

// gitBranch 尽力取当前分支名；非仓库或无 git 时返回空串。
func gitBranch() string {
	out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// localTenant 是本地单用户形态的租户 ID；云端部署时由请求入口按 auth 替换。
const localTenant = "local"

func main() {
	resumeID := flag.String("resume", "", "resume session by id")
	cont := flag.Bool("continue", false, "continue the most recent session")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	model := llm.NewClient(cfg.APIKey, cfg.BaseURL, cfg.Model)
	todos := todo.NewManager()

	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	env := prompt.Env{Workspace: wd, Platform: runtime.GOOS + "/" + runtime.GOARCH}
	// 技能目录第一层：启动时扫描 ~/.agents/skills，把每个 SKILL.md 的
	// 元数据目录注入 system prompt；正文不进 prompt，由模型命中后用
	// 既有 read_file 按目录里的路径自取。目录缺失时静默跳过。
	// 同一份列表也喂给 TUI 的 "/" 下拉，供用户显式发起技能调用。
	var skills []skill.Meta
	if home, err := os.UserHomeDir(); err == nil {
		skills = skill.Scan(filepath.Join(home, ".agents", "skills"))
		env.Skills = skill.Catalog(skills)
	}

	// 会话库：打不开属于装配错误，启动即死，不做半拉子记录。
	dbPath := cfg.DBPath
	if dbPath == "" {
		if dbPath, err = config.DefaultDBPath(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	store, err := sqlite.Open(dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	defer store.Close()
	// TUI 占着终端，日志改写到会话库旁边的 rune.log；打不开就退回 stderr。
	if f, err := os.OpenFile(filepath.Join(filepath.Dir(dbPath), "rune.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		slog.SetDefault(slog.New(slog.NewTextHandler(f, nil)))
	}
	// 信任根：tenant+workspace 在此确定，SessionID 待会话确定后焊入。
	baseCtx := scope.WithScope(context.Background(),
		scope.Scope{TenantID: localTenant, Workspace: wd})

	// p 先声明后赋值：askAt / task.OnEvent 闭包捕获它，Send 只在 agent
	// 运行期间触发（那时已 NewProgram），不会命中 nil。
	var p *tea.Program
	// askAt 生成指定深度代理的权限确认投递函数；depth>0 时 TUI 标注来源子代理。
	askAt := func(depth int) func(agent.ToolCall, string, int, int) <-chan bool {
		return func(tc agent.ToolCall, reason string, seq, total int) <-chan bool {
			reply := make(chan bool, 1)
			p.Send(tui.PermRequestMsg{Call: tc, Reason: reason, Seq: seq, Total: total, Depth: depth, Reply: reply})
			return reply
		}
	}

	// 权限名单暂时集中在启动代码里；稳定后迁到配置文件加载。
	// SafeTools 必须与工具 Spec().Name 一致，改名会静默失效，下方有校验。
	// 注意 go run 不在 safe 前缀中——它执行任意代码，等同于任意命令。
	permCfg := permission.Config{
		// todo_write 只写进程内存，属于安全工具，不弹确认。
		SafeTools: []string{"get_current_time", "list_dir", "read_file", todo.ToolName},
		SafeCommandPrefixes: []string{
			"ls", "pwd", "echo", "cat", "head", "grep", "wc",
			"date", "whoami", "hostname", "which",
			"go version", "go build", "go vet", "go test",
			"git status", "git log", "git diff", "git show",
		},
		ForbiddenPatterns: []string{
			"rm -rf", "rm -fr", "sudo ", "mkfs", "dd if=", "shutdown",
			"reboot", ":(){", "chmod -R 777", "> /dev/", "diskutil",
		},
		ForbiddenPaths: []string{
			".env", "id_rsa", ".pem", ".key", "credentials", "/etc/shadow", ".aws",
		},
	}
	baseTools := []tools.Tool{
		tools.CurrentTime{},
		tools.ListDir{},
		tools.ReadFile{Blobs: store},
		tools.RunCommand{},
		todos,
	}
	toolSet := slices.Clone(baseTools)
	if cfg.SubAgent {
		// task 本身不弹确认：spawn 子代理没有直接副作用，
		// 子代理内部的每次工具调用仍逐个过权限闸。
		permCfg.SafeTools = append(permCfg.SafeTools, subagent.ToolName)
	}
	desk := permission.New(permCfg)

	if cfg.SubAgent {
		task := subagent.New(model, baseTools, subagent.DefaultPrompt, cfg.SubAgentNest)
		task.Store = store // 子代理会话落库：spawn 时挂 parent_id 建子会话
		task.CompactCfg = compact.Config{
			Window: cfg.ContextTokens, MaxOutput: cfg.MaxOutputTokens, Auto: cfg.AutoCompact,
		}
		// 子代理复用同一权限闸：上下文隔离 ≠ 权限隔离
		task.Wire = func(sub *agent.Agent, depth int) {
			sub.Hooks.OnPreToolUse(permission.NewHook(desk, askAt(depth)).Check)
			subBreaker := breaker.New(0, 0)
			sub.Hooks.OnPreToolUse(subBreaker.Check)
			sub.Hooks.OnPostToolUse(subBreaker.Observe)
			sub.Hooks.OnPreChat(subBreaker.Nag)
		}
		task.OnEvent = func(ev subagent.Event) { p.Send(tui.SubagentMsg{Event: ev}) }
		toolSet = append(toolSet, task)
	}
	// compact 是主代理专属工具：loop 按名特判，不进子代理的 baseTools。
	toolSet = append(toolSet, compact.Tool{})
	registry := tools.NewRegistry(toolSet...)
	for _, name := range permCfg.SafeTools {
		if !registry.Has(name) {
			fmt.Fprintf(os.Stderr, "warning: safe tool %q not registered\n", name)
		}
	}
	a := agent.New(model, registry, maxToolSteps)
	a.Rec, a.Requests = store, store // loop 逐条落库 + 每次请求留痕
	// 压缩器：thresholds/卸载/摘要/熔断都在它身上；Overhead 是每次请求
	// 随消息一起发送的工具清单，估算时要计入。
	specsJSON, _ := json.Marshal(registry.Specs())
	comp, err := compact.New(model, store, compact.Config{
		Window: cfg.ContextTokens, MaxOutput: cfg.MaxOutputTokens,
		Auto:     cfg.AutoCompact,
		Overhead: compact.TextTokens(string(specsJSON)),
		Reattach: todos.Snapshot,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	a.Compactor = comp
	// 工具段取自注册表：开启子代理时 task 自动出现在清单里，无需另行提示。
	env.Tools = registry.Specs()

	// 会话确定：--resume 精确回放 / --continue 最近会话 / 默认新建。
	// resume 不建新会话——续写同一 session，审计链不中断。
	sysMsg := agent.Message{Role: agent.RoleSystem, Content: prompt.Build(env)}
	initial := []agent.Message{sysMsg}
	var sessID string
	switch {
	case *resumeID != "":
		if initial, err = store.LoadHistory(baseCtx, *resumeID); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		sessID = *resumeID
	case *cont:
		var recent []session.Session
		if recent, err = store.ListSessions(baseCtx, 1); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if len(recent) > 0 {
			sessID = recent[0].ID
			if initial, err = store.LoadHistory(baseCtx, sessID); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
		}
	}
	if sessID == "" {
		// 新会话（含 --continue 空库退化）：进程启动即建，其后全部 append
		// 都进这条 session。
		var sess session.Session
		if sess, err = store.CreateSession(baseCtx, session.Meta{
			Model:     cfg.Model,
			Workspace: wd,
			Kind:      session.KindMain,
		}); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		sessID = sess.ID
		sessCtx := scope.WithSession(baseCtx, sessID)
		id, err := store.Append(sessCtx, sysMsg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		initial[0].ID = id
	}
	sessScope := scope.Scope{TenantID: localTenant, SessionID: sessID, Workspace: wd}

	p = tea.NewProgram(tui.New(a, tui.Info{
		Model:   cfg.Model,
		Cwd:     wd,
		Branch:  gitBranch(),
		Session: sessID[:8],
		Tools:   len(toolSet),
	}, initial, sessScope, store, skills),
		tea.WithAltScreen(),
	)
	// 工具调用与流式输出事件通过 Program.Send 桥接进 UI 循环
	a.OnToolCall = func(tc agent.ToolCall) {
		p.Send(tui.ToolCallMsg{Call: tc})
	}
	// 工具结果（含被 hook 阻止的调用）回报给 UI，给工具行补状态与耗时
	a.OnToolResult = func(r agent.ToolResult) {
		p.Send(tui.ToolResultMsg{Res: r})
	}
	a.OnPartial = func(pt agent.Partial) {
		p.Send(tui.PartialMsg(pt))
	}
	// hook 注入的消息（todo nag、压缩提醒等）经 Program.Send 在对话区留痕
	a.OnInject = func(msg agent.Message) {
		p.Send(tui.InjectMsg{Content: msg.Content})
	}
	// todo 列表每次写入后推给 TUI，驱动输入框上方的任务面板
	todos.OnChange = func(items []todo.Item) {
		p.Send(tui.TodoMsg{Items: items})
	}
	// 权限判定挂为 PreToolUse hook（主代理 depth=0）；确认请求经
	// Program.Send 投递进 UI 队列，通道容量为 1，答复永不阻塞 TUI。
	a.Hooks.OnPreToolUse(permission.NewHook(desk, askAt(0)).Check)
	// 退化调用拦截：PostToolUse 按签名计连续失败/重复结果/同名连败，
	// PreToolUse 命中即阻止，PreChat 注入连败提醒。排在权限闸之后。
	brk := breaker.New(0, 0)
	a.Hooks.OnPreToolUse(brk.Check)
	a.Hooks.OnPostToolUse(brk.Observe)
	a.Hooks.OnPreChat(brk.Nag)
	// 压缩提醒 hook 排在首位：用量过提醒线时每个压缩周期注入一次，
	// 让模型择机压缩（plan §4.4）。
	a.Hooks.OnPreChat(comp.ReminderHook())
	// todo nag：连续 3 轮未写 todo 时注入提醒（全完成则静默）；
	// nag 计数器随每轮用户输入归零，按对话轮次统计。
	a.Hooks.OnPreChat(todos.Nag)
	a.Hooks.OnUserPromptSubmit(func(context.Context, string) (string, string) {
		todos.ResetRounds()
		return "", ""
	})

	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
