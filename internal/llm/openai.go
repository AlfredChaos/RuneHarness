// Package llm 用 openai-go SDK 实现 agent.LLM 接口（适配器）。
// 所有 SDK 类型都收敛在本文件内，不向外泄漏。
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"

	"runeharness/internal/agent"
	"runeharness/internal/config"
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
		Messages: toParams(history, c.model),
		Tools:    toTools(specs),
		// 要求流末尾回传 usage，作为上下文估算锚点；不支持的端点忽略该字段。
		StreamOptions: openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)},
	})
	defer func() { _ = stream.Close() }()

	var acc openai.ChatCompletionAccumulator
	var raw strings.Builder
	// usage 取最后一个带 usage 的 chunk，不用 accumulator 的累加值：
	// 部分兼容端点每个 chunk 都回传累计 usage，累加会重复计数。
	var usage *agent.Usage
	for stream.Next() {
		chunk := stream.Current()
		acc.AddChunk(chunk)
		if chunk.Usage.PromptTokens > 0 {
			usage = &agent.Usage{
				Prompt:     int(chunk.Usage.PromptTokens),
				Completion: int(chunk.Usage.CompletionTokens),
				Cached:     int(chunk.Usage.PromptTokensDetails.CachedTokens),
			}
		}
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
		return agent.Response{}, classify(err)
	}
	if len(acc.Choices) == 0 {
		return agent.Response{}, errors.New("empty response from API")
	}
	choice := acc.Choices[0]
	msg := fromMessage(choice.Message)
	msg.Usage = usage
	return agent.Response{
		Message:      msg,
		FinishReason: agent.FinishReason(choice.FinishReason),
	}, nil
}

// contextLengthMarkers 是各家端点"上下文超长"错误的特征串（小写匹配）。
// OpenAI 兼容端点没有标准错误类型：多表现为 400 + context_length_exceeded，
// 也有 prompt_too_long、413 与各种措辞。
var contextLengthMarkers = []string{
	"context_length_exceeded", "prompt_too_long", "maximum context length",
	"context length", "context window", "too many tokens", "prompt is too long",
	"reduce the length",
}

// classify 把端点的上下文超长错误包装为 agent.ContextLengthError：从报文
// 里解析真实窗口上限（被动自愈用），保留原错误供展示；其余错误原样返回。
func classify(err error) error {
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	if apiErr.StatusCode == 413 {
		return &agent.ContextLengthError{Limit: parseLimit(apiErr), Err: err}
	}
	if apiErr.StatusCode != 400 {
		return err
	}
	text := strings.ToLower(apiErr.Code + " " + apiErr.Type + " " + apiErr.Message + " " + apiErr.RawJSON())
	for _, m := range contextLengthMarkers {
		if strings.Contains(text, m) {
			return &agent.ContextLengthError{Limit: parseLimit(apiErr), Err: err}
		}
	}
	return err
}

// limitAfterMarker 匹配 "...context length... is N tokens" 类报文里的
// 窗口上限。取第一个 4-8 位数字：这类报文通常是 "上限 N、你的请求 M"，
// 第一个数字是上限。
var limitAfterMarker = regexp.MustCompile(`(\d[\d,]{3,8})\s*tokens`)

// parseLimit 尽力从报错正文提取窗口上限；没解析到返回 0。
func parseLimit(apiErr *openai.Error) int {
	for _, s := range []string{apiErr.Message, apiErr.RawJSON()} {
		m := limitAfterMarker.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		if n, err := strconv.Atoi(strings.ReplaceAll(m[1], ",", "")); err == nil && n >= 1_000 {
			return n
		}
	}
	return 0
}

