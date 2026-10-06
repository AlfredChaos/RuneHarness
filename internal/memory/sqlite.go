package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动，database/sql 名为 "sqlite"

	"runeharness/internal/scope"
)

// schemaVersion 是当前 schema 版本；结构演进时在 migrate 追加分支并 +1。
const schemaVersion = 1

// ErrNotFound 表示目标记忆不存在。
var ErrNotFound = errors.New("memory: not found")

// store 是 Store 的 SQLite 实现。每张表都带 tenant_id+space 列，
// 租户从 ctx scope 取、空间由画像+scope 推导——跨空间读写无从谈起。
type store struct {
	db   *sql.DB
	prof Profile
}

var _ Store = (*store)(nil)

// Open 打开（必要时创建）记忆库并迁移到最新 schema。path 传 ":memory:"
// 得到进程内库（测试用）。
func Open(path string, prof Profile) (*store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	if path != ":memory:" {
		dsn += "&_pragma=journal_mode(WAL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite 单写者，串行化写
	s := &store{db: db, prof: prof}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *store) migrate() error {
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v >= schemaVersion {
		return nil
	}
	if v < 1 {
		for _, q := range []string{
			`CREATE TABLE IF NOT EXISTS memories (
				tenant_id  TEXT NOT NULL,
				space      TEXT NOT NULL,
				name       TEXT NOT NULL,
				type       TEXT NOT NULL DEFAULT '',
				descr      TEXT NOT NULL DEFAULT '',
				body       TEXT NOT NULL DEFAULT '',
				entities   TEXT NOT NULL DEFAULT '[]',
				related    TEXT NOT NULL DEFAULT '[]',
				source_sess TEXT NOT NULL DEFAULT '',
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL,
				PRIMARY KEY (tenant_id, space, name)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_memories_space
				ON memories(tenant_id, space, updated_at)`,
			`CREATE TABLE IF NOT EXISTS memory_log (
				id         INTEGER PRIMARY KEY AUTOINCREMENT,
				tenant_id  TEXT NOT NULL,
				space      TEXT NOT NULL,
				name       TEXT NOT NULL,
				op         TEXT NOT NULL,
				old_body   TEXT NOT NULL DEFAULT '',
				new_body   TEXT NOT NULL DEFAULT '',
				reason     TEXT NOT NULL DEFAULT '',
				sess       TEXT NOT NULL DEFAULT '',
				created_at INTEGER NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS idx_memlog_space
				ON memory_log(tenant_id, space, id)`,
			`CREATE TABLE IF NOT EXISTS memory_meta (
				tenant_id TEXT NOT NULL,
				space     TEXT NOT NULL,
				key       TEXT NOT NULL,
				value     TEXT NOT NULL DEFAULT '',
				PRIMARY KEY (tenant_id, space, key)
			)`,
		} {
			if _, err := s.db.Exec(q); err != nil {
				return err
			}
		}
	}
	if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return err
	}
	return nil
}

// keyOf 解析 (tenant, space) 写入键：tenant 是隔离边界（ctx 信任根），
// space 是画像决定的寻址维度。调用方拿不到这两个值，模型更是无从指定。
func (s *store) keyOf(ctx context.Context) (tenant, space string, err error) {
	sc, err := scope.FromContext(ctx)
	if err != nil {
		return "", "", err
	}
	return sc.TenantID, spaceKey(sc, s.prof), nil
}

