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
