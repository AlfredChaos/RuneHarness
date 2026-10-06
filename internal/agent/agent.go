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

	// Compactor 是上下文压缩能力（internal/compact 实现）；nil 表示不压缩。
	Compactor Compactor

	// Requests 记录每次 Chat 的发送留痕（水印 + 视图哈希，出错时含完整
	// 发送形态）；nil 表示不记录。写失败按 fail the turn 处理。
	Requests RequestLogger
}

// Recorder 接收 loop 追加进 history 的每条消息，由外部存储实现
// （internal/session.Store 结构满足）。tenant/session 目标经 ctx 读取。
// 返回分配的行 id，loop 写回 Message.ID，压缩决策按行 id 引用消息。
type Recorder interface {
	Append(ctx context.Context, msg Message) (int64, error)
}

// Compactor 是上下文压缩能力，由 internal/compact 实现。三个方法对应
// loop 的三个挂点；返回的 history 未变化时原样返回。
type Compactor interface {
	// Backfill 在一批工具结果追加进 history 前规范化：空结果写固定标记、
	// 超限结果落 blob 换指针。calls 与 results 一一对应。
	Backfill(ctx context.Context, calls []ToolCall, results []Message) ([]Message, error)
	// Maintain 在每次 Chat 前调用：按阈值卸载、摘要；用量超过阻断线且
	// 无法压缩时返回 ErrContextFull。
	Maintain(ctx context.Context, history []Message) ([]Message, error)
	// Compact 立即执行压缩；reason 取 "model" / "manual" / "reactive"。返回的
	// history 总是当前有效视图（失败时可能已完成卸载），调用方应一律采用；
	// history 末尾的消息总留在尾部。
	Compact(ctx context.Context, history []Message, reason string) ([]Message, error)
	// NoteLimit 上报端点在超长报文里给出的真实窗口上限（被动自愈）；
	// 实现方把有效窗口钳到 min(配置, limit) 并持久化。
	NoteLimit(ctx context.Context, limit int)
}

// RequestLog 是一次 Chat 的发送留痕。
type RequestLog struct {
	ViewHash string // 发送形态的哈希，重放核对用（见 ViewHash）
	Payload  string // 仅 Chat 出错时填：完整发送形态 JSON
	Error    string // Chat 错误原文
}

// RequestLogger 接收每次 Chat 的留痕，由外部存储实现。
type RequestLogger interface {
	LogRequest(ctx context.Context, r RequestLog) error
}

// CompactToolName 是模型侧 compact 工具名：loop 对它特判（4.7），
// 不经 Registry 执行。
const CompactToolName = "compact"

// CompactDoneText 是 compact 工具成功时回填的固定结果。
const CompactDoneText = "context compacted"

var (
	// ErrContextLength 表示端点因上下文超长拒绝请求，由 LLM 适配器包装；
	// loop 捕获后做一次 reactive 压缩并重试。
	ErrContextLength = errors.New("context length exceeded")
	// ErrContextFull 表示用量已超过阻断线且当前无法压缩，loop 拒发请求。
	ErrContextFull = errors.New("context window is full; run /compact or start a new session")
)

// ContextLengthError 是端点上下文超长错误的结构化形态：Err 保留原始报文，
// Limit 是从报文里解析出的真实窗口上限（0 表示没解析到）。适配器返回它，
// loop 据此做一次 reactive 压缩并把 Limit 上报给压缩器做被动自愈。
type ContextLengthError struct {
	Limit int
	Err   error
}

