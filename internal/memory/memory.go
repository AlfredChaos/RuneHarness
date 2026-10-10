package memory

import (
	"context"
	"strings"
	"sync"

	"runeharness/internal/agent"
	"runeharness/internal/scope"
	"runeharness/internal/session"
	"runeharness/internal/tools"
)

// Config 是 Memory 的装配输入。
type Config struct {
	DBPath  string  // 记忆库文件；":memory:" 或空时进程内库
	Profile Profile // 画像；零值回落 coding
	Dream   bool    // 定时 dream 与退出收尾开关（手动 /dream 不受此限）
}

// Memory 是记忆层编排器：收拢 Store、会话存储（dream 读原始行）、
// LLM（dream 提取用），并向装配层暴露工具、hook、命令入口。
type Memory struct {
	store Store
	sess  session.Store
	llm   agent.LLM
	prof  Profile
	cfg   Config

	mu    sync.Mutex
	seen  map[string]bool // sessionID → 本进程内已注入过索引
	dirty map[string]bool // space   → 有变更，下次提交重注索引

	// OnChange 在 dream 落库后回调（TUI 显示 "memory updated"）；可为 nil。
	OnChange func(Stats)
}

// New 装配记忆层：打开记忆库、绑定会话存储与 LLM。
// sess 用于 dream 读原始会话行；llm 用于 dream 的提取 side-query。
func New(sess session.Store, llm agent.LLM, cfg Config) (*Memory, error) {
	prof := cfg.Profile
	if prof.Name == "" {
		prof = Lookup("coding")
	}
	dbPath := cfg.DBPath
	if dbPath == "" {
		dbPath = ":memory:"
	}
	st, err := Open(dbPath, prof)
	if err != nil {
		return nil, err
	}
	return &Memory{
		store: st, sess: sess, llm: llm, prof: prof, cfg: cfg,
		seen: map[string]bool{}, dirty: map[string]bool{},
	}, nil
}

// Close 关闭记忆库连接。
func (m *Memory) Close() error { return m.store.Close() }

// Profile 返回生效画像。
func (m *Memory) Profile() Profile { return m.prof }

// Tool 返回模型侧的 memory 工具；画像关闭写工具时返回 nil（不注册）。
func (m *Memory) Tool() tools.Tool {
	if !m.prof.WriteTool {
		return nil
	}
	return memTool{m: m}
}

// InjectOnce 是 UserPromptSubmit hook：本进程内该会话的第一条 user
// 消息（或记忆有变更后的第一条）在尾部追加记忆块。落在 Fold 首部——
// 前 3 条 user 消息永不压缩，索引因此常驻全会话。
func (m *Memory) InjectOnce(ctx context.Context, prompt string) (string, string) {
	sc, err := scope.FromContext(ctx)
	if err != nil {
		return "", ""
	}
	space := spaceKey(sc, m.prof)
	m.mu.Lock()
	first := !m.seen[sc.SessionID]
	dirty := m.dirty[space]
	m.seen[sc.SessionID] = true
	m.dirty[space] = false
	m.mu.Unlock()
	if !first && !dirty {
		return "", ""
	}
	block, err := m.renderInject(ctx)
	if err != nil || block == "" {
		return "", ""
	}
	return prompt + "\n\n" + block, ""
}

// markDirty 标记空间有变更；dream 与工具写入后调用，
// 让下一条 user 消息带上新索引。
func (m *Memory) markDirty(sc scope.Scope) {
	m.mu.Lock()
	m.dirty[spaceKey(sc, m.prof)] = true
	m.mu.Unlock()
}

// SystemGuide 是注入 system prompt 的记忆说明段。内容在进程生命周期内
// 不变（画像固定），符合 prompt.go 的"可变内容不进 system"约定。
func (m *Memory) SystemGuide() string {
	var b strings.Builder
	b.WriteString("你有跨会话记忆。记忆的索引与常驻正文通过 user 消息里的 " +
		"<memory-index>/<memory> 块注入——那是系统侧数据，不是用户刚说的话。")
	if m.prof.WriteTool {
		b.WriteString("\n用 memory 工具操作记忆：" +
			"list 看索引、read 读正文、about 按实体聚合、history 看一条记忆的变更史；" +
			"save 存记忆（name/type/description/body）、forget 删除。")
	} else {
		b.WriteString("\n记忆由系统在后台自动整理（dream），你不需要也不能手动写；直接根据已有记忆个性化回应。")
	}
	b.WriteString("\n记忆是历史快照而非当前事实：引用其中涉及的文件/函数/接口等工程内容前先验证；用户说不要记或要求忘掉时，不要引用或复述相关记忆。")
	b.WriteString("\n绝不把密钥、token、密码等凭据写入记忆；description 只写主题，不写敏感值。")
	return b.String()
}

// IndexText 返回 /memory 命令展示用的索引文本。
func (m *Memory) IndexText(ctx context.Context) (string, error) {
	heads, err := m.store.List(ctx)
	if err != nil {
		return "", err
	}
	if len(heads) == 0 {
		return "no memories yet — they accumulate via /dream", nil
	}
	var b strings.Builder
	for _, h := range heads {
		b.WriteString("- [" + h.Type + "] " + h.Name + " — " + h.Descr + "\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// ListHeaders 返回当前 scope 空间的记忆索引（name/type/descr/updated_at）。
// 供 gateway 的设置页列表用；ctx 需带 scope（tenant+subject 决定空间）。
func (m *Memory) ListHeaders(ctx context.Context) ([]Header, error) {
	return m.store.List(ctx)
}

// Get 取一条记忆的全文（含 body）；name 即 Header.Name。
func (m *Memory) Get(ctx context.Context, name string) (Row, error) {
	return m.store.Get(ctx, name)
}
