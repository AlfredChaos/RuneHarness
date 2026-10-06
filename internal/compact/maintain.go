package compact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"runeharness/internal/agent"
	"runeharness/internal/scope"
)

// maxFailures 是熔断阈值：摘要压缩连续失败 3 次后停手（plan §4.8，对齐 CC）。
const maxFailures = 3

// ErrCircuitOpen 表示熔断器已断开：自动通道与模型 compact 工具不再调 API，
// 用户 /compact 仍可作为人工试探执行。
var ErrCircuitOpen = fmt.Errorf("compaction unavailable: circuit open after %d consecutive failures", maxFailures)

// state 是 session_state 里压缩相关的小状态（非投影状态，可覆盖写）。
type state struct {
	Failures    int `json:"compact_failures"`
	WindowLimit int `json:"window_limit,omitempty"` // 端点报出的真实窗口（被动自愈钳制）
}

// Maintain 在每次 Chat 前调用（plan §4.4）：用量 ≥ 强制压缩线时先卸载中段，
// 卸载后仍过线再做摘要。摘要失败不中止本轮，history 原样继续、计入熔断；
// 只有用量已过阻断线且压不下来时返回 agent.ErrContextFull 拒发。
func (c *Compactor) Maintain(ctx context.Context, view []agent.Message) ([]agent.Message, error) {
	est := Estimate(view, c.cfg.Overhead)
	if est < c.cfg.CompactLine() {
		return view, nil
	}
	if !c.cfg.Auto {
		return view, c.blockIfFull(est)
	}
	next, saved, err := c.offload(ctx, view, c.segment(view, false))
	if err != nil {
		return view, err
	}
	if saved > 0 {
		view, est = withAnchorShift(next, saved), est-saved
		slog.Info("context offloaded", "session", sessionOf(ctx), "saved_tokens", saved, "tokens", est)
	}
	if est < c.cfg.CompactLine() {
		return view, nil
	}
	st, err := c.loadState(ctx)
	if err != nil {
		return view, err
	}
	if st.Failures >= maxFailures {
		return view, c.blockIfFull(est)
	}
	if next, err = c.runSummary(ctx, view, st, "auto", ""); err != nil {
		if ctx.Err() != nil {
			return view, ctx.Err()
		}
		return view, c.blockIfFull(est)
	}
	return next, nil
}

// Prime 在子代理启动前对继承快照做无损卸载（plan §4.9）：用量 ≥ 提醒线时
// 卸载中段，低于提醒线不动。不做摘要——子代理一启动就花一次摘要费用不值。
func (c *Compactor) Prime(ctx context.Context, view []agent.Message) ([]agent.Message, error) {
	if Estimate(view, c.cfg.Overhead) < c.cfg.ReminderLine() {
		return view, nil
	}
	next, saved, err := c.offload(ctx, view, c.segment(view, false))
	if err != nil {
		return view, err
	}
	if saved > 0 {
		slog.Info("subagent start offloaded", "session", sessionOf(ctx), "saved_tokens", saved)
		return withAnchorShift(next, saved), nil
	}
	return view, nil
}

// Compact 立即执行压缩（模型 compact 工具 / 用户 /compact / reactive）：先卸载
// 中段，再摘要。reactive 的尾部直接取底线（更激进）。返回的 history 总是当前
// 有效视图——摘要失败时可能已完成卸载，调用方应采用它。熔断断开时只有
// manual 继续执行，作为人工试探。
func (c *Compactor) Compact(ctx context.Context, view []agent.Message, reason string) ([]agent.Message, error) {
	return c.compact(ctx, view, reason, "")
}

// CompactWith 同 Compact，extra 是用户 /compact 带的附加指令，追加在摘要提示之后。
func (c *Compactor) CompactWith(ctx context.Context, view []agent.Message, extra string) ([]agent.Message, error) {
	return c.compact(ctx, view, "manual", extra)
}

func (c *Compactor) compact(ctx context.Context, view []agent.Message, reason, extra string) ([]agent.Message, error) {
	st, err := c.loadState(ctx)
	if err != nil {
		return view, err
	}
	if st.Failures >= maxFailures && reason != "manual" {
		return view, ErrCircuitOpen
	}
	floor := reason == "reactive"
	next, saved, err := c.offload(ctx, view, c.segment(view, floor))
	if err != nil {
		return view, err
	}
	if saved > 0 {
		view = withAnchorShift(next, saved)
	}
	return c.runSummary(ctx, view, st, reason, extra)
}

