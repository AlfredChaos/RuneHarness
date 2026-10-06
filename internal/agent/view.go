package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// wireMessage 是一条消息里真正发给 API 的字段：Thinking、IsError、ID、
// Kind、Usage 都不回传，不参与哈希。
type wireMessage struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

func wireView(history []Message) []wireMessage {
	out := make([]wireMessage, len(history))
	for i, m := range history {
		out[i] = wireMessage{Role: m.Role, Content: m.Content, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID}
	}
	return out
}

// ViewPayload 返回发送形态的 JSON（出错请求的现场证据）。
func ViewPayload(history []Message) string {
	b, _ := json.Marshal(wireView(history)) // 字段全是可序列化类型，不会失败
	return string(b)
}

// ViewHash 返回发送形态的 sha256：按存储重放出的视图与当时的哈希比对，
// 可发现组装逻辑变更导致的字节漂移。
func ViewHash(history []Message) string {
	sum := sha256.Sum256([]byte(ViewPayload(history)))
	return hex.EncodeToString(sum[:])
}
