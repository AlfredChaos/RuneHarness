package llm

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"

	"runeharness/internal/agent"
)

// 模拟流式分片：累积 -> 扣掉半个标签的尾巴 -> 切分，复刻 Chat 里的展示路径。
func emit(chunks []string) (thinking, body string) {
	var raw strings.Builder
	for _, c := range chunks {
		raw.WriteString(c)
		display := raw.String()
		if n := trailingTagPrefixLen(display); n > 0 {
			display = display[:len(display)-n]
		}
		thinking, body = splitThinking(display)
	}
	return thinking, body
}

func TestSplitThinkingAcrossChunks(t *testing.T) {
	// 用户场景：思考链内容跨多个 chunk，且 think 标签被拆到两个 chunk
	thinking, body := emit([]string{"<think>123", "456", "789</think>", "我是 minimax"})
	if thinking != "123456789" {
		t.Errorf("thinking = %q, want %q", thinking, "123456789")
	}
	if body != "我是 minimax" {
		t.Errorf("body = %q, want %q", body, "我是 minimax")
	}
}

func TestSplitThinkingTagSplitAcrossChunks(t *testing.T) {
	// 标签本身被拆开："<thi" + "nk>..."。
	// 过程中 <thi 不会被误当正文（被尾部前缀扣住），最终结果正确。
	var bodies []string
	var raw strings.Builder
	for _, c := range []string{"你好<thi", "nk>想一想", "</think>结论"} {
		raw.WriteString(c)
		display := raw.String()
		if n := trailingTagPrefixLen(display); n > 0 {
			display = display[:len(display)-n]
		}
		_, b := splitThinking(display)
		bodies = append(bodies, b)
	}
	if bodies[0] != "你好" {
		t.Errorf("first partial body = %q, want %q (半个标签不该进正文)", bodies[0], "你好")
	}
	thinking, body := emit([]string{"你好<thi", "nk>想一想", "</think>结论"})
	if thinking != "想一想" || body != "你好结论" {
		t.Errorf("got thinking=%q body=%q, want %q/%q", thinking, body, "想一想", "你好结论")
	}
}

func TestSplitThinkingUnclosed(t *testing.T) {
	// 流被截断、</think> 没到达：剩余内容全归 thinking
	thinking, body := emit([]string{"<think>思考中", "还在想"})
	if thinking != "思考中还在想" || body != "" {
		t.Errorf("got thinking=%q body=%q", thinking, body)
	}
}

func TestSplitThinkingNoTag(t *testing.T) {
	thinking, body := emit([]string{"普通回复", "无标签"})
	if thinking != "" || body != "普通回复无标签" {
		t.Errorf("got thinking=%q body=%q", thinking, body)
	}
}

func TestTrailingTagPrefixLen(t *testing.T) {
	cases := map[string]int{
		"abc":         0,
		"abc<":        1,
		"abc<thi":     4,
		"abc<think":   6,
		"abc<think>":  0, // 完整标签不算前缀
		"abc</th":     4,
		"abc<think>x": 0,
	}
	for in, want := range cases {
		if got := trailingTagPrefixLen(in); got != want {
			t.Errorf("trailingTagPrefixLen(%q) = %d, want %d", in, got, want)
		}
	}
}

