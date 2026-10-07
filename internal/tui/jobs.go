package tui

// 后台任务与定时调度在 TUI 侧的交付面：事件消息、空闲 drain、注入
// 起跑与 /tasks /cron 本地查询。从 model.go 拆出收敛文件规模；
// 渲染层（rowBg/rowCron）按目录职责留在 feed.go。

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"runeharness/internal/agent"
	"runeharness/internal/bgtask"
	"runeharness/internal/cron"
	"runeharness/internal/scope"
)

// BgTaskMsg 由 bgtask.Manager.OnEvent 回调经 Program.Send 注入：
// 任务到达终态或停滞提醒。收到后渲染一行状态；agent 空闲时把积压的
// <task_notification> drain 成一条 user 消息自动续跑（忙碌时由运行中
// loop 的 PreChat 自取，这里只留展示行）。
type BgTaskMsg struct{ Event bgtask.Event }

// CronMsg 由 cron.Scheduler.OnFire 回调经 Program.Send 注入：
// 一条任务到点触发（或启动时的漏跑汇总）。收到后渲染一行状态；
// agent 空闲时把积压事件 drain 成一条 user 消息自动开跑——与 bg
// 通知不同，cron 不打断运行中的轮（定时任务是新工作单元，不是
// 进行中的旁路信息），只走空闲交付。
type CronMsg struct{ Event cron.Event }

// injectAndRun 把一条 KindInject user 消息追加进 history（留痕 + 落库）
// 并起跑一轮 agent 循环——步数续跑与 bg/cron 通知注入共用此路径。
// ok=false 表示落库失败：消息已从 history 回滚，调用方应把刚取出
// 的队列内容 Requeue 归还，保持"被消费才离队列"的语义。
func (m *Model) injectAndRun(content string) (tea.Cmd, bool) {
	ctx, cancel := context.WithCancel(scope.WithScope(context.Background(), m.sc))
	m.history = append(m.history, agent.Message{Role: agent.RoleUser, Kind: agent.KindInject, Content: content})
	m.appendLine(m.rowHook(content))
	if err := m.record(ctx, &m.history[len(m.history)-1]); err != nil {
		cancel()
		m.history = m.history[:len(m.history)-1]
		m.appendLine(m.rowNote("✘", "session append failed: "+err.Error()))
		return nil, false
	}
	m.busy = true
	m.busySince = time.Now()
	m.activity = "waiting for model"
	m.cancel = cancel
	history := m.history
	return tea.Batch(m.spinner.Tick, func() tea.Msg {
		next, err := m.agent.Run(ctx, history)
		return turnDoneMsg{history: next, err: err}
	}), true
}

// maybeDrainBg 在 agent 空闲时把积压的后台任务通知注入为一条 user 消息
// 并起跑新一轮；忙碌（运行中 loop 会在下一次 PreChat 自取通知）或
// 无 bg 支持 / 队列为空时返回 nil。
// bg 通知是进行中任务的旁路信息而不是新用户轮：故意不走
// UserPromptSubmit——todo 轮次归零、记忆"首条消息"注入都不该被它搅动
// （cron 触发是新工作单元，才按用户输入同规则过钩子链）。
func (m *Model) maybeDrainBg() tea.Cmd {
	if m.bg == nil || m.busy {
		return nil
	}
	text := m.bg.Drain()
	if text == "" {
		return nil
	}
	if cmd, ok := m.injectAndRun(text); ok {
		return cmd
	}
	m.bg.Requeue(text) // 落库失败归还队列，通知不丢
	return nil
}

// drainQueued 是空闲交付的统一入口：bg 通知优先（与进行中工作相关，
// 紧迫），cron 其次（新工作单元）。一次只交付一路——被交付的那轮把
// busy 置位，另一路等下一轮空闲。
func (m *Model) drainQueued() tea.Cmd {
	if cmd := m.maybeDrainBg(); cmd != nil {
		return cmd
	}
	return m.maybeDrainCron()
}

