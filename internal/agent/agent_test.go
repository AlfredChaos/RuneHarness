package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"runeharness/internal/tools"
)

// cancelTool 在 Run 里取消传入的 ctx，模拟"执行途中被用户中断"。
type cancelTool struct {
	spec   tools.Spec
	cancel context.CancelFunc
	out    string
}

func (t cancelTool) Spec() tools.Spec { return t.spec }
func (t cancelTool) Run(context.Context, json.RawMessage) (string, error) {
	t.cancel()
	return t.out, nil
}

// deadLLM 阻塞到 ctx 结束再返回其错误，模拟被取消的流式调用。
type deadLLM struct{}

func (deadLLM) Chat(ctx context.Context, _ []Message, _ []tools.Spec, _ func(Partial)) (Response, error) {
	<-ctx.Done()
	return Response{}, ctx.Err()
}

// boomLLM 返回与 ctx 无关的普通错误（如 API 5xx）。
type boomLLM struct{}

func (boomLLM) Chat(context.Context, []Message, []tools.Spec, func(Partial)) (Response, error) {
	return Response{}, errors.New("boom")
}

// 批中中断：已执行的调用结果落历史，未执行的补 interrupted 占位，
// 整个 assistant 消息的 tool_calls 全都有对应 tool 结果。
func TestInterruptFillsRemainingToolCalls(t *testing.T) {
	ran := 0
	ctx, cancel := context.WithCancel(context.Background())
	reg := tools.NewRegistry(
		cancelTool{spec: tools.Spec{Name: "t1"}, cancel: cancel, out: "partial"},
		stubTool{spec: tools.Spec{Name: "t2"}, ran: &ran, out: "never"},
	)
	llm := &stubLLM{script: []Response{
		toolCallResp(
			ToolCall{ID: "1", Name: "t1", Arguments: json.RawMessage(`{}`)},
			ToolCall{ID: "2", Name: "t2", Arguments: json.RawMessage(`{}`)},
		),
	}}
	a := New(llm, reg, 10)

	hist, err := a.Run(ctx, userHistory())
	var ie *InterruptedError
	if !errors.As(err, &ie) {
		t.Fatalf("err = %v, want *InterruptedError", err)
	}
	if ran != 0 {
		t.Fatalf("t2 ran %d times after interrupt, want 0", ran)
	}
	// user + assistant(2 calls) + tool(t1=partial) + tool(t2=interrupted)
	if len(hist) != 4 {
		t.Fatalf("history len = %d, want 4: %+v", len(hist), hist)
	}
	if hist[2].Content != "partial" || hist[2].IsError {
		t.Fatalf("executed call result = %+v, want success 'partial'", hist[2])
	}
	last := hist[3]
	if last.Role != RoleTool || last.ToolCallID != "2" || !last.IsError {
		t.Fatalf("last msg = %+v, want interrupted error result for call 2", last)
	}
}

// Chat 阶段中断：history 原样返回（末尾是 user 消息，可续跑）。
func TestInterruptDuringChat(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := New(deadLLM{}, tools.NewRegistry(), 10)

	hist, err := a.Run(ctx, userHistory())
	var ie *InterruptedError
	if !errors.As(err, &ie) {
		t.Fatalf("err = %v, want *InterruptedError", err)
	}
	if len(hist) != 1 || hist[0].Role != RoleUser {
		t.Fatalf("history = %+v, want unchanged input", hist)
	}
}

// spyRecorder 按调用顺序记录每条 Append 的消息。
type spyRecorder struct {
	msgs []Message
	err  error // 非 nil 时下一次 Append 返回该错误
}

func (s *spyRecorder) Append(_ context.Context, m Message) error {
	if s.err != nil {
		err := s.err
		s.err = nil
		return err
	}
	s.msgs = append(s.msgs, m)
	return nil
}

// 落库语义：loop 追加进 history 的每条消息都同步到达 Recorder，
// 顺序与 history 一致——含 assistant、tool_result、PreChat 注入。
func TestRecorderSeesEveryAppend(t *testing.T) {
	ran := 0
	reg := tools.NewRegistry(stubTool{spec: tools.Spec{Name: "t"}, ran: &ran, out: "ok"})
	llm := &stubLLM{script: []Response{
		toolCallResp(ToolCall{ID: "1", Name: "t", Arguments: json.RawMessage(`{}`)}),
		stopResp("done"),
	}}
	a := New(llm, reg, 10)
	rec := &spyRecorder{}
	a.Rec = rec
	nagged := false
	a.Hooks.OnPreChat(func(context.Context, []Message) string {
		if !nagged {
			nagged = true
			return "nag"
		}
		return ""
	})

	hist, err := a.Run(context.Background(), userHistory())
	if err != nil {
		t.Fatal(err)
	}
	// hist: user, user(nag), assistant(tool_calls), tool, assistant(stop)
	// rec 不含传入的初始 user（调用方消息不经 loop），含其余 4 条
	if len(rec.msgs) != 4 {
		t.Fatalf("recorded %d msgs, want 4: %+v", len(rec.msgs), rec.msgs)
	}
	for i, m := range rec.msgs {
		want := hist[i+1]
		if m.Role != want.Role || m.Content != want.Content {
			t.Fatalf("rec[%d] = %+v, want history entry %+v", i, m, want)
		}
	}
}

// 落库失败 fail the turn：Run 返回错误，已落库部分保留。
func TestRecorderErrorFailsTurn(t *testing.T) {
	reg := tools.NewRegistry()
	llm := &stubLLM{script: []Response{stopResp("done")}}
	a := New(llm, reg, 10)
	rec := &spyRecorder{err: errors.New("disk full")}
	a.Rec = rec

	hist, err := a.Run(context.Background(), userHistory())
	if err == nil || err.Error() != "disk full" {
		t.Fatalf("err = %v, want 'disk full'", err)
	}
	// assistant 消息已进 history，record 失败即中止
	if len(hist) != 2 || hist[1].Role != RoleAssistant {
		t.Fatalf("history = %+v, want user+assistant", hist)
	}
}

// 与 ctx 无关的 LLM 错误仍是致命错误（恢复路径见 agent.go TODO）。
func TestLLMErrorStillFatal(t *testing.T) {
	a := New(boomLLM{}, tools.NewRegistry(), 10)
	hist, err := a.Run(context.Background(), userHistory())
	var ie *InterruptedError
	if errors.As(err, &ie) {
		t.Fatal("non-ctx error should not become InterruptedError")
	}
	if err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v, want 'boom'", err)
	}
	if hist != nil {
		t.Fatalf("history = %+v, want nil on fatal error", hist)
	}
}
