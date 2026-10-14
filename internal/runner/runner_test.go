package runner

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"runeharness/internal/agent"
	"runeharness/internal/session"
)

// fakeStore 是内存 SessionStore：记录 append、回放 history。
type fakeStore struct {
	mu       sync.Mutex
	sessions []session.Session
	msgs     map[string][]agent.Message
	nextID   int64
}

func newFakeStore() *fakeStore { return &fakeStore{msgs: map[string][]agent.Message{}} }

func (f *fakeStore) CreateSession(ctx context.Context, meta session.Meta) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	s := session.Session{ID: "s1", SubjectID: meta.SubjectID}
	f.sessions = append(f.sessions, s)
	return s, nil
}

func (f *fakeStore) Append(ctx context.Context, msg agent.Message) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	msg.ID = f.nextID
	f.msgs["s1"] = append(f.msgs["s1"], msg)
	return f.nextID, nil
}

func (f *fakeStore) LoadHistory(ctx context.Context, sid string) ([]agent.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agent.Message(nil), f.msgs[sid]...), nil
}

// fakeHarness 返回一个不依赖真 LLM 的 Harness：run 里发一个 delta，
// 然后转交测试给的 run 函数。
func fakeHarness(run func(ctx context.Context, hist []agent.Message) ([]agent.Message, error)) Harness {
	return Harness{
		New: func(ctx context.Context, emit func(Event), ask func(agent.ToolCall, string, int, int) <-chan bool) (TurnRunner, error) {
			return func(ctx context.Context, hist []agent.Message) ([]agent.Message, error) {
				emit(Event{Event: EvDelta, Data: mustJSON(map[string]any{"content": "hi"})})
				return run(ctx, hist)
			}, nil
		},
	}
}

func key() ChatKey { return ChatKey{TenantID: "t", Channel: "bear", ChatID: "c1"} }

func TestLedgerSeqAndReplay(t *testing.T) {
	l := NewLedger(4)
	for range 6 {
		l.Append(Event{Event: EvDelta})
	}
	if l.Latest() != 6 {
		t.Fatalf("latest = %d, want 6", l.Latest())
	}
	if _, _, err := l.Since(1); !errors.Is(err, ErrLag) {
		t.Fatalf("since=1 should lag, got %v", err)
	}
	out, latest, err := l.Since(3)
	if err != nil || len(out) != 4 || latest != 6 {
		t.Fatalf("since=3 got %d events latest=%d err=%v", len(out), latest, err)
	}
	if out[0].Seq != 3 || out[3].Seq != 6 {
		t.Fatalf("replay order wrong: %+v", out)
	}
}

