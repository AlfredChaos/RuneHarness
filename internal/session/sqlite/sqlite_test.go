package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"runeharness/internal/agent"
	"runeharness/internal/scope"
	"runeharness/internal/session"
)

// ctxFor 造一个带租户/会话的 ctx；sessionID 为空表示仅租户（建会话场景）。
func ctxFor(tenant, sessionID string) context.Context {
	return scope.WithScope(context.Background(),
		scope.Scope{TenantID: tenant, SessionID: sessionID, Workspace: "/w"})
}

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustCreate(t *testing.T, s *Store, tenant string, meta session.Meta) session.Session {
	t.Helper()
	ssn, err := s.CreateSession(ctxFor(tenant, ""), meta)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return ssn
}

// 写后读：role/content/thinking/tool_calls/tool_call_id/is_error 全部 roundtrip。
func TestAppendLoadRoundtrip(t *testing.T) {
	s := open(t)
	ssn := mustCreate(t, s, "t1", session.Meta{Kind: session.KindMain})
	ctx := ctxFor("t1", ssn.ID)

	want := []agent.Message{
		{Role: agent.RoleSystem, Content: "sys"},
		{Role: agent.RoleUser, Content: "hi"},
		{Role: agent.RoleAssistant, Content: "calling", Thinking: "hmm",
			ToolCalls: []agent.ToolCall{{ID: "c1", Name: "read_file",
				Arguments: json.RawMessage(`{"path":"a.go"}`)}}},
		{Role: agent.RoleTool, ToolCallID: "c1", Content: "data", IsError: true},
	}
	want[2].Usage = &agent.Usage{Prompt: 120, Completion: 30, Cached: 64}
	for i, m := range want {
		id, err := s.Append(ctx, m)
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		want[i].ID = id // Append 返回分配的行 id，读回时一并带出
	}
	got, err := s.LoadHistory(ctx, ssn.ID)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	// CreatedAt 是落库时刻的物化列——断言比对时抹掉，不参与语义相等。
	for i := range got {
		got[i].CreatedAt = 0
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// 租户隔离：B 租户对 A 的会话读/写/列全部被拒。
func TestTenantIsolation(t *testing.T) {
	s := open(t)
	a := mustCreate(t, s, "A", session.Meta{})

	evil := ctxFor("B", a.ID)
	if _, err := s.LoadHistory(evil, a.ID); !errors.Is(err, ErrCrossTenant) {
		t.Fatalf("LoadHistory cross-tenant: got %v, want ErrCrossTenant", err)
	}
	if _, err := s.Append(evil, agent.Message{Role: agent.RoleUser, Content: "x"}); !errors.Is(err, ErrCrossTenant) {
		t.Fatalf("Append cross-tenant: got %v, want ErrCrossTenant", err)
	}
	list, err := s.ListSessions(ctxFor("B", ""), 0)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("tenant B sees %d sessions, want 0", len(list))
	}
}

// 多会话交错写入：每个会话内 id 序即写入序。
func TestAppendOrder(t *testing.T) {
	s := open(t)
	s1 := mustCreate(t, s, "t1", session.Meta{})
	s2 := mustCreate(t, s, "t1", session.Meta{})

	// 交错写两个会话，模拟主代理与子代理并发
	for i := 0; i < 5; i++ {
		for _, id := range []string{s1.ID, s2.ID} {
			_, err := s.Append(ctxFor("t1", id), agent.Message{
				Role: agent.RoleUser, Content: id[:8] + string(rune('a'+i)),
			})
			if err != nil {
				t.Fatalf("Append: %v", err)
			}
		}
	}
	for _, id := range []string{s1.ID, s2.ID} {
		hist, err := s.LoadHistory(ctxFor("t1", id), id)
		if err != nil {
			t.Fatalf("LoadHistory: %v", err)
		}
		if len(hist) != 5 {
			t.Fatalf("session %s has %d msgs, want 5", id[:8], len(hist))
		}
		for i, m := range hist {
			if want := id[:8] + string(rune('a'+i)); m.Content != want {
				t.Fatalf("msg %d = %q, want %q", i, m.Content, want)
			}
		}
	}
}

// 并发写不丢序、不报 BUSY（单写者连接串行化）。
func TestConcurrentAppend(t *testing.T) {
	s := open(t)
	s1 := mustCreate(t, s, "t1", session.Meta{})
	s2 := mustCreate(t, s, "t1", session.Meta{})

	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for _, id := range []string{s1.ID, s2.ID} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if _, err := s.Append(ctxFor("t1", id), agent.Message{Role: agent.RoleUser}); err != nil {
					errs <- err
				}
			}
		}(id)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Append: %v", err)
	}
	for _, id := range []string{s1.ID, s2.ID} {
		hist, _ := s.LoadHistory(ctxFor("t1", id), id)
		if len(hist) != 20 {
			t.Fatalf("session %s has %d msgs, want 20", id[:8], len(hist))
		}
	}
}

