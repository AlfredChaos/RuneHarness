// Package naming 在每轮对话结束后异步生成会话标题：取首轮 user+assistant
// 消息做一次性 LLM completion（tools=nil，非 agent loop），生成 ≤12 字标题
// 写回 sessions.title。任何失败都静默——标题只影响展示，不值得阻塞主流程，
// 下轮 turn_end 自然重试。防重：标题已不是渠道默认形态（channel:chatID）
// 就不再命名，给用户手动改名留位。
package naming

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"runeharness/internal/agent"
	"runeharness/internal/scope"
	"runeharness/internal/tools"
)

// maxTitleRunes 是标题长度上限（issue 口径：≤12 字）。
const maxTitleRunes = 12

// DefaultTimeout 是单次命名请求上限；超时按失败处理，静默放弃。
const DefaultTimeout = 30 * time.Second

// sysPrompt 是命名请求的 system 消息。
const sysPrompt = "你是会话标题生成器。根据下面这轮对话，起一个不超过 12 个字的标题：" +
	"概括主题、具体可辨识；只输出标题文本本身，不要引号、句号、前缀或任何解释。"

// errNoRound / errEmptyTitle 是命名跳过的内部哨兵：会话还没有完整的
// user+assistant 轮次，或模型没吐出可用文本。
var (
	errNoRound    = errors.New("naming: no complete first round yet")
	errEmptyTitle = errors.New("naming: model returned empty title")
)

// LLM 是命名需要的最小模型能力（*llm.Client 满足）：specs=nil 即不带
// 工具的一次性 completion，onPartial=nil 不需要流式回调。
type LLM interface {
	Chat(ctx context.Context, history []agent.Message, specs []tools.Spec, onPartial func(agent.Partial)) (agent.Response, error)
}

// Store 是命名需要的存储面（*sqlite.Store 满足）。
type Store interface {
	// SessionTitle 读当前标题；ErrNotFound 视为无可命名对象。
	SessionTitle(ctx context.Context, sessionID string) (string, error)
	// SetTitle 覆盖写标题。
	SetTitle(ctx context.Context, sessionID, title string) error
	// LoadRawHistory 读全部原始行，供首轮 user+assistant 提取。
	LoadRawHistory(ctx context.Context, sessionID string) ([]agent.Message, error)
}

// Namer 在 turn_end 收尾处为"仍是默认标题"的会话生成可读标题。
// 零值不可用；LLM 与 Store 必填，Timeout<=0 用 DefaultTimeout。
type Namer struct {
	LLM     LLM
	Store   Store
	Timeout time.Duration
}

// IsDefaultTitle 判断标题是否仍是渠道默认形态（channel:chatID，如 bear:main）。
func IsDefaultTitle(title, channel string) bool {
	return strings.HasPrefix(title, channel+":")
}

// NameChat 是 turn_end 的异步入口：自建带超时的 ctx，永不 panic、只打日志。
func (n *Namer) NameChat(tenant, channel, chatID, sessionID string) {
	if _, err := n.Rename(tenant, channel, sessionID, false); err != nil {
		slog.Warn("naming: skipped", "chat_id", chatID, "err", err)
	}
}

// Rename 为会话生成并写入标题，返回新标题。force 跳过默认标题检查
// （POST /v1/chats/{id}/title 手动触发用）。LLM 失败返回错误、标题不动。
func (n *Namer) Rename(tenant, channel, sessionID string, force bool) (string, error) {
	timeout := n.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	// 脱离调用方生命周期（slot 关闭/HTTP 请求结束不影响命名），自管超时。
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	ctx = scope.WithScope(ctx, scope.Scope{TenantID: tenant, SessionID: sessionID})

	title, err := n.Store.SessionTitle(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if !force && !IsDefaultTitle(title, channel) {
		return title, nil // 已有可读标题：防重，不再命名
	}
	userMsg, botMsg, err := firstRound(ctx, n.Store, sessionID)
	if err != nil {
		return "", err
	}
	resp, err := n.LLM.Chat(ctx, []agent.Message{
		{Role: agent.RoleSystem, Content: sysPrompt},
		{Role: agent.RoleUser, Content: "用户消息：" + clamp(userMsg, 500) + "\n助手回复：" + clamp(botMsg, 500)},
	}, nil, nil)
	if err != nil {
		return "", err
	}
	name := sanitize(resp.Message.Content)
	if name == "" {
		return "", errEmptyTitle
	}
	if err := n.Store.SetTitle(ctx, sessionID, name); err != nil {
		return "", err
	}
	return name, nil
}

// firstRound 提取首轮 user 与 assistant 正文；缺任一侧报 errNoRound。
func firstRound(ctx context.Context, st Store, sessionID string) (string, string, error) {
	rows, err := st.LoadRawHistory(ctx, sessionID)
	if err != nil {
		return "", "", err
	}
	var userMsg, botMsg string
	for _, m := range rows {
		if m.Kind != agent.KindMessage {
			continue // 控制行（turn_end/boundary/...）不参与命名
		}
		switch m.Role {
		case agent.RoleUser:
			if userMsg == "" && strings.TrimSpace(m.Content) != "" {
				userMsg = m.Content
			}
		case agent.RoleAssistant:
			if botMsg == "" && strings.TrimSpace(m.Content) != "" {
				botMsg = m.Content
			}
		}
		if userMsg != "" && botMsg != "" {
			return userMsg, botMsg, nil
		}
	}
	return "", "", errNoRound
}

// sanitize 清洗模型输出：压白空、去包裹引号、截到 maxTitleRunes。
func sanitize(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.Trim(s, "\"'`“”「」『』《》。、，.!?！？ ")
	s = truncate(s, maxTitleRunes)
	return strings.TrimSpace(s)
}

// clamp 截断进 prompt 的原文长度（runes）。
func clamp(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return truncate(s, n)
}

// truncate 按 rune 截断。
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
