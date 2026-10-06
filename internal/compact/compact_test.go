package compact

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"runeharness/internal/agent"
	"runeharness/internal/scope"
)

// 卸载中段：大结果换索引且 blob 可读；<1K 的保留；过期的小结果也卸载并带
// superseded 标记；>2K 的参数字段换索引且仍是合法 JSON；首尾不动；
// 落库后 LoadHistory 与内存视图一致。
func TestOffloadMiddle(t *testing.T) {
	c, st, ctx := newTestCompactor(t, nil)
	heredoc, _ := json.Marshal(map[string]string{"command": "cat > f.go <<EOF\n" + big(5_000) + "\nEOF"})
	msgs := chatOf(3, "ok", "done") // 首部 3 轮
	msgs = append(msgs,
		agent.Message{Role: agent.RoleUser, Content: "question 4"},
		agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{
			{ID: "r1", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.go"}`)},
			{ID: "w1", Name: "run_command", Arguments: heredoc},
		}},
		agent.Message{Role: agent.RoleTool, ToolCallID: "r1", Content: "old a.go"}, // 小，但被后面的重读取代
		agent.Message{Role: agent.RoleTool, ToolCallID: "w1", Content: big(3_000)},
		agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{ID: "s1", Name: "run_command", Arguments: json.RawMessage(`{"command":"ls"}`)}}},
		agent.Message{Role: agent.RoleTool, ToolCallID: "s1", Content: "tiny"},
	)
	msgs = append(msgs, chatOf(1, "ok", "done")[1:]...)
	msgs = append(msgs,
		agent.Message{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{ID: "r2", Name: "read_file", Arguments: json.RawMessage(`{"path": "a.go"}`)}}},
		agent.Message{Role: agent.RoleTool, ToolCallID: "r2", Content: "new a.go"},
	)
	view := recordAll(t, st, ctx, msgs...)
	seg := segments{}
	for j := range view { // 手工切分：下标 19（第 2 次 question 1）起为尾部
		switch {
		case j == 0:
			seg.system = append(seg.system, j)
		case j < 19:
			seg.middle = append(seg.middle, j)
		default:
			seg.tail = append(seg.tail, j)
		}
	}
	out, saved, err := c.offload(ctx, view, seg)
	if err != nil {
		t.Fatal(err)
	}
	if saved <= 0 {
		t.Fatalf("saved = %d", saved)
	}
	byCall := map[string]string{}
	for _, m := range out {
		if m.Role == agent.RoleTool {
			byCall[m.ToolCallID] = m.Content
		}
	}
	if !strings.HasPrefix(byCall["w1"], offloadedPrefix) {
		t.Fatalf("3K result should be offloaded: %q", byCall["w1"])
	}
	if byCall["s1"] != "tiny" {
		t.Fatal("<1K result that is not stale must stay")
	}
	if !strings.Contains(byCall["r1"], "superseded by later call r2") {
		t.Fatalf("stale read should be offloaded with marker: %q", byCall["r1"])
	}
	if byCall["r2"] != "new a.go" {
		t.Fatal("tail must not change")
	}
	ref := strings.TrimSuffix(strings.Split(byCall["w1"], "blob://")[1], "]")
	if full, err := st.LoadBlob(ctx, ref); err != nil || full != big(3_000) {
		t.Fatalf("offloaded blob unreadable: %v", err)
	}
	var args map[string]string
	w1 := out[14].ToolCalls // 第 4 轮的 assistant
	if err := json.Unmarshal(w1[1].Arguments, &args); err != nil {
		t.Fatalf("offloaded arguments must stay valid JSON: %v", err)
	}
	if !strings.HasPrefix(args["command"], argsPrefix) || w1[1].ID != "w1" {
		t.Fatalf("large argument not offloaded: %q", args["command"])
	}

	sc, _ := scope.FromContext(ctx)
	loaded, err := st.LoadHistory(ctx, sc.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if agent.ViewHash(loaded) != agent.ViewHash(out) {
		t.Fatal("folded view from storage differs from the in-memory view")
	}
}

