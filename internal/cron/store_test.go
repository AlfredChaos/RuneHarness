package cron

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"runeharness/internal/scope"
)

const testWorkspace = "/tmp/rune-test-ws"

func scopedCtx(ws string) context.Context {
	return scope.WithScope(context.Background(),
		scope.Scope{TenantID: "local", Workspace: ws})
}

func tempStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cron_tasks.json")
	return Open(path), path
}

func TestStoreAddSessionOnly(t *testing.T) {
	s, path := tempStore(t)
	ctx := scopedCtx(testWorkspace)
	task, err := s.Add(ctx, Task{Cron: "* * * * *", Prompt: "hi"})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if task.ID == "" || task.CreatedAt.IsZero() {
		t.Fatalf("task not finalized: %+v", task)
	}
	if len(s.SessionTasks()) != 1 {
		t.Fatal("session task missing")
	}
	// session 档不落盘
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("session task must not persist, stat err = %v", err)
	}
}

func TestStoreDurableRoundTrip(t *testing.T) {
	s, path := tempStore(t)
	ctx := scopedCtx(testWorkspace)
	task, err := s.Add(ctx, Task{
		Cron: "0 9 * * 1", Prompt: "weekly", Recurring: true,
		TZ: "Asia/Shanghai", Durable: true,
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if task.Workspace != testWorkspace || task.Tenant != "local" {
		t.Fatalf("scope stamping wrong: %+v", task)
	}
	// 重开同一文件应读回
	s2 := Open(path)
	got := s2.FileTasks(ctx)
	if len(got) != 1 || got[0].ID != task.ID || !got[0].Durable {
		t.Fatalf("round-trip tasks = %+v", got)
	}
	if got[0].TZ != "Asia/Shanghai" {
		t.Fatalf("tz lost: %+v", got[0])
	}
}

func TestStoreWorkspaceIsolation(t *testing.T) {
	s, _ := tempStore(t)
	wsA, wsB := scopedCtx("/ws/a"), scopedCtx("/ws/b")
	if _, err := s.Add(wsA, Task{Cron: "* * * * *", Prompt: "a", Durable: true}); err != nil {
		t.Fatal(err)
	}
	if got := s.FileTasks(wsA); len(got) != 1 {
		t.Fatal("wsA should see its task")
	}
	if got := s.FileTasks(wsB); len(got) != 0 {
		t.Fatal("wsB must not see wsA's task")
	}
	// Remove 同样按 scope 隔离：B 删不掉 A 的任务
	all := s.FileTasks(wsA)
	if ok, _ := s.Remove(wsB, all[0].ID); ok {
		t.Fatal("cross-workspace remove must not hit")
	}
	if ok, err := s.Remove(wsA, all[0].ID); !ok || err != nil {
		t.Fatalf("same-workspace remove: ok=%v err=%v", ok, err)
	}
}

func TestStoreRejects(t *testing.T) {
	s, _ := tempStore(t)
	ctx := scopedCtx(testWorkspace)
	for _, task := range []Task{
		{Cron: "not a cron", Prompt: "x"},
		{Cron: "* * * * *", Prompt: "x", TZ: "Not/AZone"},
		{Cron: "* * * * *", Prompt: "   "},
	} {
		if _, err := s.Add(ctx, task); err == nil {
			t.Fatalf("Add(%+v) expected error", task)
		}
	}
}

func TestStoreMaxJobs(t *testing.T) {
	s, _ := tempStore(t)
	ctx := scopedCtx(testWorkspace)
	for i := 0; i < MaxJobs; i++ {
		if _, err := s.Add(ctx, Task{Cron: "* * * * *", Prompt: "x"}); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	if _, err := s.Add(ctx, Task{Cron: "* * * * *", Prompt: "over"}); err == nil {
		t.Fatal("Add beyond MaxJobs must fail")
	}
}

func TestStoreMarkFiredPersists(t *testing.T) {
	s, path := tempStore(t)
	ctx := scopedCtx(testWorkspace)
	task, err := s.Add(ctx, Task{Cron: "* * * * *", Prompt: "x", Recurring: true, Durable: true})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := s.MarkFired(ctx, []string{task.ID}, at); err != nil {
		t.Fatalf("MarkFired: %v", err)
	}
	s2 := Open(path)
	got := s2.FileTasks(ctx)
	if len(got) != 1 || got[0].LastFired == nil || !got[0].LastFired.Equal(at) {
		t.Fatalf("LastFired not persisted: %+v", got)
	}
}

func TestStoreMalformedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cron_tasks.json")
	// 整体损坏：按空表处理，不报错
	if err := os.WriteFile(path, []byte("{garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Open(path)
	if got := s.List(scopedCtx(testWorkspace)); len(got) != 0 {
		t.Fatal("malformed file should read as empty")
	}
	// 单条损坏（坏表达式）：丢弃该条，保留好条目
	good := `{"tasks":[` +
		`{"id":"ok1","cron":"* * * * *","prompt":"x","recurring":true,` +
		`"workspace":"/tmp/rune-test-ws","tenant":"local","createdAt":"2024-01-01T00:00:00Z"},` +
		`{"id":"bad1","cron":"bogus","prompt":"x","workspace":"/tmp/rune-test-ws",` +
		`"tenant":"local","createdAt":"2024-01-01T00:00:00Z"}]}`
	if err := os.WriteFile(path, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	s = Open(path)
	got := s.FileTasks(scopedCtx(testWorkspace))
	if len(got) != 1 || got[0].ID != "ok1" {
		t.Fatalf("bad entry should be dropped, good kept: %+v", got)
	}
}

// Remove 未命中时不得产生文件写：fn 放弃 → 不建 tmp 不改 mtime。
func TestStoreRemoveAbsentNoWrite(t *testing.T) {
	s, path := tempStore(t)
	ok, err := s.Remove(scopedCtx(testWorkspace), "nosuch")
	if ok || err != nil {
		t.Fatalf("remove absent: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("remove miss must not create the tasks file")
	}
}

// List 先做文件变更检查：非属主进程（不跑 Run 的定时重载）也能在
// /cron、cron_list 里看到别的进程刚写入的任务。
func TestStoreListSeesExternalWrite(t *testing.T) {
	s, path := tempStore(t)
	ctx := scopedCtx(testWorkspace)
	body := `{"tasks":[{"id":"ext1","cron":"0 1 * * *","prompt":"ext",` +
		`"workspace":"/tmp/rune-test-ws","tenant":"local","createdAt":"2024-01-01T00:00:00Z"}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := s.List(ctx); len(got) != 1 || got[0].ID != "ext1" {
		t.Fatalf("List must refresh the mirror: %+v", got)
	}
}

// 任务文件被整体删除时清空内存镜像并视为一次重载——否则已删任务
// 会从内存态继续点火。
func TestStoreFileDeletedClearsMirror(t *testing.T) {
	s, path := tempStore(t)
	ctx := scopedCtx(testWorkspace)
	if _, err := s.Add(ctx, Task{Cron: "* * * * *", Prompt: "x", Recurring: true, Durable: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if !s.ReloadIfChanged() {
		t.Fatal("file deletion should count as a reload")
	}
	if got := s.FileTasks(ctx); len(got) != 0 {
		t.Fatal("deleted file must clear the mirror")
	}
}

func TestStoreReloadIfChanged(t *testing.T) {
	s, path := tempStore(t)
	ctx := scopedCtx(testWorkspace)
	if s.ReloadIfChanged() {
		t.Fatal("no file yet — should not reload")
	}
	if _, err := s.Add(ctx, Task{Cron: "* * * * *", Prompt: "x", Durable: true}); err != nil {
		t.Fatal(err)
	}
	// 外部直接改写文件 → mtime 变化 → 重载生效
	body := `{"tasks":[{"id":"ext1","cron":"0 1 * * *","prompt":"ext",` +
		`"workspace":"/tmp/rune-test-ws","tenant":"local","createdAt":"2024-01-01T00:00:00Z"}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if !s.ReloadIfChanged() {
		t.Fatal("mtime change should trigger reload")
	}
	got := s.FileTasks(ctx)
	if len(got) != 1 || got[0].ID != "ext1" {
		t.Fatalf("reload did not pick up external write: %+v", got)
	}
}
