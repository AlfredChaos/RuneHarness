package llm

import (
	"errors"
	"fmt"
	"testing"

	"github.com/openai/openai-go/v3"

	"runeharness/internal/agent"
)

// 上下文超长错误包装为 agent.ErrContextLength，且保留原错误；其余错误原样返回。
func TestClassifyContextLength(t *testing.T) {
	cases := []struct {
		err  *openai.Error
		want bool
	}{
		{&openai.Error{StatusCode: 400, Code: "context_length_exceeded"}, true},
		{&openai.Error{StatusCode: 400, Message: "This model's maximum context length is 128000 tokens"}, true},
		{&openai.Error{StatusCode: 400, Type: "invalid_request_error", Message: "prompt is too long"}, true},
		{&openai.Error{StatusCode: 413, Message: "payload too large"}, true},
		{&openai.Error{StatusCode: 400, Message: "invalid tool schema"}, false},
		{&openai.Error{StatusCode: 429, Message: "too many tokens per minute"}, false},
	}
	for _, c := range cases {
		wrapped := fmt.Errorf("stream: %w", c.err)
		got := classify(wrapped)
		if errors.Is(got, agent.ErrContextLength) != c.want {
			t.Errorf("%+v: context-length = %v, want %v", c.err, !c.want, c.want)
		}
		var apiErr *openai.Error
		if !errors.As(got, &apiErr) {
			t.Errorf("%+v: original error lost", c.err)
		}
	}
	if plain := errors.New("connection reset"); classify(plain) != plain {
		t.Error("non-API errors must pass through unchanged")
	}
}

// 端点报文里的窗口数字被解析进 ContextLengthError.Limit，供压缩器自愈。
func TestClassifyParsesLimit(t *testing.T) {
	msg := "This model's maximum context length is 65536 tokens. " +
		"However, your messages resulted in 70000 tokens."
	err := classify(&openai.Error{StatusCode: 400, Message: msg})
	var cle *agent.ContextLengthError
	if !errors.As(err, &cle) || cle.Limit != 65_536 {
		t.Fatalf("limit = %+v, want 65536", cle)
	}
	if !errors.Is(err, agent.ErrContextLength) {
		t.Fatal("must still match ErrContextLength")
	}
	// 报文里没有数字时 Limit 为 0，不至于把窗口钳成垃圾值
	err = classify(&openai.Error{StatusCode: 400, Message: "prompt is too long"})
	if cle := new(agent.ContextLengthError); !errors.As(err, &cle) || cle.Limit != 0 {
		t.Fatalf("limit = %+v, want 0", cle)
	}
}
