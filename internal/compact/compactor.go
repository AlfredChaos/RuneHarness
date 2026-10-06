package compact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"runeharness/internal/agent"
)

// Store 是压缩器需要的存储面，session.Store 结构满足。目标会话取自 ctx 的 scope。
type Store interface {
	Append(ctx context.Context, msg agent.Message) (int64, error)
	AppendBlob(ctx context.Context, ref, content string) error
	GetState(ctx context.Context) (json.RawMessage, error)
	PutState(ctx context.Context, state json.RawMessage) error
}

// 四段预留（plan §4.4）：从窗口顶部往下依次扣出。
const (
	reserveSummary = 20_000 // 压缩请求输出摘要
	reserveBuffer  = 13_000 // 一轮工具结果的突增缓冲
	reserveUser    = 3_000  // 用户随时插入的指令
	reserveRemind  = 20_000 // 提醒层：留给模型择机压缩
	// MinWindow 是允许的最小窗口：四段预留之外至少还要留出 8K 工作空间。
	MinWindow = reserveSummary + reserveBuffer + reserveUser + reserveRemind + 8_000
)

// Config 是压缩器的运行配置。
type Config struct {
	Window    int  // 模型上下文窗口（RUNE_CONTEXT_TOKENS）
	MaxOutput int  // 端点单次输出上限（RUNE_MAX_OUTPUT_TOKENS），限定摘要调用的 max_tokens
	Auto      bool // 强制压缩开关（RUNE_AUTOCOMPACT）；关闭时提醒、手动与模型入口仍生效
	Overhead  int  // 不在 history 里、但每次请求都发送的 token（工具清单）
	// Subagent 为子代理模式：任务消息总是钉进首部（plan §4.9）。
	Subagent bool
	// Reattach 返回摘要后重挂的附件文本（todo 列表当前状态）；可为 nil。
	Reattach func() string
}

// ReminderLine 是提醒线：W − 56K，提示模型择机压缩。
func (c Config) ReminderLine() int {
	return c.Window - reserveSummary - reserveBuffer - reserveUser - reserveRemind
}

// CompactLine 是强制压缩线：W − 33K，代码兜底压缩。
func (c Config) CompactLine() int { return c.Window - reserveSummary - reserveBuffer }

// BlockLine 是阻断线：W − 20K。auto 关闭或熔断时，用量超过它就拒发——
// 再往上，压缩请求连摘要输出都放不下。
func (c Config) BlockLine() int { return c.Window - reserveSummary }

// Compactor 实现 agent.Compactor。无内存中的会话状态：熔断计数落 Store，
// 同一个实例可服务 TUI /resume 换绑后的不同会话。
type Compactor struct {
	llm   agent.LLM
	store Store
	cfg   Config
}

// New 创建压缩器。store 不可为 nil：blob、boundary、熔断计数都依赖它。
func New(llm agent.LLM, store Store, cfg Config) (*Compactor, error) {
	if store == nil {
		return nil, errors.New("compact: store is required")
	}
	if cfg.Window < MinWindow {
		return nil, fmt.Errorf("compact: context window %d is below the minimum %d", cfg.Window, MinWindow)
	}
	return &Compactor{llm: llm, store: store, cfg: cfg}, nil
}

// Config 返回压缩器配置（提醒 hook、TUI 用量显示共用同一组阈值）。
func (c *Compactor) Config() Config { return c.cfg }

var _ agent.Compactor = (*Compactor)(nil)
