package bgtask

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"runeharness/internal/scope"
	"runeharness/internal/tools"
)

// eventLog 是线程安全的事件收集器：OnEvent 回调跑在任务 goroutine 上，
// 不能直接写测试栈上的切片。
type eventLog struct {
	mu sync.Mutex
	ev []Event
}

func (l *eventLog) add(e Event) { l.mu.Lock(); l.ev = append(l.ev, e); l.mu.Unlock() }

func (l *eventLog) all() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Event(nil), l.ev...)
}

// waitEvents 等够 n 条事件：终态落 map 与 OnEvent 回调是两个动作，
// 断言事件数前必须给 emit 一个窗口。
func (l *eventLog) waitEvents(t *testing.T, n int) []Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ev := l.all(); len(ev) >= n {
			return ev
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("got %d events, want >= %d", len(l.all()), n)
	return nil
}

// scopedCtx 造带租户工作区与会话的 ctx，与 tools 测试同一口径。
func scopedCtx(ws string) context.Context {
	return scope.WithScope(context.Background(),
		scope.Scope{TenantID: "t", SessionID: "sess1", Workspace: ws})
}

func spawn(t *testing.T, m *Manager, ctx context.Context, in tools.BGSpawn) tools.BGTask {
	t.Helper()
	bt, err := m.Spawn(ctx, in)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	return bt
}

// findTask 在 List 快照里按 id 找任务。
func findTask(t *testing.T, m *Manager, id string) Task {
	t.Helper()
	for _, tk := range m.List() {
		if tk.ID == id {
			return tk
		}
	}
	t.Fatalf("task %s not in list", id)
	return Task{}
}

// waitStatus 轮询任务直到非 running 或超时。
func waitStatus(t *testing.T, m *Manager, id string) Task {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		tk := findTask(t, m, id)
		if tk.Status != StatusRunning {
			return tk
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task %s still running after 10s", id)
	return Task{}
}

func TestSpawnCompletesAndNotifies(t *testing.T) {
	m := New(t.TempDir())
	log := &eventLog{}
	m.OnEvent = log.add

	bt := spawn(t, m, scopedCtx(t.TempDir()), tools.BGSpawn{Command: "echo hello-bg"})
	tk := waitStatus(t, m, bt.ID)

	if tk.Status != StatusCompleted || tk.ExitCode != 0 {
		t.Fatalf("status=%s exit=%d, want completed/0", tk.Status, tk.ExitCode)
	}
	data, err := os.ReadFile(tk.OutputPath)
	if err != nil || !strings.Contains(string(data), "hello-bg") {
		t.Fatalf("output file: data=%q err=%v", data, err)
	}
	// 输出路径按会话分目录。
	if !strings.HasSuffix(tk.OutputPath, filepath.Join("sess1", bt.ID+".output")) {
		t.Fatalf("output path %q not under session dir", tk.OutputPath)
	}
	msg := m.Drain()
	for _, want := range []string{"<task_notification>", "<task_id>" + bt.ID,
		"<status>completed</status>", "<output_file>" + tk.OutputPath,
		"completed (exit code 0)"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("notification missing %q:\n%s", want, msg)
		}
	}
	if m.Drain() != "" {
		t.Fatal("second drain should be empty")
	}
	if ev := log.waitEvents(t, 1); len(ev) != 1 || ev[0].Task.Status != StatusCompleted {
		t.Fatalf("events=%+v, want one completed", ev)
	}
}

func TestSpawnFailedReportsExitCode(t *testing.T) {
	m := New(t.TempDir())
	bt := spawn(t, m, scopedCtx(t.TempDir()), tools.BGSpawn{Command: "exit 3"})
	tk := waitStatus(t, m, bt.ID)
	if tk.Status != StatusFailed || tk.ExitCode != 3 {
		t.Fatalf("status=%s exit=%d, want failed/3", tk.Status, tk.ExitCode)
	}
	if !strings.Contains(m.Drain(), "failed with exit code 3") {
		t.Fatal("failure summary missing")
	}
}

func TestDescriptionFallbackToCommand(t *testing.T) {
	m := New(t.TempDir())
	bt := spawn(t, m, scopedCtx(t.TempDir()), tools.BGSpawn{Command: "true"})
	if tk := findTask(t, m, bt.ID); tk.Description != "true" {
		t.Fatalf("description=%q, want command text", tk.Description)
	}
	waitStatus(t, m, bt.ID)
}

func TestKillStopsAndSuppressesNotification(t *testing.T) {
	m := New(t.TempDir())
	log := &eventLog{}
	m.OnEvent = log.add
	bt := spawn(t, m, scopedCtx(t.TempDir()), tools.BGSpawn{Command: "sleep 60"})

	snap, err := m.Kill(bt.ID)
	if err != nil {
		t.Fatalf("kill: %v", err)
	}
	if snap.ID != bt.ID {
		t.Fatalf("kill snapshot id=%s", snap.ID)
	}
	tk := waitStatus(t, m, bt.ID)
	if tk.Status != StatusKilled {
		t.Fatalf("status=%s, want killed", tk.Status)
	}
	// kill 结果由 task_kill 工具结果告知模型——不再产生完成通知。
	if msg := m.Drain(); msg != "" {
		t.Fatalf("notification should be suppressed after kill, got:\n%s", msg)
	}
	// 终态事件仍推给 UI 留痕。
	if ev := log.waitEvents(t, 1); len(ev) != 1 || ev[0].Task.Status != StatusKilled {
		t.Fatalf("events=%+v, want one killed", ev)
	}
	if _, err := m.Kill(bt.ID); err == nil {
		t.Fatal("killing a dead task should error")
	}
	if _, err := m.Kill("bg99"); err == nil {
		t.Fatal("killing unknown task should error")
	}
}

