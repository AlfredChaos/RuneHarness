// Package gateway 是 RuneHarness 的外部接入面（docs/runeharness-api.html）：
// 一条 WS 轨（bear 长连协议）+ 一条 HTTP/SSE 轨（单消息请求-事件流响应）。
// 两条轨共用同一套事件账本与 SessionRunner——客户端任选其一接入。
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"runeharness/internal/memory"
	"runeharness/internal/runner"
	"runeharness/internal/scope"
	"runeharness/internal/agent"
)

// Token 是 Bearer token 的解析结果；tenant+subject 来自 token 声明，
// 渠道层不自带身份——token 持有者即用户。
type Token struct {
	TenantID string
	SubjectID string
}

// Authenticator 把 Bearer token 解析成租户与主体。
// 实现方做签名/有效期/吊销检查；返回 error 即 401。
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (Token, error)
}

// StaticTokens 是 RUNE_TOKENS 的解析结果：token -> {tenant, subject}。
// 形如 "tk1=tenantA:alice,tk2=tenantA:bob,tk3=tenantB:"  ——
// 缺 subject 用 "default"，缺 tenant 用 "local"。
func StaticTokens(spec string) (map[string]Token, error) {
	out := map[string]Token{}
	for _, ent := range strings.Split(spec, ",") {
		ent = strings.TrimSpace(ent)
		if ent == "" {
			continue
		}
		tok, rest, ok := strings.Cut(ent, "=")
		if !ok || strings.TrimSpace(tok) == "" {
			return nil, fmt.Errorf("RUNE_TOKENS: bad entry %q (want token=tenant:subject)", ent)
		}
		tenant, subject, _ := strings.Cut(rest, ":")
		if tenant == "" {
			tenant = "local"
		}
		if subject == "" {
			subject = "default"
		}
		out[strings.TrimSpace(tok)] = Token{TenantID: tenant, SubjectID: subject}
	}
	if len(out) == 0 {
		return nil, errors.New("RUNE_TOKENS: no tokens configured")
	}
	return out, nil
}

// StaticAuth 返回一个查表认证的 Authenticator。
func StaticAuth(tokens map[string]Token) Authenticator {
	return staticAuth{tokens}
}

type staticAuth struct{ tokens map[string]Token }

func (s staticAuth) Authenticate(ctx context.Context, tok string) (Token, error) {
	if t, ok := s.tokens[tok]; ok {
		return t, nil
	}
	return Token{}, errors.New("unknown token")
}

// bearer 从请求头取 Bearer token；缺失或格式错误返回空串。
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
}

// ---- Server ----

// Store 是 gateway 需要的窄存储接口：会话历史查询 + chat_bindings。
// *sqlite.Store 满足它；测试用内存 fake 也可。
type Store interface {
	LoadRawHistory(ctx context.Context, sessionID string) ([]agent.Message, error)
	// GetBinding 返回 (tenant,channel,chat_id) -> session_id，未绑定空串。
	GetBinding(ctx context.Context, tenant, channel, chatID string) (string, error)
	ListByTenant(ctx context.Context, tenant, channel string) ([]map[string]any, error)
}

// Server 是网关 HTTP 面：/ws（WS 轨）与 /v1/*（HTTP/SSE 轨）。
type Server struct {
	Mgr     *runner.Manager
	Auth    Authenticator
	Store   Store   // chat_bindings 查询 + 历史页
	Channel string  // 本轨道渠道名（"bear"/"http"），落到 ChatKey 与 channel 列
	// Mem 是记忆查看出口；nil 时 /v1/memory* 一律 503。
	Mem *memory.Memory

	mux *http.ServeMux
}

func NewServer(mgr *runner.Manager, auth Authenticator, store Store, channel string) *Server {
	s := &Server{Mgr: mgr, Auth: auth, Store: store, Channel: channel}
	s.mux = http.NewServeMux()
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /v1/health", s.handleHealth)
	s.mux.HandleFunc("GET /v1/chats", s.handleListChats)
	s.mux.HandleFunc("GET /v1/chats/{id}", s.handleGetChat)
	s.mux.HandleFunc("GET /v1/chats/{id}/history", s.handleHistory)
	s.mux.HandleFunc("POST /v1/chats/{id}/messages", s.handlePostMessage)
	s.mux.HandleFunc("GET /v1/chats/{id}/events", s.handleSSE)
	s.mux.HandleFunc("POST /v1/chats/{id}/stream", s.handleStream)
	s.mux.HandleFunc("POST /v1/chats/{id}/actions", s.handleAction)
	s.mux.HandleFunc("GET /v1/memory", s.handleMemoryIndex)
	s.mux.HandleFunc("GET /v1/memory/{space}", s.handleMemoryList)
	s.mux.HandleFunc("POST /v1/memory/dream", s.handleMemoryDream)
	s.mux.HandleFunc("GET /svc/history", s.handleSvcHistory)
	s.mux.HandleFunc("GET /svc/search", s.handleSvcSearch)
	s.mux.HandleFunc("GET /svc/health", s.handleHealth)
	s.mux.HandleFunc("/ws", s.handleWS)
}

// claims 从请求解析 token -> ChatKey 前缀（tenant+subject）。
func (s *Server) claims(r *http.Request) (Token, error) {
	tok := bearer(r)
	if tok == "" {
		return Token{}, errors.New("missing bearer token")
	}
	return s.Auth.Authenticate(r.Context(), tok)
}

// chatKey 组装本次请求的目标槽位。
func (s *Server) chatKey(r *http.Request, chatID string) (runner.ChatKey, error) {
	t, err := s.claims(r)
	if err != nil {
		return runner.ChatKey{}, err
	}
	return runner.ChatKey{
		TenantID: t.TenantID,
		Channel:  s.Channel,
		ChatID:   chatID,
	}, nil
}

// writeJSON / writeErr 是响应出口：所有错误走统一包 {error:{...}}。
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{
		"message": msg, "code": code,
	}})
}

// withScope 把鉴权结论注入 ctx，供下游 scope.FromContext 使用。
// TenantID 只在 ctx 里有效——handler 收到的 tenant 参数只是过滤条件。
func withScope(ctx context.Context, t Token, sessionID string) context.Context {
	return scope.WithScope(ctx, scope.Scope{
		TenantID:  t.TenantID,
		SubjectID: t.SubjectID,
		SessionID: sessionID,
	})
}
