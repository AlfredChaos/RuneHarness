package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"runeharness/internal/agent"
	"runeharness/internal/runner"
	"runeharness/internal/scope"
)

// ---- handlers（HTTP/SSE 轨）----
// 所有 handler 先过 claims()，401 由 writeErr 统一格式。

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"status": "ok", "time": time.Now().Unix()})
}

func (s *Server) handleListChats(w http.ResponseWriter, r *http.Request) {
	t, err := s.claims(r)
	if err != nil {
		writeErr(w, 401, err.Error())
		return
	}
	rows, err := s.listBindings(r.Context(), t.TenantID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"chats": rows})
}

// listBindings 直查 chat_bindings；Store 需实现 ListByTenant。
// 返回 {chat_id, channel, session_id, subject_id, active, last_active}。
func (s *Server) listBindings(ctx context.Context, tenant string) ([]map[string]any, error) {
	rows, err := s.Store.ListByTenant(ctx, tenant, s.Channel)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		cid, _ := row["chat_id"].(string)
		row["active"] = s.Mgr.Active(ctx, runner.ChatKey{
			TenantID: tenant, Channel: s.Channel, ChatID: cid,
		})
	}
	return rows, nil
}

func (s *Server) handleGetChat(w http.ResponseWriter, r *http.Request) {
	key, err := s.chatKey(r, r.PathValue("id"))
	if err != nil {
		writeErr(w, 401, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"chat_id": key.ChatID, "channel": key.Channel,
		"active": s.Mgr.Active(r.Context(), key),
	})
}

// handlePostMessage 提交一条消息：忙时 409（客户端自行串行；
// 服务端不吞积压是明确的语义边界，见 API §2.1）。
func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	key, err := s.chatKey(r, r.PathValue("id"))
	if err != nil {
		writeErr(w, 401, err.Error())
		return
	}
	var body struct {
		Content string `json:"content"`
		Display string `json:"display,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "bad JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Content) == "" {
		writeErr(w, 400, "content is empty")
		return
	}
	err = s.Mgr.Submit(r.Context(), key, runner.TurnInput{
		Prompt: body.Content, Display: body.Display,
	})
	if errors.Is(err, runner.ErrQueueFull) {
		writeErr(w, 409, "chat is busy; retry after turn_end")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 202, map[string]any{"status": "queued", "chat_id": key.ChatID})
}

// handleAction 处理 cancel/answer 两个动作。
func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	key, err := s.chatKey(r, r.PathValue("id"))
	if err != nil {
		writeErr(w, 401, err.Error())
		return
	}
	var body struct {
		Type     string `json:"type"`
		AskID    string `json:"ask_id,omitempty"`
		Approved *bool  `json:"approved,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "bad JSON: "+err.Error())
		return
	}
	switch body.Type {
	case "cancel":
		if err := s.Mgr.Interrupt(r.Context(), key); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"status": "interrupted"})
	case "answer":
		if body.AskID == "" || body.Approved == nil {
			writeErr(w, 400, "answer needs ask_id and approved")
			return
		}
		if err := s.Mgr.Answer(r.Context(), key, body.AskID, *body.Approved); err != nil {
			if errors.Is(err, runner.ErrNotFound) {
				writeErr(w, 404, "ask not found or already answered")
				return
			}
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"status": "answered"})
	default:
		writeErr(w, 400, "unknown action type: "+body.Type)
	}
}

// handleHistory 按行 id 窗口拉历史（?before=<rowID>&limit=N）。
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	key, err := s.chatKey(r, r.PathValue("id"))
	if err != nil {
		writeErr(w, 401, err.Error())
		return
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, hasMore, next, err := s.history(r.Context(), key, before, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"chat_id": key.ChatID, "items": items,
		"has_more": hasMore, "next_before": next,
	})
}

