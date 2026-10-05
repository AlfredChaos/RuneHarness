package agent

import "context"

// ToolUseInput 是 PreToolUse hook 的输入。
// Batch/Index 供需要整批信息的 hook 使用（如统计本批确认总数）。
type ToolUseInput struct {
	Call  ToolCall   // 本次调用
	Index int        // 在本步批次中的序号
	Batch []ToolCall // 本步全部 tool_calls
}

// PostToolUseInput 是 PostToolUse hook 的输入。
type PostToolUseInput struct {
	Call    ToolCall
	Result  string // 工具结果原文（即回填给模型的内容）
	IsError bool   // 结果是否为失败（对应 Registry.Call 的 IsError）
}

// UserPromptSubmitFunc 在用户输入提交后、进入 LLM 前触发。
// replace 非空则替换输入；blockReason 非空则拦截本次输入（优先于 replace）。
type UserPromptSubmitFunc func(ctx context.Context, prompt string) (replace, blockReason string)

// PreToolUseFunc 在工具执行前触发。
// 返回非空 blockReason 则阻止本次调用，该字符串作为 tool result 原文回填给
// 模型（应自带 "error:" 等前缀）。
type PreToolUseFunc func(ctx context.Context, in ToolUseInput) (blockReason string)

// PostToolUseFunc 在工具执行后触发，仅做副作用（日志、审计等）。
// 被 PreToolUse 阻止的调用不会触发。
type PostToolUseFunc func(ctx context.Context, in PostToolUseInput)

// StopFunc 在循环即将退出时触发。返回非空字符串则作为 user 消息注入并强制
// 续跑；续跑同样消耗 maxSteps 预算，防止 hook 造成无限循环。
// history 仅供只读，hook 不应修改。
type StopFunc func(ctx context.Context, history []Message) (forcePrompt string)

// PreChatFunc 在 Run 内每次调用 LLM 前触发（含 Stop hook 强制续跑的轮次）。
// 返回非空字符串则作为 user 消息注入 history，用于 nag 提醒等每轮检查；
// history 仅供只读，hook 不应修改。
type PreChatFunc func(ctx context.Context, history []Message) (inject string)

// Hooks 是 hook 注册表：每种事件挂一组回调，按注册顺序执行，
// 首个返回非空结果的回调短路后续回调（即"停"的语义）。
// 零值即空注册表，可直接使用。
type Hooks struct {
	userPromptSubmit []UserPromptSubmitFunc
	preToolUse       []PreToolUseFunc
	postToolUse      []PostToolUseFunc
	stop             []StopFunc
	preChat          []PreChatFunc
}

func (h *Hooks) OnUserPromptSubmit(f UserPromptSubmitFunc) {
	h.userPromptSubmit = append(h.userPromptSubmit, f)
}

func (h *Hooks) OnPreToolUse(f PreToolUseFunc) { h.preToolUse = append(h.preToolUse, f) }

func (h *Hooks) OnPostToolUse(f PostToolUseFunc) { h.postToolUse = append(h.postToolUse, f) }

func (h *Hooks) OnStop(f StopFunc) { h.stop = append(h.stop, f) }

func (h *Hooks) OnPreChat(f PreChatFunc) { h.preChat = append(h.preChat, f) }

// TriggerUserPromptSubmit 由调用方在用户输入进入 LLM 前触发。
func (h *Hooks) TriggerUserPromptSubmit(ctx context.Context, prompt string) (replace, blockReason string) {
	for _, f := range h.userPromptSubmit {
		if r, b := f(ctx, prompt); r != "" || b != "" {
			return r, b
		}
	}
	return "", ""
}

// TriggerPreToolUse 在每次工具调用执行前触发，返回首个非空阻止原因。
func (h *Hooks) TriggerPreToolUse(ctx context.Context, in ToolUseInput) string {
	for _, f := range h.preToolUse {
		if r := f(ctx, in); r != "" {
			return r
		}
	}
	return ""
}

// TriggerPostToolUse 在每次工具执行后触发。
func (h *Hooks) TriggerPostToolUse(ctx context.Context, in PostToolUseInput) {
	for _, f := range h.postToolUse {
		f(ctx, in)
	}
}

// TriggerStop 在循环即将退出时触发，返回首个非空续跑提示。
func (h *Hooks) TriggerStop(ctx context.Context, history []Message) string {
	for _, f := range h.stop {
		if s := f(ctx, history); s != "" {
			return s
		}
	}
	return ""
}

// TriggerPreChat 在每次调用 LLM 前触发，返回首个非空注入消息。
func (h *Hooks) TriggerPreChat(ctx context.Context, history []Message) string {
	for _, f := range h.preChat {
		if s := f(ctx, history); s != "" {
			return s
		}
	}
	return ""
}
