// Command runebot 把 RuneHarness 装配成 headless 网关进程：
// 没有 TUI，外部客户端经 WS（bear 轨）或 HTTP/SSE 接入。
//
// 与 main.go（CLI）的关系：同一套 agent/tools/permission/compact/
// memory/cron 装配，差异只在前后台——CLI 把回调送给 Bubble Tea，
// 这里送给 runner 的每 chat 事件账本；Ask 不再是弹窗，而是路由到
// 该 chat 的客户端。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"runeharness/internal/agent"
	"runeharness/internal/bgtask"
	"runeharness/internal/breaker"
	"runeharness/internal/compact"
	"runeharness/internal/config"
	"runeharness/internal/cron"
	"runeharness/internal/gateway"
	"runeharness/internal/llm"
	"runeharness/internal/memory"
	"runeharness/internal/naming"
	"runeharness/internal/permission"
	"runeharness/internal/prompt"
	"runeharness/internal/runner"
	"runeharness/internal/scope"
	"runeharness/internal/session/sqlite"
	"runeharness/internal/skill"
	"runeharness/internal/subagent"
	"runeharness/internal/todo"
	"runeharness/internal/tools"

	// 容器运行时（alpine）没有 zoneinfo，time.LoadLocation 对任何命名时区
	// 都报 invalid（cron_create tz 参数实测踩中）；嵌入 tzdata 一劳永逸。
	_ "time/tzdata"
)

const maxToolSteps = 50

