// 管理与命名端点：测试会话清理（ALF-130）+ 手动触发 AI 命名。
// 两条轨共用 Bearer 鉴权：tenant 恒来自 token，不在参数面上开放。
package gateway

import (
	"encoding/json"
	"net/http"
)

// handlePurgeChats 清理冒烟/测试会话：POST {keep:["main"]}——保留 keep
// 内的 chat，其余本租户本渠道 chat 连同会话数据（binding+messages+state）
// 事务内全删。幂等可重放；响应 {"purged":[...],"count":N}。
func (s *Server) handlePurgeChats(w http.ResponseWriter, r *http.Request) {
	t, err := s.claims(r)
	if err != nil {
		writeErr(w, 401, err.Error())
		return
	}
	if s.Purger == nil {
		writeErr(w, 501, "purge not available")
		return
	}
	var body struct {
		Keep []string `json:"keep"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "bad JSON: "+err.Error())
		return
	}
	if len(body.Keep) == 0 {
		writeErr(w, 400, `keep is required, e.g. {"keep":["main"]}`)
		return
	}
	purged, err := s.Purger.PurgeChats(r.Context(), t.TenantID, s.Channel, body.Keep)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"purged": purged, "count": len(purged)})
}

// handleChatTitle 手动触发一次 AI 命名（force：跳过默认标题检查，已命名
// 的会话也会重新生成）。同步等待结果，响应 {"chat_id","session_id","title"}。
func (s *Server) handleChatTitle(w http.ResponseWriter, r *http.Request) {
	t, err := s.claims(r)
	if err != nil {
		writeErr(w, 401, err.Error())
		return
	}
	if s.Namer == nil {
		writeErr(w, 501, "naming not available")
		return
	}
	chatID := r.PathValue("id")
	sid, err := s.Store.GetBinding(r.Context(), t.TenantID, s.Channel, chatID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if sid == "" {
		writeErr(w, 404, "unknown chat: "+chatID)
		return
	}
	title, err := s.Namer.Rename(t.TenantID, s.Channel, sid, true)
	if err != nil {
		writeErr(w, 502, "naming failed: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"chat_id": chatID, "session_id": sid, "title": title})
}
