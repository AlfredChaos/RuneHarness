package bgtask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"runeharness/internal/tools"
)

const (
	// ListToolName 是 task_list 的工具名；权限名单按此引用。
	ListToolName = "task_list"
	// KillToolName 是 task_kill 的工具名。
	KillToolName = "task_kill"
)

// ListTool 是 task_list：列全部后台任务的状态与输出路径（只读）。
type ListTool struct{ M *Manager }

func (ListTool) Spec() tools.Spec {
	return tools.Spec{
		Name: ListToolName,
		Description: "List background tasks started by run_command(run_in_background): " +
			"status, elapsed time, and output file path for each.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

func (l ListTool) Run(context.Context, json.RawMessage) (string, error) {
	ts := l.M.List()
	if len(ts) == 0 {
		return "no background tasks", nil
	}
	var b strings.Builder
	for _, t := range ts {
		fmt.Fprintf(&b, "%s [%s] %q\n  cmd: %s\n  out: %s\n",
			t.ID, statusLine(t), t.Description, t.Command, t.OutputPath)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// statusLine 渲染单行状态：running 给已运行时长，终态给结果与耗时。
func statusLine(t Task) string {
	if t.Status == StatusRunning {
		return fmt.Sprintf("running · %s", time.Since(t.StartedAt).Round(time.Second))
	}
	d := t.EndedAt.Sub(t.StartedAt).Round(time.Second)
	if t.Status == StatusCompleted {
		return fmt.Sprintf("completed · exit 0 · %s", d)
	}
	if t.ExitCode >= 0 {
		return fmt.Sprintf("%s · exit %d · %s", t.Status, t.ExitCode, d)
	}
	return fmt.Sprintf("%s · %s", t.Status, d)
}

// KillTool 是 task_kill：终止一个仍在运行的后台任务（杀整个进程组）。
type KillTool struct{ M *Manager }

func (KillTool) Spec() tools.Spec {
	return tools.Spec{
		Name:        KillToolName,
		Description: "Stop a running background task — kills its whole process group.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{"type": "string", "description": "task id from run_command(run_in_background) or task_list"},
			},
			"required": []string{"task_id"},
		},
	}
}

func (k KillTool) Run(_ context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if args.TaskID == "" {
		return "", errors.New("task_id is required")
	}
	t, err := k.M.Kill(args.TaskID)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("task %s stopped (%q)", t.ID, t.Description), nil
}
