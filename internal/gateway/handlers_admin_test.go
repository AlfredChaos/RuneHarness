// 管理与通知端点的 handler 测试：fake 能力面，验证鉴权、契约形状与
// nil 能力面的 501。命名端点用真 Namer + fake 依赖，验证端到端映射。
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"runeharness/internal/agent"
	"runeharness/internal/cron"
	"runeharness/internal/naming"
	"runeharness/internal/runner"
	"runeharness/internal/session/sqlite"
	"runeharness/internal/tools"
)

func adminServer(t *testing.T) (*httptest.Server, *fakePurger, *fakeNotifier) {
	t.Helper()
	toks, err := StaticTokens("tk1=local:alice")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(&runner.Manager{}, StaticAuth(toks), &fakeStore{msgs: map[string][]agent.Message{}}, "bear")
	p := &fakePurger{}
	n := &fakeNotifier{}
	srv.Purger = p
	srv.Notifier = n
	srv.Schedules = func(tenant string) []cron.Task {
		return []cron.Task{
			{ID: "c1", Cron: "0 9 * * *", TZ: "Asia/Shanghai", Prompt: "晨会提醒",
				Recurring: true, Tenant: tenant},
			{ID: "c2", Cron: "0 10 * * *", Prompt: "别家租户", Tenant: "other"},
		}
	}
	srv.Namer = &naming.Namer{
		LLM: &fakeNamerLLM{out: "每日晨会提醒"}, Store: &memNamingStore{title: "bear:main"},
	}
	return httptest.NewServer(srv), p, n
}

type fakePurger struct {
	tenant, channel string
	keep            []string
	purged          []string
	err             error
}

func (f *fakePurger) PurgeChats(ctx context.Context, tenant, channel string, keep []string) ([]string, error) {
	f.tenant, f.channel, f.keep = tenant, channel, keep
	if f.err != nil {
		return nil, f.err
	}
	return f.purged, nil
}

type fakeNotifier struct {
	listed []sqlite.Notification
	acked  int64
	ackIDs []int64
}

func (f *fakeNotifier) ListNotifications(ctx context.Context, tenant string) ([]sqlite.Notification, error) {
	return f.listed, nil
}
func (f *fakeNotifier) AckNotifications(ctx context.Context, tenant string, ids []int64) (int64, error) {
	f.ackIDs = ids
	return f.acked, nil
}

// memNamingStore 是命名面的内存 Store。
type memNamingStore struct{ title string }

func (m *memNamingStore) SessionTitle(ctx context.Context, sessionID string) (string, error) {
	return m.title, nil
}
func (m *memNamingStore) SetTitle(ctx context.Context, sessionID, title string) error {
	m.title = title
	return nil
}
func (m *memNamingStore) LoadRawHistory(ctx context.Context, sessionID string) ([]agent.Message, error) {
	return []agent.Message{
		{Role: agent.RoleUser, Kind: agent.KindMessage, Content: "早上九点提醒我开会"},
		{Role: agent.RoleAssistant, Kind: agent.KindMessage, Content: "好的，已设为每天提醒。"},
	}, nil
}

type fakeNamerLLM struct{ out string }

func (f *fakeNamerLLM) Chat(ctx context.Context, history []agent.Message, specs []tools.Spec, onPartial func(agent.Partial)) (agent.Response, error) {
	return agent.Response{Message: agent.Message{Role: agent.RoleAssistant, Content: f.out}}, nil
}