// history 取一页历史：chat -> session -> LoadRawHistory 按行 id 倒序，
// 翻回正序返回。before=0 拉最新一页；limit<=0 或 >200 钳到 50。
func (s *Server) history(ctx context.Context, key runner.ChatKey, before int64, limit int) ([]map[string]any, bool, int64, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	sid, err := s.Store.GetBinding(ctx, key.TenantID, key.Channel, key.ChatID)
	if err != nil {
		return nil, false, 0, err
	}
	if sid == "" {
		return []map[string]any{}, false, 0, nil
	}
	rows, err := s.Store.LoadRawHistory(scope.WithSession(
		scope.WithScope(ctx, scope.Scope{
			TenantID: key.TenantID, SubjectID: key.SubjectID,
		}), sid), sid)
	if err != nil {
		return nil, false, 0, err
	}
	var picked []agent.Message
	for i := len(rows) - 1; i >= 0; i-- {
		m := rows[i]
		if m.Kind.IsControl() || (before > 0 && m.ID >= before) {
			continue
		}
		picked = append(picked, m)
		if len(picked) == limit+1 {
			break
		}
	}
	hasMore := len(picked) > limit
	if hasMore {
		picked = picked[:limit]
	}
	items := make([]map[string]any, 0, len(picked))
	var nextBefore int64
	for i := len(picked) - 1; i >= 0; i-- {
		m := picked[i]
		if nextBefore == 0 {
			nextBefore = m.ID
		}
		items = append(items, map[string]any{
			"i": m.ID, "role": m.Role, "content": m.Content,
			"timestamp": m.CreatedAt, // unix 毫秒，与 DB created_at 同口径
		})
	}
	return items, hasMore, nextBefore, nil
}

// handleSSE 是事件流的 HTTP 等价物：GET + Last-Event-ID 续传。
// 每事件一帧：id/event/data 三行；心跳 20s 一个注释帧。
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	key, err := s.chatKey(r, r.PathValue("id"))
	if err != nil {
		writeErr(w, 401, err.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)

	var since int64
	if h := r.Header.Get("Last-Event-ID"); h != "" {
		v, _ := strconv.ParseInt(h, 10, 64)
		since = v + 1 // SSE 语义：ID 是"已收到的最后一条"
	}
	ch, _, err := s.Mgr.Subscribe(r.Context(), key, since)
	if err != nil {
		writeSSE(w, runner.Event{Event: runner.EvError,
			Data: mustErrJSON(err.Error())})
		flusher.Flush()
		return
	}
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			writeSSE(w, ev)
			flusher.Flush()
		}
	}
}

func writeSSE(w http.ResponseWriter, ev runner.Event) {
	b, _ := json.Marshal(ev)
	_, _ = w.Write([]byte("id: " + strconv.FormatInt(ev.Seq, 10) + "\n"))
	_, _ = w.Write([]byte("event: " + ev.Event + "\n"))
	_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
}

func mustErrJSON(msg string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"message": msg})
	return b
}

// ---- svc/* 诊断面 ----

func (s *Server) handleSvcHistory(w http.ResponseWriter, r *http.Request) {
	// ?chat=<id>&before=<rowID>&limit=N；与 /v1/chats/{id}/history 同逻辑。
	key, err := s.chatKey(r, r.URL.Query().Get("chat"))
	if err != nil {
		writeErr(w, 401, err.Error())
		return
	}
	if key.ChatID == "" {
		writeErr(w, 400, "missing ?chat=")
		return
	}
	before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, hasMore, next, err := s.history(r.Context(), key, before, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"chat_id": key.ChatID, "items": items,
		"has_more": hasMore, "next_before": next,
	})
}

func (s *Server) handleSvcSearch(w http.ResponseWriter, r *http.Request) {
	writeErr(w, 501, "svc/search not implemented")
}

