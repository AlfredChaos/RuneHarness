package memory

// 边界场景测试：按设计文档 §8.4/§13 的失败面组织——存储原子性、空间矩阵、
// 注入配额、ops 校验、dream 门与游标、工具行为。

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"runeharness/internal/agent"
	"runeharness/internal/scope"
	"runeharness/internal/session"
)

func ctxSub(tenant, subject, sess string) context.Context {
	return scope.WithScope(context.Background(),
		scope.Scope{TenantID: tenant, SubjectID: subject, SessionID: sess})
}

// upsertOps 批量造 upsert ops（测试夹具）。
func upsertOps(prefix string, n int, typ, body string) []Op {
	ops := make([]Op, n)
	for i := range ops {
		ops[i] = Op{
			Kind: "upsert", Name: fmt.Sprintf("%s-%d", prefix, i),
			Type: typ, Descr: "d" + strconv.Itoa(i), Body: body,
		}
	}
	return ops
}

// ── 存储：原子性与隔离矩阵 ──

// 事务原子性：一批里夹一条非法 op，整批回滚——行与流水都不落。
func TestApplyOpsAtomicity(t *testing.T) {
	m, ctx := newTestMem(t)
	err := m.store.ApplyOps(ctx, []Op{
		{Kind: "upsert", Name: "a", Type: "user", Descr: "d", Body: "b"},
		{Kind: "bogus", Name: "c"},
		{Kind: "upsert", Name: "b", Type: "user", Descr: "d", Body: "b"},
	}, "s1")
	if err == nil {
		t.Fatal("batch with unknown op should fail")
	}
	if heads, _ := m.store.List(ctx); len(heads) != 0 {
		t.Fatalf("partial batch leaked %d rows", len(heads))
	}
	if hist, _ := m.store.History(ctx, "a"); len(hist) != 0 {
		t.Fatalf("log rows leaked: %d", len(hist))
	}
}

// tombstone 复活修正：删除后重建的名字不应再出现在删除清单里。
func TestRecentDeletesResurrection(t *testing.T) {
	m, ctx := newTestMem(t)
	_ = m.store.ApplyOps(ctx, []Op{
		{Kind: "upsert", Name: "x", Type: "user", Descr: "d", Body: "b"},
		{Kind: "delete", Name: "x", Reason: "asked"},
	}, "s1")
	dels, _ := m.store.RecentDeletes(ctx, 10)
	if len(dels) != 1 || dels[0] != "x" {
		t.Fatalf("deleted name should be tombstoned: %v", dels)
	}
	_ = m.store.ApplyOps(ctx, []Op{
		{Kind: "upsert", Name: "x", Type: "user", Descr: "d2", Body: "recreated"},
	}, "s1")
	if dels, _ := m.store.RecentDeletes(ctx, 10); len(dels) != 0 {
		t.Fatalf("resurrected name still tombstoned: %v", dels)
	}
}

// subject 空间按 SubjectID 隔离，跨 workspace 共享（companion 语义）。
func TestSubjectSpaceMatrix(t *testing.T) {
	m, err := New(nil, nil, Config{DBPath: ":memory:", Profile: Lookup("companion")})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	// u1 在 workspace /a 写的记忆，u1 在 workspace /b 也可见（同人不同目录）
	wsA := ctxSub("t1", "u1", "s1")
	wsB := ctxSub("t1", "u1", "s2")
	_ = m.store.ApplyOps(wsA, []Op{
		{Kind: "upsert", Name: "p1", Type: "persona", Descr: "d", Body: "b"},
	}, "s1")
	if heads, _ := m.store.List(wsB); len(heads) != 1 {
		t.Fatalf("subject space should span workspaces, got %d", len(heads))
	}
	// u2 完全隔离
	if heads, _ := m.store.List(ctxSub("t1", "u2", "s3")); len(heads) != 0 {
		t.Fatal("cross-subject leak")
	}
}

// tenant 空间：同租户全部 workspace 共享（团队共享记忆形态）。
func TestTenantSpaceShared(t *testing.T) {
	prof := Lookup("coding")
	prof.Name, prof.Space = "team", SpaceTenant
	m, err := New(nil, nil, Config{DBPath: ":memory:", Profile: prof})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	_ = m.store.ApplyOps(ctxFor("t1", "/ws/a", "s1"), []Op{
		{Kind: "upsert", Name: "x", Type: "project", Descr: "d", Body: "b"},
	}, "s1")
	if heads, _ := m.store.List(ctxFor("t1", "/ws/b", "s2")); len(heads) != 1 {
		t.Fatal("tenant space should be visible across workspaces")
	}
	if heads, _ := m.store.List(ctxFor("t2", "/ws/a", "s3")); len(heads) != 0 {
		t.Fatal("cross-tenant leak in shared space")
	}
}

