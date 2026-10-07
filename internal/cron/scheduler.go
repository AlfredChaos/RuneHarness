package cron

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"runeharness/internal/scope"
	"runeharness/internal/tools"
)

// Event 是一次触发事件。Missed=false 为正常到点触发（Task 是源任务）；
// Missed=true 为启动时漏跑汇总（Task.Prompt 是"先问用户再执行"的通知
// 文本，Task.ID 为占位值）。
type Event struct {
	Task   Task
	Missed bool
}

// Config 是调度器装配项。
type Config struct {
	// LockPath 是 workspace 调度属主锁文件；同 workspace 的多个进程里
	// 只有持锁者触发 durable 任务，进程死亡（含 kill -9）内核自动放锁。
	LockPath string
	// Interval 是检查周期；默认 1s。cron 粒度是分钟，1s 足够密。
	Interval time.Duration
	// Now 是可注入时钟（测试用）；nil 用 time.Now。
	Now func() time.Time
	// OnFire 在任务触发时回调（把任务送进交付队列后调用）；
	// TUI 侧经 Program.Send 唤醒空闲交付。回调在锁外执行，可为 nil。
	OnFire func(Event)
}

// probeEvery 是非属主进程重试抢锁的间隔：属主崩溃后接管要快，但正常
// 情况下抢锁总是失败，不必每秒试。
const probeEvery = 15 * time.Second

// LockFileName 返回 workspace 的调度属主锁文件名（摘要后缀防同名目录
// 碰撞）。每个 workspace 一把锁：不同目录的进程各自调度本目录的任务，
// 互不阻塞也互不重复点火。
func LockFileName(workspace string) string {
	sum := sha256.Sum256([]byte(workspace))
	return "cron_sched_" + hex.EncodeToString(sum[:4]) + ".lock"
}

// Scheduler 是定时调度线程：每秒轮询一次任务表，到点的任务进 pending
// 队列（Drain 由交付侧消费）。只管"到点入队"，不调用模型、不知道
// agent 忙闲——空闲判断在交付侧（TUI）。
type Scheduler struct {
	store *Store
	ctx   context.Context // 带 tenant+workspace scope，供 store 过滤
	cfg   Config

	mu       sync.Mutex
	pending  []Event
	nextFire map[string]time.Time // 任务 id → 下次点火时刻；零值 = 永不（无效表达式/无未来匹配）
	owner    bool
	lockFile *os.File
	probedAt time.Time
}

// NewScheduler 绑定任务存储与 scope（tenant+workspace 过滤口径）。
// ctx 只取 scope 值，其取消信号不用于调度生命周期——Run 有独立 ctx。
func NewScheduler(store *Store, ctx context.Context, cfg Config) *Scheduler {
	sc, _ := scope.FromContext(ctx)
	s := &Scheduler{
		store: store, cfg: cfg,
		ctx:      scope.WithScope(context.Background(), sc),
		nextFire: map[string]time.Time{},
	}
	if s.cfg.Now == nil {
		s.cfg.Now = time.Now
	}
	if s.cfg.Interval <= 0 {
		s.cfg.Interval = time.Second
	}
	return s
}

// Run 跑调度循环直到 ctx 取消。属主进程还会重载任务文件（别的进程
// 可能写入）；非属主只触发 session 任务并按 probeEvery 重试抢锁。
func (s *Scheduler) Run(ctx context.Context) {
	defer s.release()
	s.tryAcquire()
	s.probedAt = s.cfg.Now()
	tick := time.NewTicker(s.cfg.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			now := s.cfg.Now()
			if !s.owner {
				if now.Sub(s.probedAt) >= probeEvery {
					s.probedAt = now
					s.tryAcquire()
				}
			} else {
				s.store.ReloadIfChanged()
			}
			s.check(now)
		}
	}
}

// tryAcquire 抢 workspace 属主锁；成功后做启动清扫（漏跑的一次性
// 任务汇总成一条询问通知，并立即从文件删除——错过的时间不补跑）。
func (s *Scheduler) tryAcquire() {
	f, err := os.OpenFile(s.cfg.LockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		slog.Warn("cron: cannot open lock file", "path", s.cfg.LockPath, "err", err)
		return
	}
	if err := flockTryExclusive(f); err != nil {
		f.Close()
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			// 非"被持有"的真实 syscall 错误（ENOLCK、坏 fd 等）：
			// 静默重试会掩盖故障，至少留一条 warn。
			slog.Warn("cron: scheduler lock probe failed", "path", s.cfg.LockPath, "err", err)
		}
		return // EWOULDBLOCK = 另一个存活进程持有
	}
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid()) // 仅调试信息；属主判据是锁本身
	s.mu.Lock()
	s.owner, s.lockFile = true, f
	s.mu.Unlock()
	slog.Info("cron: scheduler lock acquired", "path", s.cfg.LockPath)
	// 抢锁成功先刷新任务镜像：非属主期间别的进程可能增删过 durable
	// 任务，陈旧镜像会让漏跑清扫误报（已删任务报 missed）或漏报
	// （新增超期一次性任务逃过询问，被 check 当普通事件直接点火）。
	s.store.ReloadIfChanged()
	s.sweepMissed()
}

