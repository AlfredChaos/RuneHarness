package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"runeharness/internal/agent"
	"runeharness/internal/session"
	"runeharness/internal/scope"
)

// ── 门：静默期不跑 ──
func TestDreamSkipsInactiveCycle(t *testing.T) {
	old := time.Now().Add(-3 * time.Hour) // 超出 companion IntervalMin=120 窗口
	sess := &fakeSess{
		sessions: []session.Session{{ID: "s1", Kind: session.KindMain, UpdatedAt: old}},
		rows: map[string][]agent.Message{
			"s1": {
				{ID: 1, Role: agent.RoleUser, Content: "hi"},
				{ID: 2, Role: agent.RoleUser, Content: "在吗"},
				{ID: 3, Role: agent.RoleUser, Content: "吃了吗"},
				{ID: 4, Role: agent.RoleUser, Content: "忙吗"},
			},
		},
	}
	m, _ := New(sess, &fakeLLM{}, Config{DBPath: ":memory:", Profile: Lookup("companion")})
	defer m.Close()
	st, err := m.Dream(ctxSub("bear", "app", ""), false)
	if err != nil || st.Skipped != "no active session this cycle" {
		t.Fatalf("expected inactive skip, got %+v err=%v", st, err)
	}
}

// ── 门：preflight 判不值得 → 推 cursor 跳过 ──
func TestDreamPreflightSkips(t *testing.T) {
	now := time.Now()
	sess := &fakeSess{
		sessions: []session.Session{
			{ID: "s1", Kind: session.KindMain, TenantID: "bear", SubjectID: "app", UpdatedAt: now},
		},
		rows: map[string][]agent.Message{
			"s1": {
				{ID: 1, Role: agent.RoleUser, Content: "在吗"},
				{ID: 2, Role: agent.RoleAssistant, Content: "在"},
				{ID: 3, Role: agent.RoleUser, Content: "吃了吗"},
				{ID: 4, Role: agent.RoleAssistant, Content: "吃了"},
				{ID: 5, Role: agent.RoleUser, Content: "哈哈哈"},
			},
		},
	}
	// preflight 答 not worth
	m, _ := New(sess, &fakeLLM{replies: []string{
		`{"worth":false,"why":"都是寒暄"}`,
	}}, Config{DBPath: ":memory:", Profile: Lookup("companion")})
	defer m.Close()
	st, err := m.Dream(ctxSub("bear", "app", ""), false)
	if err != nil || !strings.Contains(st.Skipped, "not worth") {
		t.Fatalf("expected preflight skip, got %+v err=%v", st, err)
	}
	// cursor 推到了末尾 → 再跑也是 no unprocessed rows
	st, _ = m.Dream(scope.WithScope(context.Background(),
		scope.Scope{TenantID: "bear", SubjectID: "app"}), true)
	if st.Skipped != "no unprocessed rows" {
		t.Fatalf("cursor should have advanced, got %+v", st)
	}
}

// ── 门：preflight 判值得 → 走正常消化 ──
func TestDreamPreflightPasses(t *testing.T) {
	now := time.Now()
	sess := &fakeSess{
		sessions: []session.Session{
			{ID: "s1", Kind: session.KindMain, TenantID: "bear", SubjectID: "app", UpdatedAt: now},
		},
		rows: map[string][]agent.Message{
			"s1": {
				{ID: 1, Role: agent.RoleUser, Content: "我决定辞职了"},
				{ID: 2, Role: agent.RoleUser, Content: "新工作在深圳南山"},
				{ID: 3, Role: agent.RoleUser, Content: "下周入职"},
				{ID: 4, Role: agent.RoleUser, Content: "有点紧张"},
			},
		},
	}
	m, _ := New(sess, &fakeLLM{replies: []string{
		`{"worth":true,"why":"职业变动"}`,
		`[{"op":"upsert","name":"career-status","type":"persona","description":"d","body":"decided to resign, new job in Nanshan"}]`,
	}}, Config{DBPath: ":memory:", Profile: Lookup("companion")})
	defer m.Close()
	st, err := m.Dream(ctxSub("bear", "app", ""), false)
	if err != nil || st.OpsApplied != 1 {
		t.Fatalf("expected worth+extract, got %+v err=%v", st, err)
	}
	got, _ := m.store.Get(ctxSub("bear", "app", ""), "career-status")
	if !strings.Contains(got.Body, "Nanshan") {
		t.Fatalf("memory not written: %q", got.Body)
	}
}
