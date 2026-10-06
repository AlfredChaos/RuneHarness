package tui

import (
	"strings"
	"testing"

	"runeharness/internal/agent"
	"runeharness/internal/scope"
	"runeharness/internal/tools"

	"github.com/charmbracelet/x/ansi"
)

func TestExpandMermaidRendersSupportedDiagram(t *testing.T) {
	got := expandMermaid("before\n```mermaid\ngraph LR\nA --> B\n```\nafter", 80)
	if strings.Contains(got, "```mermaid") || strings.Contains(got, "A --> B") {
		t.Fatalf("mermaid source not replaced:\n%s", got)
	}
	for _, want := range []string{"before", "after", "```text", "A", "B"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

func TestExpandMermaidKeepsSourceOnFailure(t *testing.T) {
	for name, in := range map[string]string{
		"unsupported": "```mermaid\npie title x\n\"a\" : 1\n```",
		"unclosed":    "```mermaid\ngraph LR\nA --> B",
	} {
		if got := expandMermaid(in, 80); got != in {
			t.Errorf("%s: got\n%s\nwant source kept", name, got)
		}
	}
}

// 最终回复按 Markdown 渲染：强调标记被消化，不以原始语法出现在对话区。
func TestFinalMessageRendersMarkdown(t *testing.T) {
	m := New(agent.New(nil, tools.NewRegistry(), 1), Info{Model: "m"}, []agent.Message{{Role: agent.RoleSystem, Content: "sys"}}, scope.Scope{}, nil, nil, nil)
	m.Update(turnDoneMsg{history: []agent.Message{{Role: agent.RoleAssistant, Content: "# Title\n\n**bold** text"}}})
	plain := ansi.Strip(m.transcript)
	if strings.Contains(plain, "**bold**") || strings.Contains(plain, "# Title") {
		t.Fatalf("markdown syntax leaked into transcript:\n%s", plain)
	}
	if !strings.Contains(plain, "bold") {
		t.Fatalf("content lost:\n%s", m.transcript)
	}
}
