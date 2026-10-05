package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"runeharness/internal/tools"
)

// stubLLM 按脚本依次返回 Response，并记录每次收到的 history。
type stubLLM struct {
	script    []Response
	calls     int
	histories [][]Message
}

func (s *stubLLM) Chat(_ context.Context, history []Message, _ []tools.Spec, _ func(Partial)) (Response, error) {
	s.histories = append(s.histories, append([]Message(nil), history...))
	r := s.script[s.calls]
	s.calls++
	return r, nil
}

// stubTool 记录执行次数并返回固定结果。
type stubTool struct {
	spec tools.Spec
	ran  *int
	out  string
}

func (t stubTool) Spec() tools.Spec { return t.spec }
func (t stubTool) Run(context.Context, json.RawMessage) (string, error) {
	*t.ran++
	return t.out, nil
}

func toolCallResp(calls ...ToolCall) Response {
	return Response{
		Message:      Message{Role: RoleAssistant, ToolCalls: calls},
		FinishReason: FinishReasonToolCalls,
	}
}

func stopResp(text string) Response {
	return Response{
		Message:      Message{Role: RoleAssistant, Content: text},
		FinishReason: FinishReasonStop,
	}
}

func userHistory() []Message {
	return []Message{{Role: RoleUser, Content: "go"}}
}

func TestPreToolUseBlocksCall(t *testing.T) {
	ran := 0
	reg := tools.NewRegistry(stubTool{spec: tools.Spec{Name: "t"}, ran: &ran, out: "ok"})
	llm := &stubLLM{script: []Response{
		toolCallResp(ToolCall{ID: "1", Name: "t", Arguments: json.RawMessage(`{}`)}),
		stopResp("done"),
	}}
	a := New(llm, reg, 10)
	a.Hooks.OnPreToolUse(func(context.Context, ToolUseInput) string { return "error: denied" })

	hist, err := a.Run(context.Background(), userHistory())
	if err != nil {
		t.Fatal(err)
	}
	if ran != 0 {
		t.Fatalf("tool ran %d times, want 0", ran)
	}
	// hist: user, assistant(tool_calls), tool(result), assistant(stop)
	if len(hist) != 4 || hist[2].Role != RoleTool || hist[2].Content != "error: denied" {
		t.Fatalf("unexpected history: %+v", hist)
	}
	if llm.calls != 2 {
		t.Fatalf("llm called %d times, want 2", llm.calls)
	}
}

func TestPostToolUseOnlyAfterExecuted(t *testing.T) {
	ran := 0
	mk := func(name string) stubTool {
		return stubTool{spec: tools.Spec{Name: name}, ran: &ran, out: "ok"}
	}
	reg := tools.NewRegistry(mk("bad"), mk("good"))
	llm := &stubLLM{script: []Response{
		toolCallResp(
			ToolCall{ID: "1", Name: "bad", Arguments: json.RawMessage(`{}`)},
			ToolCall{ID: "2", Name: "good", Arguments: json.RawMessage(`{}`)},
		),
		stopResp("done"),
	}}
	a := New(llm, reg, 10)
	a.Hooks.OnPreToolUse(func(_ context.Context, in ToolUseInput) string {
		if in.Call.Name == "bad" {
			return "error: denied"
		}
		return ""
	})
	var posted []string
	a.Hooks.OnPostToolUse(func(_ context.Context, in PostToolUseInput) {
		posted = append(posted, in.Call.Name+"="+in.Result)
	})

	if _, err := a.Run(context.Background(), userHistory()); err != nil {
		t.Fatal(err)
	}
	if len(posted) != 1 || posted[0] != "good=ok" {
		t.Fatalf("PostToolUse fired %v, want only [good=ok]", posted)
	}
}

func TestPreToolUseSeesBatch(t *testing.T) {
	ran := 0
	reg := tools.NewRegistry(stubTool{spec: tools.Spec{Name: "t"}, ran: &ran, out: "ok"})
	calls := []ToolCall{
		{ID: "1", Name: "t", Arguments: json.RawMessage(`{"n":1}`)},
		{ID: "2", Name: "t", Arguments: json.RawMessage(`{"n":2}`)},
	}
	llm := &stubLLM{script: []Response{toolCallResp(calls...), stopResp("done")}}
	a := New(llm, reg, 10)
	var idx []int
	var batchLen []int
	a.Hooks.OnPreToolUse(func(_ context.Context, in ToolUseInput) string {
		idx = append(idx, in.Index)
		batchLen = append(batchLen, len(in.Batch))
		return ""
	})
	if _, err := a.Run(context.Background(), userHistory()); err != nil {
		t.Fatal(err)
	}
	if len(idx) != 2 || idx[0] != 0 || idx[1] != 1 || batchLen[0] != 2 || batchLen[1] != 2 {
		t.Fatalf("Index=%v BatchLen=%v, want [0 1]/[2 2]", idx, batchLen)
	}
}