// 过强制压缩线且无可卸载内容：Maintain 摘要中段，新视图 = system + 首部 +
// 摘要 + 尾部；落库视图一致；二次压缩的输入含旧摘要，首部逐字节不变。
func TestMaintainSummarizesAndRolls(t *testing.T) {
	llm := &fakeLLM{}
	c, st, ctx := newTestCompactor(t, llm)
	sc, _ := scope.FromContext(ctx)
	// 30 轮、每轮 assistant 正文 ~4K token：总量 ~120K，正文不可卸载
	view := recordAll(t, st, ctx, chatOf(30, "ok", big(16_000))...)
	next, err := c.Maintain(ctx, view)
	if err != nil {
		t.Fatal(err)
	}
	if llm.calls() != 1 {
		t.Fatalf("summary calls = %d, want 1", llm.calls())
	}
	got := viewContents(next)
	if got[0] != "sys" || got[1] != "question 1" || got[4] != "<summary>" {
		t.Fatalf("view head = %v", got[:6])
	}
	if !strings.Contains(next[4].Content, "summary #1") {
		t.Fatalf("summary text = %q", next[4].Content)
	}
	if est := Estimate(next, 0); est >= c.cfg.ReminderLine() {
		t.Fatalf("post-compaction %d tokens should be below the reminder line %d", est, c.cfg.ReminderLine())
	}
	loaded, err := st.LoadHistory(ctx, sc.SessionID)
	if err != nil || agent.ViewHash(loaded) != agent.ViewHash(next) {
		t.Fatalf("storage fold differs from in-memory view (err=%v)", err)
	}

	// 继续工作再次过线：摘要输入含旧摘要，首部不变
	more := recordAll(t, st, ctx, chatOf(25, "ok", big(16_000))[1:]...)
	next2, err := c.Maintain(ctx, append(next, more...))
	if err != nil {
		t.Fatal(err)
	}
	if llm.calls() != 2 || !strings.Contains(llm.inputs[1], "summary #1") {
		t.Fatalf("second summary input must contain the old summary")
	}
	if !slices.Equal(viewContents(next2)[:4], viewContents(next)[:4]) {
		t.Fatal("system + head must stay byte-identical across compactions")
	}
	if strings.Count(strings.Join(viewContents(next2), "|"), "<summary>") != 1 {
		t.Fatal("only the latest summary stays in the view")
	}
	loaded, _ = st.LoadHistory(ctx, sc.SessionID)
	if agent.ViewHash(loaded) != agent.ViewHash(next2) {
		t.Fatal("storage fold differs after the second compaction")
	}
}

// 熔断：调用报错 / 缺 SUMMARY / 压缩无效各计一次；第 3 次后自动通道不再调 API，
// 模型入口返回 ErrCircuitOpen；/compact 照常执行，成功后清零。
func TestBreaker(t *testing.T) {
	llm := &fakeLLM{
		errs:    []error{errBoom},
		replies: []string{"", "no summary section here", "SUMMARY:\n" + big(500_000)},
	}
	c, st, ctx := newTestCompactor(t, llm)
	// 22 轮：粗估 ~98K，介于强制压缩线 95K 与阻断线 108K 之间
	view := recordAll(t, st, ctx, chatOf(22, "ok", big(16_000))...)
	if est := Estimate(view, 0); est < c.cfg.CompactLine() || est >= c.cfg.BlockLine() {
		t.Fatalf("fixture estimate %d out of range", est)
	}
	for i := 0; i < 3; i++ {
		next, err := c.Maintain(ctx, view)
		if err != nil {
			t.Fatalf("round %d: summary failure must not fail the turn: %v", i, err)
		}
		if agent.ViewHash(next) != agent.ViewHash(view) {
			t.Fatalf("round %d: history must stay unchanged on failure", i)
		}
	}
	if st, _ := c.loadState(ctx); st.Failures != 3 {
		t.Fatalf("failures = %d, want 3", st.Failures)
	}
	if _, err := c.Maintain(ctx, view); err != nil || llm.calls() != 3 {
		t.Fatalf("open circuit: Maintain err=%v, calls=%d (want no new call)", err, llm.calls())
	}
	if _, err := c.Compact(ctx, view, "model"); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("model compact with open circuit: err = %v", err)
	}
	if _, err := c.CompactWith(ctx, view, ""); err != nil {
		t.Fatalf("manual /compact should probe and succeed: %v", err)
	}
	if st, _ := c.loadState(ctx); st.Failures != 0 {
		t.Fatalf("failures after success = %d, want 0", st.Failures)
	}
}

// 被动自愈：端点报出的窗口钳小有效配置并持久化；只往下钳。
func TestNoteLimitClampsAndPersists(t *testing.T) {
	c, _, ctx := newTestCompactor(t, nil)
	c.NoteLimit(ctx, 90_000)
	if c.cfg.Window != 90_000 {
		t.Fatalf("window = %d, want clamped to 90000", c.cfg.Window)
	}
	if c.cfg.CompactLine() != 90_000-33_000 {
		t.Fatalf("compact line = %d", c.cfg.CompactLine())
	}
	st, _ := c.loadState(ctx)
	if st.WindowLimit != 90_000 {
		t.Fatalf("persisted limit = %d", st.WindowLimit)
	}
	c.NoteLimit(ctx, 999_000) // 更大的值不得放大窗口
	if c.cfg.Window != 90_000 {
		t.Fatalf("window must not grow back: %d", c.cfg.Window)
	}
	// 新压缩器（模拟 resume）：持久化钳制在首次 loadState 时生效
	c2, _, ctx2 := newTestCompactor(t, nil)
	if err := c2.store.PutState(ctx2, json.RawMessage(`{"window_limit":80000}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.loadState(ctx2); err != nil {
		t.Fatal(err)
	}
	if c2.cfg.Window != 80_000 {
		t.Fatalf("resumed window = %d, want 80000", c2.cfg.Window)
	}
}

// 熔断期间用量过阻断线：拒发请求。
func TestBlockLineWhenCircuitOpen(t *testing.T) {
	c, st, ctx := newTestCompactor(t, &fakeLLM{})
	if err := c.saveState(ctx, state{Failures: maxFailures}); err != nil {
		t.Fatal(err)
	}
	view := recordAll(t, st, ctx, chatOf(30, "ok", big(20_000))...) // ~150K > 108K
	if _, err := c.Maintain(ctx, view); !errors.Is(err, agent.ErrContextFull) {
		t.Fatalf("err = %v, want ErrContextFull", err)
	}
}
