package naming

import (
	"context"
	"errors"
	"strings"
	"testing"

	"runeharness/internal/agent"
	"runeharness/internal/tools"
)

// fakeStore 是命名面的内存实现。
type fakeStore struct {
	title    string
	hist     []agent.Message
	setErr   error
	setCalls []string
}

func (f *fakeStore) SessionTitle(ctx context.Context, sessionID string) (string, error) {
	return f.title, nil
}
func (f *fakeStore) SetTitle(ctx context.Context, sessionID, title string) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.title = title
	f.setCalls = append(f.setCalls, title)
	return nil
}
func (f *fakeStore) LoadRawHistory(ctx context.Context, sessionID string) ([]agent.Message, error) {
	return f.hist, nil
}

// fakeLLM 记录收到的请求并返回固定文本。
type fakeLLM struct {
	out    string
	err    error
	calls  int
	lastIn []agent.Message
}

func (f *fakeLLM) Chat(ctx context.Context, history []agent.Message, specs []tools.Spec, onPartial func(agent.Partial)) (agent.Response, error) {
	f.calls++
	f.lastIn = history
	if f.err != nil {
		return agent.Response{}, f.err
	}
	return agent.Response{Message: agent.Message{Role: agent.RoleAssistant, Content: f.out}}, nil
}

func firstRoundHistory() []agent.Message {
	return []agent.Message{
		{Role: agent.RoleUser, Kind: agent.KindMessage, Content: "帮我看看这个 go 的 panic 堆栈怎么修"},
		{Role: agent.RoleAssistant, Kind: agent.KindMessage, Content: "这个 panic 是空指针解引用，修复方法是初始化 map……"},
	}
}

func TestRenameNamesDefaultTitle(t *testing.T) {
	st := &fakeStore{title: "bear:main", hist: firstRoundHistory()}
	llm := &fakeLLM{out: " 修复 go panic 堆栈的完整方案 "}
	n := &Namer{LLM: llm, Store: st, Timeout: -1} // -1 走 DefaultTimeout 分支

	got, err := n.Rename("t1", "bear", "s1", false)
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got != "修复 go panic" || len([]rune(got)) > maxTitleRunes {
		t.Fatalf("title = %q, want trimmed ≤12-rune truncation", got)
	}
	if st.title != got {
		t.Fatalf("SetTitle not persisted: %q", st.title)
	}
	if llm.calls != 1 || len(llm.lastIn) != 2 ||
		llm.lastIn[0].Role != agent.RoleSystem || llm.lastIn[1].Role != agent.RoleUser {
		t.Fatalf("LLM call shape wrong: calls=%d lastIn=%+v", llm.calls, llm.lastIn)
	}
	if strings.Contains(llm.lastIn[1].Content, "用户消息：帮我看看") == false {
		t.Fatalf("prompt missing user text: %q", llm.lastIn[1].Content)
	}
}

func TestRenameSkipsReadableTitle(t *testing.T) {
	st := &fakeStore{title: "晚饭推荐", hist: firstRoundHistory()}
	llm := &fakeLLM{out: "不该被调用"}
	n := &Namer{LLM: llm, Store: st}

	got, err := n.Rename("t1", "bear", "s1", false)
	if err != nil || got != "晚饭推荐" {
		t.Fatalf("Rename = %q, %v", got, err)
	}
	if llm.calls != 0 || len(st.setCalls) != 0 {
		t.Fatalf("readable title must short-circuit: llm=%d set=%d", llm.calls, len(st.setCalls))
	}
	// force 越过防重。
	if _, err := n.Rename("t1", "bear", "s1", true); err != nil || llm.calls != 1 {
		t.Fatalf("force rename: llm=%d err=%v", llm.calls, err)
	}
}

func TestRenameWaitsForCompleteRound(t *testing.T) {
	st := &fakeStore{title: "bear:main", hist: firstRoundHistory()[:1]} // 只有 user
	llm := &fakeLLM{out: "x"}
	n := &Namer{LLM: llm, Store: st}
	if _, err := n.Rename("t1", "bear", "s1", false); !errors.Is(err, errNoRound) {
		t.Fatalf("err = %v, want errNoRound", err)
	}
	if llm.calls != 0 {
		t.Fatalf("LLM must not be called without a full round")
	}
	// 控制行不算回复。
	st.hist = append(st.hist, agent.Message{Role: agent.RoleUser, Kind: agent.KindTurnEnd, Content: "{}"})
	if _, err := n.Rename("t1", "bear", "s1", false); !errors.Is(err, errNoRound) {
		t.Fatalf("control row must not count as assistant: %v", err)
	}
}

func TestRenameFailureSilent(t *testing.T) {
	st := &fakeStore{title: "bear:main", hist: firstRoundHistory()}
	n := &Namer{LLM: &fakeLLM{err: errors.New("api down")}, Store: st}
	if _, err := n.Rename("t1", "bear", "s1", false); err == nil {
		t.Fatal("want error from LLM failure")
	}
	if st.title != "bear:main" || len(st.setCalls) != 0 {
		t.Fatalf("title must stay untouched on failure: %q", st.title)
	}
	// 模型吐空也算失败。
	st2 := &fakeStore{title: "bear:main", hist: firstRoundHistory()}
	n2 := &Namer{LLM: &fakeLLM{out: "  \n  "}, Store: st2}
	if _, err := n2.Rename("t1", "bear", "s1", false); !errors.Is(err, errEmptyTitle) {
		t.Fatalf("err = %v, want errEmptyTitle", err)
	}
}

func TestNameChatNeverPanics(t *testing.T) {
	n := &Namer{LLM: &fakeLLM{err: errors.New("boom")}, Store: &fakeStore{}}
	n.NameChat("t1", "bear", "main", "s1") // 只要不 panic 即通过
}

func TestIsDefaultTitle(t *testing.T) {
	cases := []struct {
		title, channel string
		want           bool
	}{
		{"bear:main", "bear", true},
		{"bear:chat_abc123", "bear", true},
		{"晚饭推荐", "bear", false},
		{"bearish title", "bear", false}, // 无冒号不算默认形态
	}
	for _, c := range cases {
		if got := IsDefaultTitle(c.title, c.channel); got != c.want {
			t.Fatalf("IsDefaultTitle(%q,%q) = %v, want %v", c.title, c.channel, got, c.want)
		}
	}
}
