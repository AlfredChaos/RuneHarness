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

// Kind 区分消息行的类型。零值是普通消息；控制行不进发送形态，只记录
// 压缩决策，读侧组装视图时消费（见 session.Fold）。
type Kind string

const (
	KindMessage   Kind = ""                 // 普通消息
	KindInject    Kind = "inject"           // hook 注入的 user 消息（todo nag、压缩提醒）
	KindSummary   Kind = "compact_summary"  // 压缩摘要，role=user，进发送形态
	KindBoundary  Kind = "compact_boundary" // 控制行：压缩边界，Content 为 meta JSON
	KindViewClear Kind = "view_clear"       // 控制行：卸载决策，Content 为 meta JSON
	KindFork      Kind = "fork"             // 控制行：子会话继承标记，Content 为 meta JSON
)

// IsControl 报告该类型是否为控制行（不进发送形态）。
func (k Kind) IsControl() bool {
	return k == KindBoundary || k == KindViewClear || k == KindFork
}

// Usage 是一次 LLM 请求的真实 token 用量，挂在该请求产出的 assistant
// 消息上，作为上下文估算的锚点。
type Usage struct {
	Prompt     int `json:"prompt"`
	Completion int `json:"completion"`
	Cached     int `json:"cached"`
}

// Message 是对话历史中的一条消息。
// Role == RoleAssistant 时 ToolCalls 可能有值；
// Role == RoleTool 时 ToolCallID 关联对应的 ToolCall.ID，IsError 标记
// 该结果是否为失败（工具报错、hook 阻止、中断占位）。
// Thinking 是推理模型剥离出的思考链，仅用于展示，不回传给 API；
// IsError、ID、Kind、Usage 同样仅供内部使用，不随消息回传 API。
type Message struct {
	ID         int64 // 存储行 id，落库后由 Recorder 分配；未落库为 0
	Kind       Kind
	Role       Role
	Content    string
	Thinking   string
	ToolCalls  []ToolCall
	ToolCallID string
	IsError    bool
	Usage      *Usage
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
// FinishReason 是单次补全的元数据，不写入对话历史；端点返回的 usage
// 挂在 Message.Usage 上（端点不返回时为 nil）。
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
