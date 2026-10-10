package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"runeharness/internal/agent"
	"runeharness/internal/scope"
	"runeharness/internal/session"
)

// turnCtxKey 是 slot 注入每轮 ctx 的私有键（turnID、ledger emit）。
type turnCtxKey struct{}

// WithTurnID 把本轮 id 放进 ctx；harness 内 emit 回调用它给事件补归属。
func WithTurnID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, turnCtxKey{}, id)
}

// TurnIDFromContext 返回当前轮的 turnID；轮外调用返回空串。
func TurnIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(turnCtxKey{}).(string)
	return id
}

// slot 是单个 (tenant,channel,chat) 的执行槽：一轮跑一个 TurnRunner，
// 期间到的请求排队，Interrupt 取消当前轮，ask 经 pending 路由答复。
type slot struct {
	key     ChatKey
	manager *Manager

	// 不可变槽位状态：创建时确定。
	sessionID string
	subjectID string
	ledger    *Ledger
	runner    TurnRunner

	// 运行态（mu 保护）。
	mu      sync.Mutex
	turnSeq int             // 已开始的轮次序号；turn_start 时 +1
	busy    bool           // 有 goroutine 在跑轮链（当前轮 + 队列）
	history []agent.Message // 当前视图（含中断占位）；新建时从 store 载入
	queue   chan TurnInput  // 排队队列；cap = cfg.QueueDepth
	pendingAsk *askReq    // 当前挂起的权限确认；nil = 无
	cancel   context.CancelFunc // 当前轮的取消；nil = 空闲
	curTurn  string             // 当前轮的 turnID；emit 从这里取归属

	slotCtx context.Context // 槽位级生命周期（管理器释放或进程退出时取消）
}

// askReq 是一次挂起的权限确认。
type askReq struct {
	id     string
	reply  chan bool // 容量 1，permission.Hook 约定
	expire *time.Timer
}

// newSlot 建槽并启动派发 goroutine。sessionID/subjectID 由调用方先解析。
func (m *Manager) newSlot(ctx context.Context, key ChatKey, sessionID, subjectID string, history []agent.Message) (*slot, error) {
	s := &slot{
		key:       key,
		manager:   m,
		sessionID: sessionID,
		subjectID: subjectID,
		ledger:    NewLedger(m.cfg.LedgerCap),
		history:   history,
	}
	s.slotCtx = ctx
	q := m.cfg.QueueDepth
	if q < 0 {
		q = 0
	}
	s.queue = make(chan TurnInput, q) // QueueDepth=0 时 unbuffered：忙即拒

	ask := func(call agent.ToolCall, reason string, seq, total int) <-chan bool {
		return s.routeAsk(call, reason, seq, total)
	}
	r, err := m.harness.New(ctx, s.emit, ask)
	if err != nil {
		return nil, fmt.Errorf("harness for %s: %w", key, err)
	}
	s.runner = r
	return s, nil
}

// runChain 串行跑完当前轮再排空队列；Submit 认领 busy 后启动它。
// 轮间取 queue 与释放 busy 在同一把锁内：Submit 若见 busy=true 而入队
// 成功，那条消息必然在排空检查之前落位；排空检查若先完成则 busy 置
// false，Submit 改走"认领+起链"分支，消息不会留在无人收的 queue 里。
func (s *slot) runChain(in TurnInput) {
	cur := in
	for {
		s.runTurn(cur)
		s.mu.Lock()
		select {
		case next := <-s.queue:
			s.mu.Unlock()
			cur = next
			continue
		default:
		}
		s.busy = false
		s.mu.Unlock()
		return
	}
}


// runTurn 执行一轮：注入 user 消息、调 runner、写 turn_end 控制行、发事件。
// 出错也照常发 turn_end（status=error），不让订阅者看到"半轮"。
func (s *slot) runTurn(in TurnInput) {
	s.mu.Lock()
	s.turnSeq++
	seq := s.turnSeq
	turnID := fmt.Sprintf("t_%d_%d", seq, time.Now().UnixNano()%100000)
	turnCtx, cancel := context.WithCancel(WithTurnID(s.slotCtx, turnID))
	s.cancel = cancel
	s.curTurn = turnID
	s.mu.Unlock()

	started := time.Now()
	s.emit(Event{Event: EvTurnStart, TurnID: turnID, TurnSeq: seq,
		Data: mustJSON(map[string]any{"turn_seq": seq})})

	// user 消息落库 + 进视图：与 TUI sendUserText 同序，先 Append 再进 history。
	// TUI 里 display≠content 走 hook 改写；runner 侧外部输入无 @mention，
	// display 仅用于将来的渠道展示差异，当前直接用 Prompt。
	userMsg := agent.Message{
		Role:    agent.RoleUser,
		Content: in.Prompt,
		Channel: s.key.Channel,
	}
	sessCtx := scope.WithSession(s.slotCtx, s.sessionID)
	if _, err := s.manager.store.Append(sessCtx, userMsg); err != nil {
		cancel()
		s.finishTurn(turnID, seq, started, "error", err)
		return
	}
	s.mu.Lock()
	s.history = append(s.history, userMsg)
	s.mu.Unlock()

	next, err := s.runner(turnCtx, s.history)
	s.mu.Lock()
	s.history = next
	s.mu.Unlock()

	status := "completed"
	if err != nil {
		var interr *agent.InterruptedError
		switch {
		case errors.As(err, &interr):
			status = "interrupted"
		default:
			status = "error"
		}
	}
	cancel()
	s.finishTurn(turnID, seq, started, status, err)
}