// 子会话字段（parent/kind/depth）与首条 user 消息的 title 回填。
func TestSubSessionMetaAndTitle(t *testing.T) {
	s := open(t)
	parent := mustCreate(t, s, "t1", session.Meta{Kind: session.KindMain})
	child := mustCreate(t, s, "t1", session.Meta{
		ParentID: parent.ID, Kind: session.KindSubagent, Depth: 1, Title: "task x",
	})
	if child.ParentID != parent.ID || child.Depth != 1 || child.Kind != session.KindSubagent {
		t.Fatalf("child meta not persisted: %+v", child)
	}
	ctx := ctxFor("t1", parent.ID)
	if _, err := s.Append(ctx, agent.Message{Role: agent.RoleUser,
		Content: "这是一个很长的用户消息，用来验证标题会被截断成前四十个字符左右的内容"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	list, _ := s.ListSessions(ctxFor("t1", ""), 0)
	byID := map[string]session.Session{}
	for _, x := range list {
		byID[x.ID] = x
	}
	if got := byID[parent.ID].Title; got == "" || len([]rune(got)) > 41 {
		t.Fatalf("title = %q", got)
	}
	if byID[child.ID].Title != "task x" {
		t.Fatalf("child title overwritten: %q", byID[child.ID].Title)
	}
}

// 失败路径：不存在的会话、缺 scope、缺 session id。
func TestFailureModes(t *testing.T) {
	s := open(t)
	if _, err := s.Append(ctxFor("t1", "nonexistent"), agent.Message{Role: agent.RoleUser}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Append unknown session: got %v, want ErrNotFound", err)
	}
	if _, err := s.Append(context.Background(), agent.Message{}); err == nil {
		t.Fatal("Append without scope: want error")
	}
	if _, err := s.Append(ctxFor("t1", ""), agent.Message{}); !errors.Is(err, ErrNoSession) {
		t.Fatalf("Append without session id: got %v, want ErrNoSession", err)
	}
	if _, err := s.LoadHistory(ctxFor("t1", ""), "nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LoadHistory unknown: got %v, want ErrNotFound", err)
	}
}

// UUIDv7：单调时间序，可解析出创建时间。
func TestUUIDv7Ordering(t *testing.T) {
	s := open(t)
	s1 := mustCreate(t, s, "t1", session.Meta{})
	time.Sleep(2 * time.Millisecond)
	s2 := mustCreate(t, s, "t1", session.Meta{})
	if !(s1.ID < s2.ID) {
		t.Fatalf("uuidv7 not ordered: %s !< %s", s1.ID, s2.ID)
	}
	list, _ := s.ListSessions(ctxFor("t1", ""), 1)
	if len(list) != 1 || list[0].ID != s2.ID {
		t.Fatalf("latest = %+v, want %s", list, s2.ID)
	}
}

// 迁移幂等：同文件二次打开不重复建表。
func TestMigrateIdempotent(t *testing.T) {
	path := t.TempDir() + "/s.db"
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s1.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
}

// M8 resume 全链路：写出 → 关库 → 重开 → 按最近活跃选中 → 回放 → 续写，
// 续写消息 id 接续原有自增序。
func TestResumeAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/s.db"

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ssn := mustCreate(t, s1, "t1", session.Meta{Kind: session.KindMain, Model: "m"})
	ctx := ctxFor("t1", ssn.ID)
	for _, c := range []string{"sys", "hi", "answer"} {
		if _, err := s1.Append(ctx, agent.Message{Role: agent.RoleUser, Content: c}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	s1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	recent, err := s2.ListSessions(ctxFor("t1", ""), 1)
	if err != nil || len(recent) != 1 || recent[0].ID != ssn.ID {
		t.Fatalf("ListSessions = %+v, err=%v; want %s", recent, err, ssn.ID)
	}
	hist, err := s2.LoadHistory(ctxFor("t1", ssn.ID), ssn.ID)
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	if len(hist) != 3 || hist[2].Content != "answer" {
		t.Fatalf("hist = %+v", hist)
	}
	if _, err := s2.Append(ctx, agent.Message{Role: agent.RoleUser, Content: "more"}); err != nil {
		t.Fatalf("resume Append: %v", err)
	}
	hist, _ = s2.LoadHistory(ctxFor("t1", ssn.ID), ssn.ID)
	if len(hist) != 4 || hist[3].Content != "more" {
		t.Fatalf("resumed hist = %+v", hist)
	}
}
