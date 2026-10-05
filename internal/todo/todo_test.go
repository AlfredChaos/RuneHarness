package todo

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"runeharness/internal/agent"
)

func write(t *testing.T, m *Manager, todosJSON string) string {
	t.Helper()
	out, err := m.Run(context.Background(), json.RawMessage(todosJSON))
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	return out
}

func TestRunStoresAndRenders(t *testing.T) {
	m := NewManager()
	out := write(t, m, `{"todos":[
		{"content":"plan","status":"completed"},
		{"content":"code","status":"in_progress"},
		{"content":"test","status":"pending"}]}`)
	for _, want := range []string{"Updated 3 tasks:", "[x] plan", "[>] code", "[ ] test"} {
		if !strings.Contains(out, want) {
			t.Fatalf("board missing %q in:\n%s", want, out)
		}
	}
	// 全量替换语义：第二次写入覆盖而非追加
	out = write(t, m, `{"todos":[{"content":"only","status":"pending"}]}`)
	if !strings.Contains(out, "Updated 1 tasks:") || strings.Contains(out, "plan") {
		t.Fatalf("replace semantics broken:\n%s", out)
	}
}

func TestRunRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"missing todos":  `{}`,
		"empty list":     `{"todos":[]}`,
		"empty content":  `{"todos":[{"content":" ","status":"pending"}]}`,
		"bad status":     `{"todos":[{"content":"x","status":"done"}]}`,
		"malformed json": `{"todos":`,
	}
	for name, arg := range cases {
		m := NewManager()
		if _, err := m.Run(context.Background(), json.RawMessage(arg)); err == nil {
			t.Fatalf("%s: want error, got nil", name)
		}
	}
	// 超过 maxItems
	big := `{"todos":[` + strings.TrimSuffix(strings.Repeat(`{"content":"x","status":"pending"},`, maxItems+1), ",") + `]}`
	if _, err := NewManager().Run(context.Background(), json.RawMessage(big)); err == nil {
		t.Fatal("over maxItems: want error, got nil")
	}
}

func TestNagFiresOnIntervalRound(t *testing.T) {
	m := NewManager()
	write(t, m, `{"todos":[{"content":"pending work","status":"pending"}]}`)
	ctx := context.Background()
	for i := 1; i < nagInterval; i++ {
		if s := m.Nag(ctx, nil); s != "" {
			t.Fatalf("Nag round %d = %q, want empty", i, s)
		}
	}
	s := m.Nag(ctx, nil)
	if !strings.Contains(s, "<reminder>") || !strings.Contains(s, "pending work") {
		t.Fatalf("Nag round %d = %q, want reminder with pending item", nagInterval, s)
	}
	// 触发后归零：下一轮重新计数
	if s := m.Nag(ctx, nil); s != "" {
		t.Fatalf("Nag after fire = %q, want empty (counter reset)", s)
	}
}

func TestNagNudgesNeverPlanned(t *testing.T) {
	ctx := context.Background()
	m := NewManager()
	for i := 1; i < nagInterval; i++ {
		if s := m.Nag(ctx, nil); s != "" {
			t.Fatalf("Nag round %d = %q, want empty", i, s)
		}
	}
	if s := m.Nag(ctx, nil); !strings.Contains(s, "todo_write") {
		t.Fatalf("Nag round %d = %q, want planning nudge", nagInterval, s)
	}
}

func TestNagSilentWhenAllCompleted(t *testing.T) {
	ctx := context.Background()
	m := NewManager()
	write(t, m, `{"todos":[{"content":"done","status":"completed"}]}`)
	for i := 0; i < nagInterval*2; i++ {
		if s := m.Nag(ctx, nil); s != "" {
			t.Fatalf("Nag with all-completed fired: %q", s)
		}
	}
}

func TestResetRounds(t *testing.T) {
	ctx := context.Background()
	m := NewManager()
	m.Nag(ctx, nil)
	m.Nag(ctx, nil) // rounds=2
	m.ResetRounds()
	for i := 1; i < nagInterval; i++ {
		if s := m.Nag(ctx, nil); s != "" {
			t.Fatalf("Nag round %d after reset = %q, want empty", i, s)
		}
	}
	if s := m.Nag(ctx, nil); s == "" {
		t.Fatalf("Nag round %d after reset: want reminder, got empty", nagInterval)
	}
}

func TestRunResetsNagCounter(t *testing.T) {
	m := NewManager()
	write(t, m, `{"todos":[{"content":"a","status":"pending"}]}`)
	ctx := context.Background()
	m.Nag(ctx, nil)
	m.Nag(ctx, nil) // rounds=2
	write(t, m, `{"todos":[{"content":"a","status":"in_progress"}]}`)
	// 写后重新计数：前 nagInterval-1 轮静默，第 nagInterval 轮触发
	for i := 1; i < nagInterval; i++ {
		if s := m.Nag(ctx, nil); s != "" {
			t.Fatalf("Nag round %d after write = %q, want empty", i, s)
		}
	}
	if s := m.Nag(ctx, nil); s == "" {
		t.Fatalf("Nag round %d after write: want reminder, got empty", nagInterval)
	}
}

func TestOnChangeFiresWithCopy(t *testing.T) {
	m := NewManager()
	var got []Item
	m.OnChange = func(items []Item) { got = items }
	write(t, m, `{"todos":[{"content":"a","status":"pending"}]}`)
	if len(got) != 1 || got[0].Content != "a" {
		t.Fatalf("OnChange got %v, want [{a pending}]", got)
	}
	// 校验失败不触发 OnChange
	m.OnChange = func([]Item) { t.Fatal("OnChange fired on invalid write") }
	if _, err := m.Run(context.Background(), json.RawMessage(`{"todos":[{"content":"x","status":"bad"}]}`)); err == nil {
		t.Fatal("want error, got nil")
	}
	// 回调收到的是拷贝：外部改写不影响内部状态
	m.OnChange = func(items []Item) { items[0].Content = "mutated" }
	out := write(t, m, `{"todos":[{"content":"b","status":"pending"}]}`)
	if strings.Contains(out, "mutated") {
		t.Fatal("OnChange slice aliases internal state")
	}
}

// Nag 签名须满足 agent.PreChatFunc，保证可直接注册。
func TestNagMatchesPreChatFunc(t *testing.T) {
	var h agent.Hooks
	h.OnPreChat(NewManager().Nag)
}
