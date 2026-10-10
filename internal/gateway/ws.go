package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"runeharness/internal/runner"
)

// wsConn 是一条已升级的 WS 连接的全部状态：
// 一个写出口（sendMu 串行化全部下行帧）+ 每 chat 一个订阅泵。
type wsConn struct {
	conn   *websocket.Conn
	sendMu sync.Mutex
	subs   map[string]context.CancelFunc // chat_id -> cancel
	subMu  sync.Mutex
	tenant string
}

// 客户端上行帧（type 分发）。
type wsIn struct {
	Type    string `json:"type"`
	ChatID  string `json:"chat_id"`
	Content string `json:"content"`
	Since   int64  `json:"since"`
	AskID   string `json:"ask_id"`
	Approved *bool `json:"approved"`
	ReqID   string `json:"req_id"`
	Before  int64  `json:"before"`
	Limit   int    `json:"limit"`
}

// handleWS 升级并进入读循环；首帧不必 ready——任一帧均可起手，
// 服务端先推 {"type":"ready"} 让客户端知道连接已可用。
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	t, err := s.claims(r)
	if err != nil {
		writeErr(w, 401, err.Error())
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // token 鉴权已在前；同源策略对 bot 无意义
	})
	if err != nil {
		return
	}
	defer conn.CloseNow()

	wc := &wsConn{conn: conn, subs: map[string]context.CancelFunc{}, tenant: t.TenantID}
	defer wc.closeAll()

	wc.send(map[string]any{"type": "ready", "timestamp": time.Now().UnixMilli()})

	for {
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return // 断线：客户端重连即可，无服务端状态要清
		}
		var in wsIn
		if err := json.Unmarshal(data, &in); err != nil {
			wc.protocolError("bad frame: " + err.Error())
			continue
		}
		if err := wc.dispatch(r.Context(), s, in); err != nil {
			wc.protocolError(err.Error())
		}
	}
}

// dispatch 分发一帧命令；返回的 error 会作为 protocol error 帧回给客户端。
func (wc *wsConn) dispatch(ctx context.Context, s *Server, in wsIn) error {
	key := runner.ChatKey{TenantID: wc.tenant, Channel: s.Channel, ChatID: in.ChatID}
	switch in.Type {
	case "attach":
		if in.ChatID == "" {
			return errors.New("attach needs chat_id")
		}
		wc.attach(ctx, s, key, in.Since)
		return nil
	case "detach":
		wc.detach(in.ChatID)
		return nil
	case "message":
		if in.ChatID == "" || in.Content == "" {
			return errors.New("message needs chat_id and content")
		}
		if err := s.Mgr.Submit(ctx, key, runner.TurnInput{Prompt: in.Content}); err != nil {
			if errors.Is(err, runner.ErrQueueFull) {
				wc.sendFrame(in.ChatID, map[string]any{
					"type": "error", "code": 409, "message": "chat is busy",
				})
				return nil
			}
			return err
		}
		wc.sendFrame(in.ChatID, map[string]any{"type": "ack"})
		return nil
	case "cancel":
		if in.ChatID == "" {
			return errors.New("cancel needs chat_id")
		}
		return s.Mgr.Interrupt(ctx, key)
	case "answer":
		if in.ChatID == "" || in.AskID == "" || in.Approved == nil {
			return errors.New("answer needs chat_id, ask_id, approved")
		}
		if err := s.Mgr.Answer(ctx, key, in.AskID, *in.Approved); err != nil {
			if errors.Is(err, runner.ErrNotFound) {
				wc.sendFrame(in.ChatID, map[string]any{
					"type": "error", "code": 404, "message": "ask not found",
				})
				return nil
			}
			return err
		}
		return nil
	case "history":
		if in.ChatID == "" {
			return errors.New("history needs chat_id")
		}
		items, hasMore, next, err := s.history(ctx, key, in.Before, in.Limit)
		if err != nil {
			return err
		}
		wc.sendFrame(in.ChatID, map[string]any{
			"type": "history", "req_id": in.ReqID,
			"items": items, "has_more": hasMore, "next_before": next,
		})
		return nil
	case "ping":
		wc.send(map[string]any{"type": "pong", "timestamp": time.Now().UnixMilli()})
		return nil
	default:
		return fmt.Errorf("unknown frame type: %q", in.Type)
	}
}

// attach 启动该 chat 的事件泵；重复 attach 先 cancel 旧泵再换新
// （同 chat 多 attach 只保留最新——重复流对客户端是负担不是特性）。
func (wc *wsConn) attach(ctx context.Context, s *Server, key runner.ChatKey, since int64) {
	wc.detach(key.ChatID)
	pumpCtx, cancel := context.WithCancel(ctx)
	wc.subMu.Lock()
	wc.subs[key.ChatID] = cancel
	wc.subMu.Unlock()

	ch, latest, err := s.Mgr.Subscribe(pumpCtx, key, since)
	if err != nil {
		if errors.Is(err, runner.ErrLag) {
			wc.sendFrame(key.ChatID, map[string]any{
				"type": "event", "event": runner.EvReset,
				"data": map[string]any{"reason": "seq_fell_behind"},
			})
			return
		}
		wc.sendFrame(key.ChatID, map[string]any{
			"type": "error", "message": err.Error(),
		})
		return
	}
	wc.sendFrame(key.ChatID, map[string]any{
		"type": "attached", "chat_id": key.ChatID, "latest_seq": latest,
	})
	go func() {
		for {
			select {
			case <-pumpCtx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				wc.sendEvent(ev)
			}
		}
	}()
}

func (wc *wsConn) detach(chatID string) {
	wc.subMu.Lock()
	defer wc.subMu.Unlock()
	if c, ok := wc.subs[chatID]; ok {
		c()
		delete(wc.subs, chatID)
	}
}

func (wc *wsConn) closeAll() {
	wc.subMu.Lock()
	defer wc.subMu.Unlock()
	for _, c := range wc.subs {
		c()
	}
	wc.subs = nil
}

// sendEvent 下行一条账本事件：信封原样转发（含 seq/chat_id/turn_id/event/data）。
func (wc *wsConn) sendEvent(ev runner.Event) {
	wc.send(ev)
}

// sendFrame 下行一条协议帧：携带 chat_id 便于客户端多路复用。
func (wc *wsConn) sendFrame(chatID string, v map[string]any) {
	if _, ok := v["chat_id"]; !ok {
		v["chat_id"] = chatID
	}
	wc.send(v)
}

// send 是唯一下行出口：marshal + 单写者锁 + 5s 写超时。
func (wc *wsConn) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	wc.sendMu.Lock()
	defer wc.sendMu.Unlock()
	_ = wc.conn.Write(ctx, websocket.MessageText, b)
}

func (wc *wsConn) protocolError(msg string) {
	wc.send(map[string]any{"type": "error", "message": msg})
}

// 保证 net 包被使用（keepalive 相关扩展预留）。
var _ = net.IPv4len