// maybeDrainCron 在 agent 空闲时把已到点的定时任务注入为一条
// <scheduled_task> user 消息并起跑新一轮；忙碌或无队列时返回 nil。
// 交付走 UserPromptSubmit 钩子链（记忆注入等），与用户输入同规则；
// 被拦截或落库失败时事件归还队列——"Drain 被消费才离队列"不因其后的
// 交付失败而破例。
func (m *Model) maybeDrainCron() tea.Cmd {
	if m.cron == nil || m.busy {
		return nil
	}
	evs := m.cron.Drain()
	if len(evs) == 0 {
		return nil
	}
	content := cron.DeliverText(evs)
	ctx := scope.WithScope(context.Background(), m.sc)
	if replace, blocked := m.agent.Hooks.TriggerUserPromptSubmit(ctx, content); blocked != "" {
		m.cron.Requeue(evs)
		m.appendLine(m.rowNote("✘", "cron turn blocked: "+blocked))
		return nil
	} else if replace != "" {
		content = replace
	}
	if cmd, ok := m.injectAndRun(content); ok {
		return cmd
	}
	m.cron.Requeue(evs)
	return nil
}

// runTasks 执行 /tasks：把后台任务清单打进对话区（本地查询，不阻塞）。
func (m *Model) runTasks() tea.Cmd {
	if m.bg == nil {
		m.appendLine(m.rowNote("✘", "background tasks are not configured"))
		return nil
	}
	ts := m.bg.List()
	if len(ts) == 0 {
		m.appendLine(m.rowNote("·", "no background tasks"))
		return nil
	}
	var b strings.Builder
	for _, t := range ts {
		fmt.Fprintf(&b, "%s [%s] %s\n", t.ID, taskStatusText(t), t.Description)
		b.WriteString(dimStyle.Render("    " + t.Command + "\n"))
	}
	m.appendBlock(b.String())
	return nil
}

// runCron 执行 /cron：把当前 workspace 的定时任务清单打进对话区
// （本地查询，不阻塞）。
func (m *Model) runCron() tea.Cmd {
	if m.cron == nil {
		m.appendLine(m.rowNote("✘", "cron scheduler is not configured"))
		return nil
	}
	ts := m.cron.Tasks()
	if len(ts) == 0 {
		m.appendLine(m.rowNote("·", "no scheduled tasks"))
		return nil
	}
	var b strings.Builder
	for _, t := range ts {
		desc := cron.Humanize(t.Cron)
		if t.TZ != "" {
			desc += " · " + t.TZ
		}
		kind := "one-shot"
		if t.Recurring {
			kind = "recurring"
		}
		persist := "session"
		if t.Durable {
			persist = "durable"
		}
		fmt.Fprintf(&b, "%s  %s · %s · %s\n", t.ID, desc, kind, persist)
		if sched, err := cron.Parse(t.Cron); err == nil {
			if next, ok := sched.Next(time.Now(), t.Location()); ok {
				fmt.Fprintf(&b, "    next: %s\n", next.Format("2006-01-02 15:04 -07:00"))
			}
		}
		b.WriteString(dimStyle.Render("    " + firstLine(t.Prompt) + "\n"))
	}
	m.appendBlock(strings.TrimRight(b.String(), "\n"))
	return nil
}

// taskStatusText 是 /tasks 的状态短语（与 task_list 工具输出同口径）。
func taskStatusText(t bgtask.Task) string {
	if t.Status == bgtask.StatusRunning {
		return fmt.Sprintf("running %s", time.Since(t.StartedAt).Round(time.Second))
	}
	d := t.EndedAt.Sub(t.StartedAt).Round(time.Second)
	if t.Status == bgtask.StatusCompleted {
		return fmt.Sprintf("done exit 0 in %s", d)
	}
	if t.ExitCode >= 0 {
		return fmt.Sprintf("%s exit %d in %s", t.Status, t.ExitCode, d)
	}
	return fmt.Sprintf("%s in %s", t.Status, d)
}
