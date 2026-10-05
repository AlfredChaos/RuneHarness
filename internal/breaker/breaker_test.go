package breaker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"runeharness/internal/agent"
)

var ctx = context.Background()

func call(name, args string) agent.ToolCall {
	return agent.ToolCall{ID: "c", Name: name, Arguments: json.RawMessage(args)}
}

func fail(b *Breaker, c agent.ToolCall) {
	b.Observe(ctx, agent.PostToolUseInput{Call: c, Result: "error: nope", IsError: true})
}

// observe 记录一次执行结果；结果内容可区分，覆盖"同签名不同结果"场景。
func observe(b *Breaker, c agent.ToolCall, result string, isErr bool) {
	b.Observe(ctx, agent.PostToolUseInput{Call: c, Result: result, IsError: isErr})
}

// 同一签名连续失败 max 次后，下一次相同调用被 PreToolUse 阻止。
func TestTripsAfterMaxIdenticalFailures(t *testing.T) {
	b := New(3, 0)
	c := call("t", `{"x":1}`)
	in := agent.ToolUseInput{Call: c}

	fail(b, c)
	fail(b, c)
	if got := b.Check(ctx, in); got != "" {
		t.Fatalf("2 failures < max 3, but Check blocked: %q", got)
	}
	fail(b, c)
	got := b.Check(ctx, in)
	if got == "" || !strings.HasPrefix(got, "error:") {
		t.Fatalf("3 identical failures should block with 'error:' reason, got %q", got)
	}
}

// 触发阻止后清零：相同调用重新获得 max 次机会（外部条件可能已变）。
func TestTripResetsCounter(t *testing.T) {
	b := New(2, 0)
	c := call("t", `{}`)
	in := agent.ToolUseInput{Call: c}

	fail(b, c)
	fail(b, c)
	if b.Check(ctx, in) == "" {
		t.Fatal("should trip")
	}
	if got := b.Check(ctx, in); got != "" {
		t.Fatalf("counter should reset after trip, got block %q", got)
	}
}

// 连续成功但每次结果相同的空转也要熔断：结果不变 = 没有新信息。
func TestTripsOnIdenticalSuccessResults(t *testing.T) {
	b := New(3, 0)
	c := call("read_file", `{"path":"a.go"}`)

	for i := 0; i < 2; i++ {
		observe(b, c, "file content", false)
	}
	if got := b.Check(ctx, agent.ToolUseInput{Call: c}); got != "" {
		t.Fatalf("2 identical results < max 3, got block %q", got)
	}
	observe(b, c, "file content", false)
	got := b.Check(ctx, agent.ToolUseInput{Call: c})
	if got == "" || !strings.Contains(got, "identical output") {
		t.Fatalf("3 identical results should trip stall breaker, got %q", got)
	}
}

// 同签名但结果变化 = 合法重试（外部世界变了），不触发空转熔断。
func TestSameSigDifferentResultAllowed(t *testing.T) {
	b := New(2, 0)
	c := call("run_command", `{"command":"go test ./..."}`)

	observe(b, c, "FAIL 1 test", false)
	observe(b, c, "ok all pass", false)
	if got := b.Check(ctx, agent.ToolUseInput{Call: c}); got != "" {
		t.Fatalf("result changed between calls — legit retry, got block %q", got)
	}
}

// 成功一次或换一个签名，连续失败序列中断。
func TestResetsOnSuccessOrNewSignature(t *testing.T) {
	b := New(2, 0)

	fail(b, call("t", `{"x":1}`))
	observe(b, call("t", `{"x":1}`), "fine", false) // 成功清零
	fail(b, call("t", `{"x":1}`))
	if got := b.Check(ctx, agent.ToolUseInput{Call: call("t", `{"x":1}`)}); got != "" {
		t.Fatalf("success should reset sequence, got block %q", got)
	}

	b2 := New(2, 0)
	fail(b2, call("t", `{"x":1}`))
	fail(b2, call("t", `{"x":2}`)) // 不同签名 = 新序列，不累计
	if got := b2.Check(ctx, agent.ToolUseInput{Call: call("t", `{"x":2}`)}); got != "" {
		t.Fatalf("different signature starts a new sequence, got block %q", got)
	}
}

// 同名工具连续失败到阈值时 Nag 注入提醒（参数各异的连败才算打转）；
// 每条序列只提醒一次。
func TestNagOnSameNameFailures(t *testing.T) {
	b := New(3, 4)

	for i := 0; i < 4; i++ {
		observe(b, call("run_command", fmt.Sprintf(`{"n":%d}`, i)), "error: nope", true)
	}
	got := b.Nag(ctx, nil)
	if got == "" || !strings.Contains(got, "run_command") {
		t.Fatalf("4 consecutive failures = warnStreak, nag should fire; got %q", got)
	}
	if again := b.Nag(ctx, nil); again != "" {
		t.Fatalf("same streak should not nag twice, got %q", again)
	}
}

// 回归：同名工具连续成功（如批量读文件）是正常工作，不应 nag。
func TestNagIgnoresSuccessfulStreak(t *testing.T) {
	b := New(3, 4)
	c := call("read_file", `{"path":"a.go"}`)
	for i := 0; i < 10; i++ {
		observe(b, c, fmt.Sprintf("content of file%d", i), false)
	}
	if got := b.Nag(ctx, nil); got != "" {
		t.Fatalf("successful streak is normal work, should not nag; got %q", got)
	}
}

// 一次成功或换工具即中断连败序列；重新连败到阈值可再次提醒。
func TestNagStreakResetAndRearm(t *testing.T) {
	b := New(3, 3)

	observe(b, call("t", `{"a":1}`), "error: x", true)
	observe(b, call("t", `{"a":2}`), "error: y", true)
	if got := b.Nag(ctx, nil); got != "" {
		t.Fatalf("2 < 3 should stay silent, got %q", got)
	}

	observe(b, call("t", `{"a":3}`), "ok", false) // 成功中断连败
	for i := 0; i < 3; i++ {
		observe(b, call("t", fmt.Sprintf(`{"b":%d}`, i)), "error: z", true)
	}
	if got := b.Nag(ctx, nil); got == "" {
		t.Fatal("new failure streak reaching threshold should nag again")
	}
}
