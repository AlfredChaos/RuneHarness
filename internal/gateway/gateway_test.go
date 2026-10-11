package gateway

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
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

// newCaptureServer 同 newServer，但 harness 记录每轮收到的 user 消息，
// 供"附件透传到 TurnInput"断言用。每个实例只应提交一轮。
func newCaptureServer(t *testing.T) (*httptest.Server, func() []agent.Message) {
	t.Helper()
	toks, err := StaticTokens("tk1=local:alice")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var users []agent.Message
	mgr := runner.NewManager(runner.Config{}, runner.Harness{
		New: func(ctx context.Context, emit func(runner.Event), ask func(agent.ToolCall, string, int, int) <-chan bool) (runner.TurnRunner, error) {
			return func(ctx context.Context, hist []agent.Message) ([]agent.Message, error) {
				mu.Lock()
				for _, m := range hist {
					if m.Role == agent.RoleUser {
						users = append(users, m)
					}
				}
				mu.Unlock()
				emit(runner.Event{Event: runner.EvDelta,
					Data: json.RawMessage(`{"content":"pong"}`)})
				return append(hist, agent.Message{Role: agent.RoleAssistant, Content: "pong"}), nil
			}, nil
		},
	}, &fakeStore{msgs: map[string][]agent.Message{}}, &fakeStore{msgs: map[string][]agent.Message{}})
	srv := NewServer(mgr, StaticAuth(toks), &fakeStore{msgs: map[string][]agent.Message{}}, "bear")
	return httptest.NewServer(srv), func() []agent.Message {
		mu.Lock()
		defer mu.Unlock()
		return append([]agent.Message(nil), users...)
	}
}