// About 的 LIKE 通配符转义：实体名带 %/_ 不应放大匹配面。
func TestAboutEscapesWildcards(t *testing.T) {
	m, ctx := newTestMem(t)
	_ = m.store.ApplyOps(ctx, []Op{
		{Kind: "upsert", Name: "a", Type: "user", Descr: "d", Body: "b", Entities: []string{"50_off"}},
		{Kind: "upsert", Name: "b", Type: "user", Descr: "d", Body: "b", Entities: []string{"500off"}},
	}, "s1")
	rows, err := m.store.About(ctx, "50%off")
	if err != nil || len(rows) != 0 {
		t.Fatalf("%%-wildcard should match nothing, got %d err=%v", len(rows), err)
	}
	if rows, _ := m.store.About(ctx, "50_off"); len(rows) != 1 {
		t.Fatalf("underscore entity should match exactly, got %d", len(rows))
	}
}

// ── 注入配额 ──

// 索引超长截断：条目多到顶配额时必须留截断标记而不是爆量。
func TestInjectIndexTruncation(t *testing.T) {
	m, ctx := newTestMem(t)
	long := strings.Repeat("d", 60)
	ops := upsertOps("entry", 150, "project", "b")
	for i := range ops {
		ops[i].Descr = long
	}
	if err := m.store.ApplyOps(ctx, ops, "s1"); err != nil {
		t.Fatal(err)
	}
	block, err := m.renderInject(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(block, "index truncated") {
		t.Fatal("oversized index must carry truncation marker")
	}
}

// 常驻正文预算：总量到 MaxBodyBytes 后截断后续，单条超 4KB 先压缩。
func TestInjectBodyBudget(t *testing.T) {
	m, ctx := newTestMem(t)
	// 5 条 user 型（AlwaysInject），每条 4000B：预算 16384 只放得下 4 条。
	// 同批 updated_at 相同，命中哪 4 条不定——只断言块数。
	_ = m.store.ApplyOps(ctx, upsertOps("big", 5, "user", strings.Repeat("x", 4000)), "s1")
	block, _ := m.renderInject(ctx)
	if n := strings.Count(block, "<memory name="); n != 4 {
		t.Fatalf("body budget should fit exactly 4 blocks, got %d", n)
	}
}

// 辞典外类型兜底：索引分组顺序后仍能看到，不会丢。
func TestInjectUnknownTypeFallback(t *testing.T) {
	m, ctx := newTestMem(t)
	_ = m.store.ApplyOps(ctx, []Op{
		{Kind: "upsert", Name: "exotic", Type: "legacy", Descr: "d", Body: "b"},
	}, "s1")
	block, _ := m.renderInject(ctx)
	if !strings.Contains(block, "[legacy] exotic") {
		t.Fatalf("unknown-type memory should still appear in index: %q", block)
	}
}

// ctx 无 scope 时注入静默不动作（装配遗漏不该炸掉输入链）。
func TestInjectNoScope(t *testing.T) {
	m, _ := newTestMem(t)
	if rep, blocked := m.InjectOnce(context.Background(), "hi"); rep != "" || blocked != "" {
		t.Fatal("no-scope ctx must not inject")
	}
}

// ── dream 门 ──

func seedSession(sessID string, n int) *fakeSess {
	sess := &fakeSess{
		sessions: []session.Session{{ID: sessID, Kind: session.KindMain, Workspace: "/ws/proj"}},
		rows:     map[string][]agent.Message{},
	}
	for i := 1; i <= n; i++ {
		sess.rows[sessID] = append(sess.rows[sessID],
			agent.Message{ID: int64(i), Role: agent.RoleUser, Content: fmt.Sprintf("turn %d", i)})
	}
	return sess
}

// 自动 dream 的三道门：冷却、熔断、物料；手动只过锁。
func TestDreamGates(t *testing.T) {
	sess := seedSession("s1", 12)
	llm := &fakeLLM{replies: []string{`[]`}}
	m, err := New(sess, llm, Config{DBPath: ":memory:", Profile: Lookup("coding")})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := ctxFor("t1", "/ws/proj", "cur")

	// 冷却：刚写过 last_dream_at，自动路径被拦
	_ = m.store.SetMeta(ctx, metaLastDream, time.Now().Format(time.RFC3339Nano))
	if st, _ := m.Dream(ctx, false); st.Skipped != "cooldown" {
		t.Fatalf("cooldown gate: %+v", st)
	}
	// 手动绕过冷却与物料
	if st, err := m.Dream(ctx, true); err != nil || st.Skipped != "" {
		t.Fatalf("manual should pass gates: %+v err=%v", st, err)
	}

	// 熔断：连续失败 3 次后自动路径被拦；手动仍可跑。
	// 注意语义：欠账为空时提前返回不算"成功"，不清熔断计数——
	// 所以先补行再验证清零。
	_ = m.store.SetMeta(ctx, metaFailures, "3")
	if st, _ := m.Dream(ctx, false); !strings.Contains(st.Skipped, "breaker") {
		t.Fatalf("breaker gate: %+v", st)
	}
	for i := 13; i <= 16; i++ {
		sess.rows["s1"] = append(sess.rows["s1"],
			agent.Message{ID: int64(i), Role: agent.RoleUser, Content: "more"})
	}
	if _, err := m.Dream(ctx, true); err != nil {
		t.Fatal(err)
	}
	if v, _ := m.store.Meta(ctx, metaFailures); v != "" {
		t.Fatalf("success should reset failures, got %q", v)
	}
}

// 物料门：欠账不足 MinRows 时自动跳过，手动照常。
func TestDreamMinRowsGate(t *testing.T) {
	sess := seedSession("s1", 3) // coding MinRows=8，3 行不够
	m, err := New(sess, &fakeLLM{replies: []string{`[]`}},
		Config{DBPath: ":memory:", Profile: Lookup("coding")})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := ctxFor("t1", "/ws/proj", "cur")
	if st, _ := m.Dream(ctx, false); !strings.Contains(st.Skipped, "unprocessed") {
		t.Fatalf("min-rows gate: %+v", st)
	}
	if st, _ := m.Dream(ctx, true); st.Skipped != "" {
		t.Fatalf("manual bypasses min-rows: %+v", st)
	}
}

// 锁互斥：持有锁期间任何 dream（含手动）跳过。
func TestDreamLockExclusion(t *testing.T) {
	sess := seedSession("s1", 12)
	m, err := New(sess, &fakeLLM{}, Config{DBPath: ":memory:", Profile: Lookup("coding")})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := ctxFor("t1", "/ws/proj", "cur")
	unlock, ok, _ := m.store.Lock(ctx, time.Hour)
	if !ok {
		t.Fatal("acquire")
	}
	if st, _ := m.Dream(ctx, true); st.Skipped != "another dream is running" {
		t.Fatalf("held lock should skip dream: %+v", st)
	}
	_ = unlock()
	if st, _ := m.Dream(ctx, true); st.Skipped == "another dream is running" {
		t.Fatal("released lock should admit dream")
	}
}

// ── dream 游标与失败恢复 ──

// 批间失败：s1 消化完成、s2 提取炸掉——水位只进 s1；修好后续跑只消化 s2。
func TestDreamPartialFailureResume(t *testing.T) {
	sess := &fakeSess{
		sessions: []session.Session{
			{ID: "s1", Kind: session.KindMain, Workspace: "/ws/proj"},
			{ID: "s2", Kind: session.KindMain, Workspace: "/ws/proj"},
		},
		rows: map[string][]agent.Message{
			"s1": {{ID: 1, Role: agent.RoleUser, Content: "a"}, {ID: 2, Role: agent.RoleUser, Content: "b"}},
			"s2": {{ID: 3, Role: agent.RoleUser, Content: "c"}, {ID: 4, Role: agent.RoleUser, Content: "d"}},
		},
	}
	llm := &fakeLLM{
		failAt: 2,
		replies: []string{
			`[{"op":"upsert","name":"from-s1","type":"user","description":"d","body":"b"}]`,
			"", // s2 的第一次调用：failAt 命中
			`[{"op":"upsert","name":"from-s2","type":"user","description":"d","body":"b"}]`,
		},
	}
	m, err := New(sess, llm, Config{DBPath: ":memory:", Profile: Lookup("coding")})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := ctxFor("t1", "/ws/proj", "cur")

	st, err := m.Dream(ctx, true)
	if err == nil {
		t.Fatal("expected dream failure on second session")
	}
	if _, e := m.store.Get(ctx, "from-s1"); e != nil {
		t.Fatal("s1 ops should have been applied")
	}
	if cur, _ := m.store.Meta(ctx, cursorPrefix+"s1"); cur != "2" {
		t.Fatalf("s1 cursor should be 2, got %q", cur)
	}
	if cur, _ := m.store.Meta(ctx, cursorPrefix+"s2"); cur != "" {
		t.Fatalf("s2 cursor should be empty, got %q", cur)
	}
	if v, _ := m.store.Meta(ctx, metaFailures); v != "1" {
		t.Fatalf("failure should bump breaker count, got %q", v)
	}

	llm.failAt = 0 // 修好端点
	st, err = m.Dream(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, e := m.store.Get(ctx, "from-s2"); e != nil {
		t.Fatal("s2 ops should be applied on resume")
	}
	// s1 的行已被水位过滤，不再消化——stats 只记 s2
	if st.Rows != 2 {
		t.Fatalf("resume should only digest s2's 2 rows, got %+v", st)
	}
}

// 水位随库持久：关掉记忆库重开（模拟进程重启），欠账不重复消化。
func TestDreamCursorPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "mem.db")
	sess := seedSession("s1", 10)

	m1, err := New(sess, &fakeLLM{replies: []string{
		`[{"op":"upsert","name":"n1","type":"user","description":"d","body":"b"}]`,
	}}, Config{DBPath: db, Profile: Lookup("coding")})
	if err != nil {
		t.Fatal(err)
	}
	ctx := ctxFor("t1", "/ws/proj", "cur")
	if st, err := m1.Dream(ctx, true); err != nil || st.OpsApplied != 1 {
		t.Fatalf("first dream: %+v err=%v", st, err)
	}
	m1.Close()

	// 新实例、同库文件：水位仍在，无欠账
	m2, err := New(sess, &fakeLLM{}, Config{DBPath: db, Profile: Lookup("coding")})
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	if st, _ := m2.Dream(ctx, true); st.Skipped != "no unprocessed rows" {
		t.Fatalf("reopened db must keep cursor: %+v", st)
	}
	if r, err := m2.store.Get(ctx, "n1"); err != nil || r.Body != "b" {
		t.Fatalf("memory should persist across reopen: %+v err=%v", r, err)
	}
}

