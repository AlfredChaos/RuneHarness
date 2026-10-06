// Package subagent 提供 task 工具：把子任务委派给一个独立的 agent loop，
// 只回传最终结论，中间过程不进入主对话历史（上下文隔离，但文件系统
// 副作用保留在共享的工作目录上）。
package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"runeharness/internal/agent"
	"runeharness/internal/compact"
	"runeharness/internal/scope"
	"runeharness/internal/session"
	"runeharness/internal/tools"
)

// ToolName 是注册到 Registry 的工具名；权限名单等按此引用。
const ToolName = "task"

// Type 是子代理类型。
type Type string

const (
	// TypeNormal 继承主代理的系统提示词与对话上下文：以"委派时刻的历史
	// 快照"为起点，任务描述作为新的 user 消息追加。
	TypeNormal Type = "normal"
	// TypeGeneralPurpose 以全新上下文执行：使用通用 system prompt，
	// 默认继承全部基础工具。
	TypeGeneralPurpose Type = "general_purpose"
)

// DefaultPrompt 是 general_purpose 子代理的默认 system prompt。
const DefaultPrompt = "You are a subagent handling a delegated subtask. " +
	"Complete it directly with your tools, then reply with a concise conclusion. " +
	"Do not ask the user questions; the caller only receives your final message. " +
	"Make the conclusion self-contained: the caller cannot open blob:// references " +
	"from your session."

// maxSteps 是子代理的安全轮数上限，独立于主代理的步数预算。
const maxSteps = 40

// DefaultTimeout 是子代理整轮运行的兜底时限。maxSteps 只限轮数，
// 单步内 LLM/工具耗时（各自 120s/30s 超时）可叠加到不可接受的程度。
// 超时取消时正在执行的工具随 ctx 中断——文件系统副作用可能已部分
// 落盘，回传给主代理的错误只作说明，不保证回滚。
const DefaultTimeout = 10 * time.Minute

// EventKind 标记子代理生命周期事件。
type EventKind int

const (
	EventSpawn    EventKind = iota // 子代理启动
	EventToolCall                  // 子代理内部的一次工具调用
	EventDone                      // 子代理结束（Err 非 nil 表示失败）
)

// Event 是子代理生命周期事件，供 UI 展示。Depth 从 1 起（主代理为 0）。
type Event struct {
	Kind   EventKind
	Depth  int
	Type   Type           // Spawn: 子代理类型
	Desc   string         // Spawn: 任务描述
	Call   agent.ToolCall // ToolCall: 调用与结果
	Result string         // ToolCall: 结果；Done: 回传的结论
	Err    error          // Done: 非 nil 表示失败
}

// Tool 实现 tools.Tool：spawn 一个子代理跑独立 agent loop。
// 嵌套由 maxDepth 控制：spawn 出的子代理深度 = t.depth+1，达到 maxDepth
// 的子代理不再持有 task 工具，物理上无法再委派。
type Tool struct {
	llm       agent.LLM
	baseTools []tools.Tool // 子代理的基础工具集（不含 task 自身）
	genPrompt string       // general_purpose 的 system prompt
	maxDepth  int          // 允许 spawn 出的子代理最大深度：1=不嵌套，2=可再开一层
	depth     int          // 本工具所在代理的深度（主代理=0）

	// Timeout 是子代理整轮运行的兜底时限；零值用 DefaultTimeout。
	Timeout time.Duration

	// Wire 装配新 spawn 的子代理（挂权限 hook 等）；可为 nil。
	Wire func(sub *agent.Agent, depth int)
	// OnEvent 上报子代理生命周期事件；可为 nil。
	OnEvent func(Event)
	// Store 为子代理会话持久化提供存储面；不可为 nil（tradeoffs D11）：
	// fork 行、blob、熔断计数都依赖它。
	// spawn 时建一条 kind=subagent 的子会话，parent_id 取当前 ctx 的
	// SessionID——runCtx 派生自父 ctx，租户自动继承，嵌套时 parent
	// 链自动正确（depth-1 的会话即 depth-2 的父）。
	Store SessionStore
	// CompactCfg 是子代理压缩器配置；Window/MaxOutput/Auto 与主代理同源，
	// Subagent 标志与 Overhead（子代理自己的工具清单）由 spawn 填。
	CompactCfg compact.Config
}

