package tui

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"runeharness/internal/agent"
	"runeharness/internal/bgtask"
	"runeharness/internal/cron"
	"runeharness/internal/scope"
	"runeharness/internal/tools"

	"github.com/charmbracelet/x/ansi"
)

// 回归：带 tool_calls 的 assistant 消息，其正文在工具开始执行时必须
// 提交进 transcript——否则它会随下一步的流式 pending 被覆盖而丢失。
func TestToolCallCommitsPendingText(t *testing.T) {
	m := New(agent.New(nil, tools.NewRegistry(), 1), Info{Model: "m"}, []agent.Message{{Role: agent.RoleSystem, Content: "sys"}}, scope.Scope{}, nil, nil, nil, nil, nil)
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
	m := New(agent.New(nil, tools.NewRegistry(), 1), Info{Model: "m"}, []agent.Message{{Role: agent.RoleSystem, Content: "sys"}}, scope.Scope{}, nil, nil, nil, nil, nil)
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
	m := New(agent.New(nil, tools.NewRegistry(), 1), Info{Model: "m"}, []agent.Message{{Role: agent.RoleSystem, Content: "sys"}}, scope.Scope{}, nil, nil, nil, nil, nil)
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
	m := New(agent.New(nil, tools.NewRegistry(), 1), Info{Model: "MiniMax-M3"}, []agent.Message{{Role: agent.RoleSystem, Content: "sys"}}, scope.Scope{}, nil, nil, nil, nil, nil)
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
	m := New(agent.New(nil, tools.NewRegistry(), 1), Info{Model: "m"}, []agent.Message{{Role: agent.RoleSystem, Content: "sys"}}, scope.Scope{}, nil, nil, nil, nil, nil)
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

// 后台任务完成通知：正常收尾的空闲轮把它注入为 KindInject user 消息并
// 续跑；错误路径（中断/撞墙/调用失败）不 drain，通知留队列等下一轮。
func TestBgNotificationDrain(t *testing.T) {
	newModel := func() (*Model, *bgtask.Manager) {
		bgm := bgtask.New(t.TempDir())
		m := New(agent.New(nil, tools.NewRegistry(), 1), Info{Model: "m"},
			[]agent.Message{{Role: agent.RoleSystem, Content: "sys"}},
			scope.Scope{TenantID: "t", Workspace: t.TempDir(), SessionID: "s"},
			nil, nil, nil, bgm, nil)
		return m, bgm
	}
	finishTask := func(t *testing.T, bgm *bgtask.Manager) {
		t.Helper()
		ctx := scope.WithScope(context.Background(),
			scope.Scope{TenantID: "t", SessionID: "s", Workspace: t.TempDir()})
		if _, err := bgm.Spawn(ctx, tools.BGSpawn{
			Command: "echo done", Timeout: 10 * time.Second}); err != nil {
			t.Fatalf("Spawn error = %v", err)
		}
		for i := 0; i < 200; i++ {
			if ts := bgm.List(); len(ts) > 0 && ts[len(ts)-1].Status == bgtask.StatusCompleted {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("task did not complete in time")
	}
	histDone := []agent.Message{{Role: agent.RoleAssistant, Content: "ok"}}

	// 正常收尾：通知注入 history 并返回续跑 cmd。
	m, bgm := newModel()
	finishTask(t, bgm)
	m.busy = true
	if cmd := m.maybeDrainBg(); cmd != nil {
		t.Fatal("busy model should not drain")
	}
	if _, cmd := m.Update(turnDoneMsg{history: histDone}); cmd == nil {
		t.Fatal("clean turnDone should drain pending notification into a new run")
	}
	last := m.history[len(m.history)-1]
	if last.Kind != agent.KindInject || !strings.Contains(last.Content, "<task_notification>") {
		t.Fatalf("injected msg = %+v, want KindInject <task_notification>", last)
	}

	// 错误收尾：通知留队列，不自动起跑。
	m, bgm = newModel()
	finishTask(t, bgm)
	if _, cmd := m.Update(turnDoneMsg{history: histDone, err: errors.New("api down")}); cmd != nil {
		t.Fatal("error turnDone should not auto-run pending notifications")
	}
	if n := len(m.history); n != 0 && m.history[n-1].Kind == agent.KindInject {
		t.Fatal("error path injected a notification message")
	}
	if bgm.Drain() == "" {
		t.Fatal("notification should remain queued for the next turn")
	}
}

// UserPromptSubmit 拦截 cron 交付时，事件必须归还调度队列而不是丢弃
// ——Drain 语义是"被消费才离队列"。
func TestCronDrainBlockedRequeues(t *testing.T) {
	store := cron.Open(filepath.Join(t.TempDir(), "cron_tasks.json"))
	sched := cron.NewScheduler(store,
		scope.WithScope(context.Background(), scope.Scope{TenantID: "t", Workspace: "/w"}),
		cron.Config{})
	sched.Requeue([]cron.Event{{Task: cron.Task{ID: "j1", Prompt: "hi"}}})

	ag := agent.New(nil, tools.NewRegistry(), 1)
	ag.Hooks.OnUserPromptSubmit(func(context.Context, string) (string, string) {
		return "", "no cron today"
	})
	m := New(ag, Info{Model: "m"},
		[]agent.Message{{Role: agent.RoleSystem, Content: "sys"}},
		scope.Scope{TenantID: "t", Workspace: "/w", SessionID: "s"},
		nil, nil, nil, nil, sched)

	if cmd := m.maybeDrainCron(); cmd != nil {
		t.Fatal("blocked cron turn must not run")
	}
	if evs := sched.Drain(); len(evs) != 1 || evs[0].Task.ID != "j1" {
		t.Fatalf("blocked events must be requeued, got %+v", evs)
	}
}
