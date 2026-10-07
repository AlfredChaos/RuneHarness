// Package bgtask 提供后台 shell 任务：run_command 的 run_in_background
// 把慢命令交给独立进程组执行，stdout/stderr 合并落盘
// （<baseDir>/<sessionID>/<taskID>.output）；完成/停滞时把
// <task_notification> 排进内存队列，由 agent loop 的 PreChat 挂点
// （运行中）或 TUI 空闲路径注入为 user 消息。
//
// 任务只持进程内存态，不持久化：进程退出即全部终止（Shutdown），
// 输出文件留在盘上供事后翻阅。当前仅 POSIX（sh -c + 进程组信号）。
package bgtask

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"runeharness/internal/scope"
	"runeharness/internal/tools"
)

// defaultTaskTimeout 是 Spawn 未给 Timeout 时的兜底时长。
// 正常路径由调用方（run_command）显式给出上限，此处仅防御直接用例。
const defaultTaskTimeout = 30 * time.Minute

// Status 是任务生命周期状态。
type Status string

const (
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusKilled    Status = "killed"
)

// Task 是后台任务的对外快照。
type Task struct {
	ID          string
	Description string // 通知与列表展示用短描述；缺省取命令首部
	Command     string
	Status      Status
	ExitCode    int // 未结束为 -1
	OutputPath  string
	StartedAt   time.Time
	EndedAt     time.Time
}

// Event 是任务事件：Stalled=true 表示停滞提醒（任务仍在运行），
// 否则 Task 处于终态。
type Event struct {
	Task    Task
	Stalled bool
}

// task 是 Manager 内部的任务记录；展示字段内嵌 Task。
type task struct {
	Task
	seq           int // 启动序号，List 排序用
	pgid          int
	cancel        context.CancelFunc
	file          *os.File
	notified      bool // 终态消息已经其他途径送达模型（task_kill 置位）
	killRequested bool // 经 Kill/Shutdown 主动终止
	deadline      bool // 到点被杀（区分 killed-by-timeout 与 killed-by-request）
	done          chan struct{}
}

// Manager 管理后台任务注册表与待注入的通知队列。
// 队列是进程全局的：跨会话切换（/resume）后通知仍按到达顺序送达
// 当前会话——后台任务是进程级事实，哪个会话看到不构成归属错误。
type Manager struct {
	baseDir string
	mu      sync.Mutex
	seq     int
	tasks   map[string]*task
	pending []string

	// 停滞看门狗参数：实例级配置（测试可按实例缩短），进程内不变。
	stallEvery     time.Duration
	stallThreshold time.Duration

	// OnEvent 在任务到达终态或停滞提醒时回调（TUI 展示 + 空闲唤醒）；
	// 回调在锁外执行，nil 可用。
	OnEvent func(Event)
}

// New 创建 Manager；baseDir 下按 sessionID 再分一层目录放输出文件。
func New(baseDir string) *Manager {
	return &Manager{
		baseDir: baseDir, tasks: map[string]*task{},
		stallEvery: stallEveryDefault, stallThreshold: stallThresholdDefault,
	}
}

// Spawn 启动后台命令并立即返回任务句柄；实现 tools.BGSpawner。
// 任务生存期只受自身 timeout / Kill / Shutdown 约束：调用方取消
// （用户 Esc、父轮结束）不会杀死它——故用 WithoutCancel 剥离取消链，
// scope 值仍随 ctx 传递。
func (m *Manager) Spawn(ctx context.Context, in tools.BGSpawn) (tools.BGTask, error) {
	sc, err := scope.FromContext(ctx)
	if err != nil {
		return tools.BGTask{}, err
	}
	if sc.Workspace == "" {
		return tools.BGTask{}, errors.New("scope: empty workspace")
	}
	if in.Command == "" {
		return tools.BGTask{}, errors.New("command is required")
	}
	if in.Timeout <= 0 {
		in.Timeout = defaultTaskTimeout
	}

	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("bg%d", m.seq)
	seq := m.seq
	m.mu.Unlock()

	dir := filepath.Join(m.baseDir, sessionDir(sc.SessionID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return tools.BGTask{}, err
	}
	outPath := filepath.Join(dir, id+".output")
	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return tools.BGTask{}, err
	}

	tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), in.Timeout)
	cmd := exec.CommandContext(tctx, "sh", "-c", in.Command)
	cmd.Dir = sc.Workspace
	// 独立进程组 + 级联杀组：sh -c 常再生子进程，CommandContext 默认
	// 只 Process.Kill 主进程会留下孤儿；Setpgid 后子进程自成组长，pgid==pid。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return nil // 恒 nil：让 Wait 回报真实退出码，不覆盖成 kill 错误
	}
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		cancel()
		return tools.BGTask{}, err
	}

	desc := in.Description
	if desc == "" {
		desc = truncateRunes(in.Command, 60)
	}
	t := &task{
		Task: Task{
			ID: id, Description: desc, Command: in.Command,
			Status: StatusRunning, ExitCode: -1, OutputPath: outPath,
			StartedAt: time.Now(),
		},
		seq: seq, pgid: cmd.Process.Pid, cancel: cancel, file: f,
		done: make(chan struct{}),
	}
	m.mu.Lock()
	m.tasks[id] = t
	m.mu.Unlock()

	go m.wait(t, cmd, tctx)
	go m.watchStall(t)
	return tools.BGTask{ID: id, OutputPath: outPath}, nil
}

