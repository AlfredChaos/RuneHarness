// Package runner 提供 headless 会话执行层：把外部渠道的
// (channel, chat_id, prompt) 组织成与 TUI 等价的 agent 轮次，
// 并把轮次过程广播为每 chat 单调递增的事件流。
//
// 三个核心件：
//   - Ledger：事件账本，每 chat 一个 ring buffer + 单调 seq，订阅者按
//     since 追补，落后出窗返回 ErrLag 让上游发 reset。
//   - Manager：每 chat 一个 slot，槽内轮次串行、多余排队（或拒绝），
//     Interrupt 取消当前轮，Answer 应答待定的权限确认。
//   - Harness：装配 *agent.Agent 与外部能力（permission ask、bgtask
//     通知、cron 注入等），runner 只认 agent 层接口，不知具体实现。
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"runeharness/internal/agent"
	"runeharness/internal/session"
)

// ChatKey 唯一标识一个外部会话槽位。
type ChatKey struct {
	TenantID  string // 租户边界；来自 token 认证，由调用方注入
	Channel   string // 来源渠道（"bear"/"http"/"qq"…），审计与隔离用
	ChatID    string // 渠道内会话标识（bear 客户端给的 `^[A-Za-z0-9_-]{1,64}$`）
	SubjectID string // 记忆层 subject 空间归属；空则按租户默认空间
}

func (k ChatKey) String() string {
	return fmt.Sprintf("%s/%s/%s", k.TenantID, k.Channel, k.ChatID)
}

// Event 是一条广播到订阅者的事件帧。Seq 在单个 chat 内单调递增，
// 跨 chat 无顺序保证。TurnID 标识所属轮次，跨轮事件（reset、inject）为空。
type Event struct {
	Seq     int64           `json:"seq"`
	ChatID  string          `json:"chat_id"`
	Event   string          `json:"event"`            // delta | tool_call | tool_result | inject | ask | interrupt | turn_start | turn_end | reset | error | notification
	TurnID  string          `json:"turn_id"`          // 该事件归属的轮次；跨轮次事件为空
	TurnSeq int             `json:"turn_seq"`         // 轮次序号（1 起，queued -> running 每次 +1）
	At      int64           `json:"timestamp"`        // unix 毫秒；服务端产生该事件的时刻
	Data    json.RawMessage `json:"data"`             // 事件专有载荷
}

// 事件名常量。data 字段的 JSON 结构在 docs/runeharness-api.html §6 定义。
const (
	EvDelta       = "delta"        // {thinking?, content?}：本轮累积快照（全量替换，不是增量）
	EvReasoning   = "reasoning_delta"
	EvToolCall    = "tool_call"    // {id, name, arguments}：一次工具调用分发
	EvToolResult  = "tool_result"  // {id, output, is_error, blocked, duration_ms}
	EvInject      = "inject"       // {content}：hook 注入的消息（todo nag、压缩提醒等）
	EvAsk         = "ask"          // {ask_id, call:{...}, reason, seq, total}：权限确认挂起
	EvInterrupt   = "interrupt"    // {reason?}：本轮被打断
	EvTurnStart   = "turn_start"   // {turn_seq, queued_from?}：轮次开跑
	EvTurnEnd     = "turn_end"     // {status, turn_seq, usage?}：轮次终局
	EvReset       = "reset"        // {reason}：会话边界后移（压缩等），since 前序作废
	EvError       = "error"        // {message}：轮次内非致命异常
	EvNotification= "notification" // {kind: bg|cron|missed, task_id?, prompt?}：旁路输入已注入
)

// ErrLag 表示订阅者 since 已落后出 buffer 上界，无法无丢失续传。
var ErrLag = errors.New("seq falls behind retained window")

// ErrQueueFull 表示该 chat 的排队队列已满，本次 Submit 被拒。
var ErrQueueFull = errors.New("turn queue is full")

// ErrNotFound 表示 chat 无活跃槽位（Interrupt/Answer 打在不存在的槽上）。
var ErrNotFound = errors.New("chat slot not found")

// ---- Ledger ----

const defaultLedgerCap = 2000

