// Package todo 提供进程内存中的 TODO 列表工具与 nag 提醒。
// todo_write 不给 agent 增加执行能力，只增加规划能力：模型把计划序列化
// 为工具参数留在 history 近端，对抗长上下文中的注意力稀释。
package todo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"runeharness/internal/agent"
	"runeharness/internal/tools"
)

// ToolName 是注册到 Registry 的工具名；权限名单等按此引用。
const ToolName = "todo_write"

// nagInterval 是连续多少轮 LLM 调用未写 todo 后触发提醒。
const nagInterval = 10

// maxItems 限制列表长度，防止模型写入失控的长列表。
const maxItems = 20

// Item 是 TODO 列表中的一项。
type Item struct {
	Content string `json:"content"`
	Status  string `json:"status"` // pending | in_progress | completed
}

// Manager 持有 TODO 列表与 nag 计数器（距上次 todo_write 的 LLM 轮数）。
// 它同时实现 tools.Tool（被 Registry 按名分发）与 agent.PreChatFunc
// （被 agent loop 在每轮调用 LLM 前执行），两类状态收敛在同一对象内。
// 仅由 agent goroutine 访问，无需加锁。
type Manager struct {
	items  []Item
	rounds int

	// OnChange 在每次列表写入成功后回调，供 UI 同步展示；可为 nil。
	// 传入切片是拷贝，消费方可安全持有。
	OnChange func(items []Item)
}

func NewManager() *Manager { return &Manager{} }

func (m *Manager) Spec() tools.Spec {
	return tools.Spec{
		Name: ToolName,
		Description: "Create and update the session task list. For multi-step work, plan the steps " +
			"with this tool before executing, then rewrite the full list to track progress.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"todos": map[string]any{
					"type":        "array",
					"description": "the full task list; each call replaces the previous one",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"content": map[string]any{"type": "string", "description": "task description"},
							"status": map[string]any{
								"type": "string",
								"enum": []string{"pending", "in_progress", "completed"},
							},
						},
						"required": []string{"content", "status"},
					},
				},
			},
			"required": []string{"todos"},
		},
	}
}

// Run 校验并全量替换 TODO 列表，同时归零 nag 计数器。
// 返回渲染后的看板：模型可自检写入结果，TUI 经 ToolCallMsg 直接展示。
func (m *Manager) Run(_ context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Todos []Item `json:"todos"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if len(args.Todos) == 0 {
		return "", errors.New("todos must not be empty")
	}
	if len(args.Todos) > maxItems {
		return "", fmt.Errorf("too many todos: %d > %d", len(args.Todos), maxItems)
	}
	for i, t := range args.Todos {
		if strings.TrimSpace(t.Content) == "" {
			return "", fmt.Errorf("todo #%d: content is required", i+1)
		}
		switch t.Status {
		case "pending", "in_progress", "completed":
		default:
			return "", fmt.Errorf("todo #%d: invalid status %q", i+1, t.Status)
		}
	}
	m.items = args.Todos
	m.rounds = 0
	if m.OnChange != nil {
		m.OnChange(slices.Clone(m.items))
	}
	return m.board(), nil
}

// Nag 实现 agent.PreChatFunc：每轮计数；达到 nagInterval 时归零并按
// 列表状态决定是否提醒（由循环注入为 user 消息）：
//   - 列表为空（从未规划）：能跑满 nagInterval 轮工具调用的任务不琐碎，
//     提醒模型考虑用 todo_write 跟踪进度；
//   - 存在未完成项：提醒更新并附清单；
//   - 列表全部完成：静默返回空串。
func (m *Manager) Nag(_ context.Context, _ []agent.Message) string {
	m.rounds++
	if m.rounds < nagInterval {
		return ""
	}
	m.rounds = 0
	if len(m.items) == 0 {
		return "<reminder>This task spans multiple tool calls; consider tracking progress with todo_write.</reminder>"
	}
	pending := m.unfinished()
	if len(pending) == 0 {
		return ""
	}
	return "<reminder>Update your todos. Unfinished: " + strings.Join(pending, "; ") + "</reminder>"
}

// ResetRounds 在新一轮用户输入时归零 nag 计数器（由调用方挂在
// UserPromptSubmit 上），使"连续 N 轮未写 todo"按对话轮次统计，
// 避免多个琐碎轮次跨 turn 累积后误触提醒。
func (m *Manager) ResetRounds() { m.rounds = 0 }

// Snapshot 返回当前任务清单文本，供压缩摘要后重挂；无任务时返回空串。
func (m *Manager) Snapshot() string {
	if len(m.items) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Current task list:")
	for _, t := range m.items {
		fmt.Fprintf(&b, "\n  [%s] %s", t.Status, t.Content)
	}
	return b.String()
}

// unfinished 返回未完成项（pending / in_progress）的 content 列表。
func (m *Manager) unfinished() []string {
	var out []string
	for _, t := range m.items {
		if t.Status != "completed" {
			out = append(out, t.Content)
		}
	}
	return out
}

func (m *Manager) board() string {
	icons := map[string]string{"pending": " ", "in_progress": ">", "completed": "x"}
	var b strings.Builder
	fmt.Fprintf(&b, "Updated %d tasks:", len(m.items))
	for _, t := range m.items {
		fmt.Fprintf(&b, "\n  [%s] %s", icons[t.Status], t.Content)
	}
	return b.String()
}
