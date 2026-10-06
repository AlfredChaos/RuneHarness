// e2e 是压缩功能的端到端驱动：装配真实 LLM + sqlite + compact + subagent，
// 按脚本灌水用户轮，观察提醒/卸载/摘要/熔断/子代理各路径是否生效。
// 用法：RUNE_E2E_WINDOW=80000 go run ./cmd/e2e（读根目录 .env）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"runeharness/internal/agent"
	"runeharness/internal/compact"
	"runeharness/internal/config"
	"runeharness/internal/llm"
	"runeharness/internal/scope"
	"runeharness/internal/session"
	"runeharness/internal/session/sqlite"
	"runeharness/internal/subagent"
	"runeharness/internal/tools"
)

var (
	window = flag.Int("w", 80_000, "工作窗口（把线压低，几轮即可触发）")
	dbPath = flag.String("db", "", "会话库路径；默认临时文件")
	keep   = flag.Bool("keep", false, "结束后保留测试文件与 DB")
)

// padFile 生成 ~lines 行的填充文件，secret 放在第 secretLine 行。
func padFile(path string, lines, secretLine int, secret string) error {
	var b strings.Builder
	for i := 1; i <= lines; i++ {
		if i == secretLine {
			fmt.Fprintf(&b, "%s\n", secret)
		} else {
			fmt.Fprintf(&b, "padding line %04d for context pressure test\n", i)
		}
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(1)
	}
}

