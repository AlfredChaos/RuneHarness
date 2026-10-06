package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// 回归：thinkBody 若整段 Render，lipgloss 会把每行补空格对齐到最宽
// 行宽，空格尾巴再被 row 的硬换行折成多余空行，think 段间距被放大。
// 逐行渲染后，源文本一个空行只对应输出一个空白行。
func TestRowThinkBlankLines(t *testing.T) {
	m := &Model{width: 80}
	long := strings.Repeat("a ", 80) // 160 列，必折行且是最宽行
	body := long + "\n\nshort para"
	out := ansi.Strip(m.rowThink(body, time.Now()))
	blank := 0
	for _, ln := range strings.Split(out, "\n") {
		if strings.TrimSpace(ln) == "" {
			blank++
		}
	}
	if blank != 1 {
		t.Fatalf("blank lines = %d, want 1; out = %q", blank, out)
	}
	if !strings.Contains(out, "short para") {
		t.Fatalf("think body truncated; out = %q", out)
	}
}