// Ledger 是每 chat 一个的追加式事件账本。Append 分配 seq；Subscribers
// 收到 Append 后的 live 事件；Since 返回 [since, ∞) 内的存量。
// 内存 ring buffer，不落盘——断线重连用 LoadHistory 拉历史，用 since 追增量。
type Ledger struct {
	mu     sync.Mutex
	cap    int
	buf    []Event // 环形缓冲，head = (startSeq-1) % cap
	start  int64   // 已分配的最小 seq（首条 = 1）
	next   int64   // 下一条要分配的 seq
	subs   map[int]chan Event
	nextID int
	closed bool
}

// NewLedger 建一个容量 cap 的账本；cap<=0 用默认。
func NewLedger(cap int) *Ledger {
	if cap <= 0 {
		cap = defaultLedgerCap
	}
	// start 是"buffer 最老事件的 seq"：空 buffer 视为 1（首条将分配的），
	// 溢出推进时 head() 才不出现负下标。
	return &Ledger{cap: cap, start: 1, next: 1, subs: map[int]chan Event{}}
}

// Append 追加一条事件并广播给所有订阅者。返回分配的 seq。
// chat/turn 字段由调用方填好；这里只管编号与缓存。
func (l *Ledger) Append(ev Event) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return -1
	}
	ev.Seq = l.next
	l.next++
	if len(l.buf) == l.cap {
		// 弹出最老的一条：start 推进
		l.buf[l.head()] = ev
		l.start++
	} else {
		l.buf = append(l.buf, ev)
	}
	for _, ch := range l.subs {
		select {
		case ch <- ev:
		default: // 慢消费者丢帧；读端靠 seq 间隙自检，不背压 ledger
		}
	}
	return ev.Seq
}

// head 返回当前最老事件在 buf 中的下标（仅 len(buf)==cap 时有意义）。
func (l *Ledger) head() int {
	return int((l.start - 1) % int64(l.cap))
}

// Since 返回 seq>=since 的存量事件；since 为 0 时返回全部缓存。
// 若 since 落后于可保留窗口，返回 ErrLag。
func (l *Ledger) Since(since int64) ([]Event, int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) == 0 {
		return nil, l.next - 1, nil
	}
	lowest := l.start
	if since < lowest && since > 0 {
		return nil, 0, fmt.Errorf("since=%d: %w (lowest=%d)", since, ErrLag, lowest)
	}
	var out []Event
	for _, ev := range l.ordered() {
		if ev.Seq >= since {
			out = append(out, ev)
		}
	}
	return out, l.next - 1, nil
}

// Latest 返回当前已分配的 seq 上限（无事件时为 0）。
func (l *Ledger) Latest() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.next - 1
}

// ordered 按 seq 升序返回 buffer 内容（调用方须持锁）。
func (l *Ledger) ordered() []Event {
	out := make([]Event, 0, len(l.buf))
	if len(l.buf) < l.cap {
		return append(out, l.buf...)
	}
	h := l.head()
	out = append(out, l.buf[h:]...)
	out = append(out, l.buf[:h]...)
	return out
}

// Subscribe 返回一个接收 Append 后 live 事件的 channel；cap 满了丢帧，
// 读端按 seq 间隙自检。调用 Unsubscribe 或 Ledger.Close 停止。
func (l *Ledger) Subscribe() (ch <-chan Event, unsubscribe func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	id := l.nextID
	l.nextID++
	c := make(chan Event, 256)
	l.subs[id] = c
	return c, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		delete(l.subs, id)
	}
}

// Close 关闭所有订阅 channel；后续 Append 返回 -1。
func (l *Ledger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.closed = true
	for id, ch := range l.subs {
		close(ch)
		delete(l.subs, id)
	}
}

// ---- Manager ----

// TurnInput 是一次外部提交的轮次载荷。
type TurnInput struct {
	Prompt  string // 用户输入原文（已经过渠道层剥离，不再含 WS 信封）
	Display string // 落库与回显的展示文本；空则同 Prompt
}

// TurnOutcome 是轮次终局，由 turn_end 事件的 data 携带。
type TurnOutcome struct {
	Status   string  `json:"status"`             // completed | interrupted | error | crashed
	TurnSeq  int     `json:"turn_seq"`
	Duration int64   `json:"duration_ms"`
	Usage    *agent.Usage `json:"usage,omitempty"`
	Error    string  `json:"error,omitempty"`
}