// finishTurn 写 turn_end 控制行并广播终局；幂等（重复调用只发一次）。
func (s *slot) finishTurn(turnID string, seq int, started time.Time, status string, err error) {
	outcome := TurnOutcome{
		Status:   status,
		TurnSeq:  seq,
		Duration: time.Since(started).Milliseconds(),
	}
	if err != nil {
		outcome.Error = err.Error()
	}
	// 轮次终态落库：agent.Message.Kind=turn_end 控制行，Content 是 outcome JSON。
	row, rowErr := sessionControlRow(agent.KindTurnEnd, outcome)
	if rowErr == nil {
		sessCtx := scope.WithSession(s.slotCtx, s.sessionID)
		// 落库失败不阻塞事件流——终态通知比持久化重要（重放靠 requests 表）。
		_, _ = s.manager.store.Append(sessCtx, row)
	}
	s.emit(Event{Event: EvTurnEnd, TurnID: turnID, TurnSeq: seq,
		Data: mustJSON(outcome)})

	s.mu.Lock()
	s.cancel = nil
	s.curTurn = ""
	s.mu.Unlock()
}

// sessionControlRow 生成 turn_end 控制行。复用 session.ControlRow 的语义
// （marshal meta 进 Content、Kind 标记），但 meta 是 TurnOutcome——
// 这里直接构造避免 import internal/session 只为一个函数。
func sessionControlRow(kind agent.Kind, v any) (agent.Message, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return agent.Message{}, err
	}
	return agent.Message{Kind: kind, Content: string(b)}, nil
}

// emit 是槽位唯一的发射点；补 chat_id/turn_id 后进账本。
func (s *slot) emit(ev Event) {
	if ev.ChatID == "" {
		ev.ChatID = s.key.ChatID
	}
	s.mu.Lock()
	if ev.TurnID == "" {
		ev.TurnID = s.curTurn
	}
	if ev.TurnSeq == 0 && ev.TurnID != "" {
		ev.TurnSeq = s.turnSeq
	}
	s.mu.Unlock()
	ev.At = time.Now().UnixMilli()
	s.ledger.Append(ev)
}

// routeAsk 处理 permission 层上报的确认请求：发 ask 事件、登记 pending、
// 返回 reply channel。timeout 由 AskTimeout 兜底；slot 取消视为 deny。
func (s *slot) routeAsk(call agent.ToolCall, reason string, seq, total int) <-chan bool {
	reply := make(chan bool, 1)
	id := fmt.Sprintf("ask_%d", time.Now().UnixNano())
	req := &askReq{id: id, reply: reply}

	s.mu.Lock()
	s.pendingAsk = req
	s.mu.Unlock()

	req.expire = time.AfterFunc(s.manager.cfg.AskTimeout, func() {
		s.resolveAsk(id, false)
	})

	s.emit(Event{Event: EvAsk, Data: mustJSON(map[string]any{
		"ask_id": id,
		"call":   call,
		"reason": reason,
		"seq":    seq,
		"total":  total,
	})})

	go func() {
		select {
		case <-s.slotCtx.Done():
			s.resolveAsk(id, false)
		case <-req.reply:
		}
	}()
	return reply
}

// resolveAsk 把一次答复路由到当前挂起的 ask；应答后发空事件确认落定。
func (s *slot) resolveAsk(id string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingAsk == nil || s.pendingAsk.id != id {
		return
	}
	req := s.pendingAsk
	s.pendingAsk = nil
	req.expire.Stop()
	select {
	case req.reply <- ok:
	default:
	}
}

// Submit 把一条外部输入送进槽位：闲时直接跑，忙时排队或拒绝。
// 返回 (turnSeq 预估值, 是否入队)。ErrQueueFull 时调用方按协议回错误帧。
func (m *Manager) Submit(ctx context.Context, key ChatKey, in TurnInput) error {
	s, err := m.slotFor(ctx, key)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if !s.busy {
		// 空闲：认领 busy 并起链；不经 queue，无起跑竞态。
		s.busy = true
		s.mu.Unlock()
		go s.runChain(in)
		return nil
	}
	s.mu.Unlock()
	// 忙：queue cap=QueueDepth，满即拒（QueueDepth=0 时 unbuffered，
	// runChain 在轮间 default 分支非阻塞取——忙期间一律被拒）。
	select {
	case s.queue <- in:
		return nil
	default:
		return ErrQueueFull
	}
}

