// Package config 负责从 .env / 环境变量加载运行配置。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Config 是程序的运行配置。
type Config struct {
	APIKey  string
	BaseURL string // OpenAI 兼容端点；为空则用 SDK 默认
	Model   string

	// DBPath 是会话库文件路径（RUNE_DB）；为空时用 DefaultDBPath。
	DBPath string

	// SubAgent 开启后向 agent loop 注入 task 工具（RUNE_SUBAGENT）。
	SubAgent bool
	// SubAgentNest 开启后子代理也持有 task 工具，可再 spawn 一层
	// （RUNE_SUBAGENT_NEST；最深 depth=2，多层嵌套暂不支持）。
	SubAgentNest bool

	// ContextTokens 是工作窗口上限（RUNE_CONTEXT_TOKENS）。解析顺序：
	// 显式环境变量 > 内置模型表前缀匹配 > 200K 回落。语义是"工作窗口"
	// 而非模型真实窗口——可以故意配小以控制成本。
	ContextTokens int
	// MaxOutputTokens 是端点单次输出上限（RUNE_MAX_OUTPUT_TOKENS），限定摘要调用。
	MaxOutputTokens int
	// AutoCompact 是强制压缩开关（RUNE_AUTOCOMPACT，默认开）。
	AutoCompact bool

	// Memory 是记忆层总开关（RUNE_MEMORY，默认开）。
	Memory bool
	// MemoryDB 是记忆库文件路径（RUNE_MEMORY_DB）；为空用 DefaultMemoryDBPath。
	MemoryDB string
	// MemoryProfile 选定记忆画像（RUNE_MEMORY_PROFILE，默认 coding；
	// 可选 coding/companion/generic）。
	MemoryProfile string
	// MemoryDream 是定时 dream 与退出收尾开关（RUNE_MEMORY_DREAM，默认开；
	// 手动 /dream 不受此限）。
	MemoryDream bool
	// MemoryModel 是 dream 提取用的模型名（RUNE_MEMORY_MODEL）；
	// 为空时复用主模型。
	MemoryModel string
}

const (
	defaultContextTokens   = 200_000
	defaultMaxOutputTokens = 8192
)

// modelWindows 是常见模型家族的上下文窗口表，按名称前缀匹配，命中前缀
// 最长者胜出。表是粗口径：端点改名、版本升级都会让它过期，兜底链是
// RUNE_CONTEXT_TOKENS 显式覆盖与上下文超长报错里的数字自愈。
var modelWindows = []struct {
	prefix string
	window int
}{
	{"minimax-", 1_000_000},
	{"kimi-", 256_000},
	{"moonshot-", 256_000},
	{"deepseek-", 128_000},
	{"qwen3-", 262_144},
	{"qwen", 131_072},
	{"glm-", 200_000},
	{"gemini-", 1_000_000},
	{"claude-", 200_000},
	{"gpt-5", 400_000},
	{"gpt-4.1", 1_000_000},
	{"gpt-4o", 128_000},
	{"o3", 200_000},
	{"o4", 200_000},
}

// contextWindow 解析工作窗口：env 显式值最优先；否则按 model 查表，未命中
// 回落 defaultContextTokens。
func contextWindow(env, model string) (int, error) {
	if env != "" {
		n, err := strconv.Atoi(env)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("RUNE_CONTEXT_TOKENS must be a positive integer, got %q", env)
		}
		return n, nil
	}
	best, match := defaultContextTokens, 0
	l := strings.ToLower(model)
	for _, e := range modelWindows {
		if strings.HasPrefix(l, strings.ToLower(e.prefix)) && len(e.prefix) > match {
			best, match = e.window, len(e.prefix)
		}
	}
	return best, nil
}

// Load 读取 .env 并校验必填项。
// Overload 让 .env 覆盖已存在的同名环境变量（本项目以 .env 为准）。
func Load() (Config, error) {
	_ = godotenv.Overload()

	cfg := Config{
		APIKey:        os.Getenv("OPENAI_API_KEY"),
		BaseURL:       os.Getenv("OPENAI_BASE_URL"),
		Model:         os.Getenv("OPENAI_MODEL"),
		DBPath:        os.Getenv("RUNE_DB"),
		SubAgent:      envTrue("RUNE_SUBAGENT"),
		SubAgentNest:  envTrue("RUNE_SUBAGENT_NEST"),
		AutoCompact:   os.Getenv("RUNE_AUTOCOMPACT") == "" || envTrue("RUNE_AUTOCOMPACT"),
		Memory:        os.Getenv("RUNE_MEMORY") == "" || envTrue("RUNE_MEMORY"),
		MemoryDB:      os.Getenv("RUNE_MEMORY_DB"),
		MemoryProfile: os.Getenv("RUNE_MEMORY_PROFILE"),
		MemoryDream:   os.Getenv("RUNE_MEMORY_DREAM") == "" || envTrue("RUNE_MEMORY_DREAM"),
		MemoryModel:   os.Getenv("RUNE_MEMORY_MODEL"),
	}
	var err error
	if cfg.ContextTokens, err = contextWindow(os.Getenv("RUNE_CONTEXT_TOKENS"), cfg.Model); err != nil {
		return Config{}, err
	}
	if cfg.MaxOutputTokens, err = envInt("RUNE_MAX_OUTPUT_TOKENS", defaultMaxOutputTokens); err != nil {
		return Config{}, err
	}
	if cfg.APIKey == "" {
		return Config{}, errors.New("OPENAI_API_KEY is not set (see .env.example)")
	}
	if cfg.Model == "" {
		cfg.Model = "gpt-4o-mini"
	}
	return cfg, nil
}

// DefaultDBPath 返回会话库的默认位置：~/.rune/sessions.db。
// 各平台统一——os.UserHomeDir 按系统解析家目录：unix 读 $HOME，
// windows 读 %USERPROFILE%。
func DefaultDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".rune", "sessions.db"), nil
}

// DefaultMemoryDBPath 返回记忆库的默认位置：~/.rune/memory.db。
// 与会话库分文件：schema 各自演进，记忆可独立备份带走。
func DefaultMemoryDBPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".rune", "memory.db"), nil
}

// envInt 读正整数环境变量；未设置时取默认值，格式非法时报错（不静默回落）。
func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", key, v)
	}
	return n, nil
}

// envTrue 把环境变量按布尔解析："1/true/yes/on"（不区分大小写）为真，其余为假。
func envTrue(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