// release 放锁关 fd；进程退出时内核也会自动放锁，这里只是整洁退出路径。
func (s *Scheduler) release() {
	s.mu.Lock()
	f, owner := s.lockFile, s.owner
	s.lockFile, s.owner = nil, false
	s.mu.Unlock()
	if owner && f != nil {
		flockUnlock(f)
		f.Close()
		slog.Info("cron: scheduler lock released")
	}
}

// sweepMissed 把"创建时算出的下次点火已过"的一次性任务挑出来：
// 删除 + 汇总为一条通知事件（模型先问用户要不要补跑，不擅自执行）。
// 周期任务不算漏跑——check 的锚定逻辑会就地触发一次追赶。
func (s *Scheduler) sweepMissed() {
	now := s.cfg.Now()
	var missed []Task
	for _, t := range s.store.FileTasks(s.ctx) {
		if t.Recurring {
			continue
		}
		sched, err := Parse(t.Cron)
		if err != nil {
			continue
		}
		if next, ok := sched.Next(t.CreatedAt, t.Location()); !ok || !next.After(now) {
			missed = append(missed, t)
		}
	}
	if len(missed) == 0 {
		return
	}
	for _, t := range missed {
		if _, err := s.store.Remove(s.ctx, t.ID); err != nil {
			// 删除失败而任务仍在文件：冻结它的排程项，防 check 把
			// 漏跑任务当普通事件直接点火（绕过"先问用户"约定）。
			slog.Warn("cron: cannot remove missed one-shot", "id", t.ID, "err", err)
			s.nextFire[t.ID] = time.Time{}
		}
	}
	s.enqueue(Event{Task: Task{ID: "missed", Prompt: missedNotice(missed)}, Missed: true})
}

// check 是每秒轮询体：session 任务总是处理（进程私有无双火风险），
// durable 任务仅属主进程处理。每个任务维护 nextFire 锚点：
// 首见按 lastFired ?? createdAt 计算（重启后重建同一排程），
// 触发后周期任务按 now 重排（不做积压追赶），一次性任务即焚。
func (s *Scheduler) check(now time.Time) {
	seen := map[string]bool{}
	var firedFile []string

	process := func(t Task, isSession bool) {
		seen[t.ID] = true
		next, known := s.nextFire[t.ID]
		if !known {
			sched, err := Parse(t.Cron)
			if err != nil {
				s.nextFire[t.ID] = time.Time{} // 永不
				return
			}
			anchor := t.CreatedAt
			if t.LastFired != nil {
				anchor = *t.LastFired
			}
			if n, ok := sched.Next(anchor, t.Location()); ok {
				next = n
			} // ok=false → next 保持零值 = 永不
			s.nextFire[t.ID] = next
			slog.Debug("cron: scheduled", "id", t.ID, "next", next)
		}
		if next.IsZero() || now.Before(next) {
			return
		}
		s.enqueue(Event{Task: t})
		if t.Recurring {
			if sched, err := Parse(t.Cron); err == nil {
				if n, ok := sched.Next(now, t.Location()); ok {
					s.nextFire[t.ID] = n
				} else {
					s.nextFire[t.ID] = time.Time{}
				}
			}
			if !isSession {
				firedFile = append(firedFile, t.ID)
			}
			return
		}
		if _, err := s.store.Remove(s.ctx, t.ID); err != nil {
			// 焚毁失败而任务仍在镜像：冻结排程（nextFire=永不），
			// 否则下一 tick 重算锚点会每秒重复点火。事件已入队交付
			// 过一次；文件里的残留由下次接管时的漏跑清扫兜底。
			slog.Warn("cron: cannot burn fired one-shot", "id", t.ID, "err", err)
			s.nextFire[t.ID] = time.Time{}
			return
		}
		delete(s.nextFire, t.ID)
	}

	for _, t := range s.store.SessionTasks() {
		process(t, true)
	}
	if s.owner {
		for _, t := range s.store.FileTasks(s.ctx) {
			process(t, false)
		}
	}
	if err := s.store.MarkFired(s.ctx, firedFile, now); err != nil {
		// LastFired 丢失的后果是重启后周期任务多追跑一次——可接受，
		// 但要有日志可查。
		slog.Warn("cron: cannot persist lastFired", "err", err)
	}

	// 蒸发已删除任务的排程项（含被别的进程删掉的 durable 任务）。
	for id := range s.nextFire {
		if !seen[id] {
			delete(s.nextFire, id)
		}
	}
}