// SessionStore 是 task 落库子会话所需的存储面：会话创建 + 消息追加 +
// 压缩器依赖的 blob/状态读写 + 请求留痕。session.Store 结构满足。
type SessionStore interface {
	CreateSession(ctx context.Context, meta session.Meta) (session.Session, error)
	Append(ctx context.Context, msg agent.Message) (int64, error)
	compact.Store
	agent.RequestLogger
}

// New 创建主代理用的 task 工具（depth=0）。
// baseTools 是子代理继承的工具集；nest 为 true 时子代理可再 spawn 一层。
func New(llm agent.LLM, baseTools []tools.Tool, genPrompt string, nest bool) *Tool {
	maxDepth := 1
	if nest {
		maxDepth = 2
	}
	return &Tool{llm: llm, baseTools: baseTools, genPrompt: genPrompt, maxDepth: maxDepth}
}

func (t *Tool) Spec() tools.Spec {
	return tools.Spec{
		Name: ToolName,
		Description: "Launch a subagent to handle a self-contained subtask with its own agent loop. " +
			"Only the subagent's final conclusion is returned here; intermediate steps stay out of this conversation. " +
			"subagent_type 'general_purpose' (default) runs fully isolated: it CANNOT see this conversation, " +
			"so write 'description' as complete standalone instructions (context, file paths, expected output). " +
			"Use 'normal' instead when the subtask must continue with this conversation's context and system prompt.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"description": map[string]any{
					"type": "string",
					"description": "task instructions for the subagent. For general_purpose this is ALL it sees — " +
						"make it fully self-contained (context, paths, deliverable). " +
						"For normal it is appended after the current conversation history.",
				},
				"subagent_type": map[string]any{
					"type":        "string",
					"description": "subagent type; defaults to 'general_purpose'",
					"enum":        []string{string(TypeNormal), string(TypeGeneralPurpose)},
				},
			},
			"required": []string{"description"},
		},
	}
}

