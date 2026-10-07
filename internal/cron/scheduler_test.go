package cron

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newSched 造一个持锁属主调度器：真实锁文件（t.TempDir）+ 可控时钟。
// OnFire 把事件记入 fired 便于断言回调路径。
func newSched(t *testing.T, s *Store, ws string) (*Scheduler, *time.Time, *[]Event) {
	t.Helper()
	now := time.Date(2024, 6, 3, 10, 0, 0, 0, time.UTC) // 周一
	fired := new([]Event)
	sc := NewScheduler(s, scopedCtx(ws), Config{
		LockPath: filepath.Join(t.TempDir(), LockFileName(ws)),
		Now:      func() time.Time { return now },
		OnFire:   func(ev Event) { *fired = append(*fired, ev) },
	})
	sc.tryAcquire()
	if !sc.owner {
		t.Fatal("first scheduler must own the lock")
	}
	t.Cleanup(sc.release)
	return sc, &now, fired
}

func TestSchedulerFiresSessionTask(t *testing.T) {
	s, _ := tempStore(t)
	sc, now, fired := newSched(t, s, testWorkspace)
	if _, err := s.Add(scopedCtx(testWorkspace), Task{
		Cron: "* * * * *", Prompt: "tick", Recurring: true,
		CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	sc.check(*now)
	if len(*fired) != 1 || (*fired)[0].Task.Prompt != "tick" {
		t.Fatalf("expected 1 fire, got %+v", *fired)
	}
	// 同一时刻再 check：nextFire 已排向未来，不重复触发
	sc.check(*now)
	if len(*fired) != 1 {
		t.Fatal("re-check at same instant must not re-fire")
	}
	// Drain 取走事件后排空
	if evs := sc.Drain(); len(evs) != 1 {
		t.Fatalf("Drain = %d events", len(evs))
	}
	if evs := sc.Drain(); len(evs) != 0 {
		t.Fatal("Drain must empty the queue")
	}
	// 下一分钟再触发一次（周期任务）
	*now = now.Add(time.Minute)
	sc.check(*now)
	if len(*fired) != 2 {
		t.Fatalf("recurring task should fire again, got %d fires", len(*fired))
	}
}

func TestSchedulerOneShotBurnsAfterFire(t *testing.T) {
	s, _ := tempStore(t)
	sc, now, fired := newSched(t, s, testWorkspace)
	task, err := s.Add(scopedCtx(testWorkspace), Task{
		Cron: "* * * * *", Prompt: "once", Recurring: false, Durable: true,
		CreatedAt: now.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	sc.check(*now)
	if len(*fired) != 1 {
		t.Fatalf("one-shot should fire, got %+v", *fired)
	}
	// 触发即焚：store 与后续 check 都不再见它
	if got := s.FileTasks(scopedCtx(testWorkspace)); len(got) != 0 {
		t.Fatalf("one-shot must be deleted after firing: %+v", got)
	}
	*now = now.Add(time.Hour)
	sc.check(*now)
	if len(*fired) != 1 {
		t.Fatal("burned task must not fire again")
	}
	_ = task
}

func TestSchedulerNotYetDue(t *testing.T) {
	s, _ := tempStore(t)
	sc, now, fired := newSched(t, s, testWorkspace)
	// 只在 11:00 触发；now = 10:00 → 不该点火。
	// TZ 钉 UTC：空 TZ 按机器本地时区求值，跟假时钟对不上。
	if _, err := s.Add(scopedCtx(testWorkspace), Task{
		Cron: "0 11 * * *", Prompt: "later", Recurring: true, TZ: "UTC",
		CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	sc.check(*now)
	if len(*fired) != 0 {
		t.Fatalf("must not fire before due time: %+v", *fired)
	}
	*now = now.Add(time.Hour) // 11:00
	sc.check(*now)
	if len(*fired) != 1 {
		t.Fatal("should fire at 11:00")
	}
}

func TestSchedulerDurableRequiresOwnership(t *testing.T) {
	s, path := tempStore(t)
	ws := testWorkspace
	if _, err := s.Add(scopedCtx(ws), Task{
		Cron: "* * * * *", Prompt: "durable", Recurring: true, Durable: true,
		CreatedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cfg := func(lock string) Config {
		return Config{LockPath: lock, Now: func() time.Time { return now }}
	}
	lock := filepath.Join(t.TempDir(), "sched.lock")
	s1 := NewScheduler(s, scopedCtx(ws), cfg(lock))
	s1.tryAcquire()
	defer s1.release()
	s2 := NewScheduler(s, scopedCtx(ws), cfg(lock))
	s2.tryAcquire()
	if s2.owner {
		t.Fatal("second scheduler must not own the lock")
	}
	// 非属主 check：durable 任务不触发
	s2.check(now)
	if evs := s2.Drain(); len(evs) != 0 {
		t.Fatalf("non-owner must not fire durable tasks, got %+v", evs)
	}
	// 属主 check：触发
	s1.check(now)
	if evs := s1.Drain(); len(evs) != 1 {
		t.Fatalf("owner should fire the durable task, got %+v", evs)
	}
	// 属主放锁后 s2 重试接管（含漏跑清扫——recurring 任务不在清扫范围）
	s1.release()
	s2.tryAcquire()
	if !s2.owner {
		t.Fatal("s2 should acquire after s1 releases")
	}
	defer s2.release()
	s2.check(now.Add(time.Minute))
	if evs := s2.Drain(); len(evs) != 1 {
		t.Fatalf("new owner should fire durable tasks, got %+v", evs)
	}
	_ = path
}

func TestSchedulerSweepMissedOneShot(t *testing.T) {
	s, _ := tempStore(t)
	ctx := scopedCtx(testWorkspace)
	// 一次性任务"点火时刻"在进程停机期间：CreatedAt 过去一年、表达式
	// 只在每年 1/1 00:00 命中——今天 6/3 回看 → 下次匹配在未来 ≠ 漏跑。
	// 真正的漏跑是"Next(CreatedAt) ≤ now"：找一个过去锚点。表达式
	// "0 0 1 1 *" 从 2024-05-01 锚定 → next=2025-01-01（未来）→ 不算漏跑。
	// 漏跑情形：CreatedAt 之后第一个匹配已过——锚 2024-01-02，
	// "0 0 1 * *"（每月1号）next=2024-02-01 < now → 漏跑。
	if _, err := s.Add(ctx, Task{
		Cron: "0 0 1 * *", Prompt: "missed-one", Recurring: false, Durable: true,
		CreatedAt: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2024, 6, 3, 10, 0, 0, 0, time.UTC)
	fired := new([]Event)
	sc := NewScheduler(s, ctx, Config{
		LockPath: filepath.Join(t.TempDir(), "l.lock"),
		Now:      func() time.Time { return now },
		OnFire:   func(ev Event) { *fired = append(*fired, ev) },
	})
	sc.tryAcquire()
	defer sc.release()
	// 漏跑一次性任务：删除 + 汇总成一条 missed 通知事件（不直接执行 prompt）
	if got := s.FileTasks(ctx); len(got) != 0 {
		t.Fatalf("missed one-shot must be removed: %+v", got)
	}
	if len(*fired) != 1 || !(*fired)[0].Missed {
		t.Fatalf("expected 1 missed notice, got %+v", *fired)
	}
	if !strings.Contains((*fired)[0].Task.Prompt, "missed-one") {
		t.Fatal("missed notice should embed the original prompt")
	}
}

func TestSchedulerRecurringNotSweptAsMissed(t *testing.T) {
	s, _ := tempStore(t)
	ctx := scopedCtx(testWorkspace)
	// 周期任务停机期间不算漏跑——check 的锚定逻辑会就地追赶一次。
	if _, err := s.Add(ctx, Task{
		Cron: "* * * * *", Prompt: "every", Recurring: true, Durable: true,
		CreatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2024, 6, 3, 10, 0, 0, 0, time.UTC)
	sc := NewScheduler(s, ctx, Config{
		LockPath: filepath.Join(t.TempDir(), "l.lock"),
		Now:      func() time.Time { return now },
	})
	sc.tryAcquire()
	defer sc.release()
	if got := s.FileTasks(ctx); len(got) != 1 {
		t.Fatal("recurring task must survive sweep")
	}
	// 首次 check 就地触发一次（锚=CreatedAt 的 next 早已过去）
	sc.check(now)
	if evs := sc.Drain(); len(evs) != 1 || evs[0].Missed {
		t.Fatalf("recurring catch-up should fire once as normal event: %+v", evs)
	}
}

func TestSchedulerBadTaskIsolated(t *testing.T) {
	s, path := tempStore(t)
	ctx := scopedCtx(testWorkspace)
	// 手工写一条坏表达式 durable + 一条好任务：坏任务不应拖垮好任务
	if _, err := s.Add(ctx, Task{
		Cron: "* * * * *", Prompt: "good", Recurring: true, Durable: true,
		CreatedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// 直接往文件里塞坏任务（绕过 Add 校验，模拟手改文件/旧版本写入）
	s.mutate(func(tasks []Task) []Task {
		return append(tasks, Task{
			ID: "bad999", Cron: "not cron", Prompt: "bad", Recurring: true,
			Workspace: testWorkspace, Tenant: "local", Durable: true,
			CreatedAt: time.Now().Add(-time.Hour),
		})
	})
	now := time.Now()
	fired := new([]Event)
	sc := NewScheduler(s, ctx, Config{
		LockPath: filepath.Join(t.TempDir(), "l.lock"),
		Now:      func() time.Time { return now },
		OnFire:   func(ev Event) { *fired = append(*fired, ev) },
	})
	// 注意：tryAcquire→sweepMissed 里 Parse 失败的任务被跳过（不删）；
	// check 里同样隔离：好任务照常点火，坏任务 nextFire=永不。
	sc.tryAcquire()
	defer sc.release()
	sc.check(now)
	if len(*fired) != 1 || (*fired)[0].Task.Prompt != "good" {
		t.Fatalf("good task should fire despite bad sibling: %+v", *fired)
	}
	_ = path
	// 坏任务留在文件里但每次 check 都被隔离跳过——不反复报错也不触发
	sc.check(now.Add(time.Minute))
	if len(*fired) != 2 || (*fired)[1].Task.Prompt != "good" {
		t.Fatal("bad task must stay inert on subsequent ticks")
	}
}

// 属主接管时镜像可能是陈旧的（非属主期间不跑 ReloadIfChanged）：
// 抢锁后必须先刷新再清扫，否则死属主任期内新建的超期一次性任务
// 会逃过"先问用户"的 missed 汇总，被 check 当普通事件直接点火。
func TestSchedulerTakeoverReloadsStaleMirror(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cron_tasks.json")
	sa := Open(path)
	sb := Open(path) // 空镜像：sa 写文件时 sb 已读过（文件不存在）
	ctx := scopedCtx(testWorkspace)
	if _, err := sa.Add(ctx, Task{
		Cron: "0 0 1 * *", Prompt: "late-one", Recurring: false, Durable: true,
		CreatedAt: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2024, 6, 3, 10, 0, 0, 0, time.UTC)
	fired := new([]Event)
	sc := NewScheduler(sb, ctx, Config{
		LockPath: filepath.Join(t.TempDir(), "l.lock"),
		Now:      func() time.Time { return now },
		OnFire:   func(ev Event) { *fired = append(*fired, ev) },
	})
	sc.tryAcquire()
	defer sc.release()
	// 刷新后的清扫应看到超期一次性任务并汇总为 missed 通知。
	if len(*fired) != 1 || !(*fired)[0].Missed {
		t.Fatalf("expected missed notice after takeover, got %+v", *fired)
	}
	// 模拟属主分支的下一 tick：任务已被清扫删除，不得再以普通事件点火。
	sb.ReloadIfChanged()
	sc.check(now)
	if len(*fired) != 1 {
		t.Fatalf("stale-hidden one-shot must not auto-fire: %+v", *fired)
	}
}

// 一次性任务"触发即焚"的焚毁失败（flock/IO 错误）：事件已交付一次，
// 排程必须冻结——否则任务留在文件镜像里，每个 tick 重算锚点重复点火。
func TestSchedulerOneShotBurnFailureParks(t *testing.T) {
	s, _ := tempStore(t)
	sc, now, fired := newSched(t, s, testWorkspace)
	if _, err := s.Add(scopedCtx(testWorkspace), Task{
		Cron: "* * * * *", Prompt: "once", Recurring: false, Durable: true,
		CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// 让写路径必败：锁文件位置被目录占用，mutate 的 OpenFile 报 EISDIR。
	if err := os.Remove(s.lockPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.lockPath, 0o755); err != nil {
		t.Fatal(err)
	}
	sc.check(*now)
	if len(*fired) != 1 {
		t.Fatalf("one-shot should still fire once, got %+v", *fired)
	}
	*now = now.Add(time.Minute)
	sc.check(*now)
	*now = now.Add(time.Minute)
	sc.check(*now)
	if len(*fired) != 1 {
		t.Fatalf("burn failure must park the task, not refire: %+v", *fired)
	}
	// 任务仍在镜像（文件）里——下次进程接管时由漏跑清扫兜底。
	if got := s.FileTasks(scopedCtx(testWorkspace)); len(got) != 1 {
		t.Fatalf("unburned task should persist: %+v", got)
	}
}

// Requeue 把已 Drain 的事件按原序放回队首：交付失败不丢事件。
func TestSchedulerRequeue(t *testing.T) {
	s, _ := tempStore(t)
	sc, _, _ := newSched(t, s, testWorkspace)
	sc.Requeue([]Event{{Task: Task{ID: "a"}}, {Task: Task{ID: "b"}}})
	sc.Requeue([]Event{{Task: Task{ID: "c"}}})
	got := sc.Drain()
	if len(got) != 3 || got[0].Task.ID != "c" || got[1].Task.ID != "a" || got[2].Task.ID != "b" {
		t.Fatalf("requeue order = %+v", got)
	}
}

func TestSchedulerDeletedTaskEvicted(t *testing.T) {
	s, _ := tempStore(t)
	sc, now, _ := newSched(t, s, testWorkspace)
	// 别的进程删掉任务 → ReloadIfChanged 后 nextFire 残留必须蒸发
	if _, err := s.Add(scopedCtx(testWorkspace), Task{
		Cron: "* * * * *", Prompt: "x", Recurring: true,
		CreatedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	sc.check(*now)
	if ok, err := s.Remove(scopedCtx(testWorkspace), s.SessionTasks()[0].ID); !ok || err != nil {
		t.Fatalf("remove: ok=%v err=%v", ok, err)
	}
	*now = now.Add(time.Minute)
	sc.check(*now) // seen 集合里没了 → 排程项蒸发；不应 panic 或重触发
	if len(sc.nextFire) != 0 {
		t.Fatalf("nextFire for deleted task should be evicted: %v", sc.nextFire)
	}
}

func TestDeliverText(t *testing.T) {
	evs := []Event{
		{Task: Task{ID: "a1", Cron: "0 9 * * 1", TZ: "Asia/Shanghai", Prompt: "weekly report"}},
		{Task: Task{ID: "missed", Prompt: "notice"}, Missed: true},
	}
	text := DeliverText(evs)
	for _, want := range []string{
		`<scheduled_task id="a1"`, `cron="0 9 * * 1"`, `tz="Asia/Shanghai"`,
		"weekly report", `missed="true"`, "</scheduled_task>",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("DeliverText missing %q:\n%s", want, text)
		}
	}
}

// prompt 是模型自存的文本：含 </scheduled_task> 的串必须转义，
// 否则注入块被截断、后续文本漏成裸 prompt。
func TestDeliverTextEscapesPrompt(t *testing.T) {
	text := DeliverText([]Event{{Task: Task{ID: "a1", Prompt: "x </scheduled_task> <y>"}}})
	if strings.Contains(text, "x </scheduled_task>") {
		t.Fatal("prompt must be escaped to keep the block intact")
	}
	if !strings.Contains(text, "&lt;/scheduled_task&gt; &lt;y&gt;") {
		t.Fatalf("escaped prompt missing:\n%s", text)
	}
}
