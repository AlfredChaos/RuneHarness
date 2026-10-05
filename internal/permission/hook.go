package permission

import (
	"context"

	"runeharness/internal/agent"
)

// Hook 把 Desk 的三道闸包装成 agent 的 PreToolUse hook：
// Allow 放行；Deny 阻止并把原因回填给模型；Ask 经 Ask 回调等前台答复。
// 不注册此 hook 即全放行（等价于之前 desk == nil 的行为）。
type Hook struct {
	desk *Desk // 必须非 nil

	// Ask 在判定为 Ask 时向前台投递确认请求，立即返回答复通道（容量为 1）。
	// seq/total 是该请求在本批 Ask 中的编号与总数，供前台显示进度；
	// 确认按执行顺序逐个发起。nil 时 Ask 一律按 Deny 处理。
	Ask func(call agent.ToolCall, reason string, seq, total int) <-chan bool
}

func NewHook(desk *Desk, ask func(call agent.ToolCall, reason string, seq, total int) <-chan bool) *Hook {
	return &Hook{desk: desk, Ask: ask}
}

// Check 实现 agent.PreToolUseFunc。
// seq/total 通过对本批做一次纯函数预扫描得出（Desk.Check 无副作用、开销小）；
// Ask 为 nil 时本批不存在真正的确认，无需编号。
func (h *Hook) Check(ctx context.Context, in agent.ToolUseInput) string {
	v, reason := h.desk.Check(in.Call.Name, in.Call.Arguments)
	if v == Ask && h.Ask == nil {
		v, reason = Deny, "no approval channel"
	}
	switch v {
	case Ask:
		seq, total := 1, 0
		for i, c := range in.Batch {
			if bv, _ := h.desk.Check(c.Name, c.Arguments); bv == Ask {
				total++
				if i < in.Index {
					seq++
				}
			}
		}
		select {
		case ok := <-h.Ask(in.Call, reason, seq, total):
			if !ok {
				return "error: user denied this tool call (" + reason + ")"
			}
			return ""
		case <-ctx.Done():
			return "error: " + ctx.Err().Error()
		}
	case Deny:
		return "error: blocked by permission policy: " + reason
	default:
		return ""
	}
}