// Run 同步 spawn 子代理并回传其最终结论文本。
func (t *Tool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Description string `json:"description"`
		Type        string `json:"subagent_type"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	if args.Description == "" {
		return "", errors.New("description is required")
	}
	typ := Type(args.Type)
	switch typ {
	case "":
		typ = TypeGeneralPurpose
	case TypeNormal, TypeGeneralPurpose:
	default:
		return "", errors.New("unknown subagent_type: " + args.Type)
	}

	if t.Store == nil {
		return "", errors.New("task: session store is required")
	}
	sub := agent.New(t.llm, tools.NewRegistry(t.subTools()...), maxSteps)
	depth := t.depth + 1
	// 压缩器先于 Wire 注册：收尾提醒要排在其它 hook 之前抢到注入位；
	// 子代理不注册 compact 工具——它的目标是交结论，不是择机压缩。
	cmpCfg := t.CompactCfg
	cmpCfg.Subagent = true
	subSpecs, _ := json.Marshal(tools.NewRegistry(t.subTools()...).Specs())
	cmpCfg.Overhead = compact.TextTokens(string(subSpecs))
	cmp, err := compact.New(t.llm, t.Store, cmpCfg)
	if err != nil {
		return "", err
	}
	sub.Compactor = cmp
	sub.Hooks.OnPreChat(cmp.ReminderHook())
	if t.Wire != nil {
		t.Wire(sub, depth)
	}
	sub.OnToolCall = func(tc agent.ToolCall) {
		t.emit(Event{Kind: EventToolCall, Depth: depth, Call: tc})
	}

	hist := t.initialHistory(ctx, typ, args.Description)
	sc, err := scope.FromContext(ctx)
	if err != nil {
		return "", fmt.Errorf("task: %w（ctx 缺 scope，属装配 bug）", err)
	}
	subSess, err := t.Store.CreateSession(ctx, session.Meta{
		ParentID:  sc.SessionID,
		Kind:      session.KindSubagent,
		Depth:     depth,
		Title:     titleOf(args.Description),
		Workspace: sc.Workspace,
	})
	if err != nil {
		return "", err
	}
	subCtx := scope.WithSession(ctx, subSess.ID)
	// 初始历史不经过 loop 的 append 点，显式落库：
	// general_purpose 落 system+task；normal 不复制父会话快照
	// （嵌套 O(n²) 膨胀），只落 fork 控制行（记父会话与水印）+task，
	// 子会话视图 = 父会话在水印处的折叠视图 + 子会话行（plan §4.9）。
	if typ == TypeNormal {
		row, err := session.ControlRow(agent.KindFork, session.ForkMeta{
			ParentSessionID: sc.SessionID,
			ParentUptoMsgID: maxID(hist),
		})
		if err != nil {
			return "", err
		}
		if _, err := t.Store.Append(subCtx, row); err != nil {
			return "", err
		}
	} else if _, err := t.Store.Append(subCtx, hist[0]); err != nil {
		return "", err
	}
	if _, err := t.Store.Append(subCtx, hist[len(hist)-1]); err != nil {
		return "", err
	}
	sub.Rec, sub.Requests = t.Store, t.Store
	// 起点卸载：normal 型继承的快照过提醒线时，先卸载中段再跑——
	// 避免子代理第一次请求就触发整轮摘要。
	if hist, err = cmp.Prime(subCtx, hist); err != nil {
		return "", err
	}

	t.emit(Event{Kind: EventSpawn, Depth: depth, Type: typ, Desc: args.Description})
	runCtx2, cancel := context.WithTimeout(subCtx, t.timeout())
	defer cancel()
	hist, err = sub.Run(runCtx2, hist)
	if err != nil {
		// 区分超时与用户中断：超时给主代理一个可理解的说明。
		if errors.Is(runCtx2.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("subagent timed out after %s", t.timeout())
		}
		t.emit(Event{Kind: EventDone, Depth: depth, Type: typ, Err: err})
		return "", err
	}
	result := conclusion(hist)
	if strings.Contains(result, "blob://") {
		// blob:// 按调用方会话解析：父代理打不开子会话的 blob，
		// 结论里的引用对它无效，标注清楚。
		result += "\n\n(blob:// references above live in the subagent's session " +
			"and cannot be opened from this conversation.)"
	}
	t.emit(Event{Kind: EventDone, Depth: depth, Type: typ, Result: result})
	return result, nil
}

// maxID 返回视图里最大的行 id（fork 行的父会话水印）。
func maxID(hist []agent.Message) int64 {
	var m int64
	for _, h := range hist {
		m = max(m, h.ID)
	}
	return m
}

// initialHistory 构造子代理的初始对话历史。
func (t *Tool) initialHistory(ctx context.Context, typ Type, desc string) []agent.Message {
	task := agent.Message{Role: agent.RoleUser, Content: desc}
	if typ == TypeNormal {
		// 继承委派时刻的上下文快照（含系统提示词），任务作为新指令追加。
		return append(slices.Clone(agent.HistoryFromContext(ctx)), task)
	}
	return []agent.Message{
		{Role: agent.RoleSystem, Content: t.genPrompt},
		task,
	}
}

// subTools 组装子代理工具表：基础工具 +（未达嵌套上限时的）task。
func (t *Tool) subTools() []tools.Tool {
	out := make([]tools.Tool, 0, len(t.baseTools)+1)
	out = append(out, t.baseTools...)
	if t.depth+1 < t.maxDepth {
		out = append(out, t.child())
	}
	return out
}

// timeout 返回整轮兜底时限：字段优先，零值回落到 DefaultTimeout。
func (t *Tool) timeout() time.Duration {
	if t.Timeout > 0 {
		return t.Timeout
	}
	return DefaultTimeout
}

// child 是注入下一层子代理的 task 工具，共享全部依赖。
func (t *Tool) child() *Tool {
	c := *t
	c.depth = t.depth + 1
	return &c
}

func (t *Tool) emit(ev Event) {
	if t.OnEvent != nil {
		t.OnEvent(ev)
	}
}

// titleOf 把任务描述截为子会话标题（与 sqlite 的 title 回填口径一致）。
func titleOf(s string) string {
	r := []rune(s)
	if len(r) <= 40 {
		return s
	}
	return string(r[:40]) + "…"
}

// conclusion 取子代理最后一条 assistant 文本作为回传结论；
// 对话上下文整体丢弃，只有这段文本回到主代理。
func conclusion(hist []agent.Message) string {
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].Role == agent.RoleAssistant && hist[i].Content != "" {
			return hist[i].Content
		}
	}
	return "(subagent finished without a text conclusion)"
}
