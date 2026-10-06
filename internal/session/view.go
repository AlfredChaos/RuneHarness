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
	return view, nil
}
