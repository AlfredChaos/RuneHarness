package agent

import "encoding/json"

// Role 是消息的发送方角色。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall 是模型请求的一次工具调用。
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage // 模型生成的 JSON 参数原文
}

// Message 是对话历史中的一条消息。
// Role == RoleAssistant 时 ToolCalls 可能有值；
// Role == RoleTool 时 ToolCallID 关联对应的 ToolCall.ID，IsError 标记
// 该结果是否为失败（工具报错、hook 阻止、中断占位）。
// Thinking 是推理模型剥离出的思考链，仅用于展示，不回传给 API；
// IsError 同样仅供内部使用（hook、UI、熔断），不随消息回传 API。
type Message struct {
	Role       Role
	Content    string
	Thinking   string
	ToolCalls  []ToolCall
	ToolCallID string
	IsError    bool
}

// FinishReason 是模型给出的本轮结束原因（对应 API 的 finish_reason 字段）。
type FinishReason string

const (
	FinishReasonStop          FinishReason = "stop"           // 正常结束
	FinishReasonLength        FinishReason = "length"         // 输出被 max_tokens 截断
	FinishReasonToolCalls     FinishReason = "tool_calls"     // 请求调用工具
	FinishReasonContentFilter FinishReason = "content_filter" // 内容被过滤
)

// Response 是一次 LLM 调用的结果：assistant 消息 + 本轮结束原因。
// FinishReason 是单次补全的元数据，不写入对话历史。
type Response struct {
	Message      Message
	FinishReason FinishReason
}

// Partial 是流式补全过程中的累积快照：到当前为止的完整 Thinking/Content。
// 不是增量 diff——消费方直接整体替换展示。
type Partial struct {
	Thinking string
	Content  string
}
