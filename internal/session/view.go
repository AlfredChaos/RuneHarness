package session

import (
	"encoding/json"
	"fmt"

	"runeharness/internal/agent"
)

// BoundaryMeta 是 compact_boundary 控制行的内容：一次摘要压缩后视图的组装规则。
// 视图 = system + HeadIDs 对应行 + 本次摘要 + id > CoversTo 的消息行（plan §4.5）。
type BoundaryMeta struct {
	HeadIDs    []int64 `json:"head_ids"`
	CoversTo   int64   `json:"covers_to_id"`
	SummaryID  int64   `json:"summary_id"` // 本次摘要行的 id；摘要先于 boundary 落库，崩在中间只留孤儿行
	Reason     string  `json:"reason"`
	PreTokens  int     `json:"pre_tokens"`
	PostTokens int     `json:"post_tokens"`
}

// ViewClearMeta 是 view_clear 控制行的内容：一批卸载决策，按行 id 给出
// 替换后的发送形态（plan §4.3）。替换文本直接落库，读侧只做替换，不重算。
type ViewClearMeta struct {
	Results map[int64]string           `json:"results,omitempty"` // tool 行 id → 替换后的 content
	Args    map[int64][]agent.ToolCall `json:"args,omitempty"`    // assistant 行 id → 替换后的 tool_calls
}

// ForkMeta 是 fork 控制行的内容：子会话从父会话哪个水印处继承（plan §4.9）。
type ForkMeta struct {
	ParentSessionID string `json:"parent_session_id"`
	ParentUptoMsgID int64  `json:"parent_upto_msg_id"`
}

// ControlRow 把控制行 meta 编码为一条待追加的消息。
func ControlRow(kind agent.Kind, meta any) (agent.Message, error) {
	b, err := json.Marshal(meta)
	if err != nil {
		return agent.Message{}, fmt.Errorf("marshal %s meta: %w", kind, err)
	}
	return agent.Message{Kind: kind, Content: string(b)}, nil
}

// Fold 把按 id 升序的原始行组装成发送形态：取最后一条 boundary 定首部、
// 摘要与尾部，再套用全部 view_clear 替换。控制行不进视图。
// 用量锚点只对最后一条控制行之后的 assistant 行有效——之前的请求看到的
// 是被改写前的视图，其 usage 与当前视图不对应。
func Fold(rows []agent.Message) ([]agent.Message, error) {
	var bnd *BoundaryMeta
	bIdx := -1
	results := map[int64]string{}
	args := map[int64][]agent.ToolCall{}
	var lastCtrl int64
	for i, r := range rows {
		if !r.Kind.IsControl() {
			continue
		}
		lastCtrl = max(lastCtrl, r.ID)
		switch r.Kind {
		case agent.KindBoundary:
			var m BoundaryMeta
			if err := json.Unmarshal([]byte(r.Content), &m); err != nil {
				return nil, fmt.Errorf("row %d: bad boundary meta: %w", r.ID, err)
			}
			bnd, bIdx = &m, i
		case agent.KindViewClear:
			var m ViewClearMeta
			if err := json.Unmarshal([]byte(r.Content), &m); err != nil {
				return nil, fmt.Errorf("row %d: bad view_clear meta: %w", r.ID, err)
			}
			for id, c := range m.Results {
				results[id] = c
			}
			for id, tc := range m.Args {
				args[id] = tc
			}
		}
	}

	var view []agent.Message
	if bnd == nil {
		for _, r := range rows {
			if !r.Kind.IsControl() {
				view = append(view, r)
			}
		}
	} else {
		head := make(map[int64]bool, len(bnd.HeadIDs))
		for _, id := range bnd.HeadIDs {
			head[id] = true
		}
		for _, r := range rows {
			if r.Role == agent.RoleSystem && !r.Kind.IsControl() {
				view = append(view, r)
			}
		}
		for _, r := range rows {
			if head[r.ID] {
				view = append(view, r)
			}
		}
		// 摘要在 boundary 之前落库（崩在中间只留孤儿行，不丢中段）；
		// 按 SummaryID 精确定位，旧行缺省时退回"boundary 之后的第一条摘要"。
		for _, r := range rows {
			if r.ID == bnd.SummaryID {
				view = append(view, r)
				break
			}
		}
		if bnd.SummaryID == 0 {
			for _, r := range rows[bIdx+1:] {
				if r.Kind == agent.KindSummary {
					view = append(view, r)
					break
				}
			}
		}
		for _, r := range rows {
			if r.Kind.IsControl() || r.Kind == agent.KindSummary || r.Role == agent.RoleSystem ||
				head[r.ID] || r.ID <= bnd.CoversTo {
				continue
			}
			view = append(view, r)
		}
	}
	for i := range view {
		if c, ok := results[view[i].ID]; ok {
			view[i].Content = c
		}
		if tc, ok := args[view[i].ID]; ok {
			view[i].ToolCalls = tc
		}
		if view[i].ID < lastCtrl {
			view[i].Usage = nil
		}
	}
	return repairDanglingCalls(view), nil
}

// repairDanglingCalls 保证视图满足"每个 tool_call 都有结果"的协议形态。
// 全配对时原样返回（不改序、不改哈希）；有缺位才按调用序重排并补占位——
// 进程崩溃（kill -9、pod 驱逐）会把 assistant 的 tool_calls 留在库里而
// 结果行没写到，下一轮请求把悬空调用发给 OpenAI 会被协议拒掉。
// 运行期中断由 loop 补占位（agent.go），崩溃路径只能在这里兜底：
// 读侧合成，零写入，老会话自动覆盖。
func repairDanglingCalls(view []agent.Message) []agent.Message {
	byCall := map[string]agent.Message{}
	missing := map[string]bool{}
	for _, m := range view {
		if m.Role == agent.RoleTool {
			byCall[m.ToolCallID] = m
		}
	}
	for _, m := range view {
		if m.Role != agent.RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			if _, ok := byCall[tc.ID]; !ok {
				missing[tc.ID] = true
			}
		}
	}
	if len(missing) == 0 {
		return view // 全配对：零改动，哈希与内存视图一致
	}
	var out []agent.Message
	claimed := map[string]bool{}
	for _, m := range view {
		// tool 结果行不单独输出——被所属 assistant 组按序重排；
		// 未被认领的孤儿行（坏数据）保持原位，不丢。
		if m.Role == agent.RoleTool && claimed[m.ToolCallID] {
			continue
		}
		out = append(out, m)
		if m.Role != agent.RoleAssistant || len(m.ToolCalls) == 0 {
			continue
		}
		for _, tc := range m.ToolCalls {
			claimed[tc.ID] = true
			if r, ok := byCall[tc.ID]; ok {
				out = append(out, r)
				continue
			}
			out = append(out, agent.Message{
				Role:       agent.RoleTool,
				ToolCallID: tc.ID,
				Content:    "error: execution interrupted",
				IsError:    true,
			})
		}
	}
	return out
}