func (s *store) List(ctx context.Context) ([]Header, error) {
	t, sp, err := s.keyOf(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, type, descr, updated_at FROM memories
		 WHERE tenant_id=? AND space=? ORDER BY updated_at DESC`, t, sp)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Header
	for rows.Next() {
		var h Header
		var at int64
		if err := rows.Scan(&h.Name, &h.Type, &h.Descr, &at); err != nil {
			return nil, err
		}
		h.UpdatedAt = time.Unix(at, 0)
		out = append(out, h)
	}
	return out, rows.Err()
}

const rowCols = `name, type, descr, body, entities, related, source_sess, created_at, updated_at`

func scanRow(sc interface{ Scan(...any) error }) (Row, error) {
	var r Row
	var ents, rels string
	var cAt, uAt int64
	if err := sc.Scan(&r.Name, &r.Type, &r.Descr, &r.Body, &ents, &rels,
		&r.SourceSess, &cAt, &uAt); err != nil {
		return Row{}, err
	}
	_ = json.Unmarshal([]byte(ents), &r.Entities)
	_ = json.Unmarshal([]byte(rels), &r.Related)
	r.CreatedAt, r.UpdatedAt = time.Unix(cAt, 0), time.Unix(uAt, 0)
	return r, nil
}

func (s *store) Get(ctx context.Context, name string) (Row, error) {
	t, sp, err := s.keyOf(ctx)
	if err != nil {
		return Row{}, err
	}
	r, err := scanRow(s.db.QueryRowContext(ctx,
		`SELECT `+rowCols+` FROM memories
		 WHERE tenant_id=? AND space=? AND name=?`, t, sp, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Row{}, ErrNotFound
	}
	return r, err
}

func (s *store) BodiesOf(ctx context.Context, types []string) ([]Row, error) {
	t, sp, err := s.keyOf(ctx)
	if err != nil {
		return nil, err
	}
	q := `SELECT ` + rowCols + ` FROM memories WHERE tenant_id=? AND space=?`
	args := []any{t, sp}
	if len(types) > 0 {
		q += ` AND type IN (` + strings.TrimSuffix(strings.Repeat("?,", len(types)), ",") + `)`
		for _, ty := range types {
			args = append(args, ty)
		}
	}
	q += ` ORDER BY updated_at DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *store) About(ctx context.Context, entity string) ([]Row, error) {
	t, sp, err := s.keyOf(ctx)
	if err != nil {
		return nil, err
	}
	// entities 列存 JSON 字符串数组；用带引号的精确 token 匹配避免子串误伤，
	// 并转义 LIKE 通配符。
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(entity)
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+rowCols+` FROM memories
		 WHERE tenant_id=? AND space=? AND entities LIKE ? ESCAPE '\'
		 ORDER BY updated_at DESC`,
		t, sp, `%"`+esc+`"%`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ApplyOps 单事务内应用一批操作：每次写都同时在 memory_log 落一条
// 变更流水（旧值/新值/理由/会话），可重放的根基。
func (s *store) ApplyOps(ctx context.Context, ops []Op, sess string) error {
	t, sp, err := s.keyOf(ctx)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	logIt := func(op Op, oldBody string) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO memory_log
			 (tenant_id, space, name, op, old_body, new_body, reason, sess, created_at)
			 VALUES (?,?,?,?,?,?,?,?,?)`,
			t, sp, op.Name, op.Kind, oldBody, op.Body, op.Reason, sess, now)
		return err
	}
	oldBody := func(name string) string {
		var b string
		_ = tx.QueryRowContext(ctx,
			`SELECT body FROM memories WHERE tenant_id=? AND space=? AND name=?`,
			t, sp, name).Scan(&b)
		return b
	}
	for _, op := range ops {
		name := CleanName(op.Name)
		if name == "" {
			continue
		}
		op.Name = name
		switch op.Kind {
		case "upsert", "supersede":
			old := oldBody(name)
			ents, _ := json.Marshal(op.Entities)
			rels, _ := json.Marshal(op.Related)
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO memories
				 (tenant_id, space, name, type, descr, body, entities, related,
				  source_sess, created_at, updated_at)
				 VALUES (?,?,?,?,?,?,?,?,?,?,?)
				 ON CONFLICT(tenant_id, space, name) DO UPDATE SET
				  type=excluded.type, descr=excluded.descr, body=excluded.body,
				  entities=excluded.entities, related=excluded.related,
				  source_sess=excluded.source_sess, updated_at=excluded.updated_at`,
				t, sp, name, op.Type, op.Descr, op.Body,
				string(ents), string(rels), sess, now, now); err != nil {
				return err
			}
			if err := logIt(op, old); err != nil {
				return err
			}
		case "delete", "merge-into":
			old := oldBody(name)
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM memories WHERE tenant_id=? AND space=? AND name=?`,
				t, sp, name); err != nil {
				return err
			}
			if err := logIt(op, old); err != nil {
				return err
			}
		default:
			return fmt.Errorf("memory: unknown op %q", op.Kind)
		}
	}
	return tx.Commit()
}

func (s *store) History(ctx context.Context, name string) ([]Change, error) {
	t, sp, err := s.keyOf(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT op, name, old_body, new_body, reason, sess, created_at
		 FROM memory_log WHERE tenant_id=? AND space=? AND name=?
		 ORDER BY id DESC`, t, sp, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		var c Change
		var at int64
		if err := rows.Scan(&c.Op, &c.Name, &c.OldBody, &c.NewBody,
			&c.Reason, &c.Sess, &at); err != nil {
			return nil, err
		}
		c.At = time.Unix(at, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

// RecentDeletes 返回近期删除的记忆名：dream 的 tombstone 清单——用户删掉
// 的记忆不能被同一批未消化行重新提取出来。
func (s *store) RecentDeletes(ctx context.Context, limit int) ([]string, error) {
	t, sp, err := s.keyOf(ctx)
	if err != nil {
		return nil, err
	}
	// 删除后又重建的名字不算 tombstone——活记忆不进清单。
	rows, err := s.db.QueryContext(ctx,
		`SELECT name FROM (
		   SELECT name, MAX(id) AS last_id FROM memory_log
		   WHERE tenant_id=? AND space=? AND op IN ('delete','merge-into')
		   GROUP BY name
		 ) WHERE name NOT IN (
		   SELECT name FROM memories WHERE tenant_id=? AND space=?
		 ) ORDER BY last_id DESC LIMIT ?`, t, sp, t, sp, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *store) Meta(ctx context.Context, key string) (string, error) {
	t, sp, err := s.keyOf(ctx)
	if err != nil {
		return "", err
	}
	var v string
	err = s.db.QueryRowContext(ctx,
		`SELECT value FROM memory_meta WHERE tenant_id=? AND space=? AND key=?`,
		t, sp, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *store) SetMeta(ctx context.Context, key, val string) error {
	t, sp, err := s.keyOf(ctx)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO memory_meta (tenant_id, space, key, value)
		 VALUES (?,?,?,?)
		 ON CONFLICT(tenant_id, space, key) DO UPDATE SET value=excluded.value`,
		t, sp, key, val)
	return err
}

func (s *store) Count(ctx context.Context) (int, error) {
	t, sp, err := s.keyOf(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memories WHERE tenant_id=? AND space=?`, t, sp).Scan(&n)
	return n, err
}

// lockMetaKey 是 dream 互斥锁在 memory_meta 里的键。
const lockMetaKey = "dream_lock"

// Lock 用 meta 行做带 TTL 的互斥锁：值是到期时刻（unix nano）。
// MaxOpenConns(1) 已把进程内并发串行化，锁的受众是跨进程/跨实例。
func (s *store) Lock(ctx context.Context, ttl time.Duration) (func() error, bool, error) {
	cur, err := s.Meta(ctx, lockMetaKey)
	if err != nil {
		return nil, false, err
	}
	if cur != "" {
		var exp int64
		if _, err := fmt.Sscanf(cur, "%d", &exp); err == nil &&
			time.Now().UnixNano() < exp {
			return nil, false, nil // 锁还活着
		}
	}
	if err := s.SetMeta(ctx, lockMetaKey,
		fmt.Sprintf("%d", time.Now().Add(ttl).UnixNano())); err != nil {
		return nil, false, err
	}
	return func() error { return s.SetMeta(ctx, lockMetaKey, "") }, true, nil
}

func (s *store) Close() error { return s.db.Close() }
