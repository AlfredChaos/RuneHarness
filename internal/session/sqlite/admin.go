// Package sqlite 的管理面：会话标题读写与测试会话清理（ALF-130）。
// 标题存 sessions.title——创建时是渠道默认形态（channel:chatID），
// AI 命名后覆盖为可读标题；清理删 binding + 会话全部落库数据。
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SessionTitle 读会话当前标题；会话不存在返回 ErrNotFound。
func (s *Store) SessionTitle(ctx context.Context, sessionID string) (string, error) {
	sc, err := s.sessionScope(ctx)
	if err != nil {
		return "", err
	}
	if err := s.checkTenant(ctx, sc, sessionID); err != nil {
		return "", err
	}
	var title string
	err = s.db.QueryRowContext(ctx, `SELECT title FROM sessions WHERE id = ?`, sessionID).Scan(&title)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ErrNotFound, sessionID)
	}
	return title, err
}

// SetTitle 覆盖写会话标题（AI 命名与手动改名共用）；会话不存在返回 ErrNotFound。
func (s *Store) SetTitle(ctx context.Context, sessionID, title string) error {
	sc, err := s.sessionScope(ctx)
	if err != nil {
		return err
	}
	if err := s.checkTenant(ctx, sc, sessionID); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET title = ? WHERE id = ?`, title, sessionID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, sessionID)
	}
	return nil
}

// victim 是一条待清理的 chat 绑定。
type victim struct {
	chatID    string
	sessionID string
}

// PurgeChats 删除本租户本渠道 keep 之外的全部 chat：binding 连同会话
// 数据（messages/blobs/session_state/requests/session_leases/sessions）
// 在同一事务内删除，返回被清理的 chat_id 列表。仍被保留绑定引用的
// session 不删（同一 session 多绑定的防御）；子会话（parent_id 链）
// 随主会话一并清理。keep 里不存在的 chat 自然无副作用——幂等可重放。
func (s *Store) PurgeChats(ctx context.Context, tenant, channel string, keep []string) ([]string, error) {
	keepSet := make(map[string]bool, len(keep))
	for _, k := range keep {
		keepSet[k] = true
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT chat_id, session_id FROM chat_bindings WHERE tenant_id = ? AND channel = ?`,
		tenant, channel)
	if err != nil {
		return nil, err
	}
	var victims []victim
	for rows.Next() {
		var v victim
		if err := rows.Scan(&v.chatID, &v.sessionID); err != nil {
			rows.Close()
			return nil, err
		}
		if !keepSet[v.chatID] {
			victims = append(victims, v)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(victims) == 0 {
		return []string{}, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() // Commit 后为 no-op

	for _, v := range victims {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM chat_bindings WHERE tenant_id = ? AND channel = ? AND chat_id = ?`,
			tenant, channel, v.chatID); err != nil {
			return nil, err
		}
	}
	for _, v := range victims {
		// 先删后查：本事务刚删掉的绑定不再计入引用。
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM chat_bindings WHERE session_id = ?`, v.sessionID).Scan(&n); err != nil {
			return nil, err
		}
		if n > 0 {
			continue // 仍有保留绑定指向该 session：只删绑定不删数据
		}
		if err := purgeSessionTree(ctx, tx, tenant, v.sessionID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	out := make([]string, len(victims))
	for i, v := range victims {
		out[i] = v.chatID
	}
	return out, nil
}

// purgeSessionTree 删除会话及其子会话树（parent_id 链，深度封顶同
// maxForkDepth）的全部落库数据；只处理 tenant 租户内的行。
func purgeSessionTree(ctx context.Context, tx *sql.Tx, tenant, sessionID string) error {
	ids := []string{sessionID}
	seen := map[string]bool{sessionID: true}
	frontier := []string{sessionID}
	for level := 0; level < maxForkDepth && len(frontier) > 0; level++ {
		var next []string
		for _, id := range frontier {
			kids, err := childSessions(ctx, tx, tenant, id)
			if err != nil {
				return err
			}
			for _, k := range kids {
				if seen[k] {
					continue
				}
				seen[k] = true
				ids = append(ids, k)
				next = append(next, k)
			}
		}
		frontier = next
	}
	for _, id := range ids {
		for _, q := range []string{
			`DELETE FROM messages WHERE session_id = ?`,
			`DELETE FROM blobs WHERE session_id = ?`,
			`DELETE FROM session_state WHERE session_id = ?`,
			`DELETE FROM requests WHERE session_id = ?`,
			`DELETE FROM session_leases WHERE session_id = ?`,
		} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM sessions WHERE id = ? AND tenant_id = ?`, id, tenant); err != nil {
			return err
		}
	}
	return nil
}

// childSessions 列出 tenant 租户内 parent_id 直指 id 的子会话。
func childSessions(ctx context.Context, tx *sql.Tx, tenant, id string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM sessions WHERE parent_id = ? AND tenant_id = ?`, id, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kid string
		if err := rows.Scan(&kid); err != nil {
			return nil, err
		}
		out = append(out, kid)
	}
	return out, rows.Err()
}
