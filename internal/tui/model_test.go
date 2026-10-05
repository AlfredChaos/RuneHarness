package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"runeharness/internal/agent"
	"runeharness/internal/scope"
	"runeharness/internal/tools"

	"github.com/charmbracelet/x/ansi"
)

// 回归：带 tool_calls 的 assistant 消息，其正文在工具开始执行时必须
// 提交进 transcript——否则它会随下一步的流式 pending 被覆盖而丢失。
func TestToolCallCommitsPendingText(t *testing.T) {
	m := New(agent.New(nil, tools.NewRegistry(), 1), Info{Model: "m"}, []agent.Message{{Role: agent.RoleSystem, Content: "sys"}}, scope.Scope{}, nil, nil)
	m.Update(PartialMsg{Content: "delegating now"})
	m.Update(ToolCallMsg{
		Call: agent.ToolCall{Name: "task", Arguments: json.RawMessage(`{"description":"x"}`)},
	})
	if !strings.Contains(ansi.Strip(m.transcript), "delegating now") {
		t.Fatalf("mid-turn assistant text dropped; transcript = %q", m.transcript)
	}
	if m.pending != "" {
		t.Fatal("pending should be committed and cleared")
	}
}

// 中断时 Run 返回部分 history（未执行的 tool_calls 已补占位结果），
// TUI 应采纳它而不是丢弃。
func TestInterruptedKeepsPartialHistory(t *testing.T) {
	m := New(agent.New(nil, tools.NewRegistry(), 1), Info{Model: "m"}, []agent.Message{{Role: agent.RoleSystem, Content: "sys"}}, scope.Scope{}, nil, nil)
	partial := []agent.Message{
		{Role: agent.RoleUser, Content: "go"},
		{Role: agent.RoleAssistant, ToolCalls: []agent.ToolCall{{ID: "1", Name: "t"}}},
		{Role: agent.RoleTool, ToolCallID: "1", Content: "error: execution interrupted", IsError: true},
	}
	m.Update(turnDoneMsg{history: partial, err: &agent.InterruptedError{}})
	if len(m.history) != len(partial) {
		t.Fatalf("interrupted history = %d msgs, want %d (partial progress kept)",
			len(m.history), len(partial))
	}
	if !strings.Contains(ansi.Strip(m.transcript), "interrupted") {
		t.Fatalf("interrupt notice missing; transcript = %q", m.transcript)
	}
}

// 末尾空消息（正文已随上一条 tool_calls 消息提交）不应渲染裸 "agent" 标签。
func TestEmptyFinalMessage(t *testing.T) {
	m := New(agent.New(nil, tools.NewRegistry(), 1), Info{Model: "m"}, []agent.Message{{Role: agent.RoleSystem, Content: "sys"}}, scope.Scope{}, nil, nil)
	m.Update(turnDoneMsg{
		history: []agent.Message{{Role: agent.RoleAssistant}},
	})
	if strings.Contains(m.transcript, "agent\n") {
		t.Fatalf("empty final response rendered bare label; transcript = %q", m.transcript)
	}
	if !strings.Contains(m.transcript, "empty final response") {
		t.Fatalf("expected diagnostic line, transcript = %q", m.transcript)
	}
}

// busy 时状态栏应显示当前等待事项与耗时，便于区分"在等模型"和"在跑工具"。
func TestStatusLineShowsActivity(t *testing.T) {
	m := New(agent.New(nil, tools.NewRegistry(), 1), Info{Model: "MiniMax-M3"}, []agent.Message{{Role: agent.RoleSystem, Content: "sys"}}, scope.Scope{}, nil, nil)
	m.busy = true
	m.busySince = time.Now().Add(-3 * time.Second)
	m.activity = "waiting for model"
	s := m.statusLine()
	if !strings.Contains(s, "waiting for model") || !strings.Contains(s, "3s") {
		t.Fatalf("statusLine = %q, want activity + elapsed", s)
	}
}

// 撞 MaxStepsError 时应保留部分进度、注入提示并自动续跑；
// 同一轮第二次撞墙不再自动续，由用户手动决定。
func TestMaxStepsAutoResumeOnce(t *testing.T) {
	m := New(agent.New(nil, tools.NewRegistry(), 1), Info{Model: "m"}, []agent.Message{{Role: agent.RoleSystem, Content: "sys"}}, scope.Scope{}, nil, nil)
	m.busy = true
	hist := []agent.Message{{Role: agent.RoleAssistant, Content: "partial work"}}

	m.Update(turnDoneMsg{history: hist, err: &agent.MaxStepsError{Steps: 50}})
	if !m.autoResumed || !m.busy {
		t.Fatal("first cap hit should auto-resume")
	}
	last := m.history[len(m.history)-1]
	if last.Role != agent.RoleUser || !strings.Contains(last.Content, "50") {
		t.Fatalf("notice msg = %+v", last)
	}

	m.Update(turnDoneMsg{history: m.history, err: &agent.MaxStepsError{Steps: 50}})
	if m.busy {
		t.Fatal("second cap hit should not auto-resume again")
	}
}
