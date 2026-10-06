package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"runeharness/internal/scope"
	"runeharness/internal/tools"
)

// ToolName 是模型侧记忆工具名。
const ToolName = "memory"

// memTool 是 memory 工具的 tools.Tool 适配：参数里永远没有路径/租户，
// 读写目标全部从 ctx scope 与画像推导。
type memTool struct{ m *Memory }

func (t memTool) Spec() tools.Spec {
	return tools.Spec{
		Name: ToolName,
		Description: "Read and write long-term memory. Memory persists across " +
			"sessions; use list/read/about/history to inspect, save to store, " +
			"forget to delete. Never store secrets, tokens or credentials.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"op": map[string]any{
					"type": "string",
					"enum": []string{"save", "forget", "list", "read", "about", "history"},
				},
				"name":        map[string]any{"type": "string", "description": "memory slug, e.g. user-prefers-brief"},
				"type":        map[string]any{"type": "string", "description": "memory type per the profile dictionary"},
				"description": map[string]any{"type": "string", "description": "one-line index entry; no sensitive values"},
				"body":        map[string]any{"type": "string", "description": "markdown body"},
				"entities":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"related":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"reason":      map[string]any{"type": "string", "description": "why the change is made"},
				"entity":      map[string]any{"type": "string", "description": "for op=about: entity to aggregate"},
			},
			"required": []string{"op"},
		},
	}
}

type toolArgs struct {
	Op       string   `json:"op"`
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Descr    string   `json:"description"`
	Body     string   `json:"body"`
	Entities []string `json:"entities"`
	Related  []string `json:"related"`
	Reason   string   `json:"reason"`
	Entity   string   `json:"entity"`
}

func (t memTool) Run(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	var in toolArgs
	if err := json.Unmarshal(rawArgs, &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	switch in.Op {
	case "save":
		return t.save(ctx, in)
	case "forget":
		return t.forget(ctx, in)
	case "list":
		return t.m.IndexText(ctx)
	case "read":
		return t.read(ctx, in)
	case "about":
		return t.about(ctx, in)
	case "history":
		return t.history(ctx, in)
	default:
		return "", fmt.Errorf("unknown op %q (want save/forget/list/read/about/history)", in.Op)
	}
}

func (t memTool) save(ctx context.Context, in toolArgs) (string, error) {
	name := CleanName(in.Name)
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if !t.m.prof.ValidType(in.Type) {
		return "", fmt.Errorf("unknown type %q (want one of: %s)",
			in.Type, strings.Join(t.m.prof.Order, ", "))
	}
	if in.Descr == "" || in.Body == "" {
		return "", fmt.Errorf("description and body are required")
	}
	op := Op{
		Kind: "upsert", Name: name, Type: in.Type, Descr: in.Descr,
		Body: in.Body, Entities: in.Entities, Related: in.Related,
		Reason: in.Reason,
	}
	if ok, why := capContent(&op); !ok {
		return "", fmt.Errorf("refused: %s; keep memory bodies under 8KB", why)
	}
	// 代码层密钥兜底：提示词"不存凭据"是软约束，这道扫描是硬约束。
	if LooksLikeSecret(op.Descr) || LooksLikeSecret(op.Body) {
		return "", fmt.Errorf("refused: content looks like a credential; do not store secrets in memory")
	}
	sc, _ := scope.FromContext(ctx)
	err := t.m.store.ApplyOps(ctx, []Op{op}, sc.SessionID)
	if err != nil {
		return "", err
	}
	t.m.markDirty(sc)
	return "saved: " + name, nil
}

func (t memTool) forget(ctx context.Context, in toolArgs) (string, error) {
	name := CleanName(in.Name)
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	sc, _ := scope.FromContext(ctx)
	if _, err := t.m.store.Get(ctx, name); err != nil {
		return "no such memory: " + name, nil
	}
	err := t.m.store.ApplyOps(ctx, []Op{{
		Kind: "delete", Name: name, Reason: in.Reason,
	}}, sc.SessionID)
	if err != nil {
		return "", err
	}
	t.m.markDirty(sc)
	return "forgotten: " + name, nil
}

func (t memTool) read(ctx context.Context, in toolArgs) (string, error) {
	r, err := t.m.store.Get(ctx, CleanName(in.Name))
	if err != nil {
		return "no such memory: " + CleanName(in.Name), nil
	}
	return fmt.Sprintf("[%s] %s — %s\n\n%s", r.Type, r.Name, r.Descr, r.Body), nil
}

func (t memTool) about(ctx context.Context, in toolArgs) (string, error) {
	entity := in.Entity
	if entity == "" {
		entity = in.Name // 兼容把实体写在 name 里的调用形态
	}
	rows, err := t.m.store.About(ctx, entity)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "no memories about " + entity, nil
	}
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "- [%s] %s — %s\n  %s\n", r.Type, r.Name, r.Descr,
			strings.ReplaceAll(clip(r.Body, 400), "\n", "\n  "))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func (t memTool) history(ctx context.Context, in toolArgs) (string, error) {
	name := CleanName(in.Name)
	changes, err := t.m.store.History(ctx, name)
	if err != nil {
		return "", err
	}
	if len(changes) == 0 {
		return "no history for " + name, nil
	}
	var b strings.Builder
	for _, c := range changes {
		fmt.Fprintf(&b, "- %s %s (%s) %s\n", c.At.Format("2006-01-02 15:04"),
			c.Op, c.Sess, c.Reason)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
