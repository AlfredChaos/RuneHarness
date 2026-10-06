package compact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"runeharness/internal/agent"
	"runeharness/internal/tools"
)

// reminderPrefix 是两种提醒共用的开头，用于判定本压缩周期是否已提醒过。
const reminderPrefix = "<system-reminder>Context"

// mainReminder 是主代理的提醒（plan §4.4）：提示模型择机调用 compact。
const mainReminder = reminderPrefix + " usage: %d/%d tokens. Forced compaction runs at %d tokens. " +
	"At a natural break point (a subtask is done, or earlier large results are no longer needed), " +
	"call the compact tool.</system-reminder>"

// wrapUpReminder 是子代理的收尾提醒（plan §4.9）：子代理要交结论，不择机压缩。
const wrapUpReminder = reminderPrefix + " is nearly full. Finish the current step and reply with " +
	"your conclusion now.</system-reminder>"

// ReminderHook 返回提醒线的 PreChat hook：估算用量 ≥ 提醒线、且本压缩周期
// （最近一条摘要之后）还没提醒过时，注入一次固定提醒。每轮都注入会让提醒
// 本身成为膨胀源。
func (c *Compactor) ReminderHook() agent.PreChatFunc {
	return func(_ context.Context, history []agent.Message) string {
		est := Estimate(history, c.cfg.Overhead)
		if est < c.cfg.ReminderLine() || remindedThisEpoch(history) {
			return ""
		}
		// 已过强制压缩线且 auto 开启时，同一轮 Maintain 就会压缩——
		// 这时再注入"择机压缩"的提醒自相矛盾，直接静默交给代码兜底。
		if c.cfg.Auto && est >= c.cfg.CompactLine() {
			return ""
		}
		if c.cfg.Subagent {
			return wrapUpReminder
		}
		return fmt.Sprintf(mainReminder, est, c.cfg.Window, c.cfg.CompactLine())
	}
}

func remindedThisEpoch(history []agent.Message) bool {
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		if m.Kind == agent.KindSummary {
			return false
		}
		if m.Kind == agent.KindInject && strings.HasPrefix(m.Content, reminderPrefix) {
			return true
		}
	}
	return false
}

// Tool 是模型侧 compact 工具的定义。loop 对它特判（plan §4.7），Run 不会被调用；
// 只在没有装配压缩器时经 Registry 执行，返回错误。
type Tool struct{}

func (Tool) Spec() tools.Spec {
	return tools.Spec{
		Name: agent.CompactToolName,
		Description: "Compact the conversation context to free space: earlier tool results move to storage " +
			"(readable later via blob:// references) and older turns are summarized. The first user messages " +
			"and the recent turns stay verbatim. Call it at a natural break point — after finishing a subtask, " +
			"or when large earlier results are no longer needed. A system reminder reports usage when it gets high.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

func (Tool) Run(context.Context, json.RawMessage) (string, error) {
	return "", errors.New("compact is unavailable: no compactor is configured")
}
