// 通知与定时任务端点（ALF-133）：App 轮询拉通知/ack，会话页外展示
// cron 任务清单。通知由 cron 调度器在任务触发时写入 outbox。
package gateway

import (
	"encoding/json"
	"net/http"
	"time"

	"runeharness/internal/session/sqlite"
)

// handleNotifications 返回本租户未 ack 的通知：GET →
// {"notifications":[{id,kind,title,body,created_at}]}（created_at 为
// unix ms；新→旧，封顶 200 条；空列表恒为数组）。
func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	t, err := s.claims(r)
	if err != nil {
		writeErr(w, 401, err.Error())
		return
	}
	if s.Notifier == nil {
		writeErr(w, 501, "notifications not available")
		return
	}
	rows, err := s.Notifier.ListNotifications(r.Context(), t.TenantID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if rows == nil {
		rows = []sqlite.Notification{} // 契约：空库返回 [] 而非 null
	}
	writeJSON(w, 200, map[string]any{"notifications": rows})
}

// handleAckNotifications 标记通知已读：POST {ids:[1,2]} →
// {"acked":N}（N 为本次实际生效条数；重复 ack 幂等返回 0）。
func (s *Server) handleAckNotifications(w http.ResponseWriter, r *http.Request) {
	t, err := s.claims(r)
	if err != nil {
		writeErr(w, 401, err.Error())
		return
	}
	if s.Notifier == nil {
		writeErr(w, 501, "notifications not available")
		return
	}
	var body struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "bad JSON: "+err.Error())
		return
	}
	if len(body.IDs) == 0 {
		writeErr(w, 400, "ids is required, e.g. {\"ids\":[1,2]}")
		return
	}
	n, err := s.Notifier.AckNotifications(r.Context(), t.TenantID, body.IDs)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"acked": n})
}

// handleSchedules 返回本租户的定时任务：GET →
// {"schedules":[{id,cron,tz,prompt,recurring,last_fired,created_at}]}，
// 时间字段 RFC3339、last_fired 可为 null。
func (s *Server) handleSchedules(w http.ResponseWriter, r *http.Request) {
	t, err := s.claims(r)
	if err != nil {
		writeErr(w, 401, err.Error())
		return
	}
	if s.Schedules == nil {
		writeErr(w, 501, "schedules not available")
		return
	}
	out := []map[string]any{}
	for _, task := range s.Schedules(t.TenantID) {
		if task.Tenant != t.TenantID {
			continue // 双保险：装配方已按租户过滤，这里再卡一道
		}
		item := map[string]any{
			"id":        task.ID,
			"cron":      task.Cron,
			"tz":        task.TZ,
			"prompt":    task.Prompt,
			"recurring": task.Recurring,
			"last_fired": func() any {
				if task.LastFired == nil {
					return nil
				}
				return task.LastFired.Format(time.RFC3339)
			}(),
			"created_at": task.CreatedAt.Format(time.RFC3339),
		}
		out = append(out, item)
	}
	writeJSON(w, 200, map[string]any{"schedules": out})
}
