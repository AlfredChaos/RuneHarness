package compact

import (
	"fmt"
	"strings"
	"testing"

	"runeharness/internal/agent"
)

// 压缩线：frac=0 走旧线 W−33K；frac>0 取 min(W−33K, ⌊W×frac⌋)。
// W=80K frac=0.5 → 40K，且 提醒(24K) < 压缩(40K) < 阻断(60K) 次序成立。
func TestCompactLineFrac(t *testing.T) {
	cases := []struct {
		name   string
		window int
		frac   float64
		want   int
	}{
		{"frac 0 keeps old line", 128_000, 0, 95_000},
		{"1M window frac .5 → 500K", 1_000_000, 0.5, 500_000},
		{"80K window frac .5 → 40K", 80_000, 0.5, 40_000},
		{"frac too high falls back to old line", 80_000, 0.99, 47_000},
	}
	for _, tc := range cases {
		c := Config{Window: tc.window, CompactFrac: tc.frac}
		if got := c.CompactLine(); got != tc.want {
			t.Fatalf("%s: CompactLine = %d, want %d", tc.name, got, tc.want)
		}
	}
	c := Config{Window: 80_000, CompactFrac: 0.5}
	if !(c.ReminderLine() < c.CompactLine() && c.CompactLine() < c.BlockLine()) {
		t.Fatalf("line order broken: remind=%d compact=%d block=%d",
			c.ReminderLine(), c.CompactLine(), c.BlockLine())
	}
}

// 摘要提示词选择：空值用编码口径；CompanionPrompt 含纠正优先规则；
// 自定义文本含 %d 时填入摘要预算。
func TestSystemPromptVariants(t *testing.T) {
	coding := (&Compactor{cfg: Config{Window: 128_000}}).systemPrompt("")
	if !strings.Contains(coding, "Key files and identifiers") {
		t.Fatal("default prompt should keep the coding-agent sections")
	}
	comp := (&Compactor{cfg: Config{Window: 128_000, Prompt: CompanionPrompt}}).systemPrompt("")
	for _, want := range []string{"[correction]", "Corrections come first", "verbatim"} {
		if !strings.Contains(comp, want) {
			t.Fatalf("companion prompt missing %q", want)
		}
	}
	if strings.Contains(comp, "Key files and identifiers") {
		t.Fatal("companion prompt must not reuse coding sections")
	}
	custom := (&Compactor{cfg: Config{Window: 128_000, Prompt: "budget %d"}}).systemPrompt("")
	if !strings.Contains(custom, "budget 8000") {
		t.Fatalf("%%d should expand to summaryBudget, got %q", custom)
	}
}

// 首部钉住可配：HeadMessages<0 时中段从第一条非 system 消息开始，
// 不再永久钉住前 3 条真实用户消息。
func TestSegmentNoHead(t *testing.T) {
	c := &Compactor{cfg: Config{Window: 128_000, Auto: true, HeadMessages: -1}}
	view := chatOf(50, "ok", "done")
	s := c.segment(view, false)
	if len(s.head) != 0 {
		t.Fatalf("head should be empty with HeadMessages=-1, got %v", s.head)
	}
	if len(s.middle) == 0 || view[s.middle[0]].Content != "question 1" {
		t.Fatalf("middle must start at the first non-system message: %v",
			viewContents(pick(view, s.middle[:1])))
	}
	if len(s.system)+len(s.head)+len(s.middle)+len(s.tail) != len(view) {
		t.Fatal("segments must partition the view")
	}
}

// 分块滚动摘要：中段超限时不丢事件——LLM 调用次数 = 块数，
// 每个事件恰好进入一个块，最终摘要来自最后一块的输出。
func TestSummarizeChunksRollOver(t *testing.T) {
	fake := &fakeLLM{}
	c, st, ctx := newTestCompactor(t, fake)
	// 60 条大 user 消息：渲染后不裁剪（user 不 clip），中段 ~180K token，
	// 远超单块预算，必然多块。floor 切分让尾部只留最后一条。
	msgs := []agent.Message{{Role: agent.RoleSystem, Content: "sys"}}
	for n := 1; n <= 60; n++ {
		msgs = append(msgs, agent.Message{Role: agent.RoleUser,
			Content: fmt.Sprintf("event %d %s", n, big(12_000))})
	}
	msgs = append(msgs, agent.Message{Role: agent.RoleUser, Content: "now?"})
	view := recordAll(t, st, ctx, msgs...)
	seg := c.segment(view, true)
	out, err := c.summarize(ctx, view, seg, "auto", "")
	if err != nil {
		t.Fatal(err)
	}
	var events []agent.Message
	for _, j := range seg.middle {
		events = append(events, view[j])
	}
	chunks := len(c.splitEvents(events))
	if chunks < 2 {
		t.Fatalf("test setup: middle should exceed one chunk, got %d", chunks)
	}
	if fake.calls() != chunks {
		t.Fatalf("LLM calls = %d, want one per chunk (%d)", fake.calls(), chunks)
	}
	// 第 2 块起的输入必须携带上一块产出的 EXISTING SUMMARY。
	if !strings.Contains(fake.inputs[1], "summary #1") {
		t.Fatal("chunk 2 input must carry chunk 1's summary as EXISTING SUMMARY")
	}
	// 最终摘要来自最后一块。
	summMsg := out[len(seg.system)+len(seg.head)]
	if summMsg.Kind != agent.KindSummary ||
		!strings.Contains(summMsg.Content, "summary #"+fmt.Sprint(chunks)) {
		t.Fatalf("final summary should come from the last chunk, got %q", summMsg.Content)
	}
}

// renderInput 兜底：单条超大事件独占一块时不再砍半丢事件，而是裁剪成品文本。
func TestRenderInputClipsOversizedEvent(t *testing.T) {
	c := &Compactor{cfg: Config{Window: MinWindow}}
	in, err := c.renderInput("", []agent.Message{
		{Role: agent.RoleUser, Content: big(200_000)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(in, "[truncated to fit window]") {
		t.Fatal("oversized input should be clipped, not dropped")
	}
	if TextTokens(in) > c.inputLimit()+8 {
		t.Fatalf("clipped input = %d tokens > limit %d", TextTokens(in), c.inputLimit())
	}
}