func postJSONAs(t *testing.T, url, token, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func getJSONAs(t *testing.T, url, token string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestPurgeChatsEndpoint(t *testing.T) {
	ts, p, _ := adminServer(t)
	defer ts.Close()

	code, out := postJSONAs(t, ts.URL+"/v1/admin/purge-chats", "tk1", `{"keep":["main"]}`)
	if code != 200 {
		t.Fatalf("code = %d, out=%v", code, out)
	}
	if p.tenant != "local" || p.channel != "bear" || len(p.keep) != 1 || p.keep[0] != "main" {
		t.Fatalf("purger args = %v/%v/%v", p.tenant, p.channel, p.keep)
	}
	// 鉴权：坏 token 401。
	if code, _ := postJSONAs(t, ts.URL+"/v1/admin/purge-chats", "bad", `{"keep":["main"]}`); code != 401 {
		t.Fatalf("bad token code = %d", code)
	}
	// keep 缺失 400。
	if code, _ := postJSONAs(t, ts.URL+"/v1/admin/purge-chats", "tk1", `{}`); code != 400 {
		t.Fatalf("missing keep code = %d", code)
	}
}

func TestNotificationsEndpoints(t *testing.T) {
	ts, _, n := adminServer(t)
	defer ts.Close()

	n.listed = []sqlite.Notification{{ID: 7, Kind: "cron", Title: "晨会", Body: "九点", CreatedAt: 1760000000000}}
	code, out := getJSONAs(t, ts.URL+"/v1/notifications", "tk1")
	if code != 200 {
		t.Fatalf("list code = %d", code)
	}
	rows := out["notifications"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["id"].(float64) != 7 {
		t.Fatalf("rows = %v", rows)
	}

	code, out = postJSONAs(t, ts.URL+"/v1/notifications/ack", "tk1", `{"ids":[7,8]}`)
	if code != 200 || out["acked"].(float64) != 0 {
		t.Fatalf("ack = %d %v", code, out)
	}
	if len(n.ackIDs) != 2 {
		t.Fatalf("ackIDs = %v", n.ackIDs)
	}
	if code, _ := postJSONAs(t, ts.URL+"/v1/notifications/ack", "tk1", `{"ids":[]}`); code != 400 {
		t.Fatalf("empty ids code = %d", code)
	}
}

func TestSchedulesEndpoint(t *testing.T) {
	ts, _, _ := adminServer(t)
	defer ts.Close()

	code, out := getJSONAs(t, ts.URL+"/v1/schedules", "tk1")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	rows := out["schedules"].([]any)
	if len(rows) != 1 { // 别家租户被过滤
		t.Fatalf("rows = %v", rows)
	}
	row := rows[0].(map[string]any)
	if row["id"] != "c1" || row["cron"] != "0 9 * * *" || row["tz"] != "Asia/Shanghai" ||
		row["recurring"] != true || row["last_fired"] != nil {
		t.Fatalf("row = %v", row)
	}
}

func TestChatTitleEndpoint(t *testing.T) {
	ts, _, _ := adminServer(t)
	defer ts.Close()

	code, out := postJSONAs(t, ts.URL+"/v1/chats/main/title", "tk1", `{}`)
	if code != 200 {
		t.Fatalf("code = %d out=%v", code, out)
	}
	if out["title"] != "每日晨会提醒" || out["chat_id"] != "main" {
		t.Fatalf("out = %v", out)
	}
}

func TestCapabilityEndpoints501WhenNil(t *testing.T) {
	toks, err := StaticTokens("tk1=local:alice")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(&runner.Manager{}, StaticAuth(toks), &fakeStore{msgs: map[string][]agent.Message{}}, "bear")
	ts := httptest.NewServer(srv)
	defer ts.Close()

	if code, _ := postJSONAs(t, ts.URL+"/v1/admin/purge-chats", "tk1", `{"keep":["main"]}`); code != 501 {
		t.Fatalf("purge code = %d", code)
	}
	if code, _ := getJSONAs(t, ts.URL+"/v1/notifications", "tk1"); code != 501 {
		t.Fatalf("notifications code = %d", code)
	}
	if code, _ := getJSONAs(t, ts.URL+"/v1/schedules", "tk1"); code != 501 {
		t.Fatalf("schedules code = %d", code)
	}
	if code, _ := postJSONAs(t, ts.URL+"/v1/chats/main/title", "tk1", `{}`); code != 501 {
		t.Fatalf("title code = %d", code)
	}
}
