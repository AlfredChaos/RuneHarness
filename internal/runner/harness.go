package runner

import (
	"context"
	"fmt"

	"runeharness/internal/agent"
	"runeharness/internal/bgtask"
	"runeharness/internal/cron"
	"runeharness/internal/scope"
)

// AgentHarness 把一台已装配的 *agent.Agent 包成 runner 所需的
// Harness。harness 与 slot 的分工：slot 管队列、取消、账本与落库；
// harness 管回调映射与旁路注入（bg/cron drain）。
//
// 装配差异只在前台出口：TUI 走 Program.Send 进 Bubble Tea，
// 这里走 emit 进每 chat 的事件账本。sessionID 不存字段——
// slotCtx 里已带 scope（newSlot 时注入），每次用现取，多槽不串。
type AgentHarness struct {
	Agent     *agent.Agent
	Store     SessionStore
	BgMgr     *bgtask.Manager
	CronSched *cron.Scheduler
}

// New 实现 Harness.New：把 agent 回调全部折成 emit。
func (h *AgentHarness) New(slotCtx context.Context, emit func(Event), ask func(agent.ToolCall, string, int, int) <-chan bool) (TurnRunner, error) {
	a := h.Agent
	if a == nil {
		return nil, fmt.Errorf("agent harness: nil agent")
	}
	a.OnPartial = func(pt agent.Partial) {
		emit(Event{Event: EvDelta, Data: mustJSON(map[string]any{
			"thinking": pt.Thinking, "content": pt.Content,
		})})
	}
	a.OnToolCall = func(tc agent.ToolCall) {
		emit(Event{Event: EvToolCall, Data: mustJSON(tc)})
	}
	a.OnToolResult = func(r agent.ToolResult) {
		emit(Event{Event: EvToolResult, Data: mustJSON(map[string]any{
			"id": r.Call.ID, "output": r.Output, "is_error": r.IsError,
			"blocked": r.Blocked, "duration_ms": r.Dur.Milliseconds(),
		})})
	}
	a.OnInject = func(msg agent.Message) {
		emit(Event{Event: EvInject, Data: mustJSON(map[string]any{
			"content": msg.Content,
		})})
	}
	return func(turnCtx context.Context, history []agent.Message) ([]agent.Message, error) {
		// 轮间旁路注入：bg 优先（进行中任务的旁路信息）、cron 其次
		//（新工作单元）。一次只交付一路，与 TUI drainQueued 一致——
		// 被交付的下一轮 turn 自然把另一路留到下个轮间。
		if s := h.BgMgr; s != nil {
			if text := s.Drain(); text != "" {
				emit(Event{Event: EvNotification, Data: mustJSON(map[string]any{
					"kind": "bg", "content": text,
				})})
				inject := agent.Message{Role: agent.RoleUser, Kind: agent.KindInject, Content: text}
				if err := h.appendScoped(turnCtx, inject); err != nil {
					s.Requeue(text) // 落库失败归还队列，通知不丢
				} else {
					history = append(history, inject)
				}
			}
		}
		if s := h.CronSched; s != nil {
			if evs := s.Drain(); len(evs) > 0 {
				content := cron.DeliverText(evs)
				emit(Event{Event: EvNotification, Data: mustJSON(map[string]any{
					"kind": "cron", "count": len(evs),
				})})
				// cron 是新工作单元，过 UserPromptSubmit 钩子链（对齐 TUI）。
				if replace, blocked := a.Hooks.TriggerUserPromptSubmit(turnCtx, content); blocked != "" {
					s.Requeue(evs)
				} else {
					if replace != "" {
						content = replace
					}
					inject := agent.Message{Role: agent.RoleUser, Kind: agent.KindInject, Content: content}
					if err := h.appendScoped(turnCtx, inject); err != nil {
						s.Requeue(evs)
					} else {
						history = append(history, inject)
					}
				}
			}
		}
		return a.Run(turnCtx, history)
	}, nil
}

// appendScoped 把一条消息落到本槽位会话；sessionID 从 ctx scope 取。
func (h *AgentHarness) appendScoped(ctx context.Context, msg agent.Message) error {
	if h.Store == nil {
		return nil
	}
	sc, err := scope.FromContext(ctx)
	if err != nil || sc.SessionID == "" {
		return fmt.Errorf("no session scope: %w", err)
	}
	_, err = h.Store.Append(ctx, msg)
	return err
}