// Interrupt 取消当前正在跑的轮次；空闲槽是 no-op（对齐 plan：idle 无靶）。
func (m *Manager) Interrupt(ctx context.Context, key ChatKey) error {
	s, err := m.slotFor(ctx, key)
	if err != nil {
		return err
	}
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	s.emit(Event{Event: EvInterrupt, Data: mustJSON(map[string]any{
		"reason": "client_cancel",
	})})
	cancel()
	return nil
}

// Answer 把客户端的权限应答路由到当前挂起的 ask；ask_id 不匹配时拒绝。
func (m *Manager) Answer(ctx context.Context, key ChatKey, askID string, ok bool) error {
	s, err := m.slotFor(ctx, key)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingAsk == nil || s.pendingAsk.id != askID {
		return ErrNotFound
	}
	req := s.pendingAsk
	s.pendingAsk = nil
	req.expire.Stop()
	select {
	case req.reply <- ok:
	default:
	}
	return nil
}

// Subscribe 返回该 chat 的事件 channel；since>0 时先回放存量再接 live。
// 落后出窗返回 ErrLag——调用方应发 reset 让客户端全量重拉。
func (m *Manager) Subscribe(ctx context.Context, key ChatKey, since int64) (<-chan Event, int64, error) {
	s, err := m.slotFor(ctx, key)
	if err != nil {
		return nil, 0, err
	}
	events, latest, err := s.ledger.Since(since)
	if err != nil {
		return nil, 0, err
	}
	live, unsub := s.ledger.Subscribe()
	out := make(chan Event, 256)
	go func() {
		defer unsub()
		defer close(out)
		for _, ev := range events {
			out <- ev
		}
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-live:
				if !ok {
					return
				}
				select {
				case out <- ev:
				default:
				}
			}
		}
	}()
	return out, latest, nil
}

// Active 报告该 chat 是否有在跑或排队的轮次（给 GET /v1/chats 列表用）。
func (m *Manager) Active(ctx context.Context, key ChatKey) bool {
	m.mu.Lock()
	s, ok := m.slots[key]
	m.mu.Unlock()
	if !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy
}

// slotFor 取或建槽：惰性解析 binding -> session_id -> LoadHistory -> harness.New。
func (m *Manager) slotFor(ctx context.Context, key ChatKey) (*slot, error) {
	m.mu.Lock()
	if s, ok := m.slots[key]; ok {
		m.mu.Unlock()
		return s, nil
	}
	m.mu.Unlock()

	// 慢路径：查/建 session，载入历史，装配 harness。
	sc := scope.Scope{
		TenantID:  key.TenantID,
		Workspace: m.workspace(key),
		SubjectID: key.SubjectID,
	}
	// 槽位生命周期独立于请求：不能用传入 ctx——它是 HTTP/WS 请求上下文，
	// 响应一结束就取消，会把槽位一起杀掉。tenant/subject 已经进 sc，
	// 不需要请求方的取消信号与 deadline。
	base := scope.WithScope(context.Background(), sc)

	var sessionID, subjectID string
	if m.binder != nil {
		sid, err := m.binder.GetBinding(base, key.TenantID, key.Channel, key.ChatID)
		if err == nil && sid != "" {
			sessionID = sid
		}
	}
	var history []agent.Message
	if sessionID == "" {
		// 建 session：meta.SubjectID 空则 ctx 继承。
		rec, err := m.store.CreateSession(base, m.sessionMeta(key))
		if err != nil {
			return nil, err
		}
		sessionID, subjectID = rec.ID, rec.SubjectID
		if m.binder != nil {
			if _, err := m.binder.UpsertBinding(base, key.TenantID, key.Channel,
				key.ChatID, sessionID, subjectID); err != nil {
				return nil, fmt.Errorf("bind %s: %w", key, err)
			}
		}
	} else {
		h, err := m.store.LoadHistory(scope.WithSession(base, sessionID), sessionID)
		if err != nil {
			return nil, fmt.Errorf("load %s history: %w", sessionID, err)
		}
		history = h
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.slots[key]; ok { // 并发创建竞争：第二个用先到的
		return s, nil
	}
	slotCtx := scope.WithSession(base, sessionID)
	s, err := m.newSlot(slotCtx, key, sessionID, subjectID, history)
	if err != nil {
		return nil, err
	}
	m.slots[key] = s
	return s, nil
}

// workspace 给新槽位定工作目录。当前无渠道差异：统一进程 cwd。
// 渠道级 workspace 归属是 v2 话题（多机器人分目录）。
func (m *Manager) workspace(key ChatKey) string {
	return "" // scope.Workspace 空串时下游自行回落 cwd
}

// sessionMeta 组装新 session 的 Meta；渠道与 chat_id 写进 Title 供
// /v1/chats 列表展示。
func (m *Manager) sessionMeta(key ChatKey) session.Meta {
	return session.Meta{
		Kind:      session.KindMain,
		Title:     fmt.Sprintf("%s:%s", key.Channel, key.ChatID),
		SubjectID: key.SubjectID,
	}
}

// mustJSON 序列化事件载荷；marshal 失败只可能发生在未知类型，一律降级为
// {}，不让事件发射因载荷坏掉。
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