// wait 等待进程结束并定稿状态、入队完成通知（未被其他途径报告过时）。
func (m *Manager) wait(t *task, cmd *exec.Cmd, tctx context.Context) {
	err := cmd.Wait()
	m.mu.Lock()
	t.EndedAt = time.Now()
	switch {
	case err == nil:
		// 进程真实 exit 0 优先：到点/被杀请求恰好与正常结束同时发生时，
		// 如实报完成比报 killed 更贴近事实。
		t.Status, t.ExitCode = StatusCompleted, 0
	case t.killRequested:
		t.Status = StatusKilled
	case errors.Is(tctx.Err(), context.DeadlineExceeded):
		t.Status, t.deadline = StatusKilled, true
	default:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.ExitCode = ee.ExitCode()
		}
		if t.ExitCode == -1 {
			// 信号杀死且非本 Manager 发起（外部 kill / 进程组被外部清理）。
			t.Status = StatusKilled
		} else {
			t.Status = StatusFailed
		}
	}
	_ = t.file.Sync()
	_ = t.file.Close()
	close(t.done)
	snap := t.Task
	if !t.notified {
		t.notified = true
		m.pending = append(m.pending, notifyMsg(t))
	}
	m.mu.Unlock()
	m.emit(Event{Task: snap})
}

// Kill 终止运行中的任务（杀整个进程组）。终态消息已由 task_kill 的
// 工具结果直接告知模型，故置 notified 抑制完成通知入队。
func (m *Manager) Kill(id string) (Task, error) {
	m.mu.Lock()
	t, ok := m.tasks[id]
	switch {
	case !ok:
		m.mu.Unlock()
		return Task{}, fmt.Errorf("no background task %s — use task_list", id)
	case t.Status != StatusRunning:
		m.mu.Unlock()
		return Task{}, fmt.Errorf("task %s already %s", id, t.Status)
	}
	t.killRequested = true
	t.notified = true
	_ = syscall.Kill(-t.pgid, syscall.SIGKILL)
	t.cancel()
	snap := t.Task
	snap.Status = StatusKilled // wait() 落定前的投影态，调用方按已杀理解
	m.mu.Unlock()
	return snap, nil
}

// List 返回全部任务的快照，按启动顺序排列。
func (m *Manager) List() []Task {
	m.mu.Lock()
	ts := make([]*task, 0, len(m.tasks))
	for _, t := range m.tasks {
		ts = append(ts, t)
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].seq < ts[j].seq })
	out := make([]Task, len(ts))
	for i, t := range ts {
		out[i] = t.Task
	}
	m.mu.Unlock()
	return out
}

// Drain 排空待注入的通知并连成一段文本；空队列返回 ""。
// 只在返回值将被消费时调用——清空后丢失不可恢复（对应 PreChat hook
// "首个非空短路"的语义：排它返回时通知必然已离队列）；消费失败用
// Requeue 归还。
func (m *Manager) Drain() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pending) == 0 {
		return ""
	}
	s := strings.Join(m.pending, "\n")
	m.pending = nil
	return s
}

// Requeue 把已 Drain 的通知文本放回队首：交付侧落库失败时归还，
// 保持"被消费才离队列"的语义成立。
func (m *Manager) Requeue(text string) {
	if text == "" {
		return
	}
	m.mu.Lock()
	m.pending = append([]string{text}, m.pending...)
	m.mu.Unlock()
}

// Shutdown 杀死全部仍在运行的任务（进程退出路径调用）。
// 子进程经 Setpgid 脱离了本进程的信号传播域，不显式杀会成为孤儿。
// 杀完给 wait() 一个短窗口定稿（file.Sync/Close 与终态落账），
// 单个任务收割超时兜底 2s 防退出路径被吊死。
func (m *Manager) Shutdown() {
	m.mu.Lock()
	var running []*task
	for _, t := range m.tasks {
		if t.Status == StatusRunning {
			t.killRequested = true
			running = append(running, t)
		}
	}
	m.mu.Unlock()
	for _, t := range running {
		_ = syscall.Kill(-t.pgid, syscall.SIGKILL)
		t.cancel()
	}
	deadline := time.After(2 * time.Second)
	for _, t := range running {
		select {
		case <-t.done:
		case <-deadline:
			return
		}
	}
}

func (m *Manager) emit(ev Event) {
	if m.OnEvent != nil {
		m.OnEvent(ev)
	}
}

