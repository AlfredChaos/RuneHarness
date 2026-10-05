// Package llm 用 openai-go SDK 实现 agent.LLM 接口（适配器）。
// 所有 SDK 类型都收敛在本文件内，不向外泄漏。
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	"runeharness/internal/agent"
	"runeharness/internal/tools"
)

// requestTimeout 单次 LLM 请求的超时时间。
const requestTimeout = 120 * time.Second

// Client 实现 agent.LLM。
type Client struct {
	sdk   openai.Client
	model string
}

// NewClient 创建 OpenAI 兼容端点客户端；baseURL 为空时使用 SDK 默认（官方 API）。
func NewClient(apiKey, baseURL, model string) Client {
	opts := []option.RequestOption{option.WithAPIKey(apiKey)}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return Client{sdk: openai.NewClient(opts...), model: model}
}

// Chat 流式发送一轮对话请求：每个 chunk 到达时回调累积快照，
// 流结束后返回完整的 assistant 消息与 finish_reason。
func (c Client) Chat(ctx context.Context, history []agent.Message, specs []tools.Spec, onPartial func(agent.Partial)) (agent.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	stream := c.sdk.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{
		Model:    c.model,
		Messages: toParams(history),
		Tools:    toTools(specs),
	})
	defer func() { _ = stream.Close() }()

	var acc openai.ChatCompletionAccumulator
	var raw strings.Builder
	for stream.Next() {
		chunk := stream.Current()
		acc.AddChunk(chunk)
		if onPartial != nil && len(chunk.Choices) > 0 {
			raw.WriteString(chunk.Choices[0].Delta.Content)
			// 对累积文本整体重切：<think> 标签可能被拆到多个 chunk。
			// 尾部若是半个标签先扣住，避免 "<thi" 这类碎片闪现在正文里。
			display := raw.String()
			if n := trailingTagPrefixLen(display); n > 0 {
				display = display[:len(display)-n]
			}
			thinking, body := splitThinking(display)
			onPartial(agent.Partial{Thinking: thinking, Content: body})
		}
	}
	if err := stream.Err(); err != nil {
		return agent.Response{}, err
	}
	if len(acc.Choices) == 0 {
		return agent.Response{}, errors.New("empty response from API")
	}
	choice := acc.Choices[0]
	return agent.Response{
		Message:      fromMessage(choice.Message),
		FinishReason: agent.FinishReason(choice.FinishReason),
	}, nil
}

func toParams(history []agent.Message) []openai.ChatCompletionMessageParamUnion {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(history))
	for _, m := range history {
		switch m.Role {
		case agent.RoleSystem:
			out = append(out, openai.SystemMessage(m.Content))
		case agent.RoleUser:
			out = append(out, openai.UserMessage(m.Content))
		case agent.RoleAssistant:
			out = append(out, assistantParam(m))
		case agent.RoleTool:
			out = append(out, openai.ToolMessage(m.Content, m.ToolCallID))
		}
	}
	return out
}

// assistantParam 重建 assistant 消息，保留 tool_calls（模型要求原样回传）。
func assistantParam(m agent.Message) openai.ChatCompletionMessageParamUnion {
	p := openai.ChatCompletionAssistantMessageParam{}
	if m.Content != "" {
		p.Content.OfString = openai.String(m.Content)
	}
	for _, tc := range m.ToolCalls {
		p.ToolCalls = append(p.ToolCalls, openai.ChatCompletionMessageToolCallUnionParam{
			OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
				ID: tc.ID,
				Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
					Name:      tc.Name,
					Arguments: string(tc.Arguments),
				},
			},
		})
	}
	return openai.ChatCompletionMessageParamUnion{OfAssistant: &p}
}

func toTools(specs []tools.Spec) []openai.ChatCompletionToolUnionParam {
	out := make([]openai.ChatCompletionToolUnionParam, 0, len(specs))
	for _, s := range specs {
		out = append(out, openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
			Name:        s.Name,
			Description: openai.String(s.Description),
			Parameters:  shared.FunctionParameters(s.Parameters),
		}))
	}
	return out
}

func fromMessage(msg openai.ChatCompletionMessage) agent.Message {
	thinking, body := splitThinking(msg.Content)
	out := agent.Message{Role: agent.RoleAssistant, Content: body, Thinking: thinking}
	for _, tc := range msg.ToolCalls {
		if tc.Type != "function" {
			continue
		}
		out.ToolCalls = append(out.ToolCalls, agent.ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: json.RawMessage(tc.Function.Arguments),
		})
	}
	return out
}

// splitThinking 把 <think>…</think> 推理块从正文中剥离。
// MiniMax/DeepSeek 等推理模型把思考链内嵌在 content 里；
// 未闭合的 <think>（如被 max_tokens 截断）整体归入 thinking。
func splitThinking(content string) (thinking, body string) {
	var think, text strings.Builder
	rest := content
	for len(rest) > 0 {
		i := strings.Index(rest, "<think>")
		if i < 0 {
			text.WriteString(rest)
			break
		}
		text.WriteString(rest[:i])
		rest = rest[i+len("<think>"):]
		j := strings.Index(rest, "</think>")
		if j < 0 {
			think.WriteString(rest)
			break
		}
		think.WriteString(rest[:j])
		rest = rest[j+len("</think>"):]
	}
	return strings.TrimSpace(think.String()), strings.TrimSpace(text.String())
}

// trailingTagPrefixLen 返回 s 尾部与 <think>/</think> 前缀重合的长度。
// 仅用于流式展示的暂缓输出；最终消息仍按全文解析。
func trailingTagPrefixLen(s string) int {
	longest := 0
	for _, tag := range []string{"<think>", "</think>"} {
		for i := 1; i < len(tag); i++ {
			if strings.HasSuffix(s, tag[:i]) && i > longest {
				longest = i
			}
		}
	}
	return longest
}
