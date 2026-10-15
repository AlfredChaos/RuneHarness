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
	gwScope := scope.Scope{TenantID: "gateway", Workspace: wd}

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

	model := llm.NewClientTuned(cfg.APIKey, cfg.BaseURL, cfg.Model, llm.Opts{
		ReasoningEffort:     cfg.ReasoningEffort,
		Temperature:         cfg.Temperature,
		TopP:                cfg.TopP,
		MaxCompletionTokens: cfg.MaxCompletionTokens,
	})
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
	if cronStore != nil {
		// 调度器以部署唯一租户 + 进程 workspace 看任务（FileTasks/Remove
		// 都按 scope 过滤），与 durable 任务落盘打标一致才能点火/焚毁。
		cronCtx := scope.WithScope(context.Background(),
			scope.Scope{TenantID: fallbackTenant, Workspace: wd})
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
			memLLM = llm.NewClient(cfg.APIKey, cfg.BaseURL, cfg.MemoryModel)
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

	// AI 命名（ALF-130）：成功轮次结束后异步生成 ≤12 字标题写回 sessions.title。
	namer := &naming.Namer{Store: store, LLM: model}

	mgr := runner.NewManager(runner.Config{Workspace: wd}, runner.Harness{
		New: func(slotCtx context.Context, emit func(runner.Event),
			ask func(agent.ToolCall, string, int, int) <-chan bool) (runner.TurnRunner, error) {
			a := agent.New(model, registry, maxToolSteps)
			a.Rec, a.Requests = store, store
			comp, err := compact.New(model, store, compact.Config{
				Window: cfg.ContextTokens, MaxOutput: cfg.MaxOutputTokens,
				Auto:     cfg.AutoCompact,
				Overhead: compact.TextTokens(string(specsJSON)),
				Reattach: todos.Snapshot,
			})
			if err != nil {
				return nil, err
			}
			a.Compactor = comp

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
			if mem != nil {
				a.Hooks.OnUserPromptSubmit(mem.InjectOnce)
			}
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
			sys := agent.Message{Role: agent.RoleSystem, Content: prompt.BuildBot(env)}
			return runner.TurnRunner(func(turnCtx context.Context, history []agent.Message) ([]agent.Message, error) {
				// slot 会把上一轮返回的历史原样回传：剥掉旧的 sys 再挂，保证恰好一份。
				if len(history) > 0 && history[0].Role == agent.RoleSystem {
					history = history[1:]
				}
				// 用户消息过 UserPromptSubmit 钩子链（对齐 TUI model.go:524）：
				// memory InjectOnce 借此注入；cron/bg 注入行是 KindInject，不会重复触发。
				if n := len(history); n > 0 && history[n-1].Role == agent.RoleUser && history[n-1].Kind == agent.KindMessage {
					replace, blocked := a.Hooks.TriggerUserPromptSubmit(turnCtx, history[n-1].Content)
					if blocked != "" {
						return nil, fmt.Errorf("prompt blocked: %s", blocked)
					}
					if replace != "" {
						history[n-1].Content = replace
					}
				}
				return tr(turnCtx, append([]agent.Message{sys}, history...))
			}), nil
		},
	}, store, store)

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
