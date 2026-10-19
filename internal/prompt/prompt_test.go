package prompt

import (
	"strings"
	"testing"

	"runeharness/internal/tools"
)

func TestBuildSectionsInStableOrder(t *testing.T) {
	got := Build(Env{
		Tools:     []tools.Spec{{Name: "read_file", Description: "Read a file"}, {Name: "run_command", Description: "Run a command"}},
		Skills:    "- s: desc (/skills/s/SKILL.md)",
		Workspace: "/repo",
		Platform:  "darwin/arm64",
	})
	// 按变化频率从低到高排列：前缀越稳定，prompt cache 命中越长。
	order := []string{Identity, "## 工具", "- read_file: Read a file", "- run_command: Run a command", "## 技能", "/skills/s/SKILL.md", "## 工作区", "/repo", "darwin/arm64"}
	pos := -1
	for _, want := range order {
		i := strings.Index(got, want)
		if i < 0 {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
		if i < pos {
			t.Fatalf("%q out of order in:\n%s", want, got)
		}
		pos = i
	}
}

func TestBuildOmitsEmptySkills(t *testing.T) {
	got := Build(Env{Tools: []tools.Spec{{Name: "t"}}, Workspace: "/repo"})
	if strings.Contains(got, "## 技能") {
		t.Fatalf("empty skills should omit section:\n%s", got)
	}
}

// bot prompt：人设即身份（不背 Identity 段），MemoryBody 进入 ## 记忆 段。
func TestBuildBotCarriesMemoryBody(t *testing.T) {
	out := BuildBot(Env{
		Soul:        "大大熊人格",
		Tools:       []tools.Spec{{Name: "memory", Description: "操作记忆"}},
		MemoryGuide: "记忆由系统整理，不需手动维护。",
		MemoryBody:  "<memory-index>...</memory-index>\n<memory name=\"reply-style\">温柔回复</memory>",
	})
	if strings.Contains(out, "coding agent") || strings.Contains(out, "<约定>") {
		t.Fatal("bot prompt must not carry the coding-agent Identity")
	}
	for _, want := range []string{"大大熊人格", "记忆由系统整理", "reply-style"} {
		if !strings.Contains(out, want) {
			t.Fatalf("prompt missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "reply-style") < strings.Index(out, "## 记忆") {
		t.Fatal("memory body must sit inside the 记忆 section")
	}
}
