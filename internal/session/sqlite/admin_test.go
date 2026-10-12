package sqlite

import (
	"context"
	"errors"
	"testing"

	"runeharness/internal/agent"
	"runeharness/internal/session"
)

// seedChat 建一个绑定到 channel:chatID 的会话并写入一条消息，返回 session id。
func seedChat(t *testing.T, s *Store, tenant, channel, chatID, title string) string {
	t.Helper()
	ssn, err := s.CreateSession(ctxFor(tenant, ""), session.Meta{
		Kind: session.KindMain, Title: title, SubjectID: "alice",
	})
	if err != nil {
		t.Fatalf("CreateSession(%s): %v", chatID, err)
	}
	if _, err := s.UpsertBinding(ctxFor(tenant, ""), tenant, channel, chatID, ssn.ID, "alice"); err != nil {
		t.Fatalf("UpsertBinding(%s): %v", chatID, err)
	}
	if _, err := s.Append(ctxFor(tenant, ssn.ID), agent.Message{
		Role: agent.RoleUser, Kind: agent.KindMessage, Content: "hello " + chatID,
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	return ssn.ID
}

// countRows 数表行数，清理事后核对用。
func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestSessionTitleRoundtrip(t *testing.T) {
	s := open(t)
	sid := seedChat(t, s, "t1", "bear", "main", "bear:main")
	ctx := ctxFor("t1", sid)

	got, err := s.SessionTitle(ctx, sid)
	if err != nil || got != "bear:main" {
		t.Fatalf("SessionTitle = %q, %v; want bear:main", got, err)
	}
	if err := s.SetTitle(ctx, sid, "晚饭推荐"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	if got, _ := s.SessionTitle(ctx, sid); got != "晚饭推荐" {
		t.Fatalf("title after SetTitle = %q", got)
	}
	// 越租户写标题必须被拒。
	if err := s.SetTitle(ctxFor("t2", sid), sid, "x"); !errors.Is(err, ErrCrossTenant) {
		t.Fatalf("cross-tenant SetTitle err = %v, want ErrCrossTenant", err)
	}
	if err := s.SetTitle(ctx, "no-such-session", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing session SetTitle err = %v, want ErrNotFound", err)
	}
}

func TestListByTenantCarriesTitle(t *testing.T) {
	s := open(t)
	sid := seedChat(t, s, "t1", "bear", "main", "晚饭推荐")
	seedChat(t, s, "t1", "bear", "smoke-c1", "bear:smoke-c1")

	rows, err := s.ListByTenant(context.Background(), "t1", "bear")
	if err != nil {
		t.Fatalf("ListByTenant: %v", err)
	}
	byChat := map[string]string{}
	for _, r := range rows {
		byChat[r["chat_id"].(string)], _ = r["title"].(string)
	}
	if byChat["main"] != "晚饭推荐" || byChat["smoke-c1"] != "bear:smoke-c1" {
		t.Fatalf("titles = %v", byChat)
	}
	_ = sid
}

func TestPurgeChatsKeepsKeepList(t *testing.T) {
	s := open(t)
	mainSid := seedChat(t, s, "t1", "bear", "main", "bear:main")
	purge1 := seedChat(t, s, "t1", "bear", "smoke-c1", "bear:smoke-c1")
	purge2 := seedChat(t, s, "t1", "bear", "chat_mv37eofa", "bear:chat_mv37eofa")
	// 其他渠道的 chat 不受影响。
	seedChat(t, s, "t1", "qq", "smoke-c2", "bear:smoke-c2")

	// 给待清理会话挂全量表数据，验证连带删除。
	ctx := ctxFor("t1", purge1)
	if err := s.AppendBlob(ctx, "blob-1", "big text"); err != nil {
		t.Fatalf("AppendBlob: %v", err)
	}
	if err := s.PutState(ctx, []byte(`{"k":1}`)); err != nil {
		t.Fatalf("PutState: %v", err)
	}

	purged, err := s.PurgeChats(context.Background(), "t1", "bear", []string{"main"})
	if err != nil {
		t.Fatalf("PurgeChats: %v", err)
	}
	if len(purged) != 2 || purged[0] == purged[1] ||
		(purged[0] != "smoke-c1" && purged[1] != "smoke-c1") ||
		(purged[0] != "chat_mv37eofa" && purged[1] != "chat_mv37eofa") {
		t.Fatalf("purged = %v, want smoke-c1 + chat_mv37eofa", purged)
	}

	rows, err := s.ListByTenant(context.Background(), "t1", "bear")
	if err != nil || len(rows) != 1 || rows[0]["chat_id"] != "main" {
		t.Fatalf("bear rows after purge = %v, %v; want [main]", rows, err)
	}
	want := map[string]int{
		"messages": 2, "sessions": 2, "chat_bindings": 2, // main(bear) + smoke-c2(qq)
		"blobs": 0, "session_state": 0, "requests": 0, "session_leases": 0,
	}
	for table, n := range want {
		if got := countRows(t, s, table); got != n {
			t.Fatalf("%s rows = %d, want %d", table, got, n)
		}
	}
	// main 会话数据完好。
	if _, err := s.LoadRawHistory(ctxFor("t1", mainSid), mainSid); err != nil {
		t.Fatalf("kept session history: %v", err)
	}
	// 幂等：再跑一次，purged 为空、无副作用。
	if purged, err := s.PurgeChats(context.Background(), "t1", "bear", []string{"main"}); err != nil || len(purged) != 0 {
		t.Fatalf("second purge = %v, %v; want empty", purged, err)
	}
	_ = purge2
}

func TestPurgeChatsTenantIsolation(t *testing.T) {
	s := open(t)
	seedChat(t, s, "t1", "bear", "smoke-c1", "bear:smoke-c1")
	otherSid := seedChat(t, s, "t2", "bear", "smoke-c9", "bear:smoke-c9")

	purged, err := s.PurgeChats(context.Background(), "t1", "bear", []string{"main"})
	if err != nil || len(purged) != 1 {
		t.Fatalf("purge = %v, %v", purged, err)
	}
	if _, err := s.LoadRawHistory(ctxFor("t2", otherSid), otherSid); err != nil {
		t.Fatalf("other tenant session must survive: %v", err)
	}
}

func TestPurgeChatsKeepsSharedSession(t *testing.T) {
	s := open(t)
	// 同一 session 绑两个 chat：清掉其一，session 数据必须保留。
	sid := seedChat(t, s, "t1", "bear", "main", "bear:main")
	if _, err := s.UpsertBinding(ctxFor("t1", ""), "t1", "bear", "alias", sid, "alice"); err != nil {
		t.Fatalf("UpsertBinding alias: %v", err)
	}
	if _, err := s.PurgeChats(context.Background(), "t1", "bear", []string{"main"}); err != nil {
		t.Fatalf("PurgeChats: %v", err)
	}
	if _, err := s.LoadRawHistory(ctxFor("t1", sid), sid); err != nil {
		t.Fatalf("shared session must survive: %v", err)
	}
	if n := countRows(t, s, "chat_bindings"); n != 1 {
		t.Fatalf("bindings = %d, want 1", n)
	}
}
