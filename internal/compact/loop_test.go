package compact

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"runeharness/internal/agent"
	"runeharness/internal/scope"
	"runeharness/internal/session/sqlite"
	"runeharness/internal/tools"
)

// scriptLLM 是 agent 主循环的脚本化模型：按顺序返回 steps。
type scriptLLM struct {
	steps []func() (agent.Response, error)
	n     int
}

func (s *scriptLLM) Chat(context.Context, []agent.Message, []tools.Spec, func(agent.Partial)) (agent.Response, error) {
	if s.n >= len(s.steps) {
		return agent.Response{Message: agent.Message{Role: agent.RoleAssistant, Content: "done"}, FinishReason: agent.FinishReasonStop}, nil
	}
	s.n++
	return s.steps[s.n-1]()
}

func respond(m agent.Message, fr agent.FinishReason) func() (agent.Response, error) {
	m.Role = agent.RoleAssistant
	return func() (agent.Response, error) { return agent.Response{Message: m, FinishReason: fr}, nil }
}

type echoTool struct{ out string }

func (e echoTool) Spec() tools.Spec { return tools.Spec{Name: "run_command"} }
func (e echoTool) Run(context.Context, json.RawMessage) (string, error) {
	return e.out, nil
}

// newLoop 装配 agent + 压缩器 + sqlite，history 预置 n 轮大正文对话（已落库）。
func newLoop(t *testing.T, main agent.LLM, summarizer agent.LLM, turns int, toolOut string) (*agent.Agent, *sqlite.Store, context.Context, []agent.Message) {
	t.Helper()
	c, st, ctx := newTestCompactor(t, summarizer)
	a := agent.New(main, tools.NewRegistry(echoTool{out: toolOut}, Tool{}), 10)
	a.Rec, a.Requests, a.Compactor = st, st, c
	hist := recordAll(t, st, ctx, chatOf(turns, "ok", big(16_000))...)
	hist = append(hist, recordAll(t, st, ctx, agent.Message{Role: agent.RoleUser, Content: "go on"})...)
	return a, st, ctx, hist
}

// assertReplay：存储折叠出的视图与内存一致；每条请求留痕按水印重放出同一哈希。
func assertReplay(t *testing.T, st *sqlite.Store, ctx context.Context, final []agent.Message) {
	t.Helper()
	sc, _ := scope.FromContext(ctx)
	loaded, err := st.LoadHistory(ctx, sc.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if agent.ViewHash(loaded) != agent.ViewHash(final) {
		t.Fatalf("stored view differs from in-memory view:\n%v\n%v", viewContents(loaded), viewContents(final))
	}
	reqs, err := st.LoadRequests(ctx, sc.SessionID)
	if err != nil || len(reqs) == 0 {
		t.Fatalf("requests = %d, %v", len(reqs), err)
	}
	for _, r := range reqs {
		v, err := st.LoadView(ctx, sc.SessionID, r.UptoMsgID)
		if err != nil {
			t.Fatal(err)
		}
		if agent.ViewHash(v) != r.ViewHash {
			t.Fatalf("request %d: replayed view hash differs", r.ID)
		}
	}
}

// 模型发起压缩（plan §4.7）：同组其它调用先执行，compact 结果行落在尾部末尾，
// 内容为固定成功文本；落库视图与请求留痕都可重放。空结果写固定标记。
func TestLoopModelCompact(t *testing.T) {
	main := &scriptLLM{steps: []func() (agent.Response, error){
		respond(agent.Message{ToolCalls: []agent.ToolCall{
			{ID: "k1", Name: agent.CompactToolName, Arguments: json.RawMessage(`{}`)},
			{ID: "e1", Name: "run_command", Arguments: json.RawMessage(`{"command":"touch x"}`)},
		}}, agent.FinishReasonToolCalls),
	}}
	summ := &fakeLLM{}
	a, st, ctx, hist := newLoop(t, main, summ, 18, "") // ~73K：未到强制线，由模型主动压缩
	final, err := a.Run(ctx, hist)
	if err != nil {
		t.Fatal(err)
	}
	if summ.calls() != 1 {
		t.Fatalf("summary calls = %d, want 1", summ.calls())
	}
	n := len(final)
	if final[n-1].Content != "done" || final[n-2].ToolCallID != "k1" || final[n-2].Content != agent.CompactDoneText {
		t.Fatalf("tail = %v", viewContents(final[n-4:]))
	}
	if final[n-3].ToolCallID != "e1" || final[n-3].Content != EmptyOutput {
		t.Fatalf("other call result = %+v (empty output must become a marker)", final[n-3])
	}
	hasSummary := false
	for _, m := range final {
		hasSummary = hasSummary || m.Kind == agent.KindSummary
	}
	if !hasSummary {
		t.Fatal("view should contain the summary")
	}
	assertReplay(t, st, ctx, final)
}

// reactive：端点报上下文超长 → 压缩一次后重试；重试仍超长就上抛，不再压缩。
func TestLoopReactiveRetriesOnce(t *testing.T) {
	tooLong := func() (agent.Response, error) {
		return agent.Response{}, errors.Join(agent.ErrContextLength, errors.New("400 context_length_exceeded"))
	}
	main := &scriptLLM{steps: []func() (agent.Response, error){tooLong}}
	summ := &fakeLLM{}
	a, st, ctx, hist := newLoop(t, main, summ, 18, "ok")
	final, err := a.Run(ctx, hist)
	if err != nil {
		t.Fatalf("retry after reactive compaction should succeed: %v", err)
	}
	if summ.calls() != 1 || main.n != 1 {
		t.Fatalf("summary calls = %d, main steps = %d", summ.calls(), main.n)
	}
	assertReplay(t, st, ctx, final)

	main2 := &scriptLLM{steps: []func() (agent.Response, error){tooLong, tooLong, tooLong}}
	summ2 := &fakeLLM{}
	a2, _, ctx2, hist2 := newLoop(t, main2, summ2, 18, "ok")
	if _, err := a2.Run(ctx2, hist2); !errors.Is(err, agent.ErrContextLength) {
		t.Fatalf("err = %v, want ErrContextLength after one retry", err)
	}
	if summ2.calls() != 1 || main2.n != 2 {
		t.Fatalf("must compact once and retry once: summary=%d main=%d", summ2.calls(), main2.n)
	}
}

// 提醒线：过线注入一次，同一压缩周期内不重复；摘要之后重新计。
func TestReminderOncePerEpoch(t *testing.T) {
	c := testCompactor(false)
	hook := c.ReminderHook()
	hist := chatOf(19, "ok", big(16_000)) // ~77K ≥ 提醒线 72K
	msg := hook(context.Background(), hist)
	if msg == "" {
		t.Fatal("over the reminder line should inject")
	}
	hist = append(hist, agent.Message{Role: agent.RoleUser, Kind: agent.KindInject, Content: msg})
	if hook(context.Background(), hist) != "" {
		t.Fatal("must not remind twice in one epoch")
	}
	hist = append(hist, agent.Message{Role: agent.RoleUser, Kind: agent.KindSummary, Content: "s"})
	if hook(context.Background(), hist) == "" {
		t.Fatal("a new epoch after a summary should remind again")
	}
	if sub := testCompactor(true).ReminderHook()(context.Background(), chatOf(19, "ok", big(16_000))); sub != wrapUpReminder {
		t.Fatalf("subagent reminder = %q", sub)
	}
	// 已过强制线且 auto 开启：提醒静默，同轮的 Maintain 直接压缩
	if hook(context.Background(), chatOf(30, "ok", big(16_000))) != "" {
		t.Fatal("past the forced line the reminder should stay silent")
	}
}