func (e *ContextLengthError) Error() string { return e.Err.Error() }
func (e *ContextLengthError) Unwrap() error { return e.Err }
func (e *ContextLengthError) Is(target error) bool {
	return target == ErrContextLength
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
			msg := Message{Role: RoleUser, Kind: KindInject, Content: inject}
			if err := a.push(ctx, &history, msg); err != nil {
				return history, err
			}
			a.inject(msg)
		}
		// 压缩挂点：Chat 前按阈值卸载 / 摘要（plan §4.3–4.5）。
		if a.Compactor != nil {
			next, err := a.Compactor.Maintain(ctx, history)
			if err != nil {
				if ctx.Err() != nil {
					return history, &InterruptedError{}
				}
				return history, err
			}
			history = next
		}
		resp, err := a.chat(ctx, history)
		// 上下文超限：做一次 reactive 压缩后重试一次；仍失败就上抛——
		// 压过一遍还超限，再压也不会更小（plan §4.6）。
		if err != nil && errors.Is(err, ErrContextLength) && a.Compactor != nil && ctx.Err() == nil {
			// 报文里若带真实窗口上限，钳小有效窗口——配置或模型表
			// 高估时自愈（钳过的窗口持久化在 session_state）。
			var cle *ContextLengthError
			if errors.As(err, &cle) && cle.Limit > 0 {
				a.Compactor.NoteLimit(ctx, cle.Limit)
			}
			// Compact 返回的总是当前有效视图（失败时可能已卸载），一律采用。
			next, cerr := a.Compactor.Compact(ctx, history, "reactive")
			history = next
			if cerr != nil {
				err = fmt.Errorf("%w (reactive compaction failed: %v)", err, cerr)
			} else {
				resp, err = a.chat(ctx, history)
			}
		}
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
		if err := a.push(ctx, &history, msg); err != nil {
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
					msg := Message{Role: RoleUser, Kind: KindInject, Content: force}
					if err := a.push(ctx, &history, msg); err != nil {
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
		// 整批结果先收齐再规范化、追加：聚合预算要看整批（plan §4.2）。
		results := make([]Message, 0, len(msg.ToolCalls))
		calls := make([]ToolCall, 0, len(msg.ToolCalls))
		var compactCall *ToolCall // 本批第一个 compact 调用：结果在压缩后定稿（plan §4.7）
		interrupted := false
		for i, tc := range msg.ToolCalls {
			if err := ctx.Err(); err != nil {
				// 中断：给本批未执行的调用补占位结果，transcript 保持
				// "每个 tool_call 都有结果"的完整形态，调用方可续跑。
				for _, rest := range msg.ToolCalls[i:] {
					calls = append(calls, rest)
					results = append(results, Message{
						Role:       RoleTool,
						ToolCallID: rest.ID,
						Content:    "error: execution interrupted",
						IsError:    true,
					})
				}
				interrupted = true
				break
			}
			var result string
			var isErr bool
			if a.OnToolCall != nil {
				a.OnToolCall(tc)
			}
			if tc.Name == CompactToolName && a.Compactor != nil {
				if compactCall == nil {
					compactCall = &msg.ToolCalls[i]
					continue
				}
				result, isErr = "error: compact already requested in this batch", true
				a.reportResult(ToolResult{Call: tc, Output: result, IsError: true})
				calls = append(calls, tc)
				results = append(results, Message{Role: RoleTool, ToolCallID: tc.ID, Content: result, IsError: true})
				continue
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
			calls = append(calls, tc)
			results = append(results, Message{
				Role:       RoleTool,
				ToolCallID: tc.ID,
				Content:    result,
				IsError:    isErr,
			})
		}
		if compactCall != nil && interrupted {
			calls = append(calls, *compactCall)
			results = append(results, Message{Role: RoleTool, ToolCallID: compactCall.ID,
				Content: "error: execution interrupted", IsError: true})
			compactCall = nil
		}
		if a.Compactor != nil {
			// 规范化写 blob 属于"已发生事实"的持久化，不随中断取消。
			var err error
			if results, err = a.Compactor.Backfill(context.WithoutCancel(ctx), calls, results); err != nil {
				return history, err
			}
		}
		for _, r := range results {
			if err := a.push(toolCtx, &history, r); err != nil {
				return history, err
			}
		}
		if interrupted {
			return history, &InterruptedError{}
		}
		if compactCall != nil {
			var err error
			if history, err = a.compactByModel(ctx, history, *compactCall); err != nil {
				return history, err
			}
		}
	}
	return history, &MaxStepsError{Steps: a.maxSteps}
}

// chat 发起一次 LLM 调用并写请求留痕：成功记水印与视图哈希，失败另附
// 完整发送形态与错误原文，作为排查 API 错误的现场证据（plan §5.3）。
func (a *Agent) chat(ctx context.Context, history []Message) (Response, error) {
	resp, err := a.llm.Chat(ctx, history, a.registry.Specs(), a.OnPartial)
	if a.Requests == nil {
		return resp, err
	}
	// 消息水印由 LogRequest 在落库时取 MAX(messages.id)：随请求发送的形态
	// 由消息行与压缩控制行共同决定，水位必须连控制行一起覆盖。
	r := RequestLog{ViewHash: ViewHash(history)}
	if err != nil {
		r.Payload, r.Error = ViewPayload(history), err.Error()
	}
	if lerr := a.Requests.LogRequest(context.WithoutCancel(ctx), r); lerr != nil {
		return resp, errors.Join(err, fmt.Errorf("log request: %w", lerr))
	}
	return resp, err
}

// compactByModel 执行模型发起的压缩：同组其它调用已回填，摘要调用先于
// compact 的结果行；结果行落在尾部、原样保留，模型下一轮能看到压缩已完成。
// 压缩失败时结果行写失败原因，history 原样继续（plan §4.7）。
func (a *Agent) compactByModel(ctx context.Context, history []Message, tc ToolCall) ([]Message, error) {
	res := Message{Role: RoleTool, ToolCallID: tc.ID, Content: CompactDoneText}
	next, err := a.Compactor.Compact(ctx, append(slices.Clone(history), res), "model")
	// Compact 返回的总是当前有效视图（失败时可能已卸载），且末尾消息留在
	// 尾部：去掉占位，由下方 push 落库定稿。
	history = next[:len(next)-1]
	switch {
	case err == nil:
	case ctx.Err() != nil:
		res.Content, res.IsError = "error: execution interrupted", true
	default:
		res.Content, res.IsError = "error: "+err.Error(), true
	}
	a.reportResult(ToolResult{Call: tc, Output: res.Content, IsError: res.IsError})
	if perr := a.push(ctx, &history, res); perr != nil {
		return history, perr
	}
	if ctx.Err() != nil {
		return history, &InterruptedError{}
	}
	return history, nil
}

// reportResult 把工具结果回报给 UI。
func (a *Agent) reportResult(r ToolResult) {
	if a.OnToolResult != nil {
		a.OnToolResult(r)
	}
}

// push 追加一条消息到 history 并落库，把分配的行 id 写回消息。
// 落库失败时消息仍在 history 中（与"已发生"一致），由调用方中止本轮。
func (a *Agent) push(ctx context.Context, history *[]Message, msg Message) error {
	id, err := a.record(ctx, msg)
	msg.ID = id
	*history = append(*history, msg)
	return err
}

// inject 通知外部（UI）一条 hook 注入的消息。
func (a *Agent) inject(msg Message) {
	if a.OnInject != nil {
		a.OnInject(msg)
	}
}

// record 把刚追加进 history 的消息落库；Rec 为 nil 时零开销。
// 返回非 nil 表示持久化失败，Run 中止本轮（已落库部分保留）。
// record 落库一条已进 history 的消息。ctx 只带调用方信号进 Append 是错的：
// 中断/超时后落库会连同"已发生的事实"一起死掉——例如中断分支补的占位
// 结果丢库，返回错误顶成 context.Canceled 而非 InterruptedError。
// WithoutCancel 语义正解：取消停的是未来的工作，不是已发生消息的持久化。
func (a *Agent) record(ctx context.Context, msg Message) (int64, error) {
	if a.Rec == nil {
		return 0, nil
	}
	return a.Rec.Append(context.WithoutCancel(ctx), msg)
}
