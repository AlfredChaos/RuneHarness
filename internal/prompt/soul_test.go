package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"runeharness/internal/tools"
)

func TestLoadSoul(t *testing.T) {
	// env 未设：空串，不算错误。
	if got := LoadSoul(""); got != "" {
		t.Fatalf("empty path should give empty soul, got %q", got)
	}
	// 文件不存在：跳过，不算错误。
	if got := LoadSoul(filepath.Join(t.TempDir(), "missing.md")); got != "" {
		t.Fatalf("missing file should give empty soul, got %q", got)
	}
	p := filepath.Join(t.TempDir(), "SOUL.md")
	if err := os.WriteFile(p, []byte("# Soul\n说话直接，不绕弯。\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := LoadSoul(p), "# Soul\n说话直接，不绕弯。\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// 全空白文件视同缺失。
	if err := os.WriteFile(p, []byte("  \n\t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LoadSoul(p); got != "" {
		t.Fatalf("blank file should give empty soul, got %q", got)
	}
}

func TestBuildSoulAfterIdentityBeforeTools(t *testing.T) {
	got := Build(Env{
		Soul:      "你是 bear。",
		Tools:     []tools.Spec{{Name: "read_file", Description: "Read a file"}},
		Workspace: "/repo",
		Platform:  "darwin/arm64",
	})
	// SOUL 固定在 identity 段之后、tools 段之前（设计 §6.2），保持前缀稳定。
	order := []string{Identity, "## 人设", "你是 bear。", "## 工具", "- read_file"}
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

func TestBuildOmitsEmptySoul(t *testing.T) {
	got := Build(Env{Tools: []tools.Spec{{Name: "t"}}, Workspace: "/repo"})
	if strings.Contains(got, "## 人设") {
		t.Fatalf("empty soul should omit section:\n%s", got)
	}
}
