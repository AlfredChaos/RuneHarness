// Package sqlite 用单文件 SQLite 实现 session.Store。
// WAL 让读不阻塞写；单写者连接 + busy_timeout 把 SQLite 的单写者限制
// 变成显式串行，杜绝 SQLITE_BUSY 偶发。schema 版本走 PRAGMA user_version，
// 不引入迁移框架。
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite" // 纯 Go 驱动，database/sql 名为 "sqlite"

	"runeharness/internal/agent"
	"runeharness/internal/scope"
	"runeharness/internal/session"
)

// schemaVersion 是当前 schema 版本；结构演进时在 migrate 追加分支并 +1。
const schemaVersion = 1

// titleMaxRunes 是首条 user 消息回填 sessions.title 的截断长度。
const titleMaxRunes = 40

var (
	// ErrCrossTenant 表示 ctx 租户与目标记录的租户不一致——越权读写。
	ErrCrossTenant = errors.New("session: cross-tenant access denied")
	// ErrNoSession 表示 Append 时 scope 缺 SessionID（CreateSession 不需要）。
	ErrNoSession = errors.New("session: scope has no session id")
	// ErrNotFound 表示目标会话不存在。
	ErrNotFound = errors.New("session: not found")
)

// Store 是 session.Store 的 SQLite 实现。
type Store struct {
	db *sql.DB
}

var _ session.Store = (*Store)(nil)

// Open 打开（必要时创建）数据库并迁移到最新 schema；失败即返回错误，
// 调用方应启动即死，不做半拉子记录。path 传 ":memory:" 得到进程内库。
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	if path != ":memory:" {
		dsn += "&_pragma=journal_mode(WAL)" // :memory: 无 WAL 语义
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite 单写者，串行化写
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v >= schemaVersion {
		return nil
	}
	if v < 1 {
		for _, q := range []string{
			`CREATE TABLE IF NOT EXISTS sessions (
				id TEXT PRIMARY KEY,
				tenant_id TEXT NOT NULL,
				parent_id TEXT NOT NULL DEFAULT '',
				kind TEXT NOT NULL DEFAULT 'main',
				title TEXT NOT NULL DEFAULT '',
				model TEXT NOT NULL DEFAULT '',
				workspace TEXT NOT NULL DEFAULT '',
				depth INTEGER NOT NULL DEFAULT 0,
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_sessions_tenant ON sessions(tenant_id, updated_at)`,
			`CREATE TABLE IF NOT EXISTS messages (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				session_id TEXT NOT NULL REFERENCES sessions(id),
				role TEXT NOT NULL,
				content TEXT NOT NULL DEFAULT '',
				thinking TEXT NOT NULL DEFAULT '',
				tool_calls TEXT NOT NULL DEFAULT '',
				tool_call_id TEXT NOT NULL DEFAULT '',
				is_error INTEGER NOT NULL DEFAULT 0,
				created_at INTEGER NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, id)`,
		} {
			if _, err := s.db.Exec(q); err != nil {
				return err
			}
		}
	}
	// pragma 不支持占位符，版本号是常量
	if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return err
	}
	return nil
}

// tenantOf 返回会话的租户；会话不存在时返回 ErrNotFound。
func (s *Store) tenantOf(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, sessionID string) (string, error) {
	var tenant string
	err := q.QueryRowContext(ctx, `SELECT tenant_id FROM sessions WHERE id = ?`, sessionID).Scan(&tenant)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ErrNotFound, sessionID)
	}
	return tenant, err
}

