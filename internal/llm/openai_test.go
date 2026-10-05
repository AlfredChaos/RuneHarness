package llm

import (
	"strings"
	"testing"
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
