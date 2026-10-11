package tui

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"runeharness/internal/agent"
	"runeharness/internal/scope"
	"runeharness/internal/skill"
	"runeharness/internal/tools"
)

// newFileModel 建一个 workspace 指向临时目录的 Model。
func newFileModel(t *testing.T) (*Model, string) {
	t.Helper()
	ws := t.TempDir()
	m := New(agent.New(nil, tools.NewRegistry(), 1), Info{Model: "m"},
		[]agent.Message{{Role: agent.RoleSystem, Content: "sys"}},
		scope.Scope{TenantID: "t", Workspace: ws, SessionID: "s"},
		nil, []skill.Meta{}, nil, nil, nil)
	return m, ws
}

func seedFiles(t *testing.T, ws string, names ...string) {
	t.Helper()
	for _, n := range names {
		p := filepath.Join(ws, filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("content of "+n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// atToken 提取：行首/空白后的 @ 起 token；email 与已闭合 token 不触发。
func TestAtToken(t *testing.T) {
	cases := []struct {
		v      string
		cursor int // -1 表示串尾
		want   bool
		query  string
		end    int // rune 偏移
	}{
		{"@", -1, true, "", 1},
		{"@fo", -1, true, "fo", 3},
		{"see @fo", -1, true, "fo", 7},
		{"see @fo and", 7, true, "fo", 7},  // 光标在 token 尾
		{"see @fo and x", 6, true, "f", 7}, // 光标在 token 中，end 延伸到空白
		{"a@b", -1, false, "", 0},          // email
		{"see @fo ", -1, false, "", 0},     // 空白截断
		{`@"my file`, -1, true, "my file", 9},
		{`@"my file" x`, -1, false, "", 0}, // 引号已闭合
		{`pre @"a b`, -1, true, "a b", 9},
		{`@"my file" x`, 5, true, "my ", 10}, // 引号内光标：end 含收尾引号
	}
	for _, c := range cases {
		cursor := c.cursor
		if cursor < 0 {
			cursor = len([]rune(c.v))
		}
		tok, ok := atToken(c.v, cursor)
		if ok != c.want {
			t.Errorf("atToken(%q, %d) ok=%v want %v", c.v, cursor, ok, c.want)
			continue
		}
		if ok && (tok.query != c.query || tok.end != c.end) {
			t.Errorf("atToken(%q, %d) = %+v want query=%q end=%d",
				c.v, cursor, tok, c.query, c.end)
		}
	}
}

// 空查询列顶层条目；非空查询按路径/基名过滤，目录项带 "/" 可下钻。
func TestFileSuggestions(t *testing.T) {
	m, ws := newFileModel(t)
	seedFiles(t, ws, "main.go", "internal/tui/model.go", "internal/tui/view.go",
		"internal/agent/agent.go", "README.md")
	// 非 git 目录走 WalkDir；清缓存确保拾到刚写的文件
	m.fileIndex, m.fileIndexAt = nil, time.Time{}

	top, _ := m.computeSuggestions("@", 1)
	var topLabels []string
	for _, s := range top {
		topLabels = append(topLabels, s.label)
	}
	for _, want := range []string{"main.go", "README.md", "internal/"} {
		if !contains(topLabels, want) {
			t.Errorf("top-level missing %q in %v", want, topLabels)
		}
	}

	list, _ := m.computeSuggestions("@model", 6)
	if len(list) != 1 || list[0].label != "internal/tui/model.go" {
		t.Fatalf("@model = %v", labelsOf(list))
	}
	list, _ = m.computeSuggestions("@internal/", 10)
	got := labelsOf(list)
	if !contains(got, "internal/tui/") || !contains(got, "internal/agent/") {
		t.Errorf("@internal/ = %v, want dir entries", got)
	}
	if list, note := m.computeSuggestions("@zzzzzzz", 8); list != nil || note != "" {
		t.Error("no-match should not block Enter with a placeholder note")
	}
}

// 文件项 Tab/Enter 都只补全不提交；含空格路径自动加引号；目录补全成 "@dir/"。
func TestApplyFileCompletion(t *testing.T) {
	m, ws := newFileModel(t)
	seedFiles(t, ws, "my file.go", "src/a.go")
	m.fileIndex, m.fileIndexAt = nil, time.Time{}

	typeKeys(m, "see @a")
	list, _ := m.computeSuggestions(m.input.Value(), m.cursorOffset())
	if len(list) != 1 {
		t.Fatalf("suggs=%v", labelsOf(list))
	}
	m.suggs = list
	m.applySuggestion(false) // Tab
	if got := m.input.Value(); got != "see @src/a.go " {
		t.Fatalf("after tab: %q", got)
	}
	if m.cursorOffset() != len([]rune(m.input.Value())) {
		t.Error("cursor should sit right after the completion")
	}

	m2, ws2 := newFileModel(t)
	seedFiles(t, ws2, "my file.go")
	m2.fileIndex, m2.fileIndexAt = nil, time.Time{}
	typeKeys(m2, "@my")
	l, _ := m2.computeSuggestions(m2.input.Value(), m2.cursorOffset())
	m2.suggs = l
	m2.applySuggestion(true) // Enter 同样只补全
	if got := m2.input.Value(); got != `@"my file.go" ` {
		t.Fatalf("quoted completion: %q", got)
	}
}

// Enter 在文件建议打开时只补全不提交（不走 submit）。
func TestFileSuggestionEnterDoesNotSubmit(t *testing.T) {
	m, ws := newFileModel(t)
	seedFiles(t, ws, "a.go")
	m.fileIndex, m.fileIndexAt = nil, time.Time{}
	typeKeys(m, "@a")
	m.updateSuggestions()
	if len(m.suggs) == 0 {
		t.Fatal("expected file suggestions")
	}
	m.onKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.busy {
		t.Error("enter on file suggestion must not submit")
	}
	if got := m.input.Value(); got != "@a.go " {
		t.Fatalf("input=%q", got)
	}
}

// 目录项补全为 "@dir/" 并立即出下一级建议。
func TestDirCompletionDrillsDown(t *testing.T) {
	m, ws := newFileModel(t)
	seedFiles(t, ws, "src/deep/x.go")
	m.fileIndex, m.fileIndexAt = nil, time.Time{}
	typeKeys(m, "@s")
	list, _ := m.computeSuggestions(m.input.Value(), m.cursorOffset())
	var dir suggestion
	for _, s := range list {
		if s.isDir && s.label == "src/" {
			dir = s
		}
	}
	if dir.id == "" {
		t.Fatalf("no src/ dir suggestion in %v", labelsOf(list))
	}
	m.suggs = list
	m.applySuggestion(false)
	if got := m.input.Value(); got != "@src/" {
		t.Fatalf("input=%q", got)
	}
	m.updateSuggestions()
	got := labelsOf(m.suggs)
	if !contains(got, "src/deep/") {
		t.Errorf("after dir completion suggs=%v, want src/deep/", got)
	}
}

// 含空格的目录补全为 @"dir/"，光标留在引号内可继续出下级建议。
func TestQuotedDirCompletionDrillsDown(t *testing.T) {
	m, ws := newFileModel(t)
	seedFiles(t, ws, "my dir/x.go")
	m.fileIndex, m.fileIndexAt = nil, time.Time{}
	// 未加引号的 token 遇空格即终止；带空格的目录得从短查询里选出来。
	typeKeys(m, "@my")
	list, _ := m.computeSuggestions(m.input.Value(), m.cursorOffset())
	var dir suggestion
	for _, s := range list {
		if s.isDir && s.label == "my dir/" {
			dir = s
		}
	}
	if dir.id == "" {
		t.Fatalf("no 'my dir/' suggestion in %v", labelsOf(list))
	}
	m.suggs = list
	m.applySuggestion(false)
	if got := m.input.Value(); got != `@"my dir/"` {
		t.Fatalf("input=%q", got)
	}
	m.updateSuggestions()
	if got := labelsOf(m.suggs); !contains(got, "my dir/x.go") {
		t.Errorf("inside quoted dir suggs=%v", got)
	}
}

// parseMentions 提取两种形态并去重；email 不算。
func TestParseMentions(t *testing.T) {
	got := parseMentions(`read @a.go and @"dir/b file.go", mail me@x.com, @a.go again @c/`)
	want := []string{"a.go", "dir/b file.go", "c/"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

// 提交时 @path 展开为 <attachments> 内容块；逃逸路径与缺失文件内联报错。
func TestExpandMentions(t *testing.T) {
	m, ws := newFileModel(t)
	seedFiles(t, ws, "a.go")

	out, _, labels := m.expandMentions("check @a.go and @missing.go")
	if !strings.Contains(out, "<attachments>") || !strings.Contains(out, "content of a.go") {
		t.Fatalf("expanded:\n%s", out)
	}
	if !strings.Contains(out, `path="missing.go"`) || !strings.Contains(out, "[error:") {
		t.Errorf("missing file should inline error:\n%s", out)
	}
	if len(labels) != 2 || labels[0] != "a.go" {
		t.Errorf("labels=%v", labels)
	}

	// workspace 外路径放行：用户手敲的绝对路径可附
	outside := t.TempDir()
	op := filepath.Join(outside, "ext.txt")
	if err := os.WriteFile(op, []byte("external"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _, _ = m.expandMentions("see @" + op)
	if !strings.Contains(out, "external") {
		t.Errorf("outside-workspace mention should attach:\n%s", out)
	}

	// 无 @ 不变；空 workspace 不展开
	if o, _, l := m.expandMentions("plain text"); o != "plain text" || l != nil {
		t.Error("no mentions should pass through")
	}
	m.sc.Workspace = ""
	if o, _, l := m.expandMentions("hi @a.go"); o != "hi @a.go" || l != nil {
		t.Error("empty workspace should pass through")
	}
}

// 目录提及展开为递归清单。
func TestExpandDirMention(t *testing.T) {
	m, ws := newFileModel(t)
	seedFiles(t, ws, "pkg/a.go", "pkg/sub/b.go")
	out, _, labels := m.expandMentions("see @pkg")
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "sub/b.go") {
		t.Fatalf("dir listing:\n%s", out)
	}
	if labels[0] != "pkg/ (2 entries)" {
		t.Errorf("label=%q", labels[0])
	}
}

// 附件块在回放时被剥离，只留计数。
func TestSplitAttachments(t *testing.T) {
	text, n := splitAttachments("ask\n\n<attachments>\n<file path=\"a\">\nx\n</file>\n<file path=\"b\">\ny\n</file>\n</attachments>")
	if text != "ask" || n != 2 {
		t.Fatalf("text=%q n=%d", text, n)
	}
	if text, n := splitAttachments("plain"); text != "plain" || n != 0 {
		t.Error("no block passes through")
	}
}

// tinyPNGB64 是 1x1 透明 PNG（魔数可被 DetectContentType 识别为 image/png）。
const tinyPNGB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

func pngBytes(t *testing.T) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(tinyPNGB64)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 图片提及产出 image Part：不进 <file> 块、base64 不入文本，标签留 "(image)"；
// 与文本提及混用时只有文本文件进附件块。
func TestExpandImageMention(t *testing.T) {
	m, ws := newFileModel(t)
	seedFiles(t, ws, "a.go")
	if err := os.WriteFile(filepath.Join(ws, "pic.png"), pngBytes(t), 0o644); err != nil {
		t.Fatal(err)
	}
	out, parts, labels := m.expandMentions("look @pic.png and @a.go")
	if len(parts) != 1 || parts[0].Kind != agent.PartImage || parts[0].MediaType != "image/png" {
		t.Fatalf("parts=%+v", parts)
	}
	if parts[0].Filename != "pic.png" || parts[0].DataB64 == "" {
		t.Errorf("part=%+v", parts[0])
	}
	if strings.Contains(out, parts[0].DataB64) {
		t.Error("base64 must not leak into text content")
	}
	if !strings.Contains(out, `path="a.go"`) || strings.Contains(out, `path="pic.png"`) {
		t.Errorf("only text mention should enter block:\n%s", out)
	}
	if len(labels) != 2 || labels[0] != "pic.png (image)" {
		t.Errorf("labels=%v", labels)
	}

	// 纯图片提及：不产 <attachments> 块
	out, parts, _ = m.expandMentions("look @pic.png")
	if len(parts) != 1 || strings.Contains(out, "<attachments>") {
		t.Errorf("image-only mention should produce no text block:\n%s", out)
	}
}

// 嗅探不出魔数的图片按扩展名兜底（改名图片）；头部含 NUL 的非图片仍按
// 二进制略过，不吃扩展名兜底。
func TestImageExtensionFallback(t *testing.T) {
	m, ws := newFileModel(t)
	// 0xFF 非法 UTF-8、无 NUL：嗅探为 octet-stream，.png 扩展兜底命中
	junk := bytes.Repeat([]byte{0xFF}, 600)
	if err := os.WriteFile(filepath.Join(ws, "photo.png"), junk, 0o644); err != nil {
		t.Fatal(err)
	}
	_, parts, labels := m.expandMentions("see @photo.png")
	if len(parts) != 1 || parts[0].MediaType != "image/png" {
		t.Fatalf("parts=%+v", parts)
	}
	if labels[0] != "photo.png (image)" {
		t.Errorf("label=%q", labels[0])
	}

	// 含 NUL 的同名字件不兜底：按二进制略过
	if err := os.WriteFile(filepath.Join(ws, "dark.png"), append([]byte{0}, junk...), 0o644); err != nil {
		t.Fatal(err)
	}
	_, parts, labels = m.expandMentions("see @dark.png")
	if len(parts) != 0 || !strings.Contains(labels[0], "(binary)") {
		t.Fatalf("NUL file should stay binary: parts=%v label=%q", parts, labels[0])
	}
}

// pdfBytes 拼一个含一行文本的最小 PDF（xref 偏移按实际字节计算）。
func pdfBytes(t *testing.T) []byte {
	t.Helper()
	stream := "BT /F1 24 Tf 72 720 Td (Hello Rune PDF) Tj ET"
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var b bytes.Buffer
	var offs []int
	b.WriteString("%PDF-1.4\n")
	for i, o := range objs {
		offs = append(offs, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offs {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

// PDF 提及抽出文本走正文路径：文本落入附件块、不产 Part（无 pdftotext
// 的裸 CI 跳过）。
func TestPDFMentionExtraction(t *testing.T) {
	if _, err := exec.LookPath("pdftotext"); err != nil {
		t.Skip("pdftotext not installed")
	}
	m, ws := newFileModel(t)
	if err := os.WriteFile(filepath.Join(ws, "doc.pdf"), pdfBytes(t), 0o644); err != nil {
		t.Fatal(err)
	}
	out, parts, labels := m.expandMentions("read @doc.pdf")
	if len(parts) != 0 {
		t.Fatalf("pdf should not produce parts: %+v", parts)
	}
	if !strings.Contains(out, `path="doc.pdf"`) || !strings.Contains(out, "Hello Rune PDF") {
		t.Fatalf("pdf text not extracted:\n%s", out)
	}
	if labels[0] != "doc.pdf" {
		t.Errorf("label=%q", labels[0])
	}
}

// 两个抽取器都缺位时内联 not installed 标记（PATH 指向空目录）。
func TestPDFExtractorMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	m, ws := newFileModel(t)
	if err := os.WriteFile(filepath.Join(ws, "doc.pdf"), pdfBytes(t), 0o644); err != nil {
		t.Fatal(err)
	}
	out, parts, _ := m.expandMentions("read @doc.pdf")
	if len(parts) != 0 || !strings.Contains(out, "[error: pdftotext not installed]") {
		t.Fatalf("want not-installed marker:\n%s", out)
	}
}

// 超 10MB 的图片不产 Part，内联超限标记。
func TestImageOverCap(t *testing.T) {
	m, ws := newFileModel(t)
	big := append(pngBytes(t), make([]byte, attachImageCap-len(pngBytes(t))+1)...)
	if err := os.WriteFile(filepath.Join(ws, "big.png"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	out, parts, labels := m.expandMentions("see @big.png")
	if len(parts) != 0 {
		t.Fatal("oversize image should not produce parts")
	}
	if !strings.Contains(out, "[error: image exceeds 10MB limit]") {
		t.Fatalf("want size marker:\n%s", out)
	}
	if labels[0] != "big.png (image too large)" {
		t.Errorf("label=%q", labels[0])
	}
}

// 回放时图片 Part 只渲染占位标记，base64 绝不进对话区。
func TestReplayPartsMarker(t *testing.T) {
	m, _ := newFileModel(t)
	m.replayHistory([]agent.Message{{
		Role:    agent.RoleUser,
		Content: "look",
		Parts:   []agent.Part{{Kind: agent.PartImage, MediaType: "image/png", Filename: "pic.png", DataB64: "QUJD"}},
	}})
	if strings.Contains(m.transcript, "QUJD") {
		t.Fatal("base64 leaked into transcript")
	}
	if !strings.Contains(m.transcript, "[image: pic.png]") {
		t.Fatalf("no image marker:\n%s", m.transcript)
	}
}

// busy 时 Esc 取消当前轮：cancel 被调用、状态栏显示 interrupting，
// 进程不退出（等 turnDoneMsg 收尾）。
func TestEscInterruptsBusyTurn(t *testing.T) {
	m, _ := newFileModel(t)
	cancelled := false
	m.busy = true
	m.cancel = func() { cancelled = true }

	if _, cmd := m.onKey(tea.KeyMsg{Type: tea.KeyEsc}); cmd != nil {
		t.Fatal("esc should not produce a quit cmd")
	}
	if !cancelled {
		t.Fatal("esc during busy must call m.cancel")
	}
	if m.activity != "interrupting" {
		t.Errorf("activity = %q, want interrupting", m.activity)
	}

	// 其他键仍被忽略，不触发第二次取消
	cancelled = false
	m.onKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if cancelled {
		t.Error("non-esc key should not cancel")
	}
}

func labelsOf(list []suggestion) []string {
	var out []string
	for _, s := range list {
		out = append(out, s.label)
	}
	return out
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
