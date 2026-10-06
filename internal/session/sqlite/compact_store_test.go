package sqlite

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"runeharness/internal/agent"
	"runeharness/internal/session"
)

// v1 库升级到 v2：旧消息保留，kind 默认 'msg'（读回为普通消息），新表可用。
func TestMigrateV1ToV2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE sessions (id TEXT PRIMARY KEY, tenant_id TEXT NOT NULL, parent_id TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL DEFAULT 'main', title TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
			workspace TEXT NOT NULL DEFAULT '', depth INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL REFERENCES sessions(id),
			role TEXT NOT NULL, content TEXT NOT NULL DEFAULT '', thinking TEXT NOT NULL DEFAULT '',
			tool_calls TEXT NOT NULL DEFAULT '', tool_call_id TEXT NOT NULL DEFAULT '',
			is_error INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)`,
		`INSERT INTO sessions VALUES ('s1','t1','','main','','','/w',0,1,1)`,
		`INSERT INTO messages (session_id, role, content, created_at) VALUES ('s1','user','old',1)`,
		`PRAGMA user_version = 1`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open v1: %v", err)
	}
	defer s.Close()
	ctx := ctxFor("t1", "s1")
	hist, err := s.LoadHistory(ctx, "s1")
	if err != nil || len(hist) != 1 || hist[0].Content != "old" || hist[0].Kind != agent.KindMessage {
		t.Fatalf("hist = %+v, err = %v", hist, err)
	}
	if err := s.AppendBlob(ctx, "r1", "full"); err != nil {
		t.Fatalf("AppendBlob after migrate: %v", err)
	}
}

// 子会话读 blob：本会话没有时沿 parent_id 链向上找到祖先；越租户被拒。
func TestLoadBlobWalksAncestors(t *testing.T) {
	s := open(t)
	parent := mustCreate(t, s, "t1", session.Meta{Kind: session.KindMain})
	child := mustCreate(t, s, "t1", session.Meta{Kind: session.KindSubagent, ParentID: parent.ID, Depth: 1})
	if err := s.AppendBlob(ctxFor("t1", parent.ID), "p-ref", "parent full"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendBlob(ctxFor("t1", child.ID), "c-ref", "child full"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LoadBlob(ctxFor("t1", child.ID), "p-ref"); err != nil || got != "parent full" {
		t.Fatalf("child reads ancestor blob = %q, %v", got, err)
	}
	// 祖先读后代不开放（v1）
	if _, err := s.LoadBlob(ctxFor("t1", parent.ID), "c-ref"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("parent reads child blob: err = %v, want ErrNotFound", err)
	}
	if _, err := s.LoadBlob(ctxFor("t2", child.ID), "p-ref"); !errors.Is(err, ErrCrossTenant) {
		t.Fatalf("cross-tenant LoadBlob: err = %v", err)
	}
}

func TestStateRoundtrip(t *testing.T) {
	s := open(t)
	ssn := mustCreate(t, s, "t1", session.Meta{})
	ctx := ctxFor("t1", ssn.ID)
	if got, err := s.GetState(ctx); err != nil || string(got) != "{}" {
		t.Fatalf("empty state = %s, %v", got, err)
	}
	for _, v := range []string{`{"compact_failures":1}`, `{"compact_failures":2}`} {
		if err := s.PutState(ctx, json.RawMessage(v)); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := s.GetState(ctx); string(got) != `{"compact_failures":2}` {
		t.Fatalf("state = %s", got)
	}
}

// 请求留痕：消息与 blob 水印都取写入时刻本会话的最大行 id（含控制行——
// 发送形态由消息行和压缩控制行共同决定）。
func TestLogRequest(t *testing.T) {
	s := open(t)
	ssn := mustCreate(t, s, "t1", session.Meta{})
	ctx := ctxFor("t1", ssn.ID)
	if err := s.AppendBlob(ctx, "r1", "x"); err != nil {
		t.Fatal(err)
	}
	id, err := s.Append(ctx, agent.Message{Role: agent.RoleUser, Content: "u"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(ctx, agent.Message{Role: agent.RoleAssistant, Content: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.LogRequest(ctx, agent.RequestLog{ViewHash: "h", Payload: "p", Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	reqs, err := s.LoadRequests(ctx, ssn.ID)
	if err != nil || len(reqs) != 1 {
		t.Fatalf("reqs = %+v, %v", reqs, err)
	}
	r := reqs[0]
	if r.UptoMsgID != id+1 || r.UptoBlobID == 0 || r.ViewHash != "h" || r.Payload != "p" || r.Error != "boom" {
		t.Fatalf("request = %+v", r)
	}
}

func appendAll(t *testing.T, s *Store, sessionID string, msgs ...agent.Message) []int64 {
	t.Helper()
	ids := make([]int64, len(msgs))
	for i, m := range msgs {
		id, err := s.Append(ctxFor("t1", sessionID), m)
		if err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		ids[i] = id
	}
	return ids
}

func control(t *testing.T, kind agent.Kind, meta any) agent.Message {
	t.Helper()
	m, err := session.ControlRow(kind, meta)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func contents(view []agent.Message) []string {
	out := make([]string, len(view))
	for i, m := range view {
		out[i] = m.Content
	}
	return out
}

// Fold：system + 首部 + 最新摘要 + covers_to 之后的消息；卸载替换生效；
// 控制行不进视图；LoadView 按水印重放出压缩前的形态。
func TestFoldBoundaryAndViewClear(t *testing.T) {
	s := open(t)
	ssn := mustCreate(t, s, "t1", session.Meta{})
	ids := appendAll(t, s, ssn.ID,
		agent.Message{Role: agent.RoleSystem, Content: "sys"},
		agent.Message{Role: agent.RoleUser, Content: "task"},
		agent.Message{Role: agent.RoleAssistant, Content: "a1", Usage: &agent.Usage{Prompt: 10}},
		agent.Message{Role: agent.RoleUser, Content: "mid"},
		agent.Message{Role: agent.RoleTool, ToolCallID: "c1", Content: "big result"},
		agent.Message{Role: agent.RoleUser, Content: "tail"},
	)
	beforeCompact := ids[len(ids)-1]
	appendAll(t, s, ssn.ID,
		control(t, agent.KindViewClear, session.ViewClearMeta{Results: map[int64]string{ids[4]: "[offloaded]"}}),
		control(t, agent.KindBoundary, session.BoundaryMeta{HeadIDs: []int64{ids[1]}, CoversTo: ids[4]}),
		agent.Message{Role: agent.RoleUser, Kind: agent.KindSummary, Content: "summary"},
		agent.Message{Role: agent.RoleAssistant, Content: "after", Usage: &agent.Usage{Prompt: 20}},
	)
	view, err := s.LoadHistory(ctxFor("t1", ssn.ID), ssn.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sys", "task", "summary", "tail", "after"}
	if got := contents(view); !slices.Equal(got, want) {
		t.Fatalf("view = %v, want %v", got, want)
	}
	if view[len(view)-1].Usage == nil {
		t.Fatal("控制行之后的 usage 锚点应保留")
	}

	old, err := s.LoadView(ctxFor("t1", ssn.ID), ssn.ID, beforeCompact)
	if err != nil {
		t.Fatal(err)
	}
	if got := contents(old); !slices.Equal(got, []string{"sys", "task", "a1", "mid", "big result", "tail"}) {
		t.Fatalf("replayed view = %v", got)
	}
}

// fork：子会话视图 = 父会话在水印处的视图 + 子会话行；子会话的卸载可指向父会话行。
func TestForkView(t *testing.T) {
	s := open(t)
	parent := mustCreate(t, s, "t1", session.Meta{})
	pids := appendAll(t, s, parent.ID,
		agent.Message{Role: agent.RoleSystem, Content: "sys"},
		agent.Message{Role: agent.RoleUser, Content: "p-user"},
		agent.Message{Role: agent.RoleTool, ToolCallID: "c1", Content: "p-tool"},
	)
	child := mustCreate(t, s, "t1", session.Meta{Kind: session.KindSubagent, ParentID: parent.ID, Depth: 1})
	appendAll(t, s, child.ID,
		control(t, agent.KindFork, session.ForkMeta{ParentSessionID: parent.ID, ParentUptoMsgID: pids[2]}),
		agent.Message{Role: agent.RoleUser, Content: "sub-task"},
		control(t, agent.KindViewClear, session.ViewClearMeta{Results: map[int64]string{pids[2]: "[off]"}}),
	)
	// 父会话 fork 之后的新行不进子视图
	appendAll(t, s, parent.ID, agent.Message{Role: agent.RoleUser, Content: "p-later"})

	view, err := s.LoadHistory(ctxFor("t1", child.ID), child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := contents(view); !slices.Equal(got, []string{"sys", "p-user", "[off]", "sub-task"}) {
		t.Fatalf("child view = %v", got)
	}
}
