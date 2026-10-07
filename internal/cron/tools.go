package cron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"runeharness/internal/tools"
)

// 工具名常量：权限 safe 名单与装配代码按此引用，改名须同步。
const (
	CreateToolName = "cron_create"
	ListToolName   = "cron_list"
	DeleteToolName = "cron_delete"
)

// CreateTool 是 cron_create：注册一条定时任务。触发时 prompt 作为
// user 消息进入对话——模型排程的是"未来的自己该做什么"。
type CreateTool struct{ S *Store }

func (CreateTool) Spec() tools.Spec {
	return tools.Spec{
		Name: CreateToolName,
		Description: "Schedule a prompt to run at a future time — recurring on a " +
			"cron schedule, or once at a specific time. Standard 5-field cron " +
			"(minute hour day-of-month month day-of-week) evaluated in the given " +
			"timezone (default: local). \"0 9 * * 1-5\" = weekdays at 9am; " +
			"\"30 14 28 2 *\" with recurring=false = one-shot on Feb 28 14:30. " +
			"durable=true persists across restarts (default false = this session only). " +
			"Fired tasks run a full agent turn with normal permission checks, " +
			"and only fire while the assistant is idle.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"cron": map[string]any{
					"type":        "string",
					"description": "5-field cron: \"M H DoM Mon DoW\". Supports *, */N, N, N-M, N-M/S, lists.",
				},
				"prompt": map[string]any{
					"type":        "string",
					"description": "the message injected as a user turn at each fire time",
				},
				"timezone": map[string]any{
					"type":        "string",
					"description": "IANA timezone the cron fields are evaluated in, e.g. Asia/Shanghai; default local",
				},
				"recurring": map[string]any{
					"type":        "boolean",
					"description": "true (default) = fire on every match; false = fire once then auto-delete (one-shot reminders)",
				},
				"durable": map[string]any{
					"type":        "boolean",
					"description": "true = persist to disk and survive restarts; false (default) = session-only",
				},
			},
			"required": []string{"cron", "prompt"},
		},
	}
}

func (c CreateTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Cron      string `json:"cron"`
		Prompt    string `json:"prompt"`
		Timezone  string `json:"timezone"`
		Recurring *bool  `json:"recurring"`
		Durable   *bool  `json:"durable"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	sched, err := Parse(args.Cron)
	if err != nil {
		return "", fmt.Errorf("invalid cron expression %q: %w", args.Cron, err)
	}
	loc := time.Local
	if args.Timezone != "" {
		l, err := time.LoadLocation(args.Timezone)
		if err != nil {
			return "", fmt.Errorf("invalid timezone %q", args.Timezone)
		}
		loc = l
	}
	next, ok := sched.Next(time.Now(), loc)
	if !ok {
		return "", fmt.Errorf("cron expression %q does not match any calendar date", args.Cron)
	}
	recurring, durable := true, false
	if args.Recurring != nil {
		recurring = *args.Recurring
	}
	if args.Durable != nil {
		durable = *args.Durable
	}
	t, err := c.S.Add(ctx, Task{
		Cron:      sched.Expr(),
		TZ:        args.Timezone,
		Prompt:    args.Prompt,
		Recurring: recurring,
		Durable:   durable,
	})
	if err != nil {
		return "", err
	}
	kind, where := "one-shot task", "session-only (dies with this process)"
	if recurring {
		kind = "recurring job"
	}
	if durable {
		where = "persisted to " + c.S.Path()
	}
	return fmt.Sprintf("scheduled %s %s — %s%s; next fire %s; %s",
		kind, t.ID, Humanize(t.Cron), tzSuffix(t.TZ),
		next.Format("2006-01-02 15:04 -07:00"), where), nil
}

// ListTool 是 cron_list：列当前 workspace 的全部任务与下次点火时间。
type ListTool struct{ S *Store }

func (ListTool) Spec() tools.Spec {
	return tools.Spec{
		Name:        ListToolName,
		Description: "List scheduled tasks (durable and session-only) with their next fire time.",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

func (l ListTool) Run(ctx context.Context, _ json.RawMessage) (string, error) {
	ts := l.S.List(ctx)
	if len(ts) == 0 {
		return "no scheduled tasks", nil
	}
	var b strings.Builder
	for _, t := range ts {
		fmt.Fprintf(&b, "%s  %s%s", t.ID, Humanize(t.Cron), tzSuffix(t.TZ))
		if t.Recurring {
			b.WriteString("  recurring")
		} else {
			b.WriteString("  one-shot")
		}
		if t.Durable {
			b.WriteString("  durable")
		} else {
			b.WriteString("  session")
		}
		if sched, err := Parse(t.Cron); err == nil {
			if next, ok := sched.Next(time.Now(), t.Location()); ok {
				fmt.Fprintf(&b, "\n  next: %s", next.Format("2006-01-02 15:04 -07:00"))
			}
		}
		fmt.Fprintf(&b, "\n  prompt: %s\n", truncate(t.Prompt, 120))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// DeleteTool 是 cron_delete：按 id 取消任务（session 与 durable 统一）。
type DeleteTool struct{ S *Store }

func (DeleteTool) Spec() tools.Spec {
	return tools.Spec{
		Name:        DeleteToolName,
		Description: "Cancel a scheduled task by ID (from cron_create or cron_list).",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string", "description": "task id to cancel"},
			},
			"required": []string{"id"},
		},
	}
}

func (d DeleteTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if args.ID == "" {
		return "", errors.New("id is required")
	}
	ok, err := d.S.Remove(ctx, args.ID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "no such job: " + args.ID, nil
	}
	return "canceled " + args.ID, nil
}

// tzSuffix 是展示用时区尾巴；本地时区不标（默认语义）。
func tzSuffix(tz string) string {
	if tz == "" {
		return ""
	}
	return " " + tz
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
