// Package breaker 拦截退化型的工具调用模式，分三层：
//   - 失败熔断：同一签名（工具名+参数原文）连续失败达到上限后阻止相同调用；
//   - 空转熔断：同一签名连续返回逐字节相同的结果达到上限后阻止——成功也算，
//     结果不变说明调用没带来新信息，是模型幻觉打转的典型特征；
//   - 连败提醒：同名工具连续执行全部失败达到阈值时经 PreChat 注入
//     软性 nag，只提醒不阻止。只计失败——同名工具连续成功是正常工作
//     （如批量读文件），换着参数反复失败才是打转。
package breaker

import (
	"context"
	"fmt"
	"sync"

	"runeharness/internal/agent"
)

const (
	// DefaultMax 是失败/空转熔断的默认连续次数上限。
	DefaultMax = 3
	// DefaultWarnStreak 是同名工具连续失败的默认提醒阈值。
	DefaultWarnStreak = 5
)

// Breaker 统计退化调用模式并给出干预。签名 = 工具名 + 参数原文，
// 参数逐字节一致才算"同一次重试"；参数有任何变化即开始新序列。
// 所有序列都是"连续"语义：中间插入不同签名/不同结果的调用即中断。
type Breaker struct {
	max        int // 失败与空转共用的熔断阈值
	warnStreak int // 同名连败的提醒阈值

	mu sync.Mutex // 当前各层代理串行执行，锁仅作防御

	// 失败序列：同一签名连续失败计数。
	failKey  string
	failures int

	// 空转序列：同一 (签名, 结果原文) 连续重复计数。
	lastSig    string
	lastResult string
	repeats    int

	// 连败序列：同名工具连续失败计数（换工具或一次成功即中断）；
	// warned 防重复提醒。
	errName   string
	errStreak int
	warned    bool
}

// New 创建熔断器；max/warnStreak <=0 时分别使用 DefaultMax/DefaultWarnStreak。
func New(max, warnStreak int) *Breaker {
	if max <= 0 {
		max = DefaultMax
	}
	if warnStreak <= 0 {
		warnStreak = DefaultWarnStreak
	}
	return &Breaker{max: max, warnStreak: warnStreak}
}

// Observe 实现 agent.PostToolUseFunc：按结果更新各序列计数。
// 被 PreToolUse 阻止的调用不触发 Observe，阻止本身不计入任何序列。
func (b *Breaker) Observe(_ context.Context, in agent.PostToolUseInput) {
	b.mu.Lock()
	defer b.mu.Unlock()
	k := keyOf(in.Call)

	// 连败序列：同名工具的连续失败才计数——同名连续成功是正常工作
	//（批量读文件等），换着参数反复失败才是打转信号。
	if in.Call.Name == b.errName && in.IsError {
		b.errStreak++
	} else if in.IsError {
		b.errName, b.errStreak, b.warned = in.Call.Name, 1, false
	} else {
		b.errName, b.errStreak, b.warned = "", 0, false
	}

	// 空转序列：签名与结果逐字节相同才算重复；结果变化说明外部世界
	// 已变（如改完代码重跑测试），是合法重试而非打转。
	if k == b.lastSig && in.Result == b.lastResult {
		b.repeats++
	} else {
		b.lastSig, b.lastResult, b.repeats = k, in.Result, 1
	}

	// 失败序列：同一签名连续失败。
	if !in.IsError {
		b.failKey, b.failures = "", 0
	} else if k == b.failKey {
		b.failures++
	} else {
		b.failKey, b.failures = k, 1
	}
}

// Check 实现 agent.PreToolUseFunc：命中任一熔断状态即返回阻止原因，
// 该原因作为 tool 结果回填给模型。触发时清零对应计数而非永久锁死——
// 参数相同但外部条件可能已变化（如缺失的文件刚被创建）。
func (b *Breaker) Check(_ context.Context, in agent.ToolUseInput) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	k := keyOf(in.Call)

	// 失败分支在前：失败信息比空转更具体，优先向模型解释。
	if k == b.failKey && b.failures >= b.max {
		n := b.failures
		b.reset()
		return fmt.Sprintf(
			"error: identical call to %q failed %d times in a row — "+
				"stop retrying it; change approach or report the blocker to the user",
			in.Call.Name, n)
	}
	if k == b.lastSig && b.repeats >= b.max {
		n := b.repeats
		b.reset()
		return fmt.Sprintf(
			"error: %q already ran %d times with identical output — it adds no "+
				"new information; move on, or state what you expect to change",
			in.Call.Name, n)
	}
	return ""
}

// Nag 实现 agent.PreChatFunc：同名工具连续失败达到阈值时注入提醒。
// 只统计失败——同名工具连续成功是正常探索，不产生 nag。
// 每条连败序列只提醒一次，只提醒不阻止。
func (b *Breaker) Nag(_ context.Context, _ []agent.Message) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.errStreak >= b.warnStreak && !b.warned {
		b.warned = true
		return fmt.Sprintf(
			"<notice>The last %d calls to %q all failed. Stop guessing — "+
				"inspect the errors, change strategy, or report the blocker.</notice>",
			b.errStreak, b.errName)
	}
	return ""
}

// reset 清空熔断序列（连败计数与签名无关，不随熔断清零）。
func (b *Breaker) reset() {
	b.failKey, b.failures = "", 0
	b.lastSig, b.lastResult, b.repeats = "", "", 0
}

func keyOf(c agent.ToolCall) string {
	return c.Name + "\x00" + string(c.Arguments)
}
