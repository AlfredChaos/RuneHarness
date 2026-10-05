// Package agent 定义领域模型与 agent loop，不依赖具体 LLM SDK。
package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"runeharness/internal/tools"
)

// historyCtxKey 是 Run 注入工具调用 ctx 的私有键，见 HistoryFromContext。
type historyCtxKey struct{}

// HistoryFromContext 返回 Run 注入的对话历史快照：截至触发本轮工具调用的
// assistant 消息之前，即"模型决定调用工具时看到的上下文"。供需要感知当前
// 对话的工具使用（如 task 委派 normal 子代理）；在 agent 循环外调用返回 nil。
func HistoryFromContext(ctx context.Context) []Message {
	h, _ := ctx.Value(historyCtxKey{}).([]Message)
	return h
}

// LLM 是对话补全能力的抽象，由外部适配器（如 openai-go）实现。
// onPartial 为流式输出的累积快照回调；实现方在不支持流式时可忽略，调用方可传 nil。
type LLM interface {
	Chat(ctx context.Context, history []Message, tools []tools.Spec, onPartial func(Partial)) (Response, error)
}

// Agent 驱动 "调模型 -> 执行工具 -> 回填结果" 的循环。
// 循环只负责在关键节点调用 Hooks.Trigger*，权限、日志等横切逻辑全部挂在
// Hooks 上，不写进循环体。
type Agent struct {
	llm      LLM
	registry *tools.Registry
	maxSteps int

	// Hooks 是生命周期 hook 注册表，覆盖一个 agent cycle 的五个节点：
	// UserPromptSubmit（由调用方触发）、PreToolUse、PostToolUse、Stop、
	// PreChat（每次调用 LLM 前）。
	Hooks Hooks

	// OnToolCall 在每次工具调用分发前回调（含后续被 hook 阻止的调用），
	// 用于 UI 展示；可为 nil。须先于执行发射：task 这类长耗时工具执行
	// 期间会产生子代理事件，回调若落在执行后会让"调用"显示在"结果"之后。
	OnToolCall func(call ToolCall)

	// OnToolResult 在工具调用结束后回调（含被 PreToolUse 阻止的调用），
	// 携带结果原文、成败与耗时，供 UI 给工具行补状态；可为 nil。
	OnToolResult func(res ToolResult)

	// OnInject 在 hook 往 history 注入消息时回调（PreChat nag、Stop 续跑），
	// 让 UI 能看到这些非用户输入的消息；可为 nil。
	OnInject func(msg Message)

	// OnPartial 是流式输出的累积快照回调，用于 UI 实时渲染；可为 nil。
	OnPartial func(Partial)

	// Rec 是会话记录器：loop 内每次往 history 追加消息（含 hook 注入、
	// 中断占位、tool_result）时同步调用。返回 error 会中止本轮
	// （fail the turn，已落库部分保留）；nil 表示不记录。
	// 不用 hook 实现的原因：现有 hook 点覆盖不全（assistant 消息没有
	// hook 点），且 hook 语义是只读观察/注入，Append 是写入职责。
	Rec Recorder
}

// Recorder 接收 loop 追加进 history 的每条消息，由外部存储实现
// （internal/session.Store 结构满足）。tenant/session 目标经 ctx 读取。
type Recorder interface {
	Append(ctx context.Context, msg Message) error
}

func New(llm LLM, registry *tools.Registry, maxSteps int) *Agent {
	return &Agent{llm: llm, registry: registry, maxSteps: maxSteps}
}

// ToolResult 是一次工具调用的结果汇报（对应 OnToolResult 回调）。
type ToolResult struct {
	Call    ToolCall
	Output  string        // 结果原文；被阻止时为 hook 给出的阻止原因
	IsError bool          // 执行失败或被阻止
	Blocked bool          // 被 PreToolUse 阻止，未真正执行
	Dur     time.Duration // 实际执行耗时；Blocked 时为零
}

// MaxStepsError 表示单轮 Run 的 LLM 步数达到上限（一轮 = 一次 Chat
// 及其整批 tool_calls，不是工具调用次数，也不是会话额度）。
// 与其他 error 不同：此时 Run 返回的 history 保留全部中间进度，
// 调用方可注入一条提示后续跑，而不是丢弃重来。
type MaxStepsError struct{ Steps int }

