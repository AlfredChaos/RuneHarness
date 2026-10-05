// Package config 负责从 .env / 环境变量加载运行配置。
package config

import (
	"errors"
	"os"
	"path/filepath"
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
}

// Load 读取 .env 并校验必填项。
// Overload 让 .env 覆盖已存在的同名环境变量（本项目以 .env 为准）。
func Load() (Config, error) {
	_ = godotenv.Overload()

	cfg := Config{
		APIKey:       os.Getenv("OPENAI_API_KEY"),
		BaseURL:      os.Getenv("OPENAI_BASE_URL"),
		Model:        os.Getenv("OPENAI_MODEL"),
		DBPath:       os.Getenv("RUNE_DB"),
		SubAgent:     envTrue("RUNE_SUBAGENT"),
		SubAgentNest: envTrue("RUNE_SUBAGENT_NEST"),
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

// envTrue 把环境变量按布尔解析："1/true/yes/on"（不区分大小写）为真，其余为假。
func envTrue(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
