package prompt

import (
	"os"
	"strings"
)

// LoadSoul 读人设文件（SOUL.md，人格契约；设计见
// docs/runeharness-architecture.html 第 6 章）。runebot 启动时读一次，
// 内容经 Env.Soul 冻结进该进程所有会话的 system prompt——运行中的会话
// 不换人设，与 tools 段同理，保住前缀缓存。
//
// 路径为空（RUNE_SOUL 未设）或文件不存在/为空：返回空串，不算错误——
// 人设是可选外挂，缺失时行为与没有该机制的版本完全一致。
func LoadSoul(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if strings.TrimSpace(string(b)) == "" {
		return ""
	}
	return string(b)
}
