package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"runeharness/internal/agent"
	"runeharness/internal/scope"
	"runeharness/internal/session"
)

// ── 空闲门：会话静默 ≥15min 时欠账照常消化（免 MinRows/活跃门）──
func TestDreamIdleStillDigests(t *testing.T) {
	old := time.Now().Add(-3 * time.Hour) // 远超 idleDreamMin=15
	sess := &fakeSess{
		sessions: []session.Session{{ID: "s1", Kind: session.KindMain, TenantID: "bear", SubjectID: "app", UpdatedAt: old}},
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
	if err != nil || st.Skipped != "" || st.Rows != 4 {
		t.Fatalf("idle path should digest arrears, got %+v err=%v", st, err)
	}
}

// ── 空闲门：idle + 已过 30min 冷却 → 放行；仍在 30min 内 → 拦截 ──
func TestDreamIdleCooldownShortened(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour)
	mk := func() (*Memory, context.Context) {
		sess := &fakeSess{
			sessions: []session.Session{{ID: "s1", Kind: session.KindMain, TenantID: "bear", SubjectID: "app", UpdatedAt: old}},
			rows:     map[string][]agent.Message{"s1": {{ID: 1, Role: agent.RoleUser, Content: "hi"}}},
		}
		mem, _ := New(sess, &fakeLLM{}, Config{DBPath: ":memory:", Profile: Lookup("companion")})
		return mem, ctxSub("bear", "app", "")
	}
	// last_dream 45min 前：超过空闲冷却 30min → 放行（免 MinRows）。
	m, ctx := mk()
	defer m.Close()
	_ = m.store.SetMeta(ctx, metaLastDream, time.Now().Add(-45*time.Minute).Format(time.RFC3339Nano))
	st, err := m.Dream(ctx, false)
	if err != nil || st.Skipped != "" {
		t.Fatalf("idle + past idle-cooldown should digest, got %+v err=%v", st, err)
	}
	// last_dream 10min 前：仍在空闲冷却内 → 拦截。
	m2, ctx2 := mk()
	defer m2.Close()
	_ = m2.store.SetMeta(ctx2, metaLastDream, time.Now().Add(-10*time.Minute).Format(time.RFC3339Nano))
	st, _ = m2.Dream(ctx2, false)
	if st.Skipped != "cooldown" {
		t.Fatalf("idle + within idle-cooldown should skip, got %+v", st)
	}
}

// ── 活跃门：会话近期有更新 → 常规冷却（IntervalMin）仍然生效 ──
func TestDreamActiveFullCooldown(t *testing.T) {
	sess := &fakeSess{
		sessions: []session.Session{{ID: "s1", Kind: session.KindMain, TenantID: "bear", SubjectID: "app", UpdatedAt: time.Now()}},
		rows:     map[string][]agent.Message{"s1": {{ID: 1, Role: agent.RoleUser, Content: "hi"}}},
	}
	m, _ := New(sess, &fakeLLM{}, Config{DBPath: ":memory:", Profile: Lookup("companion")})
	defer m.Close()
	ctx := ctxSub("bear", "app", "")
	// last_dream 45min 前：空闲路径会放行，但会话活跃 → 常规 120min 冷却拦截。
	_ = m.store.SetMeta(ctx, metaLastDream, time.Now().Add(-45*time.Minute).Format(time.RFC3339Nano))
	st, _ := m.Dream(ctx, false)
	if st.Skipped != "cooldown" {
		t.Fatalf("active + within full cooldown should skip, got %+v", st)
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
