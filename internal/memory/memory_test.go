package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"runeharness/internal/agent"
	"runeharness/internal/scope"
	"runeharness/internal/session"
	"runeharness/internal/tools"
)

// ── fakes ──

// fakeLLM 按序返回预置的 assistant 文本；超出预置后返回空 content。
// failAt 指定第 N 次调用返回错误（0=不失效）；lastSys 记录最近一次调用
// 的 system 消息，供断言提取提示的内容（tombstone 清单等）。
type fakeLLM struct {
	replies []string
	calls   int
	failAt  int
	lastSys string
}

func (f *fakeLLM) Chat(_ context.Context, hist []agent.Message, _ []tools.Spec,
	_ func(agent.Partial)) (agent.Response, error) {
	f.calls++
	if len(hist) > 0 {
		f.lastSys = hist[0].Content
	}
	if f.calls == f.failAt {
		return agent.Response{}, errors.New("llm down")
	}
	out := ""
	if f.calls <= len(f.replies) {
		out = f.replies[f.calls-1]
	}
	return agent.Response{Message: agent.Message{Role: agent.RoleAssistant, Content: out}}, nil
}

// fakeSess 只实现 dream 用到的两个方法，其余接口方法嵌入后为零值。
type fakeSess struct {
	session.Store
	sessions []session.Session
	rows     map[string][]agent.Message
}

func (f *fakeSess) ListSessions(context.Context, int) ([]session.Session, error) {
	return f.sessions, nil
}

func (f *fakeSess) LoadRawHistory(_ context.Context, id string) ([]agent.Message, error) {
	return f.rows[id], nil
}

func ctxFor(tenant, ws, sess string) context.Context {
	return scope.WithScope(context.Background(),
		scope.Scope{TenantID: tenant, Workspace: ws, SessionID: sess})
}