func (e *MaxStepsError) Error() string {
	return fmt.Sprintf("tool loop exceeded %d steps", e.Steps)
}

// InterruptedError 表示 Run 被 ctx 取消/超时打断（用户中断、父级 deadline）。
// 与 MaxStepsError 相同：返回的 history 保留部分进度——已执行的工具结果
// 照常落历史，未执行的 tool_calls 补 interrupted 占位结果，transcript 保持
// "每个 tool_call 都有结果"的完整形态，可续跑。
type InterruptedError struct{}

func (e *InterruptedError) Error() string { return "interrupted" }

// Run 执行一轮完整的 agent 循环：把 history 末尾的用户消息交给模型，
// 模型返回 tool_calls 则执行工具并回填，直到返回纯文本。
// 成功时返回更新后的 history；失败时返回 error，调用方应丢弃本轮改动。
// 例外：*MaxStepsError / *InterruptedError 时 history 为部分进度，
// 其中中断时未执行的 tool_calls 已补占位结果，两者都可用于续跑。
func (a *Agent) Run(ctx context.Context, history []Message) ([]Message, error) {
	for step := 0; step < a.maxSteps; step++ {
		// 每次调用 LLM 前过 PreChat hook：返回非空则注入为 user 消息
		//（如 todo nag reminder）。
		if inject := a.Hooks.TriggerPreChat(ctx, history); inject != "" {
			msg := Message{Role: RoleUser, Content: inject}
			history = append(history, msg)
			if err := a.record(ctx, msg); err != nil {
				return history, err
			}
			a.inject(msg)
		}
		resp, err := a.llm.Chat(ctx, history, a.registry.Specs(), a.OnPartial)
		if err != nil {
			// ctx 取消/超时视为中断：history 原样返回（末尾是 user 消息，可续跑）。
			if ctx.Err() != nil {
				return history, &InterruptedError{}
			}
			// TODO(recovery): LLM 调用失败目前一律致命返回，应按错误类型
			// 分类恢复（对齐 CC query.ts 的 reason code 设计）：
			//
			// 瞬态故障（归 internal/llm 适配器实现，对循环透明）：
			//   - 429/529、连接错误、超时：指数退避 + 抖动
			//     min(500ms*2^n, 32s) + random(0~25%)，优先服从 Retry-After
			//     header，上限 ~10 次；连续过载（CC 阈值为 3 次 529）切备用模型。
			//   - 流式中途失败：可恢复错误在 streaming 期间暂扣不展示，
			//     流结束后才进入恢复决策。
			//
			// 上下文超限（归循环体实现，需改 history）：
			//   - prompt_too_long：做一次比 auto compact 更激进的 reactive
			//     compact 后重试一次；仍超限才放弃（再压缩不会更小）。
			//     注意 OpenAI 兼容端点没有标准错误类型，多表现为 400 +
			//     context_length_exceeded 类 code，各家不一，需按端点匹配。
			//
			// CC 的其余 reason/transition 全量标注，按需补实现：
			//   model_error / aborted_streaming / image_error（图片过大单独
			//   处理）/ stop_hook_blocking / stop_hook_prevented / hook_stopped /
			//   token_budget_continuation（token 用量未到目标时续跑，连续
			//   3 次增量 <500 token 判定无产出即停）/ blocking_limit /
			//   collapse_drain_retry。已有对应物：max_turns≈MaxStepsError、
			//   aborted_tools≈InterruptedError 的补占位逻辑。
			return nil, err
		}
		msg := resp.Message
		history = append(history, msg)
		if err := a.record(ctx, msg); err != nil {
			return history, err
		}

		// finish_reason 是模型给出的权威终止信号
		switch resp.FinishReason {
		case FinishReasonToolCalls:
			// 模型请求调用工具，继续循环
		case FinishReasonStop, "":
			if len(msg.ToolCalls) == 0 {
				// 退出前过 Stop hook：返回非空则注入为 user 消息强制续跑，
				// 续跑消耗 maxSteps 预算，防止 hook 造成无限循环。
				if force := a.Hooks.TriggerStop(ctx, history); force != "" {
					msg := Message{Role: RoleUser, Content: force}
					history = append(history, msg)
					if err := a.record(ctx, msg); err != nil {
						return history, err
					}
					a.inject(msg)
					continue
				}
				return history, nil
			}
			// 部分兼容端点漏标 finish_reason，以实际 tool_calls 为准继续执行
		default:
			// "length" = 输出被截断；"content_filter" = 内容被过滤等异常终止
			// TODO(recovery): length 走截断恢复而非报错——先放大 max_tokens
			// 重发同一请求（不追加截断输出），仍截断才把截断内容落历史并
			// 注入续写提示（CC 原文："Resume directly — no apology, no
			// recap"），续写上限 ~3 次。前置条件：Chat 需支持每轮可调
			// max_tokens，目前 LLM 接口未暴露该参数。
			return nil, fmt.Errorf("model stopped abnormally: finish_reason=%q", resp.FinishReason)
		}

		if len(msg.ToolCalls) == 0 {
			return nil, errors.New("finish_reason=tool_calls but no tool calls in message")
		}

		// 注入历史快照（不含刚追加的 assistant 消息）；clone 切断与
		// history 后续 append 的别名共享。
		toolCtx := context.WithValue(ctx, historyCtxKey{}, slices.Clone(history[:len(history)-1]))
		for i, tc := range msg.ToolCalls {
			if err := ctx.Err(); err != nil {
				// 中断：给本批未执行的调用补占位结果，transcript 保持
				// "每个 tool_call 都有结果"的完整形态，调用方可续跑。
				for _, rest := range msg.ToolCalls[i:] {
					m := Message{
						Role:       RoleTool,
						ToolCallID: rest.ID,
						Content:    "error: execution interrupted",
						IsError:    true,
					}
					history = append(history, m)
					if err := a.record(ctx, m); err != nil {
						return history, err
					}
				}
				return history, &InterruptedError{}
			}
			var result string
			var isErr bool
			if a.OnToolCall != nil {
				a.OnToolCall(tc)
			}
			// PreToolUse hook 返回非空原因即阻止本次调用，原因直接回填给模型；
			// 被阻止的调用不触发 PostToolUse。
			if reason := a.Hooks.TriggerPreToolUse(toolCtx, ToolUseInput{
				Call:  tc,
				Index: i,
				Batch: msg.ToolCalls,
			}); reason != "" {
				result, isErr = reason, true
				if a.OnToolResult != nil {
					a.OnToolResult(ToolResult{Call: tc, Output: reason, IsError: true, Blocked: true})
				}
			} else {
				start := time.Now()
				res := a.registry.Call(toolCtx, tc.Name, tc.Arguments)
				result, isErr = res.Output, res.IsError
				a.Hooks.TriggerPostToolUse(toolCtx, PostToolUseInput{
					Call: tc, Result: res.Output, IsError: res.IsError,
				})
				if a.OnToolResult != nil {
					a.OnToolResult(ToolResult{Call: tc, Output: res.Output, IsError: res.IsError, Dur: time.Since(start)})
				}
			}
			m := Message{
				Role:       RoleTool,
				ToolCallID: tc.ID,
				Content:    result,
				IsError:    isErr,
			}
			history = append(history, m)
			if err := a.record(toolCtx, m); err != nil {
				return history, err
			}
		}
	}
	return history, &MaxStepsError{Steps: a.maxSteps}
}

// inject 通知外部（UI）一条 hook 注入的消息。
func (a *Agent) inject(msg Message) {
	if a.OnInject != nil {
		a.OnInject(msg)
	}
}

// record 把刚追加进 history 的消息落库；Rec 为 nil 时零开销。
// 返回非 nil 表示持久化失败，Run 中止本轮（已落库部分保留）。
func (a *Agent) record(ctx context.Context, msg Message) error {
	if a.Rec == nil {
		return nil
	}
	return a.Rec.Append(ctx, msg)
}