// CreateSession 用 ctx.TenantID 建会话；meta 不含租户（隔离边界只能来自 ctx）。
func (s *Store) CreateSession(ctx context.Context, meta session.Meta) (session.Session, error) {
	sc, err := scope.FromContext(ctx)
	if err != nil {
		return session.Session{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return session.Session{}, fmt.Errorf("uuidv7: %w", err)
	}
	kind := meta.Kind
	if kind == "" {
		kind = session.KindMain
	}
	ws := meta.Workspace
	if ws == "" {
		ws = sc.Workspace
	}
	now := time.Now().UnixMilli()
	sid := id.String()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO sessions
		(id, tenant_id, parent_id, kind, title, model, workspace, depth, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		sid, sc.TenantID, meta.ParentID, string(kind), meta.Title, meta.Model, ws,
		meta.Depth, now, now); err != nil {
		return session.Session{}, err
	}
	return session.Session{
		ID: sid, TenantID: sc.TenantID, ParentID: meta.ParentID, Kind: kind,
		Title: meta.Title, Model: meta.Model, Workspace: ws, Depth: meta.Depth,
		CreatedAt: time.UnixMilli(now).UTC(), UpdatedAt: time.UnixMilli(now).UTC(),
	}, nil
}

// Append 把一条消息写入 ctx.SessionID 的会话：messages.id 自增即全局时序。
// 同 tx 内校验租户归属并刷新 updated_at；首条 user 消息回填空 title。
func (s *Store) Append(ctx context.Context, msg agent.Message) error {
	sc, err := scope.FromContext(ctx)
	if err != nil {
		return err
	}
	if sc.SessionID == "" {
		return ErrNoSession
	}
	var toolCalls []byte
	if len(msg.ToolCalls) > 0 {
		if toolCalls, err = json.Marshal(msg.ToolCalls); err != nil {
			return fmt.Errorf("marshal tool_calls: %w", err)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // Commit 后为 no-op

	tenant, err := s.tenantOf(ctx, tx, sc.SessionID)
	if err != nil {
		return err
	}
	if tenant != sc.TenantID {
		return ErrCrossTenant
	}
	var isErr int
	if msg.IsError {
		isErr = 1
	}
	now := time.Now().UnixMilli()
	if _, err = tx.ExecContext(ctx, `INSERT INTO messages
		(session_id, role, content, thinking, tool_calls, tool_call_id, is_error, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		sc.SessionID, string(msg.Role), msg.Content, msg.Thinking, string(toolCalls),
		msg.ToolCallID, isErr, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE sessions SET updated_at = ? WHERE id = ?`, now, sc.SessionID); err != nil {
		return err
	}
	// 会话无 title 且本条是 user 消息：截断回填，--continue 列表可读
	if msg.Role == agent.RoleUser {
		title := truncateRunes(msg.Content, titleMaxRunes)
		if _, err = tx.ExecContext(ctx,
			`UPDATE sessions SET title = ? WHERE id = ? AND title = ''`,
			title, sc.SessionID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// LoadHistory 按 id 升序回放全量消息；会话属于其他租户时返回 ErrCrossTenant。
func (s *Store) LoadHistory(ctx context.Context, sessionID string) ([]agent.Message, error) {
	sc, err := scope.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	tenant, err := s.tenantOf(ctx, s.db, sessionID)
	if err != nil {
		return nil, err
	}
	if tenant != sc.TenantID {
		return nil, ErrCrossTenant
	}
	rows, err := s.db.QueryContext(ctx, `SELECT role, content, thinking, tool_calls,
		tool_call_id, is_error FROM messages WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hist []agent.Message
	for rows.Next() {
		var m agent.Message
		var role, toolCalls string
		var isErr int
		if err := rows.Scan(&role, &m.Content, &m.Thinking, &toolCalls,
			&m.ToolCallID, &isErr); err != nil {
			return nil, err
		}
		m.Role = agent.Role(role)
		m.IsError = isErr != 0
		if toolCalls != "" {
			if err := json.Unmarshal([]byte(toolCalls), &m.ToolCalls); err != nil {
				return nil, fmt.Errorf("unmarshal tool_calls: %w", err)
			}
		}
		hist = append(hist, m)
	}
	return hist, rows.Err()
}

// ListSessions 按 updated_at 倒序列本租户会话。
func (s *Store) ListSessions(ctx context.Context, limit int) ([]session.Session, error) {
	sc, err := scope.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	q := `SELECT id, tenant_id, parent_id, kind, title, model, workspace, depth,
		created_at, updated_at FROM sessions WHERE tenant_id = ? ORDER BY updated_at DESC`
	args := []any{sc.TenantID}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []session.Session
	for rows.Next() {
		var ssn session.Session
		var kind string
		var created, updated int64
		if err := rows.Scan(&ssn.ID, &ssn.TenantID, &ssn.ParentID, &kind, &ssn.Title,
			&ssn.Model, &ssn.Workspace, &ssn.Depth, &created, &updated); err != nil {
			return nil, err
		}
		ssn.Kind = session.Kind(kind)
		ssn.CreatedAt = time.UnixMilli(created).UTC()
		ssn.UpdatedAt = time.UnixMilli(updated).UTC()
		out = append(out, ssn)
	}
	return out, rows.Err()
}

// Close 关闭底层连接。
func (s *Store) Close() error { return s.db.Close() }
