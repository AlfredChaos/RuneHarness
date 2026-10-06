package compact

import "runeharness/internal/agent"

// 首尾保护（plan §3.2）：卸载与摘要只处理中段。
const (
	headMessages  = 3      // 首部：前 3 条真实用户消息
	headBudget    = 8_000  // 首部合计上限（第 1 条总是钉住）
	tailTurns     = 40     // 尾部：最近 40 个用户轮
	tailBudget    = 40_000 // 尾部合计上限
	summaryBudget = 8_000  // SUMMARY 段上限，也用于切分时的预算检查
)

// segments 是一次切分的结果，元素是 view 的下标，按 view 顺序。
type segments struct {
	system []int
	head   []int
	middle []int // 含旧摘要
	tail   []int
}

// realUser 报告 m 是否为真实用户消息（不含 hook 注入与摘要）。
func realUser(m agent.Message) bool {
	return m.Role == agent.RoleUser && m.Kind == agent.KindMessage
}

// cutPoint 报告 view[i] 之前是否为合法切点：user 消息之前、assistant 消息之前
// （调用组整组落在切点之后）。tool 消息之前永远不切（约束 3）。
func cutPoint(m agent.Message) bool {
	return m.Role == agent.RoleUser || m.Role == agent.RoleAssistant
}

// segment 把视图切成 system / 首部 / 中段 / 尾部。floor 为 true 时尾部直接取
// 底线（reactive 的激进压缩）。首部每次按同一规则从视图重算：压缩后首部仍是
// 视图里最早的真实用户消息，所以结果稳定。
func (c *Compactor) segment(view []agent.Message, floor bool) segments {
	var s segments
	pinned := make(map[int]bool)
	i := 0
	for ; i < len(view) && view[i].Role == agent.RoleSystem; i++ {
		s.system = append(s.system, i)
		pinned[i] = true
	}

	headTok := 0
	for j := i; j < len(view) && len(s.head) < headMessages; j++ {
		if !realUser(view[j]) {
			continue
		}
		tok := Rough(view[j : j+1])
		if len(s.head) > 0 && headTok+tok > headBudget {
			break
		}
		s.head, headTok = append(s.head, j), headTok+tok
		pinned[j] = true
	}
	// 子代理只有一个用户轮：最后一条真实用户消息就是任务消息，总是钉住，
	// 否则尾部按调用组收缩时它会被切进中段、再被摘要（plan §4.9）。
	if c.cfg.Subagent {
		for j := len(view) - 1; j >= i; j-- {
			if realUser(view[j]) {
				if !pinned[j] {
					s.head = append(s.head, j)
					pinned[j] = true
					headTok += Rough(view[j : j+1])
				}
				break
			}
		}
	}

	// 尾部预算：压缩后用量应低于提醒线，否则刚压完就会再次提醒。
	budget := min(tailBudget, c.cfg.ReminderLine()-c.cfg.Overhead-
		Rough(pick(view, s.system))-headTok-summaryBudget)

	// 底线：最后一个合法切点（当前用户轮的最后一个完整调用组）。
	// acc 是 view[start:] 中未钉住消息的 token 合计，往前扩展时累加。
	start, acc := len(view), 0
	for j := len(view) - 1; j >= i; j-- {
		if !pinned[j] {
			acc += Rough(view[j : j+1])
		}
		if !pinned[j] && cutPoint(view[j]) {
			start = j
			break
		}
	}
	if !floor {
		// 从底线往前扩展：当前轮内可按调用组扩展；更早的轮只能整轮加入
		// （起点必须是真实用户消息），最多 40 轮。放不下即停——越往前所需越多。
		lastTurn := -1
		for j := len(view) - 1; j >= i; j-- {
			if realUser(view[j]) {
				lastTurn = j
				break
			}
		}
		turns := 0
		for j := start - 1; j >= i; j-- {
			m := view[j]
			if m.Kind == agent.KindSummary {
				break // 旧摘要之前只有 system 与首部
			}
			if pinned[j] {
				continue
			}
			acc += Rough(view[j : j+1])
			if !cutPoint(m) || (j < lastTurn && !realUser(m)) {
				continue
			}
			if acc > budget {
				break
			}
			if realUser(m) {
				if turns++; turns > tailTurns {
					break
				}
			}
			start = j
		}
	}

	for j := i; j < len(view); j++ {
		switch {
		case pinned[j]:
		case j >= start:
			s.tail = append(s.tail, j)
		default:
			s.middle = append(s.middle, j)
		}
	}
	return s
}

// pick 按下标取出消息。
func pick(view []agent.Message, idx []int) []agent.Message {
	out := make([]agent.Message, len(idx))
	for k, j := range idx {
		out[k] = view[j]
	}
	return out
}