func main() {
	listen := flag.String("listen", envOr("RUNE_LISTEN", "127.0.0.1:8765"), "listen address")
	profile := flag.String("profile", envOr("RUNE_MEMORY_PROFILE", "generic"), "memory profile")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if cfg.MemoryProfile == "" {
		cfg.MemoryProfile = *profile
	}

	// 日志级别：RUNE_LOG_LEVEL 默认 info，debug 打开内部打点（plan §4）。
	lvl := slog.LevelInfo
	if strings.EqualFold(os.Getenv("RUNE_LOG_LEVEL"), "debug") {
		lvl = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})))

	// 认证：RUNE_TOKENS 静态表，格式 "tk1=tenant:subject,tk2=tenant2:"。
	tokens, err := gateway.StaticTokens(os.Getenv("RUNE_TOKENS"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	auth := gateway.StaticAuth(tokens)

	// 部署唯一租户：单租户部署里 RUNE_TOKENS 表的租户即本进程全部用户。
	// cron 链（调度器点火 scope、通知归属回落）都按它对齐——gwScope 的
	// "gateway" 是进程内部身份，与 durable 任务落盘时按请求租户打的标
	// 不一致会让任务永不点火、通知写成任何租户都查不到的孤儿。
	fallbackTenant := "local"
	for _, name := range slices.Sorted(maps.Keys(tokens)) {
		if t := tokens[name].TenantID; t != "" {
			fallbackTenant = t
			break
		}
	}

	wd, _ := os.Getwd()
	// 后台组件（dream/cron 点火）用系统租户 scope：跨租户枚举会话/任务，
	// 落库与通知按数据自带归属回填真实租户。HTTP 请求路径拿不到
	// scope.System()——auth 解析出的 Token 没有 System 位。
	gwScope := scope.SystemScope(wd)

	dbPath := cfg.DBPath
	if dbPath == "" {
		if dbPath, err = config.DefaultDBPath(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	}
	store, err := sqlite.Open(dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	defer store.Close()

	opts := llm.Opts{
		ReasoningEffort:     cfg.ReasoningEffort,
		ThinkingType:        cfg.ThinkingType,
		ReasoningSplit:      cfg.ReasoningSplit,
		Temperature:         cfg.Temperature,
		TopP:                cfg.TopP,
		MaxCompletionTokens: cfg.MaxCompletionTokens,
	}
	model := llm.NewClientTuned(cfg.APIKey, cfg.BaseURL, cfg.Model, opts)
	todos := todo.NewManager()

	env := prompt.Env{Workspace: wd, Platform: runtime.GOOS + "/" + runtime.GOARCH}
	env.Soul = prompt.LoadSoul(os.Getenv("RUNE_SOUL"))
	if home, err := os.UserHomeDir(); err == nil {
		env.Skills = skill.Catalog(skill.Scan(filepath.Join(home, ".agents", "skills")))
	}

	bgm := bgtask.New(filepath.Join(filepath.Dir(dbPath), "tasks"))

	// cron 持久化任务随 workspace；属主锁同 CLI 语义。触发时写一条通知
	// 进 outbox（ALF-133）：空闲 chat 不会被 Drain 唤醒，App 靠轮询弹窗。
	cronStore := cron.Open(filepath.Join(filepath.Dir(dbPath), "cron_tasks.json"))
	var cronSched *cron.Scheduler
	if cfg.Cron && cronStore != nil {
		// 调度器以部署唯一租户 + 进程 workspace 看任务（FileTasks/Remove
		// 都按 scope 过滤），与 durable 任务落盘打标一致才能点火/焚毁。
		// 调度器跑在 system scope：枚举全租户任务，点火时按任务自带
		// tenant 路由通知；fallbackTenant 只用于任务缺 tenant 的遗留数据。
		cronCtx := scope.WithScope(context.Background(), gwScope)
		cronSched = cron.NewScheduler(cronStore, cronCtx, cron.Config{
			LockPath: filepath.Join(filepath.Dir(dbPath), cron.LockFileName(wd)),
			OnFire: func(ev cron.Event) {
				t := ev.Task
				tenant := t.Tenant
				if tenant == "" {
					tenant = fallbackTenant // session/遗留任务缺租户：兜底归属
				}
				if _, err := store.PushNotification(context.Background(),
					tenant, "cron", cronNoticeTitle(t), t.Prompt); err != nil {
					slog.Warn("notify: cannot persist cron notification", "task", t.ID, "err", err)
				}
			},
		})
		go cronSched.Run(context.Background())
	}

	// 记忆层：dream 提取可换轻量模型，默认复用主模型。
	var mem *memory.Memory
	if cfg.Memory {
		mdbPath := cfg.MemoryDB
		if mdbPath == "" {
			if mdbPath, err = config.DefaultMemoryDBPath(); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
		}
		_ = os.MkdirAll(filepath.Dir(mdbPath), 0o755)
		var memLLM agent.LLM = model
		if cfg.MemoryModel != "" {
			// dream 复用同一组采样参数：reasoning_split 让思考不进正文，
			// 否则 <think> 会污染 ops 的 JSON 输出契约。
			memLLM = llm.NewClientTuned(cfg.APIKey, cfg.BaseURL, cfg.MemoryModel, opts)
		}
		if mem, err = memory.New(store, memLLM, memory.Config{
			DBPath:  mdbPath,
			Profile: memory.Lookup(cfg.MemoryProfile),
			Dream:   cfg.MemoryDream,
		}); err != nil {
			slog.Warn("memory disabled", "err", err)
			mem = nil
		} else {
			defer mem.Close()
			if cfg.MemoryDream {
				go mem.DreamTicker(scope.WithScope(context.Background(), gwScope))
			}
		}
	}

	// 权限闸与 CLI 同规则；ask 回调由 slot 提供（runner 路由到客户端）。
	permCfg := permission.Config{
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
		tools.CurrentTime{}, tools.ListDir{}, tools.ReadFile{Blobs: store},
		tools.RunCommand{BG: bgm}, todos,
	}
	toolSet := slices.Clone(baseTools)
	if cfg.SubAgent {
		permCfg.SafeTools = append(permCfg.SafeTools, subagent.ToolName)
	}
	toolSet = append(toolSet, bgtask.ListTool{M: bgm}, bgtask.KillTool{M: bgm})
	permCfg.SafeTools = append(permCfg.SafeTools, bgtask.ListToolName, bgtask.KillToolName)
	toolSet = append(toolSet, compact.Tool{})
	if cronStore != nil {
		toolSet = append(toolSet,
			cron.CreateTool{S: cronStore}, cron.ListTool{S: cronStore}, cron.DeleteTool{S: cronStore})
		permCfg.SafeTools = append(permCfg.SafeTools,
			cron.CreateToolName, cron.ListToolName, cron.DeleteToolName)
	}
	if mem != nil {
		if t := mem.Tool(); t != nil {
			toolSet = append(toolSet, t)
			permCfg.SafeTools = append(permCfg.SafeTools, memory.ToolName)
		}
	}
	desk := permission.New(permCfg)

	var taskTool *subagent.Tool
	if cfg.SubAgent {
		taskTool = subagent.New(model, baseTools, subagent.DefaultPrompt, cfg.SubAgentNest)
		taskTool.Store = store
		taskTool.CompactCfg = compact.Config{
			Window: cfg.ContextTokens, MaxOutput: cfg.MaxOutputTokens, Auto: cfg.AutoCompact,
		}
		toolSet = append(toolSet, taskTool)
	}
	registry := tools.NewRegistry(toolSet...)
	for _, name := range permCfg.SafeTools {
		if !registry.Has(name) {
			slog.Warn("safe tool not registered", "name", name)
		}
	}
	specsJSON, _ := json.Marshal(registry.Specs())

	// 启动配置一览：本次排查只能靠 kubectl+数据库反推生效配置，启动行
	// 把模型/窗口/思考/记忆/灵魂/工具钉在日志里。不打正文与凭据。
	slog.Info("runebot config",
		"model", cfg.Model,
		"reasoning_effort", cfg.ReasoningEffort,
		"thinking", cfg.ThinkingType,
		"reasoning_split", cfg.ReasoningSplit,
		"context_window", cfg.ContextTokens,
		"window_source", windowSource(),
		"memory_profile", cfg.MemoryProfile,
		"memory_model", envOr("MEMORY_MODEL", cfg.Model),
		"memory_db", envOr("MEMORY_DB", "~/.rune/memory.db"),
		"soul", soulInfo(),
		"tools", toolNames(registry),
		"auto_compact", cfg.AutoCompact,
	)

	// AI 命名（ALF-130）：成功轮次结束后异步生成 ≤12 字标题写回 sessions.title。
	namer := &naming.Namer{Store: store, LLM: model}

	// 压缩器全进程共享一个实例：compact 是会话无关的（熔断/游标全在
	// session_state，key 按 ctx 里的 session id），槽位间复用安全。
	comp, err := compact.New(model, store, compact.Config{
		Window: cfg.ContextTokens, MaxOutput: cfg.MaxOutputTokens,
		Auto:         cfg.AutoCompact,
		Overhead:     compact.TextTokens(string(specsJSON)),
		Reattach:     todos.Snapshot,
		CompactFrac:  0.5,                     // 占用过半就压缩（bot 决策 2026-10-19）
		HeadMessages: -1,                      // bot 会话首部是旧寒暄，不钉
		Prompt:       compact.CompanionPrompt, // 纠正优先、带标签的陪伴版
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	mgr := runner.NewManager(runner.Config{Workspace: wd}, runner.Harness{
		// 空闲压缩：槽位静默 15min 且估算用量过提醒线时后台 Compact。
		// 用户消息到达即取消（runner 管互斥与取消）。
		IdleCompact: func(ctx context.Context, history []agent.Message) []agent.Message {
			est := compact.Estimate(history, compact.TextTokens(string(specsJSON)))
			if est < comp.Config().ReminderLine() {
				return nil
			}
			next, err := comp.Compact(ctx, history, "idle")
			if err != nil {
				slog.Warn("runner: idle compact failed", "err", err)
			}
			return next
		},
		New: func(slotCtx context.Context, emit func(runner.Event),
			ask func(agent.ToolCall, string, int, int) <-chan bool) (runner.TurnRunner, error) {
			a := agent.New(model, registry, maxToolSteps)
			a.Rec, a.Requests = store, store
			a.Compactor = comp
			// 请求级运行时上下文：当前时间 + 距上一条消息的间隔；
			// 只进发送形态，不落库、不进返回的 history。
			a.Decorate = runtimeDecorate

			// 权限：本槽位的 ask 走 runner 路由（客户端弹确认）。
			a.Hooks.OnPreToolUse(permission.NewHook(desk, ask).Check)
			brk := breaker.New(0, 0)
			a.Hooks.OnPreToolUse(brk.Check)
			a.Hooks.OnPostToolUse(brk.Observe)
			a.Hooks.OnPreChat(brk.Nag)
			a.Hooks.OnPreChat(func(context.Context, []agent.Message) string { return bgm.Drain() })
			a.Hooks.OnPreChat(comp.ReminderHook())
			a.Hooks.OnPreChat(todos.Nag)
			a.Hooks.OnUserPromptSubmit(func(context.Context, string) (string, string) {
				todos.ResetRounds()
				return "", ""
			})
			// 子代理复用同一权限闸：slot 的 ask 通道对子代理同样生效。
			if taskTool != nil {
				taskTool.Wire = func(sub *agent.Agent, depth int) {
					sub.Hooks.OnPreToolUse(permission.NewHook(desk, ask).Check)
					subBreaker := breaker.New(0, 0)
					sub.Hooks.OnPreToolUse(subBreaker.Check)
					sub.Hooks.OnPostToolUse(subBreaker.Observe)
					sub.Hooks.OnPreChat(subBreaker.Nag)
				}
			}
			ah := &runner.AgentHarness{Agent: a, Store: store, BgMgr: bgm, CronSched: cronSched}
			tr, err := ah.New(slotCtx, emit, ask)
			if err != nil {
				return nil, err
			}
			// 常驻 system：bot 模式走 BuildBot——人设顶到 system 顶层，不背
			// coding-agent 的 Identity 段；工具/记忆说明首启时冻结进 env。
			if env.Tools == nil {
				env.Tools = registry.Specs()
			}
			if env.MemoryGuide == "" && mem != nil {
				env.MemoryGuide = mem.SystemGuide()
			}
			// 记忆常驻：每轮比对 memory_log 版本，变了才用 Render 的新块
			// 重建 system——dream 写完规则下一轮生效，位置固定在最前。
			// env 跨槽位共享且 MemoryBody 因 subject 而异：重建走本地副本，
			// 避免本槽记忆泄漏到别的 chat 的 system。
			var mu sync.Mutex
			sysVer := int64(-1)
			var sys agent.Message
			rebuild := func(ctx context.Context) agent.Message {
				mu.Lock()
				defer mu.Unlock()
				first := sys.Role == ""
				if first {
					sys = agent.Message{Role: agent.RoleSystem, Content: prompt.BuildBot(env)}
				}
				if mem == nil {
					return sys
				}
				v, err := mem.Version(ctx)
				if err != nil || v == sysVer {
					return sys
				}
				r, err := mem.Render(ctx)
				if err != nil {
					slog.Warn("memory render failed", "err", err)
					return sys
				}
				slotEnv := env
				slotEnv.MemoryBody = r.Text
				reason := "version-change"
				if first {
					reason = "first"
				}
				sc, _ := scope.FromContext(ctx)
				slog.Info("memory: injected", "session", sc.SessionID,
					"space", sc.TenantID+"/"+sc.SubjectID,
					"index_bytes", r.IndexBytes, "body_bytes", r.BodyBytes,
					"version", r.Version, "reason", reason)
				sysVer = v
				sys = agent.Message{Role: agent.RoleSystem, Content: prompt.BuildBot(slotEnv)}
				return sys
			}
			return runner.TurnRunner(func(turnCtx context.Context, history []agent.Message) ([]agent.Message, error) {
				// slot 会把上一轮返回的历史原样回传：剥掉旧的 sys 再挂，保证恰好一份。
				if len(history) > 0 && history[0].Role == agent.RoleSystem {
					history = history[1:]
				}
				// 用户消息过 UserPromptSubmit 钩子链（对齐 TUI model.go:524）：
				// cron/bg 注入行是 KindInject，不会重复触发。
				if n := len(history); n > 0 && history[n-1].Role == agent.RoleUser && history[n-1].Kind == agent.KindMessage {
					replace, blocked := a.Hooks.TriggerUserPromptSubmit(turnCtx, history[n-1].Content)
					if blocked != "" {
						return nil, fmt.Errorf("prompt blocked: %s", blocked)
					}
					if replace != "" {
						history[n-1].Content = replace
					}
				}
				return tr(turnCtx, append([]agent.Message{rebuild(turnCtx)}, history...))
			}), nil
		},
	}, store, store)

	// 空闲压缩巡视：槽位静默满 15min 且用量过提醒线时后台 Compact（plan §1.5）。
	go mgr.WatchIdle(context.Background())

	// 命名触发链：turn_end(completed) → slot 异步回调 → namer 一次性 completion。
	mgr.OnTurnComplete = func(key runner.ChatKey, sid string) {
		namer.NameChat(key.TenantID, key.Channel, key.ChatID, sid)
	}

	srv := gateway.NewServer(mgr, auth, store, "bear")
	srv.Mem = mem // nil 时 /v1/memory* 自然 503
	srv.Namer = namer
	srv.Purger = store
	srv.Notifier = store
	if cronStore != nil {
		// 按请求租户过滤的任务清单；workspace 口径与调度器点火一致。
		srv.Schedules = func(tenant string) []cron.Task {
			ctx := scope.WithScope(context.Background(), scope.Scope{TenantID: tenant, Workspace: wd})
			return cronStore.List(ctx)
		}
	}
	httpSrv := &http.Server{Addr: *listen, Handler: srv}

	go func() {
		slog.Info("runebot listening", "addr", *listen, "model", cfg.Model)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("listen", "err", err)
			os.Exit(1)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	slog.Info("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	bgm.Shutdown()
	if mem != nil && cfg.MemoryDream {
		mem.DreamOnExit(scope.WithScope(context.Background(), gwScope))
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// cronNoticeTitle 从任务 prompt 提炼通知标题：首个非空行截 24 字，
// 空 prompt 回落「定时任务」。
func cronNoticeTitle(t cron.Task) string {
	for _, line := range strings.Split(t.Prompt, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		r := []rune(line)
		if len(r) > 24 {
			return string(r[:24]) + "…"
		}
		return line
	}
	return "定时任务"
}

// runtimeDecorate 是当前用户消息的请求级装饰：在末尾追加
// <runtime-context> 块（当前时间 + 距上一条用户消息的间隔）。
// 只改写发送副本——history 与落库行不带这块（agent.Agent.Decorate）。
func runtimeDecorate(_ context.Context, view []agent.Message) []agent.Message {
	if len(view) == 0 {
		return view
	}
	last := -1
	for i := len(view) - 1; i >= 0; i-- {
		if view[i].Role == agent.RoleUser && view[i].Kind == agent.KindMessage {
			last = i
			break
		}
	}
	if last < 0 {
		return view
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n\n<runtime-context>当前时间 %s", shanghaiNow().Format("2006-01-02 15:04 (MST)"))
	if gap := lastUserGap(view[:last]); gap > 0 {
		fmt.Fprintf(&b, "；距上一条消息 %s", formatGap(gap))
	}
	b.WriteString("</runtime-context>")
	out := slices.Clone(view)
	out[last].Content += b.String()
	return out
}

// lastUserGap 返回最近一条真实用户消息距现在的时长；无时间信息时为 0。
func lastUserGap(view []agent.Message) time.Duration {
	for i := len(view) - 1; i >= 0; i-- {
		m := view[i]
		if m.Role == agent.RoleUser && m.Kind == agent.KindMessage && m.CreatedAt > 0 {
			return time.Since(time.UnixMilli(m.CreatedAt))
		}
	}
	return 0
}

// formatGap 把间隔压成口语量级："3 分钟" / "5 小时" / "3 天"。
func formatGap(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟", max(1, int(d.Minutes())))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d 小时", int(d.Hours()))
	default:
		return fmt.Sprintf("%d 天", int(d.Hours()/24))
	}
}

var shanghai = func() *time.Location {
	l, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return l
}()

func shanghaiNow() time.Time { return time.Now().In(shanghai) }

// windowSource 报告上下文窗口的取值来源，便于排查窗口错配。
func windowSource() string {
	if os.Getenv("RUNE_CONTEXT_TOKENS") != "" {
		return "env"
	}
	return "model-table-or-default"
}

// soulInfo 返回灵魂文件的路径+字节数+mtime；未配置时返回 "none"。
func soulInfo() string {
	p := os.Getenv("RUNE_SOUL")
	if p == "" {
		return "none"
	}
	if st, err := os.Stat(p); err == nil {
		return fmt.Sprintf("%s (%dB, mtime %s)", p, st.Size(),
			st.ModTime().Format("2006-01-02"))
	}
	return p + " (missing)"
}

// toolNames 返回注册表里的工具名清单（启动配置日志用）。
func toolNames(reg *tools.Registry) string {
	specs := reg.Specs()
	names := make([]string, len(specs))
	for i, s := range specs {
		names[i] = s.Name
	}
	return strings.Join(names, ",")
}
