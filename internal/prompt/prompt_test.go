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
