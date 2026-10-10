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
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
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
	"runeharness/internal/permission"
	"runeharness/internal/prompt"
	"runeharness/internal/runner"
	"runeharness/internal/scope"
	"runeharness/internal/session/sqlite"
	"runeharness/internal/skill"
	"runeharness/internal/subagent"
	"runeharness/internal/todo"
	"runeharness/internal/tools"
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

	wd, _ := os.Getwd()
	gwScope := scope.Scope{TenantID: "gateway", Workspace: wd}
	baseCtx := scope.WithScope(context.Background(), gwScope)

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

	model := llm.NewClient(cfg.APIKey, cfg.BaseURL, cfg.Model)
	todos := todo.NewManager()

	env := prompt.Env{Workspace: wd, Platform: runtime.GOOS + "/" + runtime.GOARCH}
	if home, err := os.UserHomeDir(); err == nil {
		env.Skills = skill.Catalog(skill.Scan(filepath.Join(home, ".agents", "skills")))
	}

	bgm := bgtask.New(filepath.Join(filepath.Dir(dbPath), "tasks"))

	// cron 持久化任务随 workspace；属主锁同 CLI 语义。
	cronStore := cron.Open(filepath.Join(filepath.Dir(dbPath), "cron_tasks.json"))
	var cronSched *cron.Scheduler
	if cronStore != nil {
		cronSched = cron.NewScheduler(cronStore, baseCtx, cron.Config{
			LockPath: filepath.Join(filepath.Dir(dbPath), cron.LockFileName(wd)),
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

	mgr := runner.NewManager(runner.Config{}, runner.Harness{
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
			return ah.New(slotCtx, emit, ask)
		},
	}, store, store)

	srv := gateway.NewServer(mgr, auth, store, "bear")
	srv.Mem = mem // nil 时 /v1/memory* 自然 503
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
