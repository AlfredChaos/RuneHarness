package tui

import (
	"fmt"
	"strings"

	"github.com/AlexanderGrooff/mermaid-ascii/pkg/diagram"
	"github.com/AlexanderGrooff/mermaid-ascii/pkg/render"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
)

// detectMarkdownStyle 按终端背景选择 glamour 主题。必须在 Program.Run
// 之前调用：运行期查询终端背景会与 Bubble Tea 抢读 stdin。
func detectMarkdownStyle() string {
	if lipgloss.HasDarkBackground() {
		return styles.DarkStyle
	}
	return styles.LightStyle
}

// renderMarkdown 把 agent 正文按 Markdown 渲染为终端 ANSI 文本；
// ```mermaid 块先转换为字符图。渲染失败时回退为纯文本软换行，保证内容不丢。
func renderMarkdown(s, style string, width int) string {
	if width < 20 {
		width = 80
	}
	r, err := glamour.NewTermRenderer(glamour.WithStandardStyle(style), glamour.WithWordWrap(width-2))
	if err != nil {
		return renderText(s, width)
	}
	out, err := r.Render(expandMermaid(s, width-4))
	if err != nil {
		return renderText(s, width)
	}
	return strings.Trim(out, "\n")
}

// expandMermaid 把成对闭合的 ```mermaid 代码块替换为 mermaid-ascii 渲染的
// 字符图（仍包在代码块里以保留等宽排版）。不支持的图类型、语法错误或
// 未闭合的块保留原始源码。
func expandMermaid(s string, width int) string {
	lines := strings.Split(s, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "```mermaid" {
			out = append(out, lines[i])
			continue
		}
		end := i + 1
		for end < len(lines) && strings.TrimSpace(lines[end]) != "```" {
			end++
		}
		if end == len(lines) { // 未闭合：原样保留剩余内容
			out = append(out, lines[i:]...)
			break
		}
		if art, err := renderMermaid(strings.Join(lines[i+1:end], "\n"), width); err == nil {
			out = append(out, "```text", strings.TrimRight(art, "\n"), "```")
		} else {
			out = append(out, lines[i:end+1]...)
		}
		i = end
	}
	return strings.Join(out, "\n")
}

// renderMermaid 调用 mermaid-ascii 渲染单张图；库内部对异常输入可能 panic，
// 统一转为 error 交给调用方回退。
func renderMermaid(src string, width int) (art string, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("mermaid render panicked: %v", p)
		}
	}()
	cfg := diagram.DefaultConfig()
	cfg.MaxWidth = width
	return render.RenderDiagram(src, cfg)
}
