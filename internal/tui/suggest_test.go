package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"runeharness/internal/agent"
	"runeharness/internal/scope"
	"runeharness/internal/session"
	"runeharness/internal/session/sqlite"
	"runeharness/internal/skill"
	"runeharness/internal/tools"
)

func newSuggModel(st session.Store, skills []skill.Meta) *Model {
	return New(agent.New(nil, tools.NewRegistry(), 1), Info{Model: "m"},
		[]agent.Message{{Role: agent.RoleSystem, Content: "sys"}},
		scope.Scope{TenantID: "t", Workspace: "/w", SessionID: "cur-session"},
		st, skills)
}

// typeKeys 逐字符把文本敲进输入框（走真实 onKey 路径）。
func typeKeys(m *Model, s string) {
	for _, r := range s {
		m.onKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func seedSessions(t *testing.T, st session.Store, ctx context.Context, metas []session.Meta) []session.Session {
	t.Helper()
	var out []session.Session
	for _, meta := range metas {
		s, err := st.CreateSession(ctx, meta)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// "/" 触发命令+技能统一列表；继续输入按名称前缀过滤。
func TestCommandSuggestions(t *testing.T) {
	m := newSuggModel(nil, []skill.Meta{
		{Name: "review", Description: "code review"},
		{Name: "commit", Description: "git commit"},
	})

	got := func(v string) []string {
		list, _ := m.computeSuggestions(v, len([]rune(v)))
		var names []string
		for _, s := range list {
			names = append(names, s.label)
		}
		return names
	}

	all := got("/")
	if len(all) != 4+2 {
		t.Fatalf("/ = %v, want 4 commands + 2 skills", all)
	}
	if all[0] != "/exit" || all[1] != "/resume" || all[2] != "/todo" || all[3] != "/compact" {
		t.Fatalf("commands should come first in registry order, got %v", all)
	}

	// /compact 的描述 "context" 里有子串 "ex"：前缀命中排在描述子串命中之前
	if got := got("/ex"); len(got) != 2 || got[0] != "/exit" || got[1] != "/compact" {
		t.Fatalf("/ex = %v, want [/exit /compact]", got)
	}
	if got := got("/res"); len(got) != 1 || got[0] != "/resume" {
		t.Fatalf("/res = %v, want [/resume]", got)
	}
	// 技能按名匹配进入列表；前缀命中排在描述子串命中之前
	// （"revi" 在 resume 的描述 "previous" 里也有子串命中）。
	if got := got("/revi"); len(got) != 2 || got[0] != "/review" || got[1] != "/resume" {
		t.Fatalf("/revi = %v, want [/review /resume]", got)
	}
	// 别名命中主命令
	if got := got("/qu"); len(got) != 1 || got[0] != "/exit" {
		t.Fatalf("/qu = %v, want [/exit] via alias quit", got)
	}
	// 非 /resume 命令带参数位后不再出建议
	if list, _ := m.computeSuggestions("/todo x", 7); list != nil {
		t.Fatalf("/todo x should give no suggestions, got %v", list)
	}
	// 多行输入不出建议
	if list, _ := m.computeSuggestions("/a\nb", 4); list != nil {
		t.Fatalf("multiline input should give no suggestions")
	}
}

// 无匹配时下拉给出占位提示而不是消失。
func TestSuggestionNote(t *testing.T) {
	m := newSuggModel(nil, nil)
	_, note := m.computeSuggestions("/zzzzz", 6)
	if note == "" {
		t.Fatal("want a no-match note")
	}
}

// 选中项经 Enter 应用：无参命令直接执行、带参命令补全前缀。
func TestApplySuggestion(t *testing.T) {
	m := newSuggModel(nil, nil)
	typeKeys(m, "/resu")
	if len(m.suggs) == 0 || m.suggs[0].cmd == nil || m.suggs[0].cmd.name != "resume" {
		t.Fatalf("suggs = %+v", m.suggs)
	}
	m.onKey(tea.KeyMsg{Type: tea.KeyEnter})
	if got := m.input.Value(); got != "/resume " {
		t.Fatalf("enter on /resume should complete to '/resume ', got %q", got)
	}
	if !strings.HasPrefix(m.suggFor, "/resume ") {
		t.Fatal("suggestions should have been recomputed for the new input")
	}
}

// Enter 选中技能建议 → 直接提交为技能调用消息，SKILL.md 内容内联。
func TestSkillSuggestionExecutes(t *testing.T) {
	dir := t.TempDir()
	sp := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(sp, []byte("# review\n检查代码质量"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newSuggModel(nil, []skill.Meta{
		{Name: "review", Path: sp},
	})
	typeKeys(m, "/rev")
	if len(m.suggs) == 0 || m.suggs[0].kind != suggSkill {
		t.Fatalf("suggs = %+v", m.suggs)
	}
	m.onKey(tea.KeyMsg{Type: tea.KeyEnter})
	last := m.history[len(m.history)-1]
	if last.Role != agent.RoleUser ||
		!strings.Contains(last.Content, `"review"`) ||
		!strings.Contains(last.Content, "检查代码质量") ||
		!strings.Contains(last.Content, `<skill path=`) {
		t.Fatalf("skill invocation msg = %q", last.Content)
	}
	if m.input.Value() != "" {
		t.Fatalf("input should be cleared after submit, got %q", m.input.Value())
	}
}

// 直接键入 /技能名 args 提交同样走技能调用；未知斜杠名报 unknown。
func TestSkillSlashSubmit(t *testing.T) {
	m := newSuggModel(nil, []skill.Meta{
		{Name: "review", Path: "/skills/review/SKILL.md"},
	})
	m.input.SetValue("/review check this diff")
	if cmd := m.submit(); cmd != nil {
		_ = cmd // 返回的 tea.Cmd 会驱动 agent.Run，测试中不执行
	}
	last := m.history[len(m.history)-1]
	if !strings.Contains(last.Content, "check this diff") {
		t.Fatalf("args not appended, msg = %q", last.Content)
	}
	if !strings.Contains(ansi.Strip(m.transcript), "/review check this diff") {
		t.Fatalf("transcript should show the raw slash form, got %q", m.transcript)
	}

	m.input.SetValue("/nope")
	m.submit()
	if !strings.Contains(ansi.Strip(m.transcript), "unknown command") {
		t.Fatalf("unknown command note missing, transcript = %q", m.transcript)
	}
}

// 会话建议：同 workspace 的 main 会话、排除当前会话、关键词子串过滤。
func TestSessionSuggestions(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := scope.WithScope(context.Background(), scope.Scope{TenantID: "t"})

	seed := seedSessions(t, st, ctx, []session.Meta{
		{Workspace: "/w", Kind: session.KindMain, Title: "fix login bug"},
		{Workspace: "/w", Kind: session.KindMain, Title: "refactor tui"},
		{Workspace: "/other", Kind: session.KindMain, Title: "other ws"},
		{Workspace: "/w", Kind: session.KindSubagent, Title: "sub"},
	})
	m := newSuggModel(st, nil)

	list, _ := m.computeSuggestions("/resume ", 8)
	if len(list) != 2 {
		t.Fatalf("/resume = %d items, want 2 (same-ws main, minus current)", len(list))
	}
	for _, s := range list {
		if s.sess.Title == "other ws" || s.sess.Kind != session.KindMain {
			t.Fatalf("unexpected session in list: %+v", s.sess)
		}
	}
	list, _ = m.computeSuggestions("/resume login", 13)
	if len(list) != 1 || list[0].sess.ID != seed[0].ID {
		t.Fatalf("/resume login = %v, want only 'fix login bug'", list)
	}
	// UUIDv7 前缀是时间戳（同毫秒共享），用尾部随机段做 id 过滤
	list, _ = m.computeSuggestions("/resume "+seed[1].ID[len(seed[1].ID)-12:], 20)
	if len(list) != 1 || list[0].sess.ID != seed[1].ID {
		t.Fatalf("id filter failed, list = %v", list)
	}
	if _, note := m.computeSuggestions("/resume zzzz", 13); note == "" {
		t.Fatal("want no-match note for empty session list")
	}
}

// 选中会话 Enter → 历史切换、scope 换绑、transcript 回放。
func TestResumeSession(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	base := scope.WithScope(context.Background(), scope.Scope{TenantID: "t"})
	old := seedSessions(t, st, base, []session.Meta{
		{Workspace: "/w", Kind: session.KindMain, Title: "old work"},
	})[0]
	octx := scope.WithSession(base, old.ID)
	st.Append(octx, agent.Message{Role: agent.RoleUser, Content: "earlier question"})
	st.Append(octx, agent.Message{Role: agent.RoleAssistant, Content: "earlier answer"})

	m := newSuggModel(st, nil)
	m.resumeSession(old)
	if m.sc.SessionID != old.ID {
		t.Fatalf("scope not rebound: %q", m.sc.SessionID)
	}
	if m.info.Session != old.ID[:8] {
		t.Fatalf("topbar session = %q", m.info.Session)
	}
	if len(m.history) != 2 { // 种子会话只写了 user + assistant
		t.Fatalf("history = %d msgs", len(m.history))
	}
	if m.turns != 1 {
		t.Fatalf("turns = %d, want 1 user message replayed", m.turns)
	}
	plain := ansi.Strip(m.transcript)
	if !strings.Contains(plain, "earlier question") || !strings.Contains(plain, "earlier answer") {
		t.Fatalf("history not replayed into transcript: %q", plain)
	}
	// resume 后的新输入应落入旧会话
	if err := m.record(scope.WithScope(context.Background(), m.sc),
		&agent.Message{Role: agent.RoleUser, Content: "next"}); err != nil {
		t.Fatal(err)
	}
	hist, err := st.LoadHistory(base, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if last := hist[len(hist)-1]; last.Content != "next" {
		t.Fatalf("append went to wrong session, last = %q", last.Content)
	}
}

// Esc 关闭本次下拉且输入不变时不复活；输入变化即恢复。
func TestSuggestionDismiss(t *testing.T) {
	m := newSuggModel(nil, nil)
	typeKeys(m, "/ex")
	if len(m.suggs) == 0 {
		t.Fatal("suggestions should show for /ex")
	}
	m.onKey(tea.KeyMsg{Type: tea.KeyEscape})
	if len(m.suggs) != 0 {
		t.Fatal("esc should dismiss")
	}
	// 输入值不变时的重算（光标移动等路径）不应重新触发
	m.updateSuggestions()
	if len(m.suggs) != 0 {
		t.Fatal("dismissed suggestions should stay hidden for same input")
	}
	typeKeys(m, "i") // "/ex" → "/exi" 仍命中 /exit
	if len(m.suggs) == 0 {
		t.Fatal("input change should re-trigger suggestions")
	}
}

// 上下键在列表内回绕移动选中。
func TestSuggestionNavWraps(t *testing.T) {
	m := newSuggModel(nil, nil)
	typeKeys(m, "/")
	n := len(m.suggs)
	if n < 2 {
		t.Fatalf("want >=2 suggestions, got %d", n)
	}
	m.onKey(tea.KeyMsg{Type: tea.KeyUp})
	if m.suggSel != n-1 {
		t.Fatalf("up at 0 should wrap to %d, got %d", n-1, m.suggSel)
	}
	m.onKey(tea.KeyMsg{Type: tea.KeyDown})
	if m.suggSel != 0 {
		t.Fatalf("down at last should wrap to 0, got %d", m.suggSel)
	}
}

// fmtAgo 的相对时间分档。
func TestFmtAgo(t *testing.T) {
	now := time.Now()
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "now"},
		{5 * time.Minute, "5m"},
		{3 * time.Hour, "3h"},
		{9 * 24 * time.Hour, "9d"},
	}
	for _, c := range cases {
		if got := fmtAgo(now.Add(-c.d)); got != c.want {
			t.Fatalf("fmtAgo(%v ago) = %q, want %q", c.d, got, c.want)
		}
	}
}
