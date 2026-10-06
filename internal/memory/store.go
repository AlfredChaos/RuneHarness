// Package memory 实现跨会话记忆：memories 表存当前信念视图，memory_log
// 记 append-only 变更流水，memory_meta 记游标/锁/熔断。读路径是会话第一条
// user 消息里的索引注入；写路径是 memory 工具直写与 dream 消化。
// 设计见 docs/runeharness-memory-plan.html。
package memory

import (
	"context"
	"regexp"
	"strings"
	"time"

	"runeharness/internal/scope"
)

// SpaceKind 是记忆空间的隔离维度，由画像选定。
type SpaceKind string

const (
	SpaceWorkspace SpaceKind = "workspace" // 本目录（coding 默认）
	SpaceSubject   SpaceKind = "subject"   // 当前用户，跨工作区（companion 默认）
	SpaceTenant    SpaceKind = "tenant"    // 租户共享
)

// Row 是一条记忆记录（当前信念视图）。
type Row struct {
	Name       string   // slug，写路径上由 CleanName 清洗
	Type       string   // 画像字典里的类型名
	Descr      string   // 一句话描述，进索引
	Body       string   // Markdown 正文
	Entities   []string // 涉及的实体标签（写时归一）
	Related    []string // 关联的记忆名
	SourceSess string   // 来源会话
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Header 是索引渲染用的记忆头（不含正文）。
type Header struct {
	Name      string
	Type      string
	Descr     string
	UpdatedAt time.Time
}

// Change 是 memory_log 里的一条变更记录；流水即审计与回放数据源。
type Change struct {
	Op      string // upsert | supersede | delete | merge-into
	Name    string
	OldBody string
	NewBody string
	Reason  string
	Sess    string
	At      time.Time
}

// Op 是一次记忆写操作；dream 产出、工具产出，经校验后进 ApplyOps。
type Op struct {
	Kind     string   `json:"op"`   // upsert | supersede | delete | merge-into
	Name     string   `json:"name"` // merge-into 时是来源名
	Type     string   `json:"type"`
	Descr    string   `json:"description"`
	Body     string   `json:"body"`
	Entities []string `json:"entities"`
	Related  []string `json:"related"`
	Target   string   `json:"target"` // merge-into 的目标名
	Reason   string   `json:"reason"` // supersede/merge-into 必填
}

// Stats 是一次 dream 的统计结果。
type Stats struct {
	Sessions   int    // 消化过的会话数
	Rows       int    // 消化过的原始行数
	OpsApplied int    // 应用的记忆操作数（不含整理）
	TidyMerged int    // 整理阶段删除的条目数
	Skipped    string // 未执行的原因（非空表示被门拦下）
}

// Store 是记忆存储面。租户与空间由实现方从 ctx 的 scope 及画像解析——
// 接口签名里没有租户参数，模型生成的参数无法指定读写目标
// （与 session.Store 同一套路）。
type Store interface {
	// List 返回本空间全部记忆头（updated_at 倒序）。
	List(ctx context.Context) ([]Header, error)
	// Get 按名取一条记忆；不存在返回 ErrNotFound。
	Get(ctx context.Context, name string) (Row, error)
	// BodiesOf 返回指定类型的完整记忆行；types 为空返回全部。
	BodiesOf(ctx context.Context, types []string) ([]Row, error)
	// About 返回实体标签含 entity 的记忆行。
	About(ctx context.Context, entity string) ([]Row, error)
	// ApplyOps 在单事务内应用一批操作：写行 + 写流水。sess 是来源会话。
	ApplyOps(ctx context.Context, ops []Op, sess string) error
	// History 返回一条记忆的变更流水（新在前）。
	History(ctx context.Context, name string) ([]Change, error)
	// RecentDeletes 返回近期删除的记忆名（tombstone，防 dream 复活）。
	RecentDeletes(ctx context.Context, limit int) ([]string, error)
	// Meta/SetMeta 读写本空间的 KV 状态（游标、锁、熔断计数）。
	Meta(ctx context.Context, key string) (string, error)
	SetMeta(ctx context.Context, key, val string) error
	// Count 返回本空间记忆条数。
	Count(ctx context.Context) (int, error)
	// Lock 取 dream 互斥锁（meta 行 + 过期时间）。返回 (unlock, ok, err)。
	Lock(ctx context.Context, ttl time.Duration) (func() error, bool, error)
	Close() error
}

// spaceKey 解析当前 ctx 归属的记忆空间键。tenant 不放进键——租户隔离
// 由存储层按 scope.TenantID 列级隔离，键只需在租户内唯一。
func spaceKey(sc scope.Scope, p Profile) string {
	switch p.Space {
	case SpaceSubject:
		sub := sc.SubjectID
		if sub == "" {
			sub = "local-user"
		}
		return "subject/" + sub
	case SpaceTenant:
		return "shared"
	default:
		return "ws/" + CleanName(sc.Workspace)
	}
}

var slugChars = regexp.MustCompile(`[^a-z0-9]+`)

// CleanName 把任意字符串规整成 slug：小写、非字母数字折成 "-"、压 120 字节。
// 模型传入的 name 经此清洗，路径逃逸字符在入口处消掉。
func CleanName(s string) string {
	s = slugChars.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-")
	s = strings.Trim(s, "-")
	if len(s) > 120 {
		s = strings.TrimRight(s[:120], "-")
	}
	return s
}
