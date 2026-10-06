package compact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"runeharness/internal/agent"
	"runeharness/internal/session/sqlite"
	"runeharness/internal/tools"
)

// recordAll 把消息逐条落库并写回行 id，模拟 loop 的 push。
func recordAll(t *testing.T, st *sqlite.Store, ctx context.Context, msgs ...agent.Message) []agent.Message {
	t.Helper()
	out := make([]agent.Message, len(msgs))
	for i, m := range msgs {
		id, err := st.Append(ctx, m)
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		m.ID = id
		out[i] = m
	}
	return out
}

// turn 造一个用户轮：user → assistant(tool_call) → tool → assistant(text)。
func turn(n int, toolOut, final string) []agent.Message {
	id := fmt.Sprintf("call-%d", n)
	args, _ := json.Marshal(map[string]string{"command": fmt.Sprintf("step %d", n)})
	return []agent.Message{
		{Role: agent.RoleUser, Content: fmt.Sprintf("question %d", n)},
		{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{ID: id, Name: "run_command", Arguments: args}}},
		{Role: agent.RoleTool, ToolCallID: id, Content: toolOut},
		{Role: agent.RoleAssistant, Content: final},
	}
}

func chatOf(turns int, toolOut, final string) []agent.Message {
	msgs := []agent.Message{{Role: agent.RoleSystem, Content: "sys"}}
	for n := 1; n <= turns; n++ {
		msgs = append(msgs, turn(n, toolOut, final)...)
	}
	return msgs
}

// fakeLLM 是脚本化的摘要模型：按顺序返回 replies，记录每次收到的输入。
type fakeLLM struct {
	mu      sync.Mutex
	replies []string
	errs    []error
	inputs  []string
}

func (f *fakeLLM) Chat(_ context.Context, history []agent.Message, _ []tools.Spec, _ func(agent.Partial)) (agent.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inputs = append(f.inputs, history[len(history)-1].Content)
	n := len(f.inputs) - 1
	if n < len(f.errs) && f.errs[n] != nil {
		return agent.Response{}, f.errs[n]
	}
	reply := "ANALYSIS:\nstuff happened\n\nSUMMARY:\nsummary #" + fmt.Sprint(n+1)
	if n < len(f.replies) && f.replies[n] != "" {
		reply = f.replies[n]
	}
	return agent.Response{Message: agent.Message{Role: agent.RoleAssistant, Content: reply},
		FinishReason: agent.FinishReasonStop}, nil
}

func (f *fakeLLM) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.inputs)
}

var errBoom = errors.New("boom")

// viewContents 便于断言视图组成。
func viewContents(view []agent.Message) []string {
	out := make([]string, len(view))
	for i, m := range view {
		out[i] = m.Content
		if m.Kind == agent.KindSummary {
			out[i] = "<summary>"
		}
	}
	return out
}

func big(n int) string { return strings.Repeat("x", n) }