// sessionDir 把会话 id 转成名下的输出子目录；空 id（装配早期）归入 "_"。
// id 直接拼进文件路径，按不可信输入处理：白名单 [A-Za-z0-9_-]，其余
// 字符（含路径分隔符与点）一律替换成 _，防 "../" 越出 tasks 根目录。
func sessionDir(id string) string {
	if id == "" {
		return "_"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, id)
}

// ── 停滞看门狗 ──
//
// 后台命令没有 stdin：卡在交互式提示（(y/n)、Press Enter 等）的命令会
// 无声地挂到天荒地老。输出文件大小停滞超阈值且尾行像提示语时，
// 入队一条无 <status> 的提醒（非终态事件），一次性触发后即停。
// 只是"慢"而无提示特征的命令（git log -S、长构建）不打扰。

const (
	stallEveryDefault     = 5 * time.Second
	stallThresholdDefault = 45 * time.Second
	stallTailBytes        = int64(1024)
)

var promptPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\(y/n\)`),
	regexp.MustCompile(`(?i)\[y/n\]`),
	regexp.MustCompile(`(?i)\(yes/no\)`),
	regexp.MustCompile(`(?i)\b(?:do you|would you|shall i|are you sure|ready to)\b.*\?\s*$`),
	regexp.MustCompile(`(?i)press (any key|enter)`),
	regexp.MustCompile(`(?i)continue\?\s*$`),
	regexp.MustCompile(`(?i)overwrite\?\s*$`),
}

// looksLikePrompt 检查输出尾部的最后一行是否像等待键盘输入的提示语。
func looksLikePrompt(tail string) bool {
	trimmed := strings.TrimRight(tail, "\n")
	last := trimmed
	if i := strings.LastIndexByte(trimmed, '\n'); i >= 0 {
		last = trimmed[i+1:]
	}
	for _, p := range promptPatterns {
		if p.MatchString(last) {
			return true
		}
	}
	return false
}

func (m *Manager) watchStall(t *task) {
	tick := time.NewTicker(m.stallEvery)
	defer tick.Stop()
	var lastSize int64
	lastGrow := time.Now()
	for {
		select {
		case <-t.done:
			return
		case <-tick.C:
		}
		fi, err := os.Stat(t.OutputPath)
		if err != nil {
			continue
		}
		if fi.Size() > lastSize {
			lastSize, lastGrow = fi.Size(), time.Now()
			continue
		}
		if time.Since(lastGrow) < m.stallThreshold {
			continue
		}
		tail := tailFile(t.OutputPath, stallTailBytes)
		if !looksLikePrompt(tail) {
			// 不是提示语：重置窗口，下一个 45s 周期再判。
			lastGrow = time.Now()
			continue
		}
		m.mu.Lock()
		if t.Status != StatusRunning {
			m.mu.Unlock()
			return
		}
		m.pending = append(m.pending, stallMsg(t, tail))
		snap := t.Task
		m.mu.Unlock()
		m.emit(Event{Task: snap, Stalled: true})
		return
	}
}

// tailFile 读取文件末尾 n 字节；读不到返回空串。
func tailFile(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	off := max(fi.Size()-n, 0)
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil {
		return ""
	}
	return string(buf)
}

// ── 通知文本 ──
//
// 终态通知带 <status>，停滞提醒不带（非终态）。输出路径随通知给出，
// 模型用 read_file 直接读——这是 CC 已确认的取回通道（其
// TaskOutputTool 已弃用改走 Read on output file）。

func notifyMsg(t *task) string {
	var summary string
	switch {
	case t.deadline:
		summary = fmt.Sprintf("Background command %q timed out and was killed", t.Description)
	case t.Status == StatusCompleted:
		summary = fmt.Sprintf("Background command %q completed (exit code 0)", t.Description)
	case t.Status == StatusFailed:
		summary = fmt.Sprintf("Background command %q failed with exit code %d", t.Description, t.ExitCode)
	default:
		summary = fmt.Sprintf("Background command %q was stopped", t.Description)
	}
	return "<task_notification>\n" +
		"<task_id>" + t.ID + "</task_id>\n" +
		"<status>" + string(t.Status) + "</status>\n" +
		"<output_file>" + t.OutputPath + "</output_file>\n" +
		"<summary>" + tools.EscapeXML(summary) + "</summary>\n" +
		"</task_notification>"
}

func stallMsg(t *task, tail string) string {
	return "<task_notification>\n" +
		"<task_id>" + t.ID + "</task_id>\n" +
		"<output_file>" + t.OutputPath + "</output_file>\n" +
		"<summary>" + tools.EscapeXML(fmt.Sprintf(
		"Background command %q appears to be waiting for interactive input", t.Description)) +
		"</summary>\n" +
		"</task_notification>\nLast output:\n" + strings.TrimRight(tail, "\n") +
		"\n\nThe command is likely blocked on an interactive prompt. Use task_kill to stop it " +
		"and re-run with piped input (e.g., `echo y | command`) or a non-interactive flag."
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