func TestStopHookForcesContinuation(t *testing.T) {
	llm := &stubLLM{script: []Response{stopResp("first"), stopResp("second")}}
	a := New(llm, tools.NewRegistry(), 10)
	fires := 0
	a.Hooks.OnStop(func(context.Context, []Message) string {
		fires++
		if fires == 1 {
			return "keep going"
		}
		return ""
	})

	hist, err := a.Run(context.Background(), userHistory())
	if err != nil {
		t.Fatal(err)
	}
	if llm.calls != 2 {
		t.Fatalf("llm called %d times, want 2", llm.calls)
	}
	// 第二次 Chat 收到的 history 末尾应是 hook 注入的 user 消息
	last := llm.histories[1][len(llm.histories[1])-1]
	if last.Role != RoleUser || last.Content != "keep going" {
		t.Fatalf("injected message = %+v, want user 'keep going'", last)
	}
	if hist[len(hist)-1].Content != "second" {
		t.Fatalf("final reply = %q, want 'second'", hist[len(hist)-1].Content)
	}
}

func TestStopHookBoundedByMaxSteps(t *testing.T) {
	llm := &stubLLM{script: []Response{stopResp("a"), stopResp("b"), stopResp("c"), stopResp("d")}}
	a := New(llm, tools.NewRegistry(), 3)
	a.Hooks.OnStop(func(context.Context, []Message) string { return "again" })

	if _, err := a.Run(context.Background(), userHistory()); err == nil {
		t.Fatal("want maxSteps error, got nil")
	}
	if llm.calls != 3 {
		t.Fatalf("llm called %d times, want 3 (maxSteps)", llm.calls)
	}
}

func TestPreChatInjectsMessage(t *testing.T) {
	ran := 0
	reg := tools.NewRegistry(stubTool{spec: tools.Spec{Name: "t"}, ran: &ran, out: "ok"})
	llm := &stubLLM{script: []Response{
		toolCallResp(ToolCall{ID: "1", Name: "t", Arguments: json.RawMessage(`{}`)}),
		stopResp("done"),
	}}
	a := New(llm, reg, 10)
	calls := 0
	a.Hooks.OnPreChat(func(context.Context, []Message) string {
		calls++
		if calls == 2 {
			return "nag"
		}
		return ""
	})
	var injected []Message
	a.OnInject = func(m Message) { injected = append(injected, m) }

	hist, err := a.Run(context.Background(), userHistory())
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("PreChat fired %d times, want 2 (每轮 LLM 调用前一次)", calls)
	}
	// 第 2 次 Chat 收到的 history 末尾应是注入的 user 消息
	last := llm.histories[1][len(llm.histories[1])-1]
	if last.Role != RoleUser || last.Content != "nag" {
		t.Fatalf("injected message = %+v, want user 'nag'", last)
	}
	if len(injected) != 1 || injected[0].Role != RoleUser || injected[0].Content != "nag" {
		t.Fatalf("OnInject got %v, want [user 'nag']", injected)
	}
	// hist: user, assistant(tool_calls), tool(result), user(nag), assistant(stop)
	if len(hist) != 5 || hist[3].Role != RoleUser || hist[3].Content != "nag" {
		t.Fatalf("unexpected history: %+v", hist)
	}
}

func TestPreChatSilentByDefault(t *testing.T) {
	llm := &stubLLM{script: []Response{stopResp("hi")}}
	a := New(llm, tools.NewRegistry(), 10)
	hist, err := a.Run(context.Background(), userHistory())
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 {
		t.Fatalf("history len = %d, want 2 (无 hook 注入)", len(hist))
	}
}

func TestMaxStepsErrorPreservesHistory(t *testing.T) {
	ran := 0
	reg := tools.NewRegistry(stubTool{spec: tools.Spec{Name: "t"}, ran: &ran, out: "ok"})
	llm := &stubLLM{script: []Response{
		toolCallResp(ToolCall{ID: "1", Name: "t", Arguments: json.RawMessage(`{}`)}),
		toolCallResp(ToolCall{ID: "2", Name: "t", Arguments: json.RawMessage(`{}`)}),
		toolCallResp(ToolCall{ID: "3", Name: "t", Arguments: json.RawMessage(`{}`)}),
	}}
	a := New(llm, reg, 3)

	hist, err := a.Run(context.Background(), userHistory())
	var mse *MaxStepsError
	if !errors.As(err, &mse) || mse.Steps != 3 {
		t.Fatalf("err = %v, want *MaxStepsError{Steps:3}", err)
	}
	// 部分进度保留：user + 3×(assistant + tool result)
	if len(hist) != 7 {
		t.Fatalf("history len = %d, want 7", len(hist))
	}
}

func TestTriggerShortCircuit(t *testing.T) {
	var h Hooks
	secondRan := false
	h.OnPreToolUse(func(context.Context, ToolUseInput) string { return "first" })
	h.OnPreToolUse(func(context.Context, ToolUseInput) string { secondRan = true; return "second" })
	if got := h.TriggerPreToolUse(context.Background(), ToolUseInput{}); got != "first" {
		t.Fatalf("got %q, want 'first'", got)
	}
	if secondRan {
		t.Fatal("second hook should be short-circuited")
	}
}

func TestUserPromptSubmit(t *testing.T) {
	var h Hooks
	h.OnUserPromptSubmit(func(_ context.Context, p string) (string, string) { return p + "!", "" })
	if r, b := h.TriggerUserPromptSubmit(context.Background(), "hi"); r != "hi!" || b != "" {
		t.Fatalf("got (%q,%q), want (hi!,'')", r, b)
	}

	var h2 Hooks
	h2.OnUserPromptSubmit(func(context.Context, string) (string, string) { return "", "nope" })
	if r, b := h2.TriggerUserPromptSubmit(context.Background(), "hi"); b != "nope" {
		t.Fatalf("got (%q,%q), want blocked 'nope'", r, b)
	}
}