func run() error {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "rhe2e")
	if err != nil {
		return err
	}
	db := *dbPath
	if db == "" {
		db = filepath.Join(dir, "e2e.db")
	}
	if !*keep {
		defer os.RemoveAll(dir)
	}
	fmt.Printf("== e2e dir: %s (db %s)\n", dir, db)
	fmt.Printf("== window %d → reminder %d / compact %d / block %d (est)\n",
		*window, *window-56_000, *window-33_000, *window-20_000)

	store, err := sqlite.Open(db)
	if err != nil {
		return err
	}
	defer store.Close()

	model := llm.NewClient(cfg.APIKey, cfg.BaseURL, cfg.Model)
	tools0 := []tools.Tool{
		tools.CurrentTime{}, tools.ListDir{}, tools.ReadFile{Blobs: store},
		tools.RunCommand{}, compact.Tool{},
	}
	task := subagent.New(model, tools0[:4], subagent.DefaultPrompt, false)
	task.Store = store
	task.CompactCfg = compact.Config{Window: *window, Auto: true}
	toolSet := append(tools0, task)
	reg := tools.NewRegistry(toolSet...)

	a := agent.New(model, reg, 50)
	a.Rec, a.Requests = store, store
	specsJSON, _ := json.Marshal(reg.Specs())
	cmp, err := compact.New(model, store, compact.Config{
		Window: *window, MaxOutput: cfg.MaxOutputTokens, Auto: true,
		Overhead: compact.TextTokens(string(specsJSON)),
	})
	if err != nil {
		return err
	}
	a.Compactor = cmp
	a.Hooks.OnPreChat(cmp.ReminderHook())

	a.OnToolCall = func(tc agent.ToolCall) {
		fmt.Printf("   >> tool %s %s\n", tc.Name, shorten(string(tc.Arguments), 80))
	}
	a.OnToolResult = func(r agent.ToolResult) {
		fmt.Printf("   << %s → %s\n", r.Call.Name, shorten(r.Output, 80))
	}
	a.OnInject = func(m agent.Message) { fmt.Printf("   ** inject: %s\n", shorten(m.Content, 100)) }

	base := scope.WithScope(context.Background(),
		scope.Scope{TenantID: "e2e", Workspace: dir})
	sess, err := store.CreateSession(base, session.Meta{Model: cfg.Model, Workspace: dir, Kind: session.KindMain})
	if err != nil {
		return err
	}
	ctx := scope.WithSession(base, sess.ID)
	fmt.Printf("== session %s\n", sess.ID)

	// 压测文件：1800 行 ≈ 75KB ≈ 19K token；暗号埋在文件内部
	var files []string
	for i, s := range []string{"SECRET-APPLE-1001", "SECRET-BANANA-2002", "SECRET-PEAR-3003"} {
		f := filepath.Join(dir, fmt.Sprintf("f%d.txt", i+1))
		if err := padFile(f, 1800, 1499, s); err != nil {
			return err
		}
		files = append(files, f)
	}

	hist := []agent.Message{{
		Role:    agent.RoleSystem,
		Content: "You are a test agent. Use tools exactly as asked and reply briefly.",
	}}
	sysID, err := store.Append(ctx, hist[0])
	if err != nil {
		return err
	}
	hist[0].ID = sysID

	turn := func(user string) error {
		m := agent.Message{Role: agent.RoleUser, Content: user}
		if m.ID, err = store.Append(ctx, m); err != nil {
			return err
		}
		hist = append(hist, m)
		fmt.Printf("\n=== user: %s\n", shorten(user, 90))
		fmt.Printf("    est tokens = %d (lines %d/%d/%d)\n",
			compact.Estimate(hist, cmp.Config().Overhead),
			cmp.Config().ReminderLine(), cmp.Config().CompactLine(), cmp.Config().BlockLine())
		start := time.Now()
		hist, err = a.Run(ctx, hist)
		fmt.Printf("    run done in %s, err=%v\n", time.Since(start).Round(time.Second), err)
		for i := len(hist) - 1; i >= 0; i-- {
			if hist[i].Role == agent.RoleAssistant && hist[i].Content != "" {
				fmt.Printf("    reply: %s\n", shorten(hist[i].Content, 300))
				break
			}
		}
		return err
	}

	// Phase 1：三轮读文件，跨过提醒线与强制压缩线。
	fmt.Println("\n##### PHASE 1: pump (expect reminder → offload)")
	for i, f := range files {
		err := turn(fmt.Sprintf("用 read_file 读 %s（offset=1 limit=2000），一句话告诉我第 1499 行内容", f))
		if err != nil && i == 0 {
			return fmt.Errorf("first turn failed: %w", err)
		}
	}

	// Phase 2：注入不可卸载的中段（真实落库的合成讨论历史，含工作目标）。
	// 约 60K token 的纯文本：文件结果卸载完之后，中段剩下的还是它——
	// 卸载救不回来，下一轮才能逼出真正的摘要压缩。
	fmt.Println("\n##### PHASE 2: seed non-offloadable middle (~60K) → real summary next turn")
	desc := "详细说明：这个模块负责把输入按既定规则逐字段变换，每个字段有类型校验、" +
		"空值兜底和越界截断；输出要兼容下游三种消费者（表格、消息队列、文件导出），" +
		"字段顺序固定且不允许缺列；验收标准是十万条样本回放后所有字段齐全、无空值、" +
		"无类型漂移，且与旧版输出逐字节一致。"
	seed := strings.Repeat(desc, 5) // ~750 字 ≈ 530 token
	goal := "项目目标：汇总三个压测文件（f1/f2/f3）里的暗号，产出一张文件名→暗号的对照表，" +
		"然后逐条验证各项需求的验收口径。" // 早期埋的工作目标，每轮重复出现
	for i := 0; i < 60; i++ {
		user := fmt.Sprintf("第 %02d 项需求记录：模块 m%d 要支持配置项 c%d，优先级 p%d。\n%s\n%s",
			i, i, i, i%3, goal, seed)
		asst := fmt.Sprintf("已记录第 %02d 项需求。当前清单共 %d 项；目标不变：%s\n进展备注：%s",
			i, i+1, goal, seed)
		for _, m := range []agent.Message{
			{Role: agent.RoleUser, Content: user},
			{Role: agent.RoleAssistant, Content: asst},
		} {
			if m.ID, err = store.Append(ctx, m); err != nil {
				return err
			}
			hist = append(hist, m)
		}
	}

	// Phase 3：便宜轮触发 Maintain → 卸载不够 → 强制自动摘要（真 LLM）。
	fmt.Println("\n##### PHASE 3: trivial turn → expect offload(insufficient)+auto summary")
	if err := turn("收到，继续。"); err != nil {
		fmt.Println("    (turn errored, continuing to inspect)")
	}

	// Phase 4：回忆验证——此时中段已被摘要，考工作目标与暗号是否仍可达。
	fmt.Println("\n##### PHASE 4: recall after summary → goal + secret")
	if err := turn("我们之前定的工作目标是什么？第一个文件里埋的暗号又是什么？" +
		"不记得的就用工具找回来，不要猜。"); err != nil {
		fmt.Println("    (turn errored, continuing to inspect)")
	}

	// Phase 5：子代理（normal 继承快照——spawn 前应 Prime 卸载）。
	fmt.Println("\n##### PHASE 5: subagent spawn (expect fork row + prime offload)")
	if err := turn(fmt.Sprintf(
		"用 task 工具派一个 subagent_type=normal 的子代理统计 %s 里含 padding 的行数，只回数字",
		files[0])); err != nil {
		fmt.Println("    (subagent turn errored)")
	}

	// Phase 6：手动压缩一条（manual reason）。
	fmt.Println("\n##### PHASE 6: manual /compact equivalent")
	next, err := cmp.CompactWith(ctx, hist, "记住三个暗号和目标")
	fmt.Printf("    manual compact: err=%v, est %d → %d\n",
		err, compact.Estimate(hist, 0), compact.Estimate(next, 0))
	hist = next

	// Dump：控制行 / blobs / state / requests / 视图结构
	fmt.Println("\n##### DUMP")
	rows, err := store.LoadRawHistory(ctx, sess.ID)
	if err != nil {
		return err
	}
	var kinds map[agent.Kind]int = map[agent.Kind]int{}
	for _, r := range rows {
		kinds[r.Kind]++
	}
	fmt.Println("main rows:", len(rows), "by kind:", kinds)
	for _, r := range rows {
		if r.Kind.IsControl() || r.Kind == agent.KindSummary {
			fmt.Printf("  %s id=%d %s\n", r.Kind, r.ID, shorten(r.Content, 110))
		}
	}
	view, _ := store.LoadHistory(ctx, sess.ID)
	fmt.Println("folded view msgs:", len(view), "in-mem:", len(hist),
		"hash match:", agent.ViewHash(view) == agent.ViewHash(hist))
	reqs, _ := store.LoadRequests(ctx, sess.ID)
	ok := 0
	for _, r := range reqs {
		v, err := store.LoadView(ctx, sess.ID, r.UptoMsgID)
		if err == nil && agent.ViewHash(v) == r.ViewHash {
			ok++
		} else {
			fmt.Printf("  request %d replay MISMATCH (err=%v)\n", r.ID, err)
		}
	}
	fmt.Printf("requests: %d, replay ok: %d\n", len(reqs), ok)

	subs, _ := store.ListSessions(base, 0)
	for _, s := range subs {
		if s.Kind != session.KindSubagent {
			continue
		}
		subRows, _ := store.LoadRawHistory(ctx, s.ID)
		fork := "no"
		if len(subRows) > 0 && subRows[0].Kind == agent.KindFork {
			fork = subRows[0].Content
		}
		fmt.Printf("sub-session %s depth=%d rows=%d fork=%s\n", s.ID, s.Depth, len(subRows), shorten(fork, 100))
	}
	return nil
}

func shorten(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
