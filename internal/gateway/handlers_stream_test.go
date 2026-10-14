package gateway

// /stream 端点回归：同一 chat 的第二次提交必须只收自己那一轮的事件帧。
// 历史缺陷：handleStream 以 since=0 订阅，Ledger.Since(0) 回放全部缓存，
// 客户端在旧轮回放的 turn_end 处提前收流——新一轮照常在服务端跑完落库，
// 但请求方永远收不到（对 companion 客户端表现为“回复旧消息然后卡住”）。
import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"runeharness/internal/agent"
	"runeharness/internal/runner"
)

// newCountingServer 每轮回复带轮次编号，供断言区分“自己这轮”与“旧轮回放”。
func newCountingServer(t *testing.T) *httptest.Server {
	t.Helper()
	toks, _ := StaticTokens("tk1=local:alice")
	var n atomic.Int32
	mgr := runner.NewManager(runner.Config{}, runner.Harness{
		New: func(ctx context.Context, emit func(runner.Event), ask func(agent.ToolCall, string, int, int) <-chan bool) (runner.TurnRunner, error) {
			return func(ctx context.Context, hist []agent.Message) ([]agent.Message, error) {
				reply := fmt.Sprintf("reply-%d", n.Add(1))
				emit(runner.Event{Event: runner.EvDelta, Data: json.RawMessage(fmt.Sprintf(`{"content":%q}`, reply))})
				return append(hist, agent.Message{Role: agent.RoleAssistant, Content: reply}), nil
			}, nil
		},
	}, &fakeStore{msgs: map[string][]agent.Message{}}, &fakeStore{msgs: map[string][]agent.Message{}})
	srv := NewServer(mgr, StaticAuth(toks), &fakeStore{msgs: map[string][]agent.Message{}}, "bear")
	return httptest.NewServer(srv)
}

// readStream 提交一条消息并读完整个 /stream 响应，返回按序事件帧。
func readStream(t *testing.T, ts *httptest.Server, content string) []runner.Event {
	t.Helper()
	resp := postJSON(t, ts.URL+"/v1/chats/demo/stream", fmt.Sprintf(`{"content":%q}`, content))
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("stream = %d, want 200", resp.StatusCode)
	}
	var out []runner.Event
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var ev runner.Event
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("bad NDJSON line %q: %v", sc.Text(), err)
		}
		out = append(out, ev)
	}
	return out
}

func TestStreamCarriesOnlyOwnTurn(t *testing.T) {
	ts := newCountingServer(t)
	defer ts.Close()

	first := readStream(t, ts, "你好")
	if len(first) == 0 || first[len(first)-1].Event != runner.EvTurnEnd {
		t.Fatalf("first turn did not complete: %d frames", len(first))
	}
	maxSeq := first[len(first)-1].Seq

	second := readStream(t, ts, "今天好热")
	if len(second) == 0 {
		t.Fatal("second stream returned no frames")
	}
	var starts, ends, deltas int
	for _, ev := range second {
		if ev.TurnSeq != 2 {
			t.Fatalf("frame %s belongs to turn_seq=%d, want 2 (own turn only)", ev.Event, ev.TurnSeq)
		}
		if ev.Seq <= maxSeq {
			t.Fatalf("frame %s seq=%d is replayed ledger history (first turn ended at seq=%d)", ev.Event, ev.Seq, maxSeq)
		}
		switch ev.Event {
		case runner.EvTurnStart:
			starts++
		case runner.EvTurnEnd:
			ends++
		case runner.EvDelta:
			deltas++
		}
	}
	if starts != 1 || ends != 1 || deltas == 0 {
		t.Fatalf("frame counts starts=%d ends=%d deltas=%d, want 1/1/>=1", starts, ends, deltas)
	}
}

// 纯图片消息（空正文 + 附件）合法：附件先校验，正文仅在无附件时要求非空。
func TestEmptyContentAllowedWithAttachments(t *testing.T) {
	ts, users := newCaptureServer(t)
	defer ts.Close()

	// /stream：空正文 + 图片附件 → 200 收完本轮，user 消息带 Parts、正文为空
	resp := postJSON(t, ts.URL+"/v1/chats/demo/stream",
		`{"content":"","attachments":[{"kind":"image","media_type":"image/jpeg","data_b64":"/9j/wA=="}]}`)
	sc := bufio.NewScanner(resp.Body)
	sawEnd := false
	for sc.Scan() {
		if strings.Contains(sc.Text(), `"event":"turn_end"`) {
			sawEnd = true
			break
		}
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || !sawEnd {
		t.Fatalf("stream = %d sawEnd=%v, want 200 with turn_end", resp.StatusCode, sawEnd)
	}
	got := waitUser(t, users)
	if got.Content != "" || len(got.Parts) != 1 {
		t.Fatalf("user msg content=%q parts=%d, want empty content with 1 image part", got.Content, len(got.Parts))
	}

	// /messages：同一规则
	resp2 := postJSON(t, ts.URL+"/v1/chats/demo2/messages",
		`{"content":"","attachments":[{"kind":"image","media_type":"image/png","data_b64":"aGk="}]}`)
	resp2.Body.Close()
	if resp2.StatusCode != 202 {
		t.Fatalf("POST = %d, want 202", resp2.StatusCode)
	}

	// 无附件的空提交仍是 400（两个入口都验）
	for _, path := range []string{"/v1/chats/demo/messages", "/v1/chats/demo/stream"} {
		r := postJSON(t, ts.URL+path, `{"content":"  "}`)
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != 400 || !strings.Contains(string(b), "content is empty") {
			t.Fatalf("%s = %d %s, want 400 content is empty", path, r.StatusCode, b)
		}
	}
}