// toParams 是发送形态的唯一 marshal 点：按角色选 SDK 便捷构造；带附件
// 分片的 user 消息改走 content parts 数组（需要 model 做视觉能力门控）。
//
// 历史重放只放行最后一条带图消息的真实 Parts：更早的带图消息降级为占位
// 文本。持久化的图片 base64 每轮随历史重发会让每次调用多扛 ~MB 级上行，
// 视觉端点对大图慢且不稳（真机观测 120s requestTimeout 整轮 error）——
// 这是"发过一次图之后每轮都超时"的结构性成因。当前轮的图必在最后一条
// 带图消息上，语义不受损；更早的图在 transcript 里仍完整，只是不再喂给模型。
func toParams(history []agent.Message, model string) []openai.ChatCompletionMessageParamUnion {
	lastImg := -1
	for i, m := range history {
		if m.Role == agent.RoleUser && hasImagePart(m) {
			lastImg = i
		}
	}
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(history))
	for i, m := range history {
		switch m.Role {
		case agent.RoleSystem:
			out = append(out, openai.SystemMessage(m.Content))
		case agent.RoleUser:
			if len(m.Parts) > 0 {
				if i != lastImg && hasImagePart(m) {
					// 非当前轮的带图历史：parts 全量换占位文本（见函数头注释）
					m.Parts = nil
					m.Content = joinContent(m.Content, "[image omitted: replayed history]")
				}
				out = append(out, userParam(m, model))
			} else {
				out = append(out, openai.UserMessage(m.Content))
			}
		case agent.RoleAssistant:
			out = append(out, assistantParam(m))
		case agent.RoleTool:
			out = append(out, openai.ToolMessage(m.Content, m.ToolCallID))
		}
	}
	return out
}

func hasImagePart(m agent.Message) bool {
	for _, p := range m.Parts {
		if p.Kind == agent.PartImage {
			return true
		}
	}
	return false
}

func joinContent(a, b string) string {
	if a == "" {
		return b
	}
	return a + "\n" + b
}

// userParam 组装带附件分片的 user 消息：Content 非空时第一个 part 是文本，
// 其后每个 Part 产一个 image_url 分片，URL 用 base64 data-URI 内联——
// 附件与 prompt 同生命周期，没有公网可达 URL，hosted URL 路线不适用
// （调研 §2.1）。
//
// detail 枚举各家不同：OpenAI 是 auto/low/high（缺省 auto），MiniMax 是
// low/default/high（缺省 default）。Part.Detail 为空时完全不发送该字段，
// 各端点取各自缺省值——省略是两端交集上的安全映射；MiniMax 的缺省枚举
// "default" 恰是字段缺席时端点的行为，永远不该出现在 Part.Detail 里：
// SDK 在 marshal 时做客户端校验，只放行 auto/low/high，其余值（含
// "default"）在进程内即报错。
//
// 视觉门控在唯一 marshal 点做：纯文本模型的图片分片降级为占位文本，
// 绝不把 image_url 发给不支持图片的模型。
func userParam(m agent.Message, model string) openai.ChatCompletionMessageParamUnion {
	parts := make([]openai.ChatCompletionContentPartUnionParam, 0, len(m.Parts)+1)
	if m.Content != "" {
		parts = append(parts, openai.TextContentPart(m.Content))
	}
	for _, p := range m.Parts {
		if p.Kind != agent.PartImage || !config.ModelVision(model) {
			parts = append(parts, openai.TextContentPart(partPlaceholder(p)))
			continue
		}
		image := openai.ChatCompletionContentPartImageImageURLParam{
			URL: "data:" + p.MediaType + ";base64," + p.DataB64,
			// SDK 标了 omitzero：空串不会出现在 wire 上
			Detail: p.Detail,
		}
		parts = append(parts, openai.ImageContentPart(image))
	}
	return openai.UserMessage(parts)
}

// partPlaceholder 把发不出去的分片降级为内联占位文本，文案风格对齐
// files.go 的 [binary file omitted]。
func partPlaceholder(p agent.Part) string {
	if p.Kind != agent.PartImage {
		return "[attachment omitted: unsupported kind " + p.Kind + "]"
	}
	return "[image omitted: model is text-only]"
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
