package cron

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"runeharness/internal/scope"
)

// MaxJobs 是单个 workspace 的任务数上限（durable + session 合计），
// 防模型写入失控；达到上限时 cron_create 返回错误让模型先清理。
const MaxJobs = 50

// Task 是一条定时任务。
// durable 任务落盘（cron_tasks.json，按 workspace 过滤）、跨进程保留；
// session 任务只存进程内存，随退出消失。
type Task struct {
	ID        string     `json:"id"`
	Cron      string     `json:"cron"`                // 五段式表达式，按 TZ 求值
	TZ        string     `json:"tz,omitempty"`        // IANA 时区；空 = 进程本地时区
	Prompt    string     `json:"prompt"`              // 触发时注入的消息
	Recurring bool       `json:"recurring"`           // true 周期触发；false 触发一次后自动删除
	Workspace string     `json:"workspace"`           // durable 任务的归属 workspace（调度器只点火本 workspace 的）
	Tenant    string     `json:"tenant"`              // 归属租户（本地恒 local）
	CreatedAt time.Time  `json:"createdAt"`           // 锚点：漏跑检测与首次点火时间计算
	LastFired *time.Time `json:"lastFired,omitempty"` // 最近一次点火时间，重启后据此重排周期任务

	// Durable 仅运行时使用：true 写入 cron_tasks.json，false 只进进程
	// 内存。文件内不写此字段——落盘即 durable。
	Durable bool `json:"-"`
}

// Location 返回任务的求值时区；空 TZ 回落本地，非法 TZ 同样回落本地
// （加载时已校验剔除，此处仅防御，不重复告警）。
func (t Task) Location() *time.Location {
	if t.TZ == "" {
		return time.Local
	}
	loc, err := time.LoadLocation(t.TZ)
	if err != nil {
		return time.Local
	}
	return loc
}

// Path 返回任务文件位置（工具结果里向模型报告持久化落点）。
func (s *Store) Path() string { return s.path }

// cronFile 是 cron_tasks.json 的落盘形态。
type cronFile struct {
	Tasks []Task `json:"tasks"`
}

// Store 管理 durable（JSON 文件）与 session（进程内存）两档任务。
// 文件读改写经 cron_tasks.lock 的 flock 串行化（同 workspace 可能并存
// 多个进程，各自都可增删任务），写出走 tmp+rename 保证读侧快照一致。
// 调度属主权是另一把锁（scheduler.go），这里只管数据不丢。
type Store struct {
	path     string // cron_tasks.json
	lockPath string // cron_tasks.lock（写互斥，不是调度锁）

	mu      sync.Mutex
	durable []Task // 文件镜像（内存态，写时整体回写）
	session []Task // 进程私有任务
	mtime   time.Time
	size    int64
}

// Open 加载任务文件；文件缺失/损坏按空表处理（损坏不丢已有进程内状态——
// 调用方拿到的是空 Store，下次写入会覆盖坏文件）。Open 本身不报错：
// 真正的 IO 失败发生在 mutate 写路径，经工具结果回报给模型。
func Open(path string) *Store {
	s := &Store{
		path:     path,
		lockPath: strings.TrimSuffix(path, ".json") + ".lock",
	}
	s.reload()
	return s
}

// reload 重读文件并刷新 mtime 快照；非法条目丢弃并告警，单条坏数据
// 不拖垮整个文件。
func (s *Store) reload() {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return // 缺文件 = 空表
	}
	var f cronFile
	if err := json.Unmarshal(raw, &f); err != nil {
		slog.Warn("cron: ignoring malformed tasks file", "path", s.path, "err", err)
		return
	}
	out := f.Tasks[:0]
	for _, t := range f.Tasks {
		if t.ID == "" || t.Cron == "" || t.Prompt == "" || t.CreatedAt.IsZero() {
			slog.Warn("cron: dropping malformed task", "id", t.ID)
			continue
		}
		if _, err := Parse(t.Cron); err != nil {
			slog.Warn("cron: dropping task with bad expression", "id", t.ID, "cron", t.Cron)
			continue
		}
		if t.TZ != "" {
			if _, err := time.LoadLocation(t.TZ); err != nil {
				slog.Warn("cron: dropping task with bad timezone", "id", t.ID, "tz", t.TZ)
				continue
			}
		}
		t.Durable = true // 落盘即 durable；字段是运行时标记不随文件读写
		out = append(out, t)
	}
	s.mu.Lock()
	s.durable = out
	if fi, err := os.Stat(s.path); err == nil {
		s.mtime, s.size = fi.ModTime(), fi.Size()
	}
	s.mu.Unlock()
}

// ReloadIfChanged 在文件 mtime/size 变化时重载（调度器每秒轮询调用，
// 让别的进程写入的任务在本进程生效）。文件被整体删除时清空内存镜像
// 并视为一次重载——否则已删任务会从内存态继续点火。返回是否发生了重载。
func (s *Store) ReloadIfChanged() bool {
	fi, err := os.Stat(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s.mu.Lock()
			cleared := len(s.durable) > 0
			s.durable, s.mtime, s.size = nil, time.Time{}, 0
			s.mu.Unlock()
			return cleared
		}
		return false
	}
	s.mu.Lock()
	changed := !fi.ModTime().Equal(s.mtime) || fi.Size() != s.size
	s.mu.Unlock()
	if changed {
		s.reload()
		return true
	}
	return false
}

// List 返回当前 scope workspace 的全部任务（durable 过滤 + session 全量）。
// 先做一次文件变更检查：非属主进程不走 Run 的定时 ReloadIfChanged，
// 镜像可能停在 Open 时的快照，/cron 与 cron_list 不能拿陈旧数据应付用户。
func (s *Store) List(ctx context.Context) []Task {
	s.ReloadIfChanged()
	return append(s.FileTasks(ctx), s.SessionTasks()...)
}

