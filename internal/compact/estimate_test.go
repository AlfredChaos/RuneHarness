package compact

import (
	"encoding/json"
	"strings"
	"testing"

	"runeharness/internal/agent"
)

func TestTextTokensCJKAndASCII(t *testing.T) {
	if got := TextTokens(strings.Repeat("a", 400)); got != 100 {
		t.Fatalf("ascii 400 chars = %d, want 100", got)
	}
	if got := TextTokens(strings.Repeat("中", 100)); got != 70 {
		t.Fatalf("cjk 100 runes = %d, want 70", got)
	}
}

// 锚点之后才粗估；Thinking 不计入。
func TestEstimateUsesAnchor(t *testing.T) {
	hist := []agent.Message{
		{Role: agent.RoleUser, Content: strings.Repeat("x", 4000)},
		{Role: agent.RoleAssistant, Content: "ok", Thinking: strings.Repeat("t", 40000),
			Usage: &agent.Usage{Prompt: 1000, Completion: 50}},
		{Role: agent.RoleTool, Content: strings.Repeat("y", 400)},
	}
	if got, want := Estimate(hist, 500), 1000+50+msgOverhead+100; got != want {
		t.Fatalf("Estimate = %d, want %d", got, want)
	}
}

// 无锚点：全量粗估 + overhead，再加 10% 余量；tool_calls 参数计入。
func TestEstimateWithoutAnchor(t *testing.T) {
	hist := []agent.Message{
		{Role: agent.RoleAssistant, Thinking: strings.Repeat("t", 4000), ToolCalls: []agent.ToolCall{
			{Name: "run", Arguments: json.RawMessage(`"` + strings.Repeat("z", 398) + `"`)},
		}},
	}
	rough := msgOverhead + TextTokens("run") + 100
	if got, want := Estimate(hist, 100), (100+rough)*11/10; got != want {
		t.Fatalf("Estimate = %d, want %d", got, want)
	}
}

// document 分片的 Text 会计入粗估：一整份 PDF（数万字）如果不可见，
// 首轮带大 PDF 时 autocompact 的预估会漏掉最大头。image 的 DataB64 不计
// （视觉 token 成本与字符数无关，沿用既有低估语义）。
func TestRoughCountsDocumentText(t *testing.T) {
	msgs := []agent.Message{{Role: agent.RoleUser, Content: "hi", Parts: []agent.Part{
		{Kind: agent.PartDocument, Text: strings.Repeat("a", 800)},
		{Kind: agent.PartImage, DataB64: strings.Repeat("x", 40000)},
	}}}
	if got, want := Rough(msgs), msgOverhead+1+200; got != want {
		t.Fatalf("Rough = %d, want %d (content + doc text only)", got, want)
	}
}
