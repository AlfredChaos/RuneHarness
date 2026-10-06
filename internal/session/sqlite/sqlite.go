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
	"math"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite" // 纯 Go 驱动，database/sql 名为 "sqlite"

	"runeharness/internal/agent"
	"runeharness/internal/scope"
	"runeharness/internal/session"
)

// schemaVersion 是当前 schema 版本；结构演进时在 migrate 追加分支并 +1。
const schemaVersion = 2

// maxForkDepth 限制 fork 链与祖先 blob 查找的层数，防止脏数据成环。
const maxForkDepth = 8

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
	if v < 2 {
		// v2：压缩方案（plan 第 5 章）。kind 区分普通消息与控制行，
		// usage_json 是估算锚点；blobs / session_state / requests 三张新表。
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, q := range []string{
			`ALTER TABLE messages ADD COLUMN kind TEXT NOT NULL DEFAULT 'msg'`,
			`ALTER TABLE messages ADD COLUMN usage_json TEXT NOT NULL DEFAULT ''`,
			`CREATE TABLE IF NOT EXISTS blobs (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				session_id TEXT NOT NULL REFERENCES sessions(id),
				ref TEXT NOT NULL,
				content TEXT NOT NULL,
				byte_size INTEGER NOT NULL,
				created_at INTEGER NOT NULL,
				UNIQUE(session_id, ref)
			)`,
			`CREATE TABLE IF NOT EXISTS session_state (
				session_id TEXT PRIMARY KEY REFERENCES sessions(id),
				state_json TEXT NOT NULL DEFAULT '{}',
				updated_at INTEGER NOT NULL
			)`,
			`CREATE TABLE IF NOT EXISTS requests (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				session_id TEXT NOT NULL REFERENCES sessions(id),
				upto_msg_id INTEGER NOT NULL,
				upto_blob_id INTEGER NOT NULL DEFAULT 0,
				view_hash TEXT NOT NULL,
				payload_json TEXT NOT NULL DEFAULT '',
				error TEXT NOT NULL DEFAULT '',
				created_at INTEGER NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_requests_session ON requests(session_id)`,
		} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
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
func (s *Store) Append(ctx context.Context, msg agent.Message) (int64, error) {
	sc, err := s.sessionScope(ctx)
	if err != nil {
		return 0, err
	}
	var toolCalls, usage []byte
	if len(msg.ToolCalls) > 0 {
		if toolCalls, err = json.Marshal(msg.ToolCalls); err != nil {
			return 0, fmt.Errorf("marshal tool_calls: %w", err)
		}
	}
	if msg.Usage != nil {
		if usage, err = json.Marshal(msg.Usage); err != nil {
			return 0, fmt.Errorf("marshal usage: %w", err)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // Commit 后为 no-op

	var isErr int
	if msg.IsError {
		isErr = 1
	}
	now := time.Now().UnixMilli()
	res, err := tx.ExecContext(ctx, `INSERT INTO messages
		(session_id, role, content, thinking, tool_calls, tool_call_id, is_error, created_at, kind, usage_json)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		sc.SessionID, string(msg.Role), msg.Content, msg.Thinking, string(toolCalls),
		msg.ToolCallID, isErr, now, kindToDB(msg.Kind), string(usage))
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE sessions SET updated_at = ? WHERE id = ?`, now, sc.SessionID); err != nil {
		return 0, err
	}
	// 会话无 title 且本条是 user 消息：截断回填，--continue 列表可读
	if msg.Role == agent.RoleUser && msg.Kind == agent.KindMessage {
		title := truncateRunes(msg.Content, titleMaxRunes)
		if _, err = tx.ExecContext(ctx,
			`UPDATE sessions SET title = ? WHERE id = ? AND title = ''`,
			title, sc.SessionID); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

// sessionScope 读 ctx 的 scope，要求带 SessionID 且会话属于该租户。
func (s *Store) sessionScope(ctx context.Context) (scope.Scope, error) {
	sc, err := scope.FromContext(ctx)
	if err != nil {
		return sc, err
	}
	if sc.SessionID == "" {
		return sc, ErrNoSession
	}
	return sc, s.checkTenant(ctx, sc, sc.SessionID)
}

// checkTenant 校验会话属于 ctx 的租户。
func (s *Store) checkTenant(ctx context.Context, sc scope.Scope, sessionID string) error {
	tenant, err := s.tenantOf(ctx, s.db, sessionID)
	if err != nil {
		return err
	}
	if tenant != sc.TenantID {
		return ErrCrossTenant
	}
	return nil
}

// kindToDB / kindFromDB 在领域类型与列值之间转换：普通消息存 'msg'。
func kindToDB(k agent.Kind) string {
	if k == agent.KindMessage {
		return "msg"
	}
	return string(k)
}

func kindFromDB(s string) agent.Kind {
	if s == "msg" {
		return agent.KindMessage
	}
	return agent.Kind(s)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// LoadHistory 返回会话当前的发送形态；会话属于其他租户时返回 ErrCrossTenant。
func (s *Store) LoadHistory(ctx context.Context, sessionID string) ([]agent.Message, error) {
	return s.LoadView(ctx, sessionID, math.MaxInt64)
}

// LoadView 返回 id ≤ uptoMsgID 时的发送形态（plan §5.3 的确定性重放）。
func (s *Store) LoadView(ctx context.Context, sessionID string, uptoMsgID int64) ([]agent.Message, error) {
	sc, err := scope.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	return s.loadView(ctx, sc, sessionID, uptoMsgID, 0)
}

func (s *Store) loadView(ctx context.Context, sc scope.Scope, sessionID string, upto int64, depth int) ([]agent.Message, error) {
	if depth > maxForkDepth {
		return nil, fmt.Errorf("session %s: fork chain deeper than %d", sessionID, maxForkDepth)
	}
	if err := s.checkTenant(ctx, sc, sessionID); err != nil {
		return nil, err
	}
	rows, err := s.rawRows(ctx, sessionID, upto)
	if err != nil {
		return nil, err
	}
	// 子会话以 fork 行开头：先取父会话在 fork 水印处的视图作前缀（plan §4.9）。
	if len(rows) > 0 && rows[0].Kind == agent.KindFork {
		var fm session.ForkMeta
		if err := json.Unmarshal([]byte(rows[0].Content), &fm); err != nil {
			return nil, fmt.Errorf("row %d: bad fork meta: %w", rows[0].ID, err)
		}
		parent, err := s.loadView(ctx, sc, fm.ParentSessionID, fm.ParentUptoMsgID, depth+1)
		if err != nil {
			return nil, fmt.Errorf("fork parent %s: %w", fm.ParentSessionID, err)
		}
		rows = append(parent, rows[1:]...)
	}
	return session.Fold(rows)
}

// LoadRawHistory 按 id 升序返回全部原始行（含控制行），审计用。
func (s *Store) LoadRawHistory(ctx context.Context, sessionID string) ([]agent.Message, error) {
	sc, err := scope.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.checkTenant(ctx, sc, sessionID); err != nil {
		return nil, err
	}
	return s.rawRows(ctx, sessionID, math.MaxInt64)
}

// rawRows 读会话 id ≤ upto 的全部行；调用方已做租户校验。
func (s *Store) rawRows(ctx context.Context, sessionID string, upto int64) ([]agent.Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, kind, role, content, thinking, tool_calls,
		tool_call_id, is_error, usage_json FROM messages WHERE session_id = ? AND id <= ? ORDER BY id`,
		sessionID, upto)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hist []agent.Message
	for rows.Next() {
		var m agent.Message
		var kind, role, toolCalls, usage string
		var isErr int
		if err := rows.Scan(&m.ID, &kind, &role, &m.Content, &m.Thinking, &toolCalls,
			&m.ToolCallID, &isErr, &usage); err != nil {
			return nil, err
		}
		m.Kind = kindFromDB(kind)
		m.Role = agent.Role(role)
		m.IsError = isErr != 0
		if toolCalls != "" {
			if err := json.Unmarshal([]byte(toolCalls), &m.ToolCalls); err != nil {
				return nil, fmt.Errorf("unmarshal tool_calls: %w", err)
			}
		}
		if usage != "" {
			m.Usage = &agent.Usage{}
			if err := json.Unmarshal([]byte(usage), m.Usage); err != nil {
				return nil, fmt.Errorf("unmarshal usage: %w", err)
			}
		}
		hist = append(hist, m)
	}
	return hist, rows.Err()
}

// AppendBlob 把被削减的原文写进当前会话的 blob 表。
func (s *Store) AppendBlob(ctx context.Context, ref, content string) error {
	sc, err := s.sessionScope(ctx)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO blobs (session_id, ref, content, byte_size, created_at)
		VALUES (?,?,?,?,?)`, sc.SessionID, ref, content, len(content), time.Now().UnixMilli())
	return err
}

// LoadBlob 读当前会话的 blob；查不到时沿 parent_id 链向上查找祖先会话。
// 祖先与当前会话同租户——parent_id 只在同租户内建立，逐层仍做校验。
func (s *Store) LoadBlob(ctx context.Context, ref string) (string, error) {
	sc, err := s.sessionScope(ctx)
	if err != nil {
		return "", err
	}
	sid := sc.SessionID
	for range maxForkDepth {
		var content string
		err := s.db.QueryRowContext(ctx, `SELECT content FROM blobs WHERE session_id = ? AND ref = ?`,
			sid, ref).Scan(&content)
		if err == nil {
			return content, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		var parent, tenant string
		if err := s.db.QueryRowContext(ctx, `SELECT parent_id, tenant_id FROM sessions WHERE id = ?`,
			sid).Scan(&parent, &tenant); err != nil {
			return "", err
		}
		if tenant != sc.TenantID {
			return "", ErrCrossTenant
		}
		if parent == "" {
			break
		}
		sid = parent
	}
	return "", fmt.Errorf("blob %s: %w", ref, ErrNotFound)
}

// GetState 读当前会话的小状态；没有记录时返回 "{}"。
func (s *Store) GetState(ctx context.Context) (json.RawMessage, error) {
	sc, err := s.sessionScope(ctx)
	if err != nil {
		return nil, err
	}
	var state string
	err = s.db.QueryRowContext(ctx, `SELECT state_json FROM session_state WHERE session_id = ?`,
		sc.SessionID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return json.RawMessage("{}"), nil
	}
	return json.RawMessage(state), err
}

// PutState 覆盖写当前会话的小状态。只放非投影状态：参与发送形态组装的
// 决策一律走追加的控制行（plan §5.3）。
func (s *Store) PutState(ctx context.Context, state json.RawMessage) error {
	sc, err := s.sessionScope(ctx)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO session_state (session_id, state_json, updated_at)
		VALUES (?,?,?) ON CONFLICT(session_id) DO UPDATE SET state_json = excluded.state_json,
		updated_at = excluded.updated_at`, sc.SessionID, string(state), time.Now().UnixMilli())
	return err
}

// LogRequest 写一条请求留痕；消息与 blob 水印都取写入时刻本会话的最大行 id
// ——控制行（boundary/view_clear/fork）决定发送形态，水位必须连它们一起覆盖，
// 否则 LoadView(upto) 重放出的形态与 ViewHash 对不上。
func (s *Store) LogRequest(ctx context.Context, r agent.RequestLog) error {
	sc, err := s.sessionScope(ctx)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO requests
		(session_id, upto_msg_id, upto_blob_id, view_hash, payload_json, error, created_at)
		VALUES (?, (SELECT COALESCE(MAX(id), 0) FROM messages WHERE session_id = ?),
		(SELECT COALESCE(MAX(id), 0) FROM blobs WHERE session_id = ?), ?, ?, ?, ?)`,
		sc.SessionID, sc.SessionID, sc.SessionID, r.ViewHash, r.Payload, r.Error, time.Now().UnixMilli())
	return err
}

// LoadRequests 按 id 升序返回会话的请求留痕。
func (s *Store) LoadRequests(ctx context.Context, sessionID string) ([]session.Request, error) {
	sc, err := scope.FromContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.checkTenant(ctx, sc, sessionID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, upto_msg_id, upto_blob_id, view_hash, payload_json,
		error, created_at FROM requests WHERE session_id = ? ORDER BY id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []session.Request
	for rows.Next() {
		var r session.Request
		var created int64
		if err := rows.Scan(&r.ID, &r.UptoMsgID, &r.UptoBlobID, &r.ViewHash, &r.Payload,
			&r.Error, &created); err != nil {
			return nil, err
		}
		r.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
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