// runSummary 执行一次摘要并维护熔断计数：失败 +1，成功清零。失败时返回原视图。
func (c *Compactor) runSummary(ctx context.Context, view []agent.Message, st state, reason, extra string) ([]agent.Message, error) {
	floor := reason == "reactive"
	next, err := c.summarize(ctx, view, c.segment(view, floor), reason, extra)
	if errors.Is(err, errNothingToShrink) && !floor {
		// 中段为空：先把尾部收缩到底线再试一次（plan §4.5 第 0 步）。
		next, err = c.summarize(ctx, view, c.segment(view, true), reason, extra)
	}
	if err != nil {
		if ctx.Err() != nil {
			return view, err // 中断不算压缩失败
		}
		st.Failures++
		slog.Warn("compaction failed", "session", sessionOf(ctx), "reason", reason,
			"class", failureClass(err), "failures", st.Failures, "tokens", Estimate(view, c.cfg.Overhead), "err", err)
		if serr := c.saveState(ctx, st); serr != nil {
			return view, errors.Join(err, serr)
		}
		return view, err
	}
	slog.Info("context compacted", "session", sessionOf(ctx), "reason", reason,
		"pre_tokens", Estimate(view, c.cfg.Overhead), "post_tokens", Estimate(next, c.cfg.Overhead))
	if st.Failures != 0 {
		st.Failures = 0
		if err := c.saveState(ctx, st); err != nil {
			return next, err
		}
	}
	return next, nil
}

// NoteLimit 把端点报出的真实窗口钳进有效配置（被动自愈）：内存与
// session_state 同步更新，resume 后仍然生效。只往下钳——报文解析可能
// 抓错数字，不拿它放大窗口。
func (c *Compactor) NoteLimit(ctx context.Context, limit int) {
	if limit <= 0 {
		return
	}
	st, err := c.loadState(ctx)
	if err != nil {
		slog.Warn("cannot persist context limit", "session", sessionOf(ctx), "err", err)
		return
	}
	if st.WindowLimit == 0 || limit < st.WindowLimit {
		st.WindowLimit = limit
		if err := c.saveState(ctx, st); err != nil {
			slog.Warn("cannot persist context limit", "session", sessionOf(ctx), "err", err)
		}
	}
	if limit < c.cfg.Window {
		slog.Info("context window clamped", "session", sessionOf(ctx),
			"from", c.cfg.Window, "to", limit)
		c.cfg.Window = limit
	}
	if c.cfg.Window < MinWindow {
		slog.Warn("endpoint window below compaction minimum",
			"session", sessionOf(ctx), "window", c.cfg.Window, "min", MinWindow)
	}
}

// blockIfFull 在用量超过阻断线时返回 agent.ErrContextFull。
func (c *Compactor) blockIfFull(est int) error {
	if est >= c.cfg.BlockLine() {
		return agent.ErrContextFull
	}
	return nil
}

func (c *Compactor) loadState(ctx context.Context) (state, error) {
	var st state
	raw, err := c.store.GetState(ctx)
	if err != nil {
		return st, fmt.Errorf("load compaction state: %w", err)
	}
	if len(raw) == 0 {
		return st, nil
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, fmt.Errorf("decode compaction state: %w", err)
	}
	// 持久化的窗口钳制在 resume 后继续生效。
	if st.WindowLimit > 0 && st.WindowLimit < c.cfg.Window {
		c.cfg.Window = st.WindowLimit
	}
	return st, nil
}

func (c *Compactor) saveState(ctx context.Context, st state) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := c.store.PutState(ctx, raw); err != nil {
		return fmt.Errorf("save compaction state: %w", err)
	}
	return nil
}

// withAnchorShift 卸载后修正用量锚点：最近一条 usage 减去省下的 token，
// 避免旧锚点让下一次估算仍然过线。只改内存里的副本，不影响落库的 usage。
func withAnchorShift(view []agent.Message, saved int) []agent.Message {
	view = slices.Clone(view)
	for i := len(view) - 1; i >= 0; i-- {
		if u := view[i].Usage; u != nil {
			shifted := *u
			shifted.Prompt = max(0, shifted.Prompt-saved)
			view[i].Usage = &shifted
			break
		}
	}
	return view
}

func failureClass(err error) string {
	for _, e := range []error{errSummaryCall, errUnparsable, errIneffective} {
		if errors.Is(err, e) {
			return e.Error()
		}
	}
	return "storage_error"
}

func sessionOf(ctx context.Context) string {
	sc, _ := scope.FromContext(ctx)
	return sc.SessionID
}