// 进程组级联：sh -c 再生子进程，Kill 必须连带孙进程一起杀，
// 否则 sleep 会作为孤儿继续占用进程表。
func TestKillCascadesToProcessGroup(t *testing.T) {
	m := New(t.TempDir())
	ws := t.TempDir()
	pidFile := filepath.Join(ws, "child.pid")
	spawn(t, m, scopedCtx(ws), tools.BGSpawn{
		Command: "sleep 60 & echo $! > child.pid; wait",
	})
	deadline := time.Now().Add(5 * time.Second)
	var childPid int
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			if _, err := fmt.Sscanf(string(data), "%d", &childPid); err == nil && childPid > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPid == 0 {
		t.Fatal("child pid file never appeared")
	}
	m.Shutdown()
	// 等 wait 协程定稿，再给内核一点回收时间。
	for _, tk := range m.List() {
		waitStatus(t, m, tk.ID)
	}
	if err := syscall.Kill(childPid, 0); err == nil {
		t.Fatalf("child process %d still alive after Shutdown", childPid)
	}
}

func TestShutdownKillsRunning(t *testing.T) {
	m := New(t.TempDir())
	bt := spawn(t, m, scopedCtx(t.TempDir()), tools.BGSpawn{Command: "sleep 60"})
	m.Shutdown()
	if tk := waitStatus(t, m, bt.ID); tk.Status != StatusKilled {
		t.Fatalf("status=%s, want killed", tk.Status)
	}
}

func TestSpawnRequiresScope(t *testing.T) {
	m := New(t.TempDir())
	if _, err := m.Spawn(context.Background(), tools.BGSpawn{Command: "true"}); err == nil {
		t.Fatal("missing scope should error")
	}
}

func TestDrainCollectsMultiple(t *testing.T) {
	m := New(t.TempDir())
	b1 := spawn(t, m, scopedCtx(t.TempDir()), tools.BGSpawn{Command: "true"})
	b2 := spawn(t, m, scopedCtx(t.TempDir()), tools.BGSpawn{Command: "false"})
	waitStatus(t, m, b1.ID)
	waitStatus(t, m, b2.ID)
	msg := m.Drain()
	if !strings.Contains(msg, b1.ID) || !strings.Contains(msg, b2.ID) {
		t.Fatalf("drain should contain both ids:\n%s", msg)
	}
}

// 停滞看门狗：输出不增长且尾行是交互提示时入队一次性提醒。
func TestStallWatchdogNotifies(t *testing.T) {
	m := New(t.TempDir())
	m.stallEvery, m.stallThreshold = 20*time.Millisecond, 60*time.Millisecond
	log := &eventLog{}
	m.OnEvent = log.add
	spawn(t, m, scopedCtx(t.TempDir()), tools.BGSpawn{Command: `printf 'Overwrite? '; sleep 60`})
	defer m.Shutdown()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if msg := m.Drain(); msg != "" {
			if !strings.Contains(msg, "interactive input") || !strings.Contains(msg, "Overwrite?") {
				t.Fatalf("stall notification malformed:\n%s", msg)
			}
			if strings.Contains(msg, "<status>") {
				t.Fatal("stall notification must not carry terminal <status>")
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ev := log.waitEvents(t, 1); len(ev) != 1 || !ev[0].Stalled {
		t.Fatalf("stalled events=%+v, want one stalled", ev)
	}
}

// Drain 取出的通知在交付失败时可经 Requeue 原样归还（队首）。
func TestRequeueRestoresDrain(t *testing.T) {
	m := New(t.TempDir())
	bt := spawn(t, m, scopedCtx(t.TempDir()), tools.BGSpawn{Command: "true"})
	waitStatus(t, m, bt.ID)
	msg := m.Drain()
	if msg == "" {
		t.Fatal("expected notification")
	}
	m.Requeue(msg)
	if got := m.Drain(); got != msg {
		t.Fatal("requeued notification should come back verbatim")
	}
}

// 会话 id 直接拼进输出路径：白名单外字符（含 ../ 路径分隔符）一律
// 替换，防越出 tasks 根目录。
func TestSessionDirSanitizes(t *testing.T) {
	for in, want := range map[string]string{
		"":          "_",
		"abc-123_X": "abc-123_X",
		"../etc":    "___etc",
		"..":        "__",
		"a/b":       "a_b",
		"a.b":       "a_b",
		"a b":       "a_b",
	} {
		if got := sessionDir(in); got != want {
			t.Fatalf("sessionDir(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLooksLikePrompt(t *testing.T) {
	for tail, want := range map[string]bool{
		"Remove file? (y/n)":          true,
		"Proceed? [y/n]":              true,
		"answer (yes/no)":             true,
		"Do you want to continue?":    true,
		"Press Enter to continue":     true,
		"Overwrite?":                  true,
		"downloading packages…":       false,
		"100% completed":              false,
		"line1\nline2\nOverwrite?":    true,
		"Are you sure about this? ":   true,
		"no prompt here\njust output": false,
	} {
		if got := looksLikePrompt(tail); got != want {
			t.Fatalf("looksLikePrompt(%q)=%v, want %v", tail, got, want)
		}
	}
}
