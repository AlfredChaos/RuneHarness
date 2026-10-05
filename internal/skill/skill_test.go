package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, root, dir, content string) {
	t.Helper()
	d := filepath.Join(root, dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, manifestName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanMissingDir(t *testing.T) {
	if got := Scan(filepath.Join(t.TempDir(), "nonexistent")); got != nil {
		t.Fatalf("Scan(nonexistent) = %v, want nil", got)
	}
}

func TestScanParsesFrontmatter(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "code-review", `---
name: code-review
description: Review diffs for bugs and style issues
---
# Code Review
body here`)
	got := Scan(root)
	if len(got) != 1 {
		t.Fatalf("Scan returned %d skills, want 1", len(got))
	}
	s := got[0]
	if s.Name != "code-review" || s.Description != "Review diffs for bugs and style issues" {
		t.Fatalf("Meta = %+v", s)
	}
	want := filepath.Join(root, "code-review", manifestName)
	if s.Path != want {
		t.Fatalf("Path = %q, want %q", s.Path, want)
	}
}

func TestScanFallbacks(t *testing.T) {
	root := t.TempDir()
	// 无 frontmatter：name 退回目录名，desc 退回正文首个非空行（剥 #）
	writeSkill(t, root, "plain", "\n# Legacy Guide\nstep 1\n")
	// frontmatter 只写一半：缺的字段各自走兜底
	writeSkill(t, root, "half", `---
description: "quoted desc"
---
body`)
	got := Scan(root)
	if len(got) != 2 {
		t.Fatalf("Scan returned %d skills, want 2", len(got))
	}
	if got[0].Name != "half" || got[0].Description != "quoted desc" {
		t.Fatalf("half = %+v", got[0])
	}
	if got[1].Name != "plain" || got[1].Description != "Legacy Guide" {
		t.Fatalf("plain = %+v", got[1])
	}
}

func TestScanFollowsSymlinkedDirs(t *testing.T) {
	root := t.TempDir()
	// 真身放在 skills 目录之外，目录里只放符号链接——
	// ~/.agents/skills 集中管理外部技能的常见形态。
	outside := t.TempDir()
	writeSkill(t, outside, "linked", "---\ndescription: via symlink\n---\nbody")
	if err := os.Symlink(filepath.Join(outside, "linked"), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	// 坏链只跳过不报错
	if err := os.Symlink(filepath.Join(outside, "gone"), filepath.Join(root, "broken")); err != nil {
		t.Fatal(err)
	}
	got := Scan(root)
	if len(got) != 1 || got[0].Name != "linked" || got[0].Description != "via symlink" {
		t.Fatalf("Scan = %+v, want only the linked skill", got)
	}
	// Path 走链接侧路径，read_file 可正常穿透
	want := filepath.Join(root, "linked", manifestName)
	if got[0].Path != want {
		t.Fatalf("Path = %q, want %q", got[0].Path, want)
	}
}

func TestScanSkipsNonSkills(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "ok", "---\ndescription: fine\n---\nbody")
	// 子目录里没有 SKILL.md
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 目录顶层散落的文件不是技能
	if err := os.WriteFile(filepath.Join(root, "stray.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := Scan(root)
	if len(got) != 1 || got[0].Name != "ok" {
		t.Fatalf("Scan = %+v, want only the ok skill", got)
	}
}

func TestParseMetaIgnoresNestedAndBlockScalars(t *testing.T) {
	// 缩进的 name 属于 metadata 子结构，不是顶层字段；
	// ">" 是 YAML 折叠标量标记，按缺省处理而非吃掉 ">"。
	m := parseMeta(`---
metadata:
  name: nested-trap
description: >
  folded text
---
Real body line`, "dir-name")
	if m.Name != "dir-name" {
		t.Fatalf("Name = %q, want dir fallback (nested name ignored)", m.Name)
	}
	if m.Description != "Real body line" {
		t.Fatalf("Description = %q, want body fallback", m.Description)
	}
}

func TestCatalog(t *testing.T) {
	if got := Catalog(nil); got != "" {
		t.Fatalf("Catalog(nil) = %q, want empty", got)
	}
	skills := []Meta{
		{Name: "a", Description: "desc a", Path: "/skills/a/SKILL.md"},
		{Name: "b", Description: "desc b", Path: "/skills/b/SKILL.md"},
	}
	got := Catalog(skills)
	for _, want := range []string{"read_file", "a: desc a", "/skills/b/SKILL.md"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Catalog missing %q in:\n%s", want, got)
		}
	}
}
