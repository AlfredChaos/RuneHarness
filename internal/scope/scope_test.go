package scope

import (
	"context"
	"errors"
	"testing"
)

// 注入后应能原样读回。
func TestRoundtrip(t *testing.T) {
	want := Scope{TenantID: "t1", SessionID: "s1", Workspace: "/w"}
	ctx := WithScope(context.Background(), want)
	got, err := FromContext(ctx)
	if err != nil {
		t.Fatalf("FromContext: %v", err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// 未注入或 TenantID 为空都视为缺失——"默认租户"不存在。
func TestMissing(t *testing.T) {
	if _, err := FromContext(context.Background()); !errors.Is(err, ErrMissing) {
		t.Fatalf("empty ctx: got %v, want ErrMissing", err)
	}
	ctx := WithScope(context.Background(), Scope{SessionID: "s", Workspace: "/w"})
	if _, err := FromContext(ctx); !errors.Is(err, ErrMissing) {
		t.Fatalf("empty tenant: got %v, want ErrMissing", err)
	}
}

// WithSession 只换 SessionID，其余字段原样继承——子代理换绑会话的依据。
func TestWithSession(t *testing.T) {
	ctx := WithScope(context.Background(), Scope{TenantID: "t1", SessionID: "s1", Workspace: "/w"})
	got, err := FromContext(WithSession(ctx, "s2"))
	if err != nil {
		t.Fatalf("FromContext: %v", err)
	}
	if got.SessionID != "s2" || got.TenantID != "t1" || got.Workspace != "/w" {
		t.Fatalf("got %+v", got)
	}
}
