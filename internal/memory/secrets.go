package memory

import "regexp"

// secretPatterns 覆盖常见凭据形态。提示词的"不存"只是软约束，这道
// 代码层扫描是硬约束——dream 产物与工具写入都过它，命中即拒写。
// 模式偏保守（前缀明确的 token 形态 + 凭据赋值），宁可误拒普通文本，
// 不可放进一条真密钥。
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),                                        // OpenAI/通用 sk- 前缀
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),                                             // AWS access key
	regexp.MustCompile(`ghp_[A-Za-z0-9]{30,}`),                                         // GitHub PAT
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{30,}`),                                 // GitHub fine-grained PAT
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),                                 // Slack token
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{5,}`), // JWT
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),                           // PEM 私钥
	regexp.MustCompile(`(?i)(password|passwd|secret|api[_-]?key|access[_-]?token)\s*[:=]\s*["']?[^\s"']{8,}`),
}

// LooksLikeSecret 报告文本是否含常见凭据形态。
func LooksLikeSecret(s string) bool {
	for _, p := range secretPatterns {
		if p.MatchString(s) {
			return true
		}
	}
	return false
}