// handleStream 是"提交+读流"一体的端点：POST {content} -> NDJSON 事件流
// 直到本轮 turn_end。相比 SSE 订阅，这是单次请求内闭环，天然贴合
// fetch + ReadableStream 的消费模式；忙时 409（与 POST /messages 同规则）。
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	key, err := s.chatKey(r, r.PathValue("id"))
	if err != nil {
		writeErr(w, 401, err.Error())
		return
	}
	var body struct {
		Content string `json:"content"`
		Display string `json:"display,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "bad JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Content) == "" {
		writeErr(w, 400, "content is empty")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)

	// 先订阅再提交，保证本轮事件不落在订阅窗口之外。
	ch, _, err := s.Mgr.Subscribe(r.Context(), key, 0)
	if err != nil {
		writeNDJSON(w, runner.Event{Event: runner.EvError,
			Data: mustErrJSON(err.Error())})
		flusher.Flush()
		return
	}
	err = s.Mgr.Submit(r.Context(), key, runner.TurnInput{
		Prompt: body.Content, Display: body.Display,
	})
	if errors.Is(err, runner.ErrQueueFull) {
		writeNDJSON(w, runner.Event{Event: runner.EvError,
			Data: mustErrJSON("chat is busy")})
		flusher.Flush()
		return
	}
	if err != nil {
		writeNDJSON(w, runner.Event{Event: runner.EvError,
			Data: mustErrJSON(err.Error())})
		flusher.Flush()
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			writeNDJSON(w, ev)
			flusher.Flush()
			if ev.Event == runner.EvTurnEnd {
				return // 本轮终局：请求-响应闭环完成
			}
		}
	}
}

func writeNDJSON(w http.ResponseWriter, ev runner.Event) {
	b, _ := json.Marshal(ev)
	_, _ = w.Write(append(b, '\n'))
}

// ---- 记忆查看端点（/v1/memory）----
// 数据面挂在 Server.Mem 上；nil 时两个端点都 503。

func (s *Server) handleMemoryIndex(w http.ResponseWriter, r *http.Request) {
	t, err := s.claims(r)
	if err != nil {
		writeErr(w, 401, err.Error())
		return
	}
	if s.Mem == nil {
		writeErr(w, 503, "memory disabled")
		return
	}
	scopeCtx := scope.WithScope(r.Context(), scope.Scope{
		TenantID: t.TenantID, SubjectID: t.SubjectID,
	})
	heads, err := s.Mem.ListHeaders(scopeCtx)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": heads})
}

// handleMemoryList 返回某条记忆的全文。{space} 实际是记忆 name（slug），
// 空间本身由 ctx scope 决定——URL 只是 REST 形状的占位。
func (s *Server) handleMemoryList(w http.ResponseWriter, r *http.Request) {
	t, err := s.claims(r)
	if err != nil {
		writeErr(w, 401, err.Error())
		return
	}
	if s.Mem == nil {
		writeErr(w, 503, "memory disabled")
		return
	}
	name := r.PathValue("space")
	if name == "" {
		writeErr(w, 400, "missing memory name")
		return
	}
	scopeCtx := scope.WithScope(r.Context(), scope.Scope{
		TenantID: t.TenantID, SubjectID: t.SubjectID,
	})
	row, err := s.Mem.Get(scopeCtx, name)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	writeJSON(w, 200, row)
}

// handleMemoryDream 手动触发 dream 消化（等价 TUI /dream）。仅对 admin 用例，
// 与 memory.go 的 DreamTicker/DreamOnExit 同一入口。
func (s *Server) handleMemoryDream(w http.ResponseWriter, r *http.Request) {
	t, err := s.claims(r)
	if err != nil {
		writeErr(w, 401, err.Error())
		return
	}
	if s.Mem == nil {
		writeErr(w, 503, "memory disabled")
		return
	}
	scopeCtx := scope.WithScope(r.Context(), scope.Scope{
		TenantID: t.TenantID, SubjectID: t.SubjectID,
	})
	stats, err := s.Mem.Dream(scopeCtx, true)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, stats)
}
