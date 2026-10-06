package memory

import (
	"fmt"
	"strings"

	"runeharness/internal/agent"
)

// 提取输入的行级截断上限：user/assistant 正文放宽，工具结果/参数收紧
// （工具输出主体不进记忆，指针与结论已够提取器判断）。
const (
	renderUserClip = 2000
	renderToolClip = 800
	renderArgsClip = 300
)

// renderRows 把一段原始会话行渲染成提取器输入文本。控制行与系统行
// 不进输入；工具结果截断——压缩留下的 blob:// 指针本身就是摘要。
func renderRows(rows []agent.Message) string {
	var b strings.Builder
	for _, m := range rows {
		switch m.Role {
		case agent.RoleUser:
			label := "user"
			if m.Kind == agent.KindInject {
				label = "harness notice"
			}
			fmt.Fprintf(&b, "[%s]\n%s\n\n", label, clip(m.Content, renderUserClip))
		case agent.RoleAssistant:
			if m.Content != "" {
				fmt.Fprintf(&b, "[assistant]\n%s\n", clip(m.Content, renderUserClip))
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "[tool call %s] %s\n", tc.Name, clip(string(tc.Arguments), renderArgsClip))
			}
			b.WriteString("\n")
		case agent.RoleTool:
			label := "tool result"
			if m.IsError {
				label += " (error)"
			}
			fmt.Fprintf(&b, "[%s]\n%s\n\n", label, clip(m.Content, renderToolClip))
		}
	}
	return b.String()
}

// clip 保留头尾，中间以省略标记代替（与 compact.renderEvent 同口径）。
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n/2]) + "\n…[clipped]…\n" + string(r[len(r)-n/2:])
}
