package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"runeharness/internal/agent"
	"runeharness/internal/runner"
	"runeharness/internal/session"
)

// fakeStore 复用 runner 测试里的最小内存存储。
type fakeStore struct {
	mu   sync.Mutex
	msgs map[string][]agent.Message
	id   int64
}

func (f *fakeStore) CreateSession(ctx context.Context, meta session.Meta) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.id++
	return session.Session{ID: "s1", SubjectID: meta.SubjectID}, nil
}
func (f *fakeStore) Append(ctx context.Context, m agent.Message) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.id++
	f.msgs["s1"] = append(f.msgs["s1"], m)
	return f.id, nil
}
func (f *fakeStore) LoadHistory(ctx context.Context, sid string) ([]agent.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agent.Message(nil), f.msgs[sid]...), nil
}
func (f *fakeStore) GetBinding(ctx context.Context, tenant, channel, chatID string) (string, error) {
	return "s1", nil
}
func (f *fakeStore) UpsertBinding(ctx context.Context, tenant, channel, chatID, sid, sub string) (string, error) {
	return sid, nil
}
func (f *fakeStore) ListByTenant(ctx context.Context, tenant, channel string) ([]map[string]any, error) {
	return []map[string]any{{"chat_id": "demo", "session_id": "s1"}}, nil
}
func (f *fakeStore) LoadRawHistory(ctx context.Context, sid string) ([]agent.Message, error) {
	return f.LoadHistory(ctx, sid)
}

func newServer(t *testing.T) (*httptest.Server, *runner.Manager) {
	t.Helper()
	toks, err := StaticTokens("tk1=local:alice")
	if err != nil {
		t.Fatal(err)
	}
	mgr := runner.NewManager(runner.Config{}, runner.Harness{
		New: func(ctx context.Context, emit func(runner.Event), ask func(agent.ToolCall, string, int, int) <-chan bool) (runner.TurnRunner, error) {
			return func(ctx context.Context, hist []agent.Message) ([]agent.Message, error) {
				emit(runner.Event{Event: runner.EvDelta,
					Data: json.RawMessage(`{"content":"pong"}`)})
				return append(hist, agent.Message{Role: agent.RoleAssistant, Content: "pong"}), nil
			}, nil
		},
	}, &fakeStore{msgs: map[string][]agent.Message{}}, &fakeStore{msgs: map[string][]agent.Message{}})
	srv := NewServer(mgr, StaticAuth(toks), &fakeStore{msgs: map[string][]agent.Message{}}, "bear")
	return httptest.NewServer(srv), mgr
}

func TestStaticTokens(t *testing.T) {
	toks, err := StaticTokens("a=t1:s1, b=t2:, c=:s3")
	if err != nil {
		t.Fatal(err)
	}
	if toks["a"] != (Token{TenantID: "t1", SubjectID: "s1"}) {
		t.Fatalf("a = %+v", toks["a"])
	}
	if toks["b"].SubjectID != "default" || toks["b"].TenantID != "t2" {
		t.Fatalf("b = %+v", toks["b"])
	}
	if toks["c"].TenantID != "local" || toks["c"].SubjectID != "s3" {
		t.Fatalf("c = %+v", toks["c"])
	}
	if _, err := StaticTokens(""); err == nil {
		t.Fatal("empty spec should error")
	}
}

func TestSSERoundTrip(t *testing.T) {
	ts, _ := newServer(t)
	defer ts.Close()

	// 提交一条消息
	req, _ := http.NewRequest("POST", ts.URL+"/v1/chats/demo/messages",
		strings.NewReader(`{"content":"hi"}`))
	req.Header.Set("Authorization", "Bearer tk1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("POST = %d", resp.StatusCode)
	}

	// SSE 订阅拿 turn_end
	sreq, _ := http.NewRequest("GET", ts.URL+"/v1/chats/demo/events", nil)
	sreq.Header.Set("Authorization", "Bearer tk1")
	sresp, err := http.DefaultClient.Do(sreq)
	if err != nil {
		t.Fatal(err)
	}
	defer sresp.Body.Close()
	if ct := sresp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}

	sc := bufio.NewScanner(sresp.Body)
	deadline := time.After(3 * time.Second)
	gotEnd := make(chan bool, 1)
	go func() {
		for sc.Scan() {
			line := sc.Text()
			if strings.Contains(line, `"event":"turn_end"`) {
				gotEnd <- true
				return
			}
		}
	}()
	select {
	case <-gotEnd:
	case <-deadline:
		t.Fatal("no turn_end on SSE within 3s")
	}
}

func TestAuthRejectsBadToken(t *testing.T) {
	ts, _ := newServer(t)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/v1/chats")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("no token = %d, want 401", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/v1/chats", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 401 {
		t.Fatalf("bad token = %d, want 401", resp2.StatusCode)
	}
}

func TestCancelIdleIsNoop(t *testing.T) {
	ts, _ := newServer(t)
	defer ts.Close()
	req, _ := http.NewRequest("POST", ts.URL+"/v1/chats/demo/actions",
		strings.NewReader(`{"type":"cancel"}`))
	req.Header.Set("Authorization", "Bearer tk1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("cancel idle = %d", resp.StatusCode)
	}
}