// FileTasks 返回 ctx scope workspace 的 durable 任务（文件镜像的过滤副本）。
func (s *Store) FileTasks(ctx context.Context) []Task {
	sc, _ := scope.FromContext(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Task
	for _, t := range s.durable {
		if t.Tenant == sc.TenantID && t.Workspace == sc.Workspace {
			out = append(out, t)
		}
	}
	return out
}

// SessionTasks 返回本进程的 session-only 任务（进程私有，不经文件）。
func (s *Store) SessionTasks() []Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Task(nil), s.session...)
}

// Add 校验并登记任务；返回带分配 ID 的定稿 Task。
// durable 任务从 ctx scope 打上 tenant/workspace 标记后落盘。
func (s *Store) Add(ctx context.Context, t Task) (Task, error) {
	if _, err := Parse(t.Cron); err != nil {
		return Task{}, fmt.Errorf("invalid cron expression: %w", err)
	}
	if t.TZ != "" {
		if _, err := time.LoadLocation(t.TZ); err != nil {
			return Task{}, fmt.Errorf("invalid timezone %q", t.TZ)
		}
	}
	if strings.TrimSpace(t.Prompt) == "" {
		return Task{}, errors.New("prompt is required")
	}
	sc, _ := scope.FromContext(ctx)
	if t.Durable && (sc.TenantID == "" || sc.Workspace == "") {
		// durable 任务按 tenant+workspace 打标入库；缺 scope 会写成
		// 任何调度器都看不见的孤儿条目（FileTasks 过滤恒不命中）。
		return Task{}, errors.New("durable tasks require a scoped context (tenant+workspace)")
	}
	if len(s.List(ctx)) >= MaxJobs {
		return Task{}, fmt.Errorf("too many scheduled jobs (max %d); cancel one first", MaxJobs)
	}
	id, err := newID()
	if err != nil {
		return Task{}, err
	}
	t.ID = id
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	if !t.Durable {
		s.mu.Lock()
		s.session = append(s.session, t)
		s.mu.Unlock()
		return t, nil
	}
	t.Tenant, t.Workspace = sc.TenantID, sc.Workspace
	err = s.mutate(func(tasks []Task) []Task { return append(tasks, t) })
	return t, err
}

// Remove 删除任务（先扫 session 再扫文件）；返回是否命中与写错误。
// 文件读改写失败时调用方必须知道——一次性任务"触发即焚"依赖这个结果，
// 吞掉会让已点火的任务留在文件里重复触发。
func (s *Store) Remove(ctx context.Context, id string) (bool, error) {
	s.mu.Lock()
	for i, t := range s.session {
		if t.ID == id {
			s.session = append(s.session[:i], s.session[i+1:]...)
			s.mu.Unlock()
			return true, nil
		}
	}
	s.mu.Unlock()
	removed := false
	sc, _ := scope.FromContext(ctx)
	err := s.mutate(func(tasks []Task) []Task {
		out := tasks[:0]
		for _, t := range tasks {
			if t.ID == id && t.Tenant == sc.TenantID && t.Workspace == sc.Workspace {
				removed = true
				continue
			}
			out = append(out, t)
		}
		if !removed {
			return nil // 未命中：放弃本次回写，mtime 不做无谓抖动
		}
		return out
	})
	return removed, err
}

// MarkFired 回写 durable 任务的 LastFired（周期任务重启后据此重排，
// 不靠进程内存）。session 任务不落盘，跳过；id 全部未命中时不写文件。
func (s *Store) MarkFired(ctx context.Context, ids []string, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	matched := false
	return s.mutate(func(tasks []Task) []Task {
		for i := range tasks {
			if want[tasks[i].ID] {
				tasks[i].LastFired = &at
				matched = true
			}
		}
		if !matched {
			return nil
		}
		return tasks
	})
}

// mutate 串行化一次"读-改-写"：进程内 mutex + 跨进程 flock（锁文件
// 永不 rename，fd 指向稳定 inode，无 stale-inode 窗口）。文件整体回写
// 到临时文件后 rename，读侧始终拿到一致快照。
// fn 返回 nil 表示放弃本次写入（未命中等无变更场景），内存镜像仍同步
// 到锁内重读的最新文件内容。
func (s *Store) mutate(fn func([]Task) []Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	lk, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lk.Close()
	if err := flockExclusive(lk); err != nil {
		return fmt.Errorf("cron lock: %w", err)
	}
	defer flockUnlock(lk)

	// 锁内重读：别的进程可能已改过，内存镜像在此刻之后才算最新。
	var cur cronFile
	if raw, err := os.ReadFile(s.path); err == nil {
		if json.Unmarshal(raw, &cur) != nil {
			cur = cronFile{}
		}
	}
	for i := range cur.Tasks {
		cur.Tasks[i].Durable = true
	}
	next := fn(cur.Tasks)
	if next == nil {
		s.durable = cur.Tasks // 不写文件，但镜像趁锁内重读顺手刷新
		return nil
	}
	cur.Tasks = next

	body, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return fmt.Errorf("cron marshal: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".cron-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(body, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	s.durable = cur.Tasks
	if fi, err := os.Stat(s.path); err == nil {
		s.mtime, s.size = fi.ModTime(), fi.Size()
	}
	return nil
}

// newID 生成 8 位十六进制任务 id（与 CC 的短 id 同形态；50 上限下
// 碰撞空间足够）。
func newID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("cron id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