func TestSlotSerializesTurns(t *testing.T) {
	store := newFakeStore()
	var order []string
	var mu sync.Mutex
	mgr := NewManager(Config{QueueDepth: 4}, fakeHarness(
		func(ctx context.Context, hist []agent.Message) ([]agent.Message, error) {
			mu.Lock()
			order = append(order, "start")
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			hist = append(hist, agent.Message{Role: agent.RoleAssistant, Content: "done"})
			mu.Lock()
			order = append(order, "end")
			mu.Unlock()
			return hist, nil
		}), store, nil)

	k := key()
	if err := mgr.Submit(context.Background(), k, TurnInput{Prompt: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Submit(context.Background(), k, TurnInput{Prompt: "b"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	want := []string{"start", "end", "start", "end"}
	if len(order) != 4 {
		t.Fatalf("order = %v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order[%d] = %s, want %s (full: %v)", i, order[i], want[i], order)
		}
	}
}

func TestSlotInterruptEmitsTurnEnd(t *testing.T) {
	store := newFakeStore()
	started := make(chan struct{})
	mgr := NewManager(Config{}, fakeHarness(
		func(ctx context.Context, hist []agent.Message) ([]agent.Message, error) {
			close(started)
			<-ctx.Done()
			return hist, &agent.InterruptedError{}
		}), store, nil)

	k := key()
	if err := mgr.Submit(context.Background(), k, TurnInput{Prompt: "a"}); err != nil {
		t.Fatal(err)
	}
	<-started
	if err := mgr.Interrupt(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s, err := mgr.slotFor(context.Background(), k)
		if err != nil {
			t.Fatal(err)
		}
		out, _, _ := s.ledger.Since(0)
		for _, ev := range out {
			if ev.Event == EvTurnEnd {
				var oc TurnOutcome
				if err := json.Unmarshal(ev.Data, &oc); err != nil {
					t.Fatal(err)
				}
				if oc.Status != "interrupted" {
					t.Fatalf("status = %q, want interrupted", oc.Status)
				}
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("turn_end not emitted within deadline")
}

func TestAskRoundTrip(t *testing.T) {
	store := newFakeStore()
	mgr := NewManager(Config{AskTimeout: 2 * time.Second}, Harness{
		New: func(ctx context.Context, emit func(Event), ask func(agent.ToolCall, string, int, int) <-chan bool) (TurnRunner, error) {
			return func(ctx context.Context, hist []agent.Message) ([]agent.Message, error) {
				reply := ask(agent.ToolCall{ID: "c1", Name: "run_command"}, "needs approval", 1, 1)
				select {
				case ok := <-reply:
					if !ok {
						return hist, errors.New("denied")
					}
				case <-ctx.Done():
					return hist, ctx.Err()
				}
				return append(hist, agent.Message{Role: agent.RoleAssistant, Content: "approved"}), nil
			}, nil
		},
	}, store, nil)

	k := key()
	ctx := context.Background()
	s, err := mgr.slotFor(ctx, k)
	if err != nil {
		t.Fatal(err)
	}
	events, _, err := s.ledger.Since(0)
	_ = events
	if err != nil {
		t.Fatal(err)
	}

	if err := mgr.Submit(ctx, k, TurnInput{Prompt: "go"}); err != nil {
		t.Fatal(err)
	}
	// 轮询 ledger 等 ask 事件（订阅通道的竞态在单测里用轮询更稳）
	deadline := time.Now().Add(2 * time.Second)
	var askID string
	for time.Now().Before(deadline) && askID == "" {
		out, _, _ := s.ledger.Since(0)
		for _, ev := range out {
			if ev.Event == EvAsk {
				var d struct {
					AskID string `json:"ask_id"`
				}
				if err := json.Unmarshal(ev.Data, &d); err == nil {
					askID = d.AskID
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if askID == "" {
		t.Fatal("ask event not emitted")
	}
	if err := mgr.Answer(ctx, k, askID, true); err != nil {
		t.Fatal(err)
	}
	// 等 turn_end
	for time.Now().Before(deadline) {
		out, _, _ := s.ledger.Since(0)
		for _, ev := range out {
			if ev.Event == EvTurnEnd {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("turn_end not emitted after answer")
}

func TestQueueFullRejected(t *testing.T) {
	store := newFakeStore()
	release := make(chan struct{})
	mgr := NewManager(Config{QueueDepth: 0}, fakeHarness(
		func(ctx context.Context, hist []agent.Message) ([]agent.Message, error) {
			<-release
			return hist, nil
		}), store, nil)

	k := key()
	if err := mgr.Submit(context.Background(), k, TurnInput{Prompt: "first"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // 让第一轮被取走
	if err := mgr.Submit(context.Background(), k, TurnInput{Prompt: "second"}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("second submit should be rejected, got %v", err)
	}
	close(release)
}

// TestTurnInputAttachmentsPersist 验证 TurnInput.Attachments 原样落在
// user 消息的 Parts 上（进 history 视图，随之落库）；nil 时零行为变化。
func TestTurnInputAttachmentsPersist(t *testing.T) {
	store := newFakeStore()
	ran := make(chan []agent.Message, 1)
	mgr := NewManager(Config{}, fakeHarness(
		func(ctx context.Context, hist []agent.Message) ([]agent.Message, error) {
			ran <- hist
			return append(hist, agent.Message{Role: agent.RoleAssistant, Content: "ok"}), nil
		}), store, nil)

	atts := []agent.Part{{
		Kind: agent.PartImage, MediaType: "image/png", Filename: "shot.png",
		DataB64: "aGVsbG8=", Detail: "low",
	}}
	if err := mgr.Submit(context.Background(), key(), TurnInput{
		Prompt: "看图", Attachments: atts,
	}); err != nil {
		t.Fatal(err)
	}
	var hist []agent.Message
	select {
	case hist = <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("turn never ran")
	}

	// 视图侧：runner 收到的 history 里 user 消息带 Parts。
	var user agent.Message
	for _, m := range hist {
		if m.Role == agent.RoleUser {
			user = m
		}
	}
	if user.Content != "看图" {
		t.Fatalf("user content = %q", user.Content)
	}
	if len(user.Parts) != 1 || user.Parts[0] != atts[0] {
		t.Fatalf("user parts = %+v, want %+v", user.Parts, atts)
	}

	// 存储侧：落库的同一行带 Parts。
	rows, err := store.LoadHistory(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range rows {
		if m.Role == agent.RoleUser && len(m.Parts) == 1 && m.Parts[0] == atts[0] {
			return
		}
	}
	t.Fatalf("persisted rows lack parts: %+v", rows)
}

func TestOnTurnCompleteFiresOnlyOnCompletedTurns(t *testing.T) {
	store := newFakeStore()
	var fired []string
	done := make(chan string, 4)
	mgr := NewManager(Config{}, fakeHarness(func(ctx context.Context, hist []agent.Message) ([]agent.Message, error) {
		return append(hist, agent.Message{Role: agent.RoleAssistant, Content: "pong"}), nil
	}), store, nil)
	mgr.OnTurnComplete = func(k ChatKey, sid string) { done <- k.ChatID + "/" + sid }

	ctx := context.Background()
	if err := mgr.Submit(ctx, key(), TurnInput{Prompt: "go"}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		fired = append(fired, got)
	case <-time.After(2 * time.Second):
		t.Fatal("OnTurnComplete not fired for completed turn")
	}
	if fired[0] != "c1/s1" {
		t.Fatalf("fired = %v, want c1/s1", fired)
	}
	_ = store
}

func TestOnTurnCompleteSkippedOnErrorTurn(t *testing.T) {
	mgr := NewManager(Config{}, fakeHarness(func(ctx context.Context, hist []agent.Message) ([]agent.Message, error) {
		return hist, errors.New("llm exploded")
	}), newFakeStore(), nil)
	fired := make(chan string, 1)
	mgr.OnTurnComplete = func(k ChatKey, sid string) { fired <- k.ChatID }

	ctx := context.Background()
	if err := mgr.Submit(ctx, key(), TurnInput{Prompt: "go"}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-fired:
		t.Fatalf("hook fired on error turn: %v", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// SubscribeLive 是 /stream 的订阅原语：只转发订阅后新事件，绝不回放账本。
// 回归背景：/stream 此前误用 Subscribe(since=0)（=回放全部缓存），客户端
// 在旧轮回放的 turn_end 处提前收流。
func TestSubscribeLiveSkipsLedgerReplay(t *testing.T) {
	m := NewManager(Config{}, fakeHarness(func(ctx context.Context, hist []agent.Message) ([]agent.Message, error) {
		return append(hist, agent.Message{Role: agent.RoleAssistant, Content: "ok"}), nil
	}), newFakeStore(), nil)
	ctx := context.Background()
	if err := m.Submit(ctx, key(), TurnInput{Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for m.Active(ctx, key()) {
		if time.Now().After(deadline) {
			t.Fatal("turn never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ch, latest, err := m.SubscribeLive(ctx, key())
	if err != nil {
		t.Fatal(err)
	}
	if latest < 1 {
		t.Fatalf("latest = %d, want >= 1", latest)
	}
	select {
	case ev := <-ch:
		t.Fatalf("live-only subscription replayed %s seq=%d from ledger history", ev.Event, ev.Seq)
	case <-time.After(120 * time.Millisecond):
	}
}
