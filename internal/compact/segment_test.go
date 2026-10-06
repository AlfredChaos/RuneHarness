package compact

import (
	"testing"

	"runeharness/internal/agent"
)

func testCompactor(sub bool) *Compactor {
	return &Compactor{cfg: Config{Window: 128_000, Auto: true, Subagent: sub}}
}

// 50 个小用户轮：首部 = 前 3 条真实用户消息（跳过 inject），尾部 = 最近 40 轮。
func TestSegmentHeadAndTailTurns(t *testing.T) {
	view := chatOf(50, "ok", "done")
	// 第 1 轮之前插一条注入消息：不算用户消息
	view = append(view[:1], append([]agent.Message{{Role: agent.RoleUser, Kind: agent.KindInject, Content: "nag"}}, view[1:]...)...)
	s := testCompactor(false).segment(view, false)

	if len(s.system) != 1 || len(s.head) != 3 {
		t.Fatalf("system=%v head=%v", s.system, s.head)
	}
	for k, j := range s.head {
		if want := "question " + string(rune('1'+k)); view[j].Content != want {
			t.Fatalf("head[%d] = %q, want %q", k, view[j].Content, want)
		}
	}
	first := view[s.tail[0]]
	if first.Content != "question 11" {
		t.Fatalf("tail starts at %q, want question 11 (last 40 turns)", first.Content)
	}
	if len(s.system)+len(s.head)+len(s.middle)+len(s.tail) != len(view) {
		t.Fatal("segments must partition the view")
	}
}

// 尾部超 40K：更早的轮只能整轮加入，尾部起点是真实用户消息，合计不超预算。
func TestSegmentTailBudgetWholeTurns(t *testing.T) {
	view := chatOf(10, big(40_000), "done") // 每轮 ~10K token
	s := testCompactor(false).segment(view, false)
	first := view[s.tail[0]]
	if !realUser(first) {
		t.Fatalf("older turns must be added whole; tail starts at %+v", first)
	}
	if n := Rough(pick(view, s.tail)); n > tailBudget {
		t.Fatalf("tail = %d tokens > %d", n, tailBudget)
	}
}

// 当前轮本身超预算：在当前轮内按调用组切，起点是 assistant 消息，配对完整。
func TestSegmentCutsInsideCurrentTurn(t *testing.T) {
	view := chatOf(3, "ok", "done")
	view = append(view, agent.Message{Role: agent.RoleUser, Content: "huge task"})
	for n := 0; n < 20; n++ {
		view = append(view, turn(100+n, big(20_000), "")[1:3]...) // 20 组 × ~5K token
	}
	s := testCompactor(false).segment(view, false)
	first := view[s.tail[0]]
	if first.Role != agent.RoleAssistant {
		t.Fatalf("tail inside current turn should start at an assistant group, got %+v", first.Role)
	}
	if view[s.tail[len(s.tail)-1]].Role != agent.RoleTool {
		t.Fatal("tail must end with the complete last group")
	}
	// floor：只留最后一个调用组
	f := testCompactor(false).segment(view, true)
	if len(f.tail) != 2 {
		t.Fatalf("floor tail = %d msgs, want the last group (2)", len(f.tail))
	}
}

// 子代理模式：最后一条真实用户消息（任务）总是钉进首部，即使首部已满。
func TestSegmentSubagentPinsTask(t *testing.T) {
	view := chatOf(5, "ok", "done")
	view = append(view, agent.Message{Role: agent.RoleUser, Content: "sub task"})
	for n := 0; n < 20; n++ {
		view = append(view, turn(200+n, big(20_000), "")[1:3]...)
	}
	s := testCompactor(true).segment(view, false)
	last := s.head[len(s.head)-1]
	if view[last].Content != "sub task" {
		t.Fatalf("task message not pinned; head = %v", viewContents(pick(view, s.head)))
	}
	for _, j := range append(s.middle, s.tail...) {
		if view[j].Content == "sub task" {
			t.Fatal("task message must not appear in middle or tail")
		}
	}
}
