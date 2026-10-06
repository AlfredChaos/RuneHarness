package compact

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"runeharness/internal/agent"
	"runeharness/internal/session"
	"runeharness/internal/tools"
)

// 卸载层数值（plan §4.3）。
const (
	offloadMin    = 1_000 // 不足 1K 的结果换成索引也省不了多少
	taskKeep      = 8_000 // ≤8K 的 task 结果不卸载：子任务的浓缩结论（plan §4.9）
	argsThreshold = 2_000 // tool_calls 参数里超过 2K 的字符串字段换成索引
	taskToolName  = "task"
)

const (
	offloadedPrefix = "[tool result offloaded: "
	argsPrefix      = "<offloaded "
)

// offload 卸载中段：工具结果落 blob、原位留索引；过期结果无论大小都卸载并
// 加过期标记；超过 2K 的 tool_calls 参数字段也换成索引。有变化时写一条
// view_clear 控制行，返回新视图与省下的 token 数。
func (c *Compactor) offload(ctx context.Context, view []agent.Message, seg segments) ([]agent.Message, int, error) {
	calls := map[string]agent.ToolCall{}
	for _, m := range view {
		for _, tc := range m.ToolCalls {
			calls[tc.ID] = tc
		}
	}
	// 过期判定：同一键（read_file / list_dir 的路径、run_command 的命令串）
	// 在视图后面再次出现，较早的结果就被取代了。较新的可能在尾部，不影响判定。
	newer := map[int]string{} // 下标 → 取代它的较新调用 id
	lastByKey := map[string]string{}
	for j := len(view) - 1; j >= 0; j-- {
		m := view[j]
		if m.Role != agent.RoleTool {
			continue
		}
		key := staleKey(calls[m.ToolCallID])
		if key == "" {
			continue
		}
		if id, ok := lastByKey[key]; ok {
			newer[j] = id
		} else {
			lastByKey[key] = m.ToolCallID
		}
	}

	out := slices.Clone(view)
	meta := session.ViewClearMeta{Results: map[int64]string{}, Args: map[int64][]agent.ToolCall{}}
	saved := 0
	for _, j := range seg.middle {
		m := out[j]
		if m.ID == 0 {
			continue // 未落库的消息无法被控制行引用
		}
		switch m.Role {
		case agent.RoleTool:
			idx, err := c.offloadResult(ctx, m, calls[m.ToolCallID].Name, newer[j])
			if err != nil {
				return nil, 0, err
			}
			if idx != "" {
				saved += TextTokens(m.Content) - TextTokens(idx)
				out[j].Content = idx
				meta.Results[m.ID] = idx
			}
		case agent.RoleAssistant:
			tcs, changed, err := c.offloadArgs(ctx, m.ToolCalls)
			if err != nil {
				return nil, 0, err
			}
			if changed {
				saved += Rough([]agent.Message{{ToolCalls: m.ToolCalls}}) - Rough([]agent.Message{{ToolCalls: tcs}})
				out[j].ToolCalls = tcs
				meta.Args[m.ID] = tcs
			}
		}
	}
	if len(meta.Results) == 0 && len(meta.Args) == 0 {
		return view, 0, nil
	}
	row, err := session.ControlRow(agent.KindViewClear, meta)
	if err != nil {
		return nil, 0, err
	}
	if _, err := c.store.Append(ctx, row); err != nil {
		return nil, 0, fmt.Errorf("record view_clear: %w", err)
	}
	return out, saved, nil
}

// offloadResult 返回一条工具结果卸载后的索引；不需要卸载时返回 ""。
func (c *Compactor) offloadResult(ctx context.Context, m agent.Message, tool, newerID string) (string, error) {
	switch {
	case strings.HasPrefix(m.Content, offloadedPrefix), tool == agent.CompactToolName:
		return "", nil
	case newerID == "" && len(m.Content) < offloadMin:
		return "", nil
	case newerID == "" && tool == taskToolName && len(m.Content) <= taskKeep:
		return "", nil
	}
	ref := existingRef(m.Content)
	if ref == "" {
		var err error
		if ref, err = newRef("res"); err != nil {
			return "", err
		}
		if err := c.store.AppendBlob(ctx, ref, m.Content); err != nil {
			return "", fmt.Errorf("offload %s result: %w", tool, err)
		}
	}
	if tool == "" {
		tool = "tool"
	}
	idx := fmt.Sprintf("%s%s, %s; read with read_file %s%s", offloadedPrefix, tool, FormatSize(len(m.Content)), tools.BlobScheme, ref)
	if newerID != "" {
		idx += "; superseded by later call " + newerID
	}
	return idx + "]", nil
}

// offloadArgs 把一批 tool_calls 里超过 2K 的顶层字符串参数换成索引；参数原文
// 整段落 blob。字段名与 JSON 结构不变，参数仍是合法 JSON，tool_call id 不变。
func (c *Compactor) offloadArgs(ctx context.Context, calls []agent.ToolCall) ([]agent.ToolCall, bool, error) {
	out := slices.Clone(calls)
	changed := false
	for k, tc := range calls {
		var fields map[string]any
		if json.Unmarshal(tc.Arguments, &fields) != nil {
			continue
		}
		var big []string
		for name, v := range fields {
			if s, ok := v.(string); ok && len(s) > argsThreshold && !strings.HasPrefix(s, argsPrefix) {
				big = append(big, name)
			}
		}
		if len(big) == 0 {
			continue
		}
		ref, err := newRef("args")
		if err != nil {
			return nil, false, err
		}
		if err := c.store.AppendBlob(ctx, ref, string(tc.Arguments)); err != nil {
			return nil, false, fmt.Errorf("offload %s arguments: %w", tc.Name, err)
		}
		for _, name := range big {
			fields[name] = fmt.Sprintf("%s%s: %s%s>", argsPrefix, FormatSize(len(fields[name].(string))), tools.BlobScheme, ref)
		}
		b, err := json.Marshal(fields)
		if err != nil {
			return nil, false, fmt.Errorf("marshal offloaded arguments: %w", err)
		}
		out[k].Arguments = b
		changed = true
	}
	return out, changed, nil
}

// staleKey 返回判定"被较新调用取代"的键；不参与过期判定的工具返回 ""。
func staleKey(tc agent.ToolCall) string {
	var args struct {
		Path    string `json:"path"`
		Command string `json:"command"`
		Offset  int    `json:"offset"`
		Limit   int    `json:"limit"`
	}
	if json.Unmarshal(tc.Arguments, &args) != nil {
		return ""
	}
	switch {
	case tc.Name == "read_file" && args.Path != "":
		// 同一路径、同一范围才算取代：读另一页不让前一页过期。
		return fmt.Sprintf("read_file\x00%s\x00%d\x00%d", args.Path, args.Offset, args.Limit)
	case tc.Name == "list_dir":
		return "list_dir\x00" + args.Path
	case tc.Name == "run_command" && args.Command != "":
		return "run_command\x00" + args.Command
	}
	return ""
}

// existingRef 从 spill 指针里取出已有的 blob ref；不是指针形态时返回 ""。
func existingRef(content string) string {
	if !strings.HasPrefix(content, "<persisted-output ") {
		return ""
	}
	_, rest, ok := strings.Cut(content, `src="`+tools.BlobScheme)
	if !ok {
		return ""
	}
	ref, _, _ := strings.Cut(rest, `"`)
	return ref
}