// waitUser 轮询捕获结果直到出现一条 user 消息（轮次异步跑完）。
func waitUser(t *testing.T, users func() []agent.Message) agent.Message {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := users(); len(got) > 0 {
			return got[len(got)-1]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("turn never ran within deadline")
	return agent.Message{}
}

func postJSON(t *testing.T, url, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer tk1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestPostMessageCarriesAttachments(t *testing.T) {
	ts, users := newCaptureServer(t)
	defer ts.Close()

	resp := postJSON(t, ts.URL+"/v1/chats/demo/messages",
		`{"content":"看图","attachments":[{"kind":"image","media_type":"image/png","filename":"shot.png","data_b64":"aGVsbG8=","detail":"low"}]}`)
	resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("POST = %d", resp.StatusCode)
	}

	got := waitUser(t, users)
	if got.Content != "看图" {
		t.Fatalf("content = %q", got.Content)
	}
	want := agent.Part{Kind: "image", MediaType: "image/png", Filename: "shot.png",
		DataB64: "aGVsbG8=", Detail: "low"}
	if len(got.Parts) != 1 || got.Parts[0] != want {
		t.Fatalf("parts = %+v, want [%+v]", got.Parts, want)
	}
}

func TestStreamCarriesAttachments(t *testing.T) {
	ts, users := newCaptureServer(t)
	defer ts.Close()

	resp := postJSON(t, ts.URL+"/v1/chats/demo/stream",
		`{"content":"看图","attachments":[{"kind":"image","media_type":"image/jpeg","data_b64":"/9j/wA=="}]}`)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("stream = %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sawEnd := false
	for sc.Scan() {
		if strings.Contains(sc.Text(), `"event":"turn_end"`) {
			sawEnd = true
			break
		}
	}
	if !sawEnd {
		t.Fatal("no turn_end on stream")
	}

	got := waitUser(t, users)
	want := agent.Part{Kind: "image", MediaType: "image/jpeg", DataB64: "/9j/wA=="}
	if len(got.Parts) != 1 || got.Parts[0] != want {
		t.Fatalf("parts = %+v, want [%+v]", got.Parts, want)
	}
}

func TestAttachmentValidation(t *testing.T) {
	ts, _ := newServer(t)
	defer ts.Close()

	b64 := func(n int) string {
		return base64.StdEncoding.EncodeToString(make([]byte, n))
	}
	nine := `{"kind":"image","media_type":"image/png","data_b64":"aGk="}`
	cases := []struct {
		name string
		body string
		want string
	}{
		{"bad kind",
			`{"content":"x","attachments":[{"kind":"video","media_type":"video/mp4","data_b64":"aGk="}]}`,
			"unsupported attachment kind"},
		{"bad media type",
			`{"content":"x","attachments":[{"kind":"image","media_type":"application/pdf","data_b64":"aGk="}]}`,
			"attachment media_type must be image/*"},
		{"filename too long",
			`{"content":"x","attachments":[{"kind":"image","media_type":"image/png","filename":"` + strings.Repeat("a", 256) + `","data_b64":"aGk="}]}`,
			"attachment filename too long"},
		{"detail default forbidden",
			`{"content":"x","attachments":[{"kind":"image","media_type":"image/png","data_b64":"aGk=","detail":"default"}]}`,
			"attachment detail must be auto, low, or high"},
		{"detail unknown",
			`{"content":"x","attachments":[{"kind":"image","media_type":"image/png","data_b64":"aGk=","detail":"medium"}]}`,
			"attachment detail must be auto, low, or high"},
		{"bad base64",
			`{"content":"x","attachments":[{"kind":"image","media_type":"image/png","data_b64":"!!!"}]}`,
			"invalid base64 attachment"},
		{"too many",
			`{"content":"x","attachments":[` + nine + `,` + nine + `,` + nine + `,` + nine + `,` + nine + `,` + nine + `,` + nine + `,` + nine + `,` + nine + `]}`,
			"too many attachments"},
		{"single too large",
			`{"content":"x","attachments":[{"kind":"image","media_type":"image/png","data_b64":"` + b64(10<<20+1) + `"}]}`,
			"attachment too large"},
		{"total too large",
			`{"content":"x","attachments":[` +
				`{"kind":"image","media_type":"image/png","data_b64":"` + b64(8<<20) + `"},` +
				`{"kind":"image","media_type":"image/png","data_b64":"` + b64(8<<20) + `"},` +
				`{"kind":"image","media_type":"image/png","data_b64":"` + b64(8<<20) + `"},` +
				`{"kind":"image","media_type":"image/png","data_b64":"` + b64(8<<20) + `"}]}`,
			"attachments too large overall"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{"/v1/chats/demo/messages", "/v1/chats/demo/stream"} {
				resp := postJSON(t, ts.URL+path, tc.body)
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 400 {
					t.Fatalf("%s = %d, want 400", path, resp.StatusCode)
				}
				if !strings.Contains(string(b), tc.want) {
					t.Fatalf("%s body = %s, want contain %q", path, b, tc.want)
				}
			}
		})
	}
}

// TestWSMessageAttachmentValidation 覆盖 WS 轨的帧解析与 Submit 前校验：
// 校验失败以 error 返回（由读循环转成协议错误帧，连接不断），不触达 Submit。
// 仓库无 WS 测试 harness（现有用例全走 httptest），故不建 socket。
func TestWSMessageAttachmentValidation(t *testing.T) {
	ts, _ := newServer(t)
	defer ts.Close()
	srv := ts.Config.Handler.(*Server)
	wc := &wsConn{}

	err := wc.dispatch(context.Background(), srv, wsIn{
		Type: "message", ChatID: "demo", Content: "hi",
		Attachments: []attachmentIn{{Kind: "video", MediaType: "video/mp4", DataB64: "aGk="}},
	})
	if err == nil || err.Error() != "unsupported attachment kind" {
		t.Fatalf("dispatch err = %v, want unsupported attachment kind", err)
	}

	err = wc.dispatch(context.Background(), srv, wsIn{
		Type: "message", ChatID: "demo", Content: "hi",
		Attachments: []attachmentIn{{Kind: "image", MediaType: "image/png",
			DataB64: "aGk=", Detail: "default"}},
	})
	if err == nil || err.Error() != "attachment detail must be auto, low, or high" {
		t.Fatalf("dispatch err = %v, want detail rejection", err)
	}
}
