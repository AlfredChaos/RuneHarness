// Package compact 实现上下文压缩管线（docs/runeharness-compaction-plan.html）：
// 回填规范化（空结果标记、超限 spill）、阈值判定、早期结果卸载、滚动增量摘要、
// 熔断器、提醒 hook 与 compact 工具。压缩状态全部经 Store 持久化。
package compact

import (
	"unicode/utf8"

	"runeharness/internal/agent"
)

// msgOverhead 是每条消息在 API 侧的固定开销（role、分隔符）粗估。
const msgOverhead = 4

// Estimate 估算 history 发给 API 的 token 数（plan §4.1）：最近一条带 usage
// 的 assistant 消息为锚点——其 prompt+completion 是端点算出的真实值，已含
// 工具清单——之后的消息按字符粗估。没有锚点时全量粗估，加上 overhead（工具
// 清单等不在 history 里的部分），再多留 10% 余量：粗估误差大，宁可早压。
func Estimate(history []agent.Message, overhead int) int {
	for i := len(history) - 1; i >= 0; i-- {
		if u := history[i].Usage; u != nil {
			return u.Prompt + u.Completion + Rough(history[i+1:])
		}
	}
	return (overhead + Rough(history)) * 11 / 10
}

// Rough 按字符粗估一组消息的 token 数，只算会发给 API 的字段：Content 与
// tool_calls 的 name/arguments；不含 Thinking（它不回传）。
func Rough(msgs []agent.Message) int {
	n := 0
	for _, m := range msgs {
		n += msgOverhead + TextTokens(m.Content)
		for _, tc := range m.ToolCalls {
			n += TextTokens(tc.Name) + TextTokens(string(tc.Arguments))
		}
	}
	return n
}

// TextTokens 粗估一段文本的 token 数：ASCII 字符按 4 字符 1 token，
// 非 ASCII 字符（中文等）按 0.7 token 计——len/4 会严重低估中文。
func TextTokens(s string) int {
	ascii, other := 0, 0
	for i := 0; i < len(s); {
		if s[i] < utf8.RuneSelf {
			ascii++
			i++
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		other++
		i += size
	}
	return (ascii+3)/4 + other*7/10
}
