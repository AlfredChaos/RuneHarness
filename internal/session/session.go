// Package session 定义会话持久化的存储抽象。租户与会话目标全部经 ctx 的
// scope.Scope 注入，API 上不存在 tenant 参数——隔离边界在参数层面就不存在，
// 模型生成的工具参数无法指定写入目标。
package session

import (
	"context"
	"time"

	"runeharness/internal/agent"
)

// Kind 是会话类型。
type Kind string

const (
	KindMain     Kind = "main"     // 进程启动即建的主会话
	KindSubagent Kind = "subagent" // task 工具 spawn 的子代理会话
)

// Meta 是 CreateSession 的输入；tenant 不在这里——取自 ctx scope。
type Meta struct {
	ParentID  string // 子代理会话指回父会话；主会话为空
	Title     string // 可空，首条 user 消息后由调用方决定是否回填
	Model     string
	Workspace string
	Kind      Kind
	Depth     int // 0 = 主代理
}

// Session 是一条会话记录。
type Session struct {
	ID        string // UUIDv7（时间有序）
	TenantID  string
	ParentID  string
	Kind      Kind
	Title     string
	Model     string
	Workspace string
	Depth     int
	CreatedAt time.Time
	UpdatedAt time.Time // 每次 Append 刷新
}

// Store 是会话存储的调用面。所有方法从 ctx 读 scope：
// Append 要求 scope 同时带 TenantID 与 SessionID；CreateSession /
// LoadHistory / ListSessions 只需要 TenantID（建会话、--continue 时
// SessionID 尚未确定是合法的）。越租户读写一律返回错误。
type Store interface {
	// CreateSession 用 ctx.TenantID 建会话；返回的 Session 含分配的 UUIDv7 id。
	CreateSession(ctx context.Context, meta Meta) (Session, error)
	// Append 把一条消息追加到 ctx.SessionID 指向的会话；写失败由调用方
	// 决定语义（agent loop 是 fail the turn）。
	Append(ctx context.Context, msg agent.Message) error
	// LoadHistory 按 id 升序回放会话全量消息；会话属其他租户时报错。
	LoadHistory(ctx context.Context, sessionID string) ([]agent.Message, error)
	// ListSessions 按 updated_at 倒序列本租户会话；limit<=0 时不限量。
	ListSessions(ctx context.Context, limit int) ([]Session, error)
	Close() error
}
