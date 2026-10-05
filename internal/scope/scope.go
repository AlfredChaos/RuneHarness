// Package scope 定义随 ctx 传播的租户上下文：信任根由调用方代码注入，
// 模型生成的工具参数永远接触不到它——租户隔离是函数侧事实而非约定。
package scope

import (
	"context"
	"errors"
)

// ctxKey 是 Scope 在 ctx 中的私有键；包外无法伪造。
type ctxKey struct{}

// Scope 是一次调用链上的租户边界：谁的数据、写到哪个会话、文件操作落在哪。
// 三个字段的用途：TenantID 决定可见数据范围（隔离边界）；
// SessionID 决定消息写入目标（子代理 spawn 时被替换，见 WithSession）；
// Workspace 是文件工具的根目录（本地 = cwd，云端每租户一根）。
type Scope struct {
	TenantID  string
	SessionID string
	Workspace string
}

// ErrMissing 表示 ctx 中没有可用 scope：装配遗漏属于 bug，显式失败好过
// 静默归错租户。不提供"默认租户"兜底。
var ErrMissing = errors.New("scope: not found in context")

// WithScope 把完整 scope 注入 ctx，由进程入口（main / 云端请求入口）调用。
func WithScope(ctx context.Context, s Scope) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// WithSession 只换绑 SessionID（子代理 spawn 后写自己的 session），
// TenantID / Workspace 原样继承。
func WithSession(ctx context.Context, sessionID string) context.Context {
	s, _ := ctx.Value(ctxKey{}).(Scope)
	s.SessionID = sessionID
	return context.WithValue(ctx, ctxKey{}, s)
}

// FromContext 读取 scope。ctx 未注入或 TenantID 为空时返回 ErrMissing；
// SessionID / Workspace 是否需要非空由各消费方自行校验（如 CreateSession
// 时 SessionID 尚未生成是合法的）。
func FromContext(ctx context.Context) (Scope, error) {
	s, ok := ctx.Value(ctxKey{}).(Scope)
	if !ok || s.TenantID == "" {
		return Scope{}, ErrMissing
	}
	return s, nil
}