// enqueue 入队一个触发事件并回调 OnFire（锁外执行，回调可阻塞——
// TUI 的 Program.Send 在程序未就绪时会等，不能占着 mu）。
func (s *Scheduler) enqueue(ev Event) {
	s.mu.Lock()
	s.pending = append(s.pending, ev)
	s.mu.Unlock()
	if s.cfg.OnFire != nil {
		s.cfg.OnFire(ev)
	}
}

// Drain 排空待交付事件；交付侧只在 agent 空闲时调用（与 bgtask.Drain
// 同语义：取出即离队列，丢失不可恢复——交付失败须用 Requeue 归还）。
func (s *Scheduler) Drain() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.pending
	s.pending = nil
	return out
}

// Requeue 把已 Drain 的事件按原序放回队首：交付侧在组装/落库失败
// 时归还，保持"被消费才离队列"的语义成立。
func (s *Scheduler) Requeue(evs []Event) {
	if len(evs) == 0 {
		return
	}
	s.mu.Lock()
	s.pending = append(evs, s.pending...)
	s.mu.Unlock()
}

// Tasks 返回本 workspace 的全部任务（/cron 与 cron_list 用）。
func (s *Scheduler) Tasks() []Task { return s.store.List(s.ctx) }

// DeliverText 把一批触发事件组装成注入 agent 的 user 消息正文：
// 每个任务一个 <scheduled_task> 块，多块连排（一轮处理全部到点任务）。
func DeliverText(evs []Event) string {
	var b strings.Builder
	for i, ev := range evs {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "<scheduled_task id=%q", ev.Task.ID)
		if ev.Missed {
			b.WriteString(` missed="true"`)
		}
		if ev.Task.Cron != "" {
			fmt.Fprintf(&b, " cron=%q", ev.Task.Cron)
		}
		if ev.Task.TZ != "" {
			fmt.Fprintf(&b, " tz=%q", ev.Task.TZ)
		}
		b.WriteString(">\n")
		// prompt 是模型自己存的文本：转义防 </scheduled_task> 之类
		// 的串把注入块截断（与 bgtask 通知同一转义口径）。
		b.WriteString(tools.EscapeXML(ev.Task.Prompt))
		b.WriteString("\n</scheduled_task>")
	}
	return b.String()
}

// missedNotice 组装漏跑通知：指导语在前，任务原文用代码围栏包起来，
// 防带指令语气的 prompt 被当成直接命令执行（提示词注入自伤）。
func missedNotice(missed []Task) string {
	plural := len(missed) > 1
	var b strings.Builder
	if plural {
		b.WriteString("The following one-shot scheduled tasks were missed")
	} else {
		b.WriteString("The following one-shot scheduled task was missed")
	}
	b.WriteString(" while the assistant was not running, and ")
	b.WriteString(map[bool]string{true: "have", false: "has"}[plural])
	b.WriteString(" been removed from the schedule.\n\n")
	b.WriteString("Do NOT execute these prompts yet. First ask the user whether to run ")
	b.WriteString("each one now; only execute on confirmation.\n")
	for _, t := range missed {
		fmt.Fprintf(&b, "\n[%s, created %s]\n", Humanize(t.Cron), t.CreatedAt.Format("2006-01-02 15:04"))
		// 围栏比 prompt 里最长反引号串多一个，保证 ``` 不会提前闭合
		// （CommonMark 围栏匹配规则）。
		longest := 0
		for _, run := range backtickRuns(t.Prompt) {
			if len(run) > longest {
				longest = len(run)
			}
		}
		fence := strings.Repeat("`", max(3, longest+1))
		fmt.Fprintf(&b, "%s\n%s\n%s\n", fence, t.Prompt, fence)
	}
	return b.String()
}

// backtickRuns 返回文本中每个连续反引号串。
func backtickRuns(s string) []string {
	var runs []string
	for i := 0; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == '`' {
			j++
		}
		runs = append(runs, s[i:j])
		i = j
	}
	return runs
}