// tombstone 进提取提示：删除的记忆名应出现在 dream 的 system prompt 里。
func TestDreamTombstoneInPrompt(t *testing.T) {
	sess := seedSession("s1", 10)
	llm := &fakeLLM{replies: []string{`[]`}}
	m, err := New(sess, llm, Config{DBPath: ":memory:", Profile: Lookup("coding")})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := ctxFor("t1", "/ws/proj", "cur")

	_ = m.store.ApplyOps(ctx, []Op{
		{Kind: "upsert", Name: "secret-habit", Type: "user", Descr: "d", Body: "b"},
		{Kind: "upsert", Name: "keep-me", Type: "user", Descr: "d", Body: "b"},
		{Kind: "delete", Name: "secret-habit", Reason: "asked"},
	}, "s1")

	if _, err := m.Dream(ctx, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(llm.lastSys, "secret-habit") ||
		!strings.Contains(llm.lastSys, "keep-me") {
		t.Fatalf("extract prompt should carry manifest + tombstone:\n%s", llm.lastSys)
	}
	if !strings.Contains(llm.lastSys, "<memory-index>") {
		t.Fatal("extract prompt must warn that injected blocks are not user speech")
	}
}

// 整理段：单次 dream 改动 ≥10 触发 tidy，模型给 merge-into 后被并入删除。
func TestDreamTidyMerge(t *testing.T) {
	ops := make([]Op, 12)
	for i := range ops {
		ops[i] = Op{Kind: "upsert", Name: fmt.Sprintf("m%d", i),
			Type: "project", Descr: "d", Body: fmt.Sprintf("body %d", i)}
	}
	raw, _ := json.Marshal(ops)
	llm := &fakeLLM{replies: []string{
		string(raw), // 提取：12 条 upsert → OpsApplied=12 ≥ tidyMinOps
		`[{"op":"supersede","name":"m0","type":"project","description":"merged","body":"m0+m1 merged","reason":"duplicates"},
		  {"op":"merge-into","name":"m1","target":"m0","reason":"duplicate of m0"}]`, // 整理
	}}
	m, err := New(seedSession("s1", 12), llm,
		Config{DBPath: ":memory:", Profile: Lookup("coding")})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := ctxFor("t1", "/ws/proj", "cur")

	st, err := m.Dream(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if llm.calls != 2 {
		t.Fatalf("tidy should make a second LLM call, got %d", llm.calls)
	}
	if st.TidyMerged != 1 {
		t.Fatalf("TidyMerged=%d", st.TidyMerged)
	}
	if _, err := m.store.Get(ctx, "m1"); err != ErrNotFound {
		t.Fatal("merged source should be deleted")
	}
	if r, _ := m.store.Get(ctx, "m0"); r.Body != "m0+m1 merged" {
		t.Fatalf("merge target body: %q", r.Body)
	}
	// merge-into 也是 tombstone——来源名不能复活
	if dels, _ := m.store.RecentDeletes(ctx, 10); !slices.Contains(dels, "m1") {
		t.Fatalf("merged source should be tombstoned: %v", dels)
	}
}

// 工作区过滤：workspace 画像只消化本目录的会话；subject 画像跨目录。
func TestDreamWorkspaceFilter(t *testing.T) {
	sess := &fakeSess{
		sessions: []session.Session{
			{ID: "s1", Kind: session.KindMain, Workspace: "/ws/proj"},
			{ID: "s2", Kind: session.KindMain, Workspace: "/ws/other"},
		},
		rows: map[string][]agent.Message{
			"s1": {{ID: 1, Role: agent.RoleUser, Content: "a"}},
			"s2": {{ID: 2, Role: agent.RoleUser, Content: "b"}},
		},
	}
	m, err := New(sess, &fakeLLM{replies: []string{`[]`}},
		Config{DBPath: ":memory:", Profile: Lookup("coding")})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := ctxFor("t1", "/ws/proj", "cur")
	if st, _ := m.Dream(ctx, true); st.Sessions != 1 || st.Rows != 1 {
		t.Fatalf("workspace space must skip other-dir sessions: %+v", st)
	}

	// companion（subject 空间）：两个 workspace 的会话都消化
	m2, err := New(sess, &fakeLLM{replies: []string{`[]`, `[]`}},
		Config{DBPath: ":memory:", Profile: Lookup("companion")})
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	if st, _ := m2.Dream(ctxSub("t1", "u1", "cur"), true); st.Sessions != 2 {
		t.Fatalf("subject space should digest all sessions, got %+v", st)
	}
}

// 退出收尾实际跑通：DreamOnExit 手动路径消化欠账。
func TestDreamOnExitRuns(t *testing.T) {
	sess := seedSession("s1", 10)
	m, err := New(sess, &fakeLLM{replies: []string{
		`[{"op":"upsert","name":"n1","type":"user","description":"d","body":"b"}]`,
	}}, Config{DBPath: ":memory:", Profile: Lookup("coding")})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := ctxFor("t1", "/ws/proj", "cur")
	m.DreamOnExit(ctx)
	if _, err := m.store.Get(ctx, "n1"); err != nil {
		t.Fatal("exit dream should have digested arrears")
	}
}

// OnChange 只在接受产出时回调；空跑/被门拦不吵 UI。
func TestOnChangeOnlyOnWrites(t *testing.T) {
	fired := 0
	sess := seedSession("s1", 10)
	m, err := New(sess, &fakeLLM{replies: []string{
		`[{"op":"upsert","name":"n1","type":"user","description":"d","body":"b"}]`,
	}}, Config{DBPath: ":memory:", Profile: Lookup("coding")})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	m.OnChange = func(Stats) { fired++ }
	ctx := ctxFor("t1", "/ws/proj", "cur")
	if _, err := m.Dream(ctx, true); err != nil {
		t.Fatal(err)
	}
	if fired != 1 {
		t.Fatalf("OnChange fired %d times", fired)
	}
	if _, err := m.Dream(ctx, true); err != nil { // 欠账空 → skipped，不回调
		t.Fatal(err)
	}
	if fired != 1 {
		t.Fatalf("skipped dream must not fire OnChange, fired=%d", fired)
	}
}

// ── 工具 ──

func TestToolRoundtrip(t *testing.T) {
	m, ctx := newTestMem(t)
	tool := m.Tool()
	run := func(args string) (string, error) {
		return tool.Run(ctx, json.RawMessage(args))
	}
	if _, err := run(`{"op":"save","name":"User Prefs","type":"user","description":"prefs","body":"likes tabs","entities":["user"]}`); err != nil {
		t.Fatal(err)
	}
	if out, _ := run(`{"op":"read","name":"user-prefs"}`); !strings.Contains(out, "likes tabs") {
		t.Fatalf("read: %q", out)
	}
	if out, _ := run(`{"op":"list"}`); !strings.Contains(out, "user-prefs") {
		t.Fatalf("list: %q", out)
	}
	if out, _ := run(`{"op":"about","entity":"user"}`); !strings.Contains(out, "user-prefs") {
		t.Fatalf("about: %q", out)
	}
	if out, _ := run(`{"op":"history","name":"user-prefs"}`); !strings.Contains(out, "upsert") {
		t.Fatalf("history: %q", out)
	}
	if out, _ := run(`{"op":"forget","name":"user-prefs","reason":"done"}`); out != "forgotten: user-prefs" {
		t.Fatalf("forget: %q", out)
	}
	// 软失败：不存在的记忆不报错，返回可读提示
	if out, _ := run(`{"op":"read","name":"ghost"}`); !strings.Contains(out, "no such memory") {
		t.Fatalf("read missing: %q", out)
	}
	if out, _ := run(`{"op":"forget","name":"ghost"}`); !strings.Contains(out, "no such memory") {
		t.Fatalf("forget missing: %q", out)
	}
	// 非法输入硬失败
	for _, args := range []string{
		`{"op":"nope"}`,
		`{"op":"save","name":"","type":"user","description":"d","body":"b"}`,
		`{"op":"save","name":"x","type":"bogus","description":"d","body":"b"}`,
		`{"op":"save","name":"x","type":"user","description":"d"}`,
		`{"op":"save","name":"x","type":"user","description":"d","body":"api_key = abcdef123456"}`,
	} {
		if _, err := run(args); err == nil {
			t.Fatalf("args %s should fail", args)
		}
	}
	// 空空间不注入：刚才已 forget 干净
	if rep, _ := m.InjectOnce(ctxFor("t1", "/ws/proj", "s2"), "hi"); rep != "" {
		t.Fatalf("empty space must not inject, got %q", rep)
	}
	// 工具写走 dirty → 其他会话的首条消息带新索引
	if _, err := run(`{"op":"save","name":"x","type":"user","description":"d","body":"b"}`); err != nil {
		t.Fatal(err)
	}
	if rep, _ := m.InjectOnce(ctxFor("t1", "/ws/proj", "s3"), "hi"); !strings.Contains(rep, "<memory-index") {
		t.Fatal("first submit of a new session should inject index")
	}
}

// companion 关写工具：Tool() 返回 nil，不注册。
func TestToolAbsentForCompanion(t *testing.T) {
	m, err := New(nil, nil, Config{DBPath: ":memory:", Profile: Lookup("companion")})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if m.Tool() != nil {
		t.Fatal("companion profile must not expose the memory tool")
	}
}

// 超长 body 拒收、超长 descr 截断——两条写路径共用配额。
func TestContentCaps(t *testing.T) {
	m, ctx := newTestMem(t)
	tool := m.Tool()

	// 工具路径：8KB+ body 被拒
	big := strings.Repeat("x", maxBodyBytes+1)
	args, _ := json.Marshal(toolArgs{Op: "save", Name: "x", Type: "user",
		Descr: "d", Body: big})
	if _, err := tool.Run(ctx, args); err == nil {
		t.Fatal("oversized body must be refused")
	}
	// dream 路径同样拦
	if ops := m.validateOps(ctx, []Op{{Kind: "upsert", Name: "x", Type: "user",
		Descr: "d", Body: big}}); len(ops) != 0 {
		t.Fatal("oversized body must be rejected in dream ops too")
	}
	// descr 截断（不拒收）
	long := strings.Repeat("d", maxDescrRunes+50)
	ops := m.validateOps(ctx, []Op{{Kind: "upsert", Name: "x", Type: "user",
		Descr: long, Body: "b"}})
	if len(ops) != 1 || len([]rune(ops[0].Descr)) != maxDescrRunes {
		t.Fatalf("descr should be truncated to %d runes", maxDescrRunes)
	}
}