// marshalBody 把发送形态编成完整请求体 JSON，断言用；不发起网络请求。
func marshalBody(t *testing.T, hist []agent.Message, model string) string {
	t.Helper()
	b, err := json.Marshal(openai.ChatCompletionNewParams{
		Model:    model,
		Messages: toParams(hist, model),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// 带附件分片的 user 消息：content 变数组，文本分片在前、image_url 随后，
// URL 是 base64 data-URI；Part.Detail 空时 detail 字段整个缺席（OpenAI
// 缺省 auto、MiniMax 缺省 default，省略是交集安全映射）。
func TestToParamsWithParts(t *testing.T) {
	hist := []agent.Message{{Role: agent.RoleUser, Content: "看图",
		Parts: []agent.Part{
			{Kind: agent.PartImage, MediaType: "image/png", Filename: "a.png", DataB64: "aGVsbG8="},
			{Kind: agent.PartImage, MediaType: "image/jpeg", DataB64: "d29ybGQ=", Detail: "low"},
		}}}
	body := marshalBody(t, hist, "minimax-m3")
	for _, want := range []string{
		`"content":[`, // 数组形态，不再是整串文本
		`{"text":"看图","type":"text"}`,
		`{"image_url":{"url":"data:image/png;base64,aGVsbG8="},"type":"image_url"}`,
		`"url":"data:image/jpeg;base64,d29ybGQ=","detail":"low"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\ngot: %s", want, body)
		}
	}
	if strings.Contains(body, `"detail":"auto"`) {
		t.Errorf("empty Detail must omit the field entirely, got: %s", body)
	}
}

// 纯文本模型收到图片分片：降级为占位文本，请求体里没有任何 image_url。
func TestToParamsTextOnlyDegradation(t *testing.T) {
	hist := []agent.Message{{Role: agent.RoleUser, Content: "",
		Parts: []agent.Part{{Kind: agent.PartImage, MediaType: "image/png", Filename: "a.png", DataB64: "aGVsbG8="}}}}
	for _, model := range []string{"deepseek-chat", "minimax-m2", ""} {
		body := marshalBody(t, hist, model)
		if strings.Contains(body, "image_url") || strings.Contains(body, "aGVsbG8=") {
			t.Errorf("model %q: image payload must not reach the wire, got: %s", model, body)
		}
		if !strings.Contains(body, "[image omitted: model is text-only]") {
			t.Errorf("model %q: degraded marker missing, got: %s", model, body)
		}
	}
}

// 无分片消息保持既有字符串路径；空 Content 的分片消息不产空文本分片。
func TestToParamsWithoutPartsUnchanged(t *testing.T) {
	body := marshalBody(t, []agent.Message{{Role: agent.RoleUser, Content: "hi"}}, "minimax-m3")
	if !strings.Contains(body, `"content":"hi"`) || strings.Contains(body, `"content":[`) {
		t.Errorf("string content path changed, got: %s", body)
	}
	body = marshalBody(t, []agent.Message{{Role: agent.RoleUser,
		Parts: []agent.Part{{Kind: agent.PartImage, MediaType: "image/png", DataB64: "aGVsbG8="}}}}, "minimax-m3")
	if strings.Contains(body, `"text":""`) {
		t.Errorf("empty Content must not produce an empty text part, got: %s", body)
	}
}

// 历史图片防重放毒化：只有最后一条带图消息的 Parts 上 wire，更早的降级为
// 占位文本——否则持久化 base64 每轮随行，大图把调用拖到 requestTimeout。
// 回归现场：真机图片轮后连纯文本轮也 120s context deadline exceeded。
func TestToParamsOnlyLatestImageKept(t *testing.T) {
	hist := []agent.Message{
		{Role: agent.RoleUser, Content: "旧图",
			Parts: []agent.Part{{Kind: agent.PartImage, MediaType: "image/png", DataB64: "T0xESU1BR0U="}}},
		{Role: agent.RoleAssistant, Content: "看到了"},
		{Role: agent.RoleUser, Content: "新图",
			Parts: []agent.Part{{Kind: agent.PartImage, MediaType: "image/jpeg", DataB64: "TkVXSU1BR0U="}}},
	}
	body := marshalBody(t, hist, "minimax-m3")
	if strings.Contains(body, "T0xESU1BR0U=") {
		t.Errorf("stale image payload leaked into wire:\n%s", body)
	}
	if !strings.Contains(body, "[image omitted: replayed history]") {
		t.Errorf("stale image must degrade to placeholder text:\n%s", body)
	}
	if !strings.Contains(body, "data:image/jpeg;base64,TkVXSU1BR0U=") {
		t.Errorf("latest image must keep real parts:\n%s", body)
	}
}
