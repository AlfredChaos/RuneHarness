package permission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"runeharness/internal/agent"
)

func call(name, arg string) agent.ToolCall {
	return agent.ToolCall{Name: name, Arguments: json.RawMessage(arg)}
}

func batch(calls ...agent.ToolCall) []agent.ToolCall { return calls }

func TestHookDeny(t *testing.T) {
	h := NewHook(New(Config{ForbiddenPatterns: []string{"rm -rf"}}), nil)
	in := agent.ToolUseInput{
		Call:  call("run_command", `{"command":"rm -rf /tmp/x"}`),
		Index: 0,
		Batch: batch(call("run_command", `{"command":"rm -rf /tmp/x"}`)),
	}
	got := h.Check(context.Background(), in)
	if !strings.HasPrefix(got, "error: blocked by permission policy:") {
		t.Fatalf("got %q, want blocked-by-policy error", got)
	}
}

func TestHookAllow(t *testing.T) {
	h := NewHook(New(Config{SafeTools: []string{"read_file"}}), nil)
	in := agent.ToolUseInput{Call: call("read_file", `{"path":"a.txt"}`), Batch: batch(call("read_file", `{"path":"a.txt"}`))}
	if got := h.Check(context.Background(), in); got != "" {
		t.Fatalf("got %q, want pass-through", got)
	}
}

func TestHookAskApprovedAndDenied(t *testing.T) {
	newHook := func(ok bool) *Hook {
		return NewHook(New(Config{}), func(agent.ToolCall, string, int, int) <-chan bool {
			reply := make(chan bool, 1)
			reply <- ok
			return reply
		})
	}
	in := agent.ToolUseInput{Call: call("write_file", `{"path":"a.txt"}`), Batch: batch(call("write_file", `{"path":"a.txt"}`))}

	if got := newHook(true).Check(context.Background(), in); got != "" {
		t.Fatalf("approved: got %q, want pass-through", got)
	}
	if got := newHook(false).Check(context.Background(), in); !strings.HasPrefix(got, "error: user denied") {
		t.Fatalf("denied: got %q, want user-denied error", got)
	}
}

// Ask 为 nil 时按 Deny 处理，且不计入编号。
func TestHookAskWithoutChannel(t *testing.T) {
	h := NewHook(New(Config{}), nil)
	in := agent.ToolUseInput{Call: call("write_file", `{"path":"a.txt"}`), Batch: batch(call("write_file", `{"path":"a.txt"}`))}
	if got := h.Check(context.Background(), in); !strings.Contains(got, "no approval channel") {
		t.Fatalf("got %q, want no-approval-channel error", got)
	}
}

// 混合批次中只对 Ask 判定编号：allow,ask,ask → 两次询问分别 (1/2)、(2/2)。
func TestHookSeqTotal(t *testing.T) {
	type seen struct{ seq, total int }
	var got []seen
	h := NewHook(New(Config{SafeTools: []string{"read_file"}}), func(_ agent.ToolCall, _ string, seq, total int) <-chan bool {
		got = append(got, seen{seq, total})
		reply := make(chan bool, 1)
		reply <- true
		return reply
	})
	b := batch(
		call("read_file", `{"path":"a"}`),
		call("write_file", `{"path":"b"}`),
		call("write_file", `{"path":"c"}`),
	)
	for i, c := range b {
		h.Check(context.Background(), agent.ToolUseInput{Call: c, Index: i, Batch: b})
	}
	if len(got) != 2 || got[0] != (seen{1, 2}) || got[1] != (seen{2, 2}) {
		t.Fatalf("seq/total = %+v, want [{1 2} {2 2}]", got)
	}
}