// Binder 解析 (channel,chat_id) -> session_id 的持久映射。
type Binder interface {
	// GetBinding 返回 chat 映射的 session id；未建绑定时返回空串。
	GetBinding(ctx context.Context, tenant, channel, chatID string) (sessionID string, err error)
	// UpsertBinding 建立/刷新绑定，返回（可能与既有绑定冲突时的）最终 session id。
	UpsertBinding(ctx context.Context, tenant, channel, chatID, sessionID, subjectID string) (string, error)
}

// TurnRunner 是"在这个 ctx 上跑完一轮 agent 循环"的抽象；harness.go 把
// *agent.Agent 包装成它。返回的 history 已是本轮结束后的完整视图
// （含注入与占位结果），err 区分完成/中断/失败。
type TurnRunner func(ctx context.Context, history []agent.Message) ([]agent.Message, error)

// Harness 由调用方（cmd/runebot）提供：给定一个 slot 的发射面，
// 组装出能跑 TurnRunner 的东西（本质是把 agent.New + hooks + callbacks
// 装成一轮执行器）。
//
// emit 由 slot 持有：harness 内任何回调（OnPartial / OnToolCall /
// permission ask …）都通过它把事件写进账本；turnID 由 slot 在
// turn_start 时注入 ctx（见 WithTurnID），harness 不应自己生成。
//
// ask 是 permission 层的回话通道：harness 的 PreToolUse 判为 Ask 时
// 调它，得到 <-chan bool；runner 侧把应答路由回该 channel。ask 回调
// 立即返回，不阻塞。
type Harness struct {
	// New 建一个 slot 级执行器。slotCtx 生命周期等于槽位（随 Manager 释放
	// 或进程退出而取消），每轮的 ctx 由 slot 派生（带 turnID、可取消）。
	// emit/ask 两个回调由 slot 传入，harness 不应保存引用跨调用复用。
	New func(slotCtx context.Context, emit func(ev Event), ask func(call agent.ToolCall, reason string, seq, total int) <-chan bool) (TurnRunner, error)

	// OnSlotClose 在槽位关闭时回调（清理 goroutine、释放租约等）；nil 可。
	OnSlotClose func()
}

// Config 是 Manager 的行为参数。
type Config struct {
	// QueueDepth 是每 chat 允许排队的最大轮数；超出即拒绝 Submit。
	// 0 表示不排队（busy 时 Submit 返回 ErrQueueFull）。API 文档建议 0：
	// 客户端自行串行，服务端不吞积压。
	QueueDepth int

	// LedgerCap 是事件账本容量；<=0 用默认 2000。
	LedgerCap int

	// AskTimeout 是权限确认等待上限；超时按 deny 处理并让事件流看到
	// ask_expired。<=0 用默认 120s。
	AskTimeout time.Duration

	// DrainInterval 是 bg/cron 通知注入的轮询间隔（TUI 侧是事件驱动，
	// runner 没有 UI 消息泵，用周期探）。<=0 用默认 3s。
	DrainInterval time.Duration
}

func (c Config) withDefaults() Config {
	if c.LedgerCap <= 0 {
		c.LedgerCap = defaultLedgerCap
	}
	if c.AskTimeout <= 0 {
		c.AskTimeout = 120 * time.Second
	}
	if c.DrainInterval <= 0 {
		c.DrainInterval = 3 * time.Second
	}
	return c
}

// SessionStore 是 runner 需要的会话持久化能力。
type SessionStore interface {
	CreateSession(ctx context.Context, meta session.Meta) (session.Session, error)
	Append(ctx context.Context, msg agent.Message) (int64, error)
	LoadHistory(ctx context.Context, sessionID string) ([]agent.Message, error)
}

// Manager 管理 (TenantID,Channel,ChatID) -> slot 的映射。
type Manager struct {
	cfg     Config
	harness Harness
	store   SessionStore
	binder  Binder // nil 表示不落 chat_bindings（进程内映射，重启丢）

	mu    sync.Mutex
	slots map[ChatKey]*slot
}

func NewManager(cfg Config, h Harness, store SessionStore, binder Binder) *Manager {
	return &Manager{
		cfg:     cfg.withDefaults(),
		harness: h,
		store:   store,
		binder:  binder,
		slots:   map[ChatKey]*slot{},
	}
}
