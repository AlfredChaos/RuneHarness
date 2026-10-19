// replay 是风格回放评测驱动（plan §0）：读本地 sessions.db 副本，按给定
// 会话与"纠正点"行 id，用指定 model × 视图（原始/压缩）重建发送形态，
// 重放该点之后的 N 轮，按 internal/eval/style 的四检查器统计违规率。
//
// 真端点驱动，不进 go test。回放数据是用户私人对话：输入库与输出一律放
// ~/.rune/replay/，不进入仓库（plan §0 警告）。
//
// 用法：
//
//	go run ./cmd/replay -db ~/.rune/replay/sessions.db -session <id> \
//	    -point 1024 -turns 4 -models "M3,M3-flash" -compact
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"runeharness/internal/agent"
	"runeharness/internal/compact"
	"runeharness/internal/config"
	"runeharness/internal/eval/style"
	"runeharness/internal/llm"
	"runeharness/internal/scope"
	"runeharness/internal/session/sqlite"
)

var (
	dbPath   = flag.String("db", "", "sessions.db 副本路径（必须显式给，防误读真库）")
	sessID   = flag.String("session", "", "会话 id")
	pointID  = flag.Int64("point", 0, "纠正点行 id：重放该行之后的消息")
	nTurns   = flag.Int("turns", 3, "每个检查点重放的轮数")
	models   = flag.String("models", "", "逗号分隔的模型名；默认取 .env 的 OPENAI_MODEL")
	compactV = flag.Bool("compact", false, "对视图先跑一次压缩（压缩后口径）")
	role     = flag.String("role", "", "RoleKept 检查的角色名（如 小小狗）")
)

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "replay:", err)
		os.Exit(1)
	}
}

func run() error {
	if *dbPath == "" || *sessID == "" || *pointID <= 0 {
		return fmt.Errorf("need -db, -session, -point")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	modelNames := strings.Split(*models, ",")
	if *models == "" {
		modelNames = []string{cfg.Model}
	}
	store, err := sqlite.Open(*dbPath)
	if err != nil {
		return err
	}
	defer store.Close()
	// 离线工具：读租户会话需要 SystemScope（回放数据不出本机，scope 只
	// 决定读侧租户过滤）。
	ctx := scope.WithSession(scope.WithScope(context.Background(), scope.SystemScope(".")), *sessID)

	history, err := store.LoadHistory(ctx, *sessID)
	if err != nil {
		return err
	}
	// 纠正点定位：point 是 messages.id，其后第一条 user 消息是重放起点。
	cut := -1
	for i, m := range history {
		if m.ID == *pointID {
			cut = i
			break
		}
	}
	if cut < 0 {
		return fmt.Errorf("message id %d not found in session %s", *pointID, *sessID)
	}
	base := history[:cut+1]
	following := history[cut+1:]
	// 重放输入：point 之后的真实 user 消息，最多 turns 条。
	var inputs []string
	for _, m := range following {
		if m.Role == agent.RoleUser && m.Kind == agent.KindMessage {
			inputs = append(inputs, m.Content)
			if len(inputs) >= *nTurns {
				break
			}
		}
	}
	if len(inputs) == 0 {
		return fmt.Errorf("no user messages after id %d", *pointID)
	}

	outDir := filepath.Join(os.Getenv("HOME"), ".rune", "replay")
	_ = os.MkdirAll(outDir, 0o755)

	fmt.Printf("%-14s %-9s %6s %6s %6s %6s\n",
		"model", "view", "Q-end", "latin", "repeat", "role")
	for _, name := range modelNames {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		for _, useCompact := range []bool{false, *compactV} {
			stats, replies, err := replayCell(ctx, cfg, store, name, base, inputs, useCompact)
			if err != nil {
				return fmt.Errorf("%s compact=%v: %w", name, useCompact, err)
			}
			view := "raw"
			if useCompact {
				view = "compact"
			}
			fmt.Printf("%-14s %-9s %6s %6s %6s %6s\n",
				name, view,
				rate(stats["q"], len(replies)),
				rate(stats["latin"], len(replies)),
				rate(stats["repeat"], len(replies)),
				rate(stats["role"], len(replies)))
			// 每条违规落一行到输出文件，便于逐条人工复核。
			log := filepath.Join(outDir, fmt.Sprintf("replay-%s-%s-%d.txt",
				name, view, *pointID))
			_ = os.WriteFile(log, []byte(strings.Join(replies, "\n---\n")), 0o600)
		}
	}
	fmt.Println("detail logs →", outDir)
	return nil
}

// replayCell 跑一格 (model × view)：逐条真实 user 消息补一轮回复，
// 检查器逐项计数。返回违规计数、全部回复文本与 error。
func replayCell(ctx context.Context, cfg config.Config, store *sqlite.Store,
	modelName string, base []agent.Message, inputs []string, useCompact bool) (map[string]int, []string, error) {
	// thinking 参数只对 M3 下发：M3.1-Flash 不接受 reasoning_split 分叉，
	// 混发会让摘要调用产出空/坏正文（见 5035 Flash-compact 失败）。
	opts := llm.Opts{ReasoningEffort: cfg.ReasoningEffort}
	if strings.Contains(modelName, "MiniMax-M3") && !strings.Contains(modelName, "Flash") {
		opts.ThinkingType = cfg.ThinkingType
		opts.ReasoningSplit = cfg.ReasoningSplit
	}
	client := llm.NewClientTuned(cfg.APIKey, cfg.BaseURL, modelName, opts)
	view := append([]agent.Message(nil), base...)
	if useCompact {
		comp, err := compact.New(client, store, compact.Config{
			Window: cfg.ContextTokens, MaxOutput: cfg.MaxOutputTokens,
			CompactFrac: 0.5, HeadMessages: -1, Prompt: compact.CompanionPrompt,
		})
		if err != nil {
			return nil, nil, err
		}
		v, err := comp.Compact(ctx, view, "replay")
		if err != nil {
			return nil, nil, fmt.Errorf("compact view: %w", err)
		}
		view = v
	}
	stats := map[string]int{"q": 0, "latin": 0, "repeat": 0, "role": 0}
	var replies []string
	var recent []string
	for _, in := range inputs {
		view = append(view, agent.Message{Role: agent.RoleUser, Content: in})
		resp, err := client.Chat(ctx, view, nil, nil)
		if err != nil {
			return stats, replies, err
		}
		reply := resp.Message.Content
		replies = append(replies, reply)
		if style.EndsWithQuestion(reply) != "" {
			stats["q"]++
		}
		if style.LatinMix(reply) != "" {
			stats["latin"]++
		}
		if style.RepeatPhrase(reply, tail(recent, 3), 12) != "" {
			stats["repeat"]++
		}
		if style.RoleKept(*role)(reply) != "" {
			stats["role"]++
		}
		recent = append(recent, reply)
		view = append(view, resp.Message)
	}
	return stats, replies, nil
}

func rate(n, total int) string {
	if total == 0 {
		return "-"
	}
	return fmt.Sprintf("%d/%d", n, total)
}

func tail(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