func newTestMem(t *testing.T) (*Memory, context.Context) {
	t.Helper()
	m, err := New(nil, nil, Config{DBPath: ":memory:", Profile: Lookup("coding")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m, ctxFor("t1", "/ws/proj", "s1")
}

// ── 纯函数 ──

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{
		"User Likes Go":     "user-likes-go",
		"  ../etc/passwd  ": "etc-passwd",
		"a/b\\c:d":          "a-b-c-d",
		"中文空格 test":         "test", // 非 ASCII 段全部折成连字符后去掉
		"already-fine":      "already-fine",
		"--lead--trail--":   "lead-trail",
	} {
		if got := CleanName(in); got != want {
			t.Errorf("CleanName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLooksLikeSecret(t *testing.T) {
	for _, s := range []string{
		"token is sk-AbC123xYz789qRsTuVwXyZ",
		"aws key AKIAIOSFODNN7EXAMPLE",
		"ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"JWT eyJhbGciOiJIUzI1NiIs.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIE...",
		`db password: "s3cr3t-p@ssw0rd"`,
		"api_key = abcdef123456",
	} {
		if !LooksLikeSecret(s) {
			t.Errorf("LooksLikeSecret(%q) = false, want true", s)
		}
	}
	for _, s := range []string{
		"user prefers tabs for Go files",
		"api_key is read from env var, never hardcoded",
		"the deadline is 2026-11-01",
	} {
		if LooksLikeSecret(s) {
			t.Errorf("LooksLikeSecret(%q) = true, want false", s)
		}
	}
}

func TestParseOps(t *testing.T) {
	if ops := parseOps(`[{"op":"upsert","name":"a","type":"user","description":"d","body":"b"}]`); len(ops) != 1 || ops[0].Name != "a" {
		t.Fatalf("plain array: %+v", ops)
	}
	fenced := "好的，提取结果：\n```json\n[{\"op\":\"delete\",\"name\":\"x\"}]\n```\n结束"
	if ops := parseOps(fenced); len(ops) != 1 || ops[0].Kind != "delete" {
		t.Fatalf("fenced: %+v", ops)
	}
	if parseOps("no json here") != nil {
		t.Fatal("garbage should yield nil")
	}
	if parseOps("not json [oops]") != nil {
		t.Fatal("invalid json should yield nil")
	}
}

// ── Store ──

func TestStoreCRUDAndLog(t *testing.T) {
	m, ctx := newTestMem(t)
	st := m.store

	err := st.ApplyOps(ctx, []Op{{
		Kind: "upsert", Name: "user-likes-go", Type: "user",
		Descr: "prefers Go", Body: "User prefers Go for CLI tools.",
		Entities: []string{"user", "go"},
	}}, "s1")
	if err != nil {
		t.Fatal(err)
	}
	r, err := st.Get(ctx, "user-likes-go")
	if err != nil || r.Body != "User prefers Go for CLI tools." || r.Entities[1] != "go" {
		t.Fatalf("Get: %+v err=%v", r, err)
	}

	// supersede 保留旧值进流水
	err = st.ApplyOps(ctx, []Op{{
		Kind: "supersede", Name: "user-likes-go", Type: "user",
		Descr: "moved to Rust", Body: "User now prefers Rust.", Reason: "switched stacks",
	}}, "s1")
	if err != nil {
		t.Fatal(err)
	}
	r, _ = st.Get(ctx, "user-likes-go")
	if r.Body != "User now prefers Rust." {
		t.Fatalf("supersede did not update body: %q", r.Body)
	}
	hist, err := st.History(ctx, "user-likes-go")
	if err != nil || len(hist) != 2 {
		t.Fatalf("History len=%d err=%v", len(hist), err)
	}
	if hist[0].Op != "supersede" || hist[0].OldBody != "User prefers Go for CLI tools." ||
		hist[0].Reason != "switched stacks" {
		t.Fatalf("supersede log: %+v", hist[0])
	}

	// delete → tombstone
	err = st.ApplyOps(ctx, []Op{{Kind: "delete", Name: "user-likes-go", Reason: "user asked"}}, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Get(ctx, "user-likes-go"); err != ErrNotFound {
		t.Fatalf("Get after delete err=%v, want ErrNotFound", err)
	}
	dels, _ := st.RecentDeletes(ctx, 10)
	if len(dels) != 1 || dels[0] != "user-likes-go" {
		t.Fatalf("RecentDeletes: %v", dels)
	}
	if hist, _ := st.History(ctx, "user-likes-go"); len(hist) != 3 {
		t.Fatalf("History after delete len=%d", len(hist))
	}
}

func TestStoreTenantAndSpaceIsolation(t *testing.T) {
	m, _ := newTestMem(t)
	st := m.store
	writeCtx := ctxFor("t1", "/ws/proj", "s1")
	if err := st.ApplyOps(writeCtx, []Op{{
		Kind: "upsert", Name: "a", Type: "user", Descr: "d", Body: "b",
	}}, "s1"); err != nil {
		t.Fatal(err)
	}
	// 跨租户不可见
	if heads, _ := st.List(ctxFor("t2", "/ws/proj", "s2")); len(heads) != 0 {
		t.Fatal("cross-tenant leak")
	}
	// 同租户不同 workspace 不可见
	if heads, _ := st.List(ctxFor("t1", "/ws/other", "s1")); len(heads) != 0 {
		t.Fatal("cross-space leak")
	}
	if heads, _ := st.List(writeCtx); len(heads) != 1 {
		t.Fatalf("same space should see 1, got %d", len(heads))
	}
}

func TestStoreAboutAndMeta(t *testing.T) {
	m, ctx := newTestMem(t)
	st := m.store
	_ = st.ApplyOps(ctx, []Op{
		{Kind: "upsert", Name: "a", Type: "user", Descr: "d", Body: "b", Entities: []string{"kid", "school"}},
		{Kind: "upsert", Name: "b", Type: "user", Descr: "d", Body: "b", Entities: []string{"kidding"}},
	}, "s1")
	rows, err := st.About(ctx, "kid")
	if err != nil || len(rows) != 1 || rows[0].Name != "a" {
		t.Fatalf("About should match exact entity token, got %+v err=%v", rows, err)
	}
	// meta KV
	if v, _ := st.Meta(ctx, "k"); v != "" {
		t.Fatal("missing meta should be empty")
	}
	_ = st.SetMeta(ctx, "k", "v1")
	if v, _ := st.Meta(ctx, "k"); v != "v1" {
		t.Fatalf("meta=%q", v)
	}
}

func TestLock(t *testing.T) {
	m, ctx := newTestMem(t)
	st := m.store
	unlock, ok, err := st.Lock(ctx, time.Hour)
	if err != nil || !ok {
		t.Fatalf("first lock ok=%v err=%v", ok, err)
	}
	if _, ok, _ := st.Lock(ctx, time.Hour); ok {
		t.Fatal("second lock should fail while held")
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.Lock(ctx, time.Hour); !ok {
		t.Fatal("lock after release should succeed")
	}
	// 过期锁可被抢走
	_ = st.SetMeta(ctx, lockMetaKey, "1") // 1970 年即过期
	if _, ok, _ := st.Lock(ctx, time.Hour); !ok {
		t.Fatal("expired lock should be stealable")
	}
}

// ── 校验 ──

func TestValidateOps(t *testing.T) {
	m, ctx := newTestMem(t)
	_ = m.store.ApplyOps(ctx, []Op{{
		Kind: "upsert", Name: "old-belief", Type: "user", Descr: "d", Body: "likes apples",
	}}, "s1")

	ops := m.validateOps(ctx, []Op{
		{Kind: "upsert", Name: "x", Type: "bogus", Descr: "d", Body: "b"},                    // 类型不在辞典
		{Kind: "supersede", Name: "ghost", Type: "user", Descr: "d", Body: "b", Reason: "r"}, // 目标不存在
		{Kind: "supersede", Name: "old-belief", Type: "user", Descr: "d", Body: "b"},         // 缺 reason
		{Kind: "supersede", Name: "old-belief", Type: "user", Descr: "d", Body: "b", Reason: "changed mind"},
		{Kind: "upsert", Name: "cred", Type: "user", Descr: "d", Body: "api_key = abcdef123456"}, // 密钥
		{Kind: "delete", Name: "nobody"},
		{Kind: "wat", Name: "x"},
	})
	if len(ops) != 1 || ops[0].Kind != "supersede" || ops[0].Name != "old-belief" {
		t.Fatalf("validateOps kept %+v", ops)
	}
}

// ── 注入 ──

func TestInjectOnce(t *testing.T) {
	m, ctx := newTestMem(t)

	// 空空间不注入（但 seen 已记）
	if rep, _ := m.InjectOnce(ctx, "hello"); rep != "" {
		t.Fatalf("empty space should not inject, got %q", rep)
	}
	// 写入一条 AlwaysInject 类型（coding 的 user）与一条非注入类型
	_ = m.store.ApplyOps(ctx, []Op{
		{Kind: "upsert", Name: "whoami", Type: "user", Descr: "backend dev", Body: "User is a backend dev."},
		{Kind: "upsert", Name: "proj-x", Type: "project", Descr: "deadline", Body: "Ship 2026-12-01."},
	}, "s1")

	// 首条消息后变更标记由 dirty 驱动：直接标 dirty 再注入
	m.markDirty(scope.Scope{TenantID: "t1", Workspace: "/ws/proj"})
	rep, _ := m.InjectOnce(ctx, "hello")
	if !strings.Contains(rep, "<memory-index") || !strings.Contains(rep, "whoami") {
		t.Fatalf("inject missing index/name: %q", rep)
	}
	if !strings.Contains(rep, `<memory name="whoami"`) {
		t.Fatalf("user-type body should be pinned: %q", rep)
	}
	if strings.Contains(rep, `<memory name="proj-x"`) {
		t.Fatalf("project-type body must not be pinned: %q", rep)
	}
	// 无变更的第二条消息不再注入
	if rep, _ := m.InjectOnce(ctx, "again"); rep != "" {
		t.Fatalf("second submit should not inject, got %q", rep)
	}
}

// ── dream ──

func TestDreamEndToEnd(t *testing.T) {
	sess := &fakeSess{
		sessions: []session.Session{{ID: "s1", Kind: session.KindMain, Workspace: "/ws/proj"}},
		rows: map[string][]agent.Message{
			"s1": {
				{ID: 1, Role: agent.RoleUser, Content: "I prefer dark themes and Go."},
				{ID: 2, Role: agent.RoleAssistant, Content: "noted"},
				{ID: 3, Kind: agent.KindBoundary, Role: agent.RoleUser, Content: "{}"}, // 控制行应被跳过
			},
		},
	}
	llm := &fakeLLM{replies: []string{
		`[{"op":"upsert","name":"user-prefers-go","type":"user","description":"prefers Go + dark themes","body":"User prefers Go and dark themes.","entities":["user"]}]`,
	}}
	m, err := New(sess, llm, Config{DBPath: ":memory:", Profile: Lookup("coding")})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := ctxFor("t1", "/ws/proj", "s1")

	stats, err := m.Dream(ctx, true)
	if err != nil || stats.Skipped != "" {
		t.Fatalf("Dream: stats=%+v err=%v", stats, err)
	}
	if stats.OpsApplied != 1 || stats.Rows != 2 || stats.Sessions != 1 {
		t.Fatalf("stats=%+v", stats)
	}
	r, err := m.store.Get(ctx, "user-prefers-go")
	if err != nil || r.SourceSess != "s1" {
		t.Fatalf("row=%+v err=%v", r, err)
	}
	// 流水落库
	if hist, _ := m.store.History(ctx, "user-prefers-go"); len(hist) != 1 || hist[0].Sess != "s1" {
		t.Fatalf("history=%+v", hist)
	}
	// 水位推进后无欠账
	stats, _ = m.Dream(ctx, true)
	if stats.Skipped != "no unprocessed rows" {
		t.Fatalf("second dream should be empty, got %+v", stats)
	}
	// 新行续消化：水位按行 id 推进
	sess.rows["s1"] = append(sess.rows["s1"],
		agent.Message{ID: 4, Role: agent.RoleUser, Content: "I hate apples now."})
	stats, _ = m.Dream(ctx, true)
	if stats.Rows != 1 {
		t.Fatalf("expected 1 new row, got %+v", stats)
	}
}
