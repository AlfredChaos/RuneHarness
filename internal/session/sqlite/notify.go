// 通知 outbox（ALF-133）：服务端主动产生的待提醒事件（cron 触发等）落库，
// App 轮询 /v1/notifications 拉取后 ack。写入方（调度器）与读取方（网关）
// 都显式带 tenant——不经 ctx scope，写读口径一致。
package sqlite

import (
	"context"
	"strings"
	"time"
)

// Notification 是 /v1/notifications 的一行（未读通知）。
type Notification struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	CreatedAt int64  `json:"created_at"` // unix ms
}

// PushNotification 写一条通知，返回分配的 id。
func (s *Store) PushNotification(ctx context.Context, tenant, kind, title, body string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO notifications (tenant_id, kind, title, body, created_at) VALUES (?,?,?,?,?)`,
		tenant, kind, title, body, time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListNotifications 返回该租户未 ack 的通知，新→旧，封顶 200 条。
func (s *Store) ListNotifications(ctx context.Context, tenant string) ([]Notification, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, title, body, created_at FROM notifications
		 WHERE tenant_id = ? AND acked_at = 0 ORDER BY id DESC LIMIT 200`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Notification
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.Kind, &n.Title, &n.Body, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// AckNotifications 把 ids 中属于该租户且未读的通知标记已读，返回生效条数。
// 幂等：重复 ack 同一批 id，第二次生效 0 条。
func (s *Store) AckNotifications(ctx context.Context, tenant string, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var q strings.Builder
	q.WriteString(`UPDATE notifications SET acked_at = ? WHERE tenant_id = ? AND acked_at = 0 AND id IN (`)
	args := []any{time.Now().UnixMilli(), tenant}
	for i, id := range ids {
		if i > 0 {
			q.WriteByte(',')
		}
		q.WriteByte('?')
		args = append(args, id)
	}
	q.WriteByte(')')
	res, err := s.db.ExecContext(ctx, q.String(), args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
