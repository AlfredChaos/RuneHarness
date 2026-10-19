// Package style 是回放评测的回复检查器（plan §0.2）：纯函数，不进线上链路。
// 每个检查器只回答"这一条回复违规吗"，供 cmd/replay 汇总成违规率表。
package style

import (
	"regexp"
	"strings"
	"unicode"
)

// Checker 检查一条 assistant 回复；返回违规描述（空串 = 通过）。
type Checker func(reply string) string

// EndsWithQuestion 判回复是否以问句结尾：剥掉末尾空白与 emoji 后，
// 命中 ？/? 即违规——陪伴场景里"每句都反问"是已确认的风格问题。
func EndsWithQuestion(reply string) string {
	r := []rune(strings.TrimSpace(reply))
	for len(r) > 0 {
		last := r[len(r)-1]
		if unicode.IsSpace(last) || isEmojiTail(last) {
			r = r[:len(r)-1]
			continue
		}
		if last == '？' || last == '?' {
			return "reply ends with a question"
		}
		return ""
	}
	return ""
}

// isEmojiTail 判定结尾字符是否应剥掉的"装饰性尾巴"：emoji 落在
// Misc Symbols / Emoji 各平面区间，标点（。！，）保留作判定依据。
func isEmojiTail(r rune) bool {
	return r >= 0x1F000 || (r >= 0x2600 && r <= 0x27BF)
}

// latinWord 匹配 ASCII 字母词（含 ' 缩写的尾巴）。
var latinWord = regexp.MustCompile(`[A-Za-z][A-Za-z']*`)

// LatinAllow 是夹英文检查的白名单：专名与技术词放行（对齐 plan §0.2
// 的 Alfred/bigbigbear/switch 等）。全部小写比较。
var LatinAllow = map[string]bool{
	"alfred": true, "bigbigbear": true, "switch": true,
	"wifi": true, "app": true, "id": true, "ok": true, "vs": true,
	"m3": true, "flash": true, "api": true, "llm": true,
}

// LatinMix 数中文回复里夹带的拉丁词（不含白名单与 URL/代码段）。
// 返回违规描述含抓到的词，便于回放输出定位。
func LatinMix(reply string) string {
	// 先剔除 code fence 与 URL——代码与链接里的英文不算风格违规。
	clean := stripCodeAndURLs(reply)
	hits := map[string]int{}
	for _, w := range latinWord.FindAllString(clean, -1) {
		lw := strings.ToLower(strings.Trim(w, "'"))
		if lw == "" || LatinAllow[lw] {
			continue
		}
		hits[w]++
	}
	if len(hits) == 0 {
		return ""
	}
	words := make([]string, 0, len(hits))
	for w := range hits {
		words = append(words, w)
	}
	return "latin words mixed in: " + strings.Join(words, ",")
}

var codeFence = regexp.MustCompile("(?s)```.*?```|`[^`]*`")
var urlRe = regexp.MustCompile(`https?://\S+`)

func stripCodeAndURLs(s string) string {
	s = codeFence.ReplaceAllString(s, " ")
	return urlRe.ReplaceAllString(s, " ")
}

// RepeatPhrase 判回复是否复述了近期回复里的长片段：
// 与每条 earlier 共享的最长子串长度 ≥ minShared 即违规。
// "总是重复"的口径按最长公共子串度量，简单可靠。
func RepeatPhrase(reply string, earlier []string, minShared int) string {
	for _, e := range earlier {
		if n := longestCommonSubstring(reply, e); n >= minShared {
			return "shares a >=N-char fragment with an earlier reply"
		}
	}
	return ""
}

// longestCommonSubstring 返回两字符串最长公共子串的 rune 长度。
// O(len(a)*len(b)) 的 DP——回复文本不过几千字，回放场景够用。
func longestCommonSubstring(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 || len(rb) == 0 {
		return 0
	}
	prev := make([]int, len(rb)+1)
	best := 0
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		for j := 1; j <= len(rb); j++ {
			if ra[i-1] == rb[j-1] {
				cur[j] = prev[j-1] + 1
				if cur[j] > best {
					best = cur[j]
				}
			}
		}
		prev = cur
	}
	return best
}

// RoleKept 判用户指定的角色名在故事回复中仍出现且未被替换：
// 回复既不含 role 名，又出现角色叙写信号时才计违规——
// 用户没点名的普通回复不算丢角色。
func RoleKept(role string) Checker {
	return func(reply string) string {
		if role == "" {
			return ""
		}
		if strings.Contains(reply, role) {
			return ""
		}
		// 回复里还在写角色扮演的痕迹（"她/他" 的第三人称叙写）
		// 却没提指定角色名 → 角色被替换的嫌疑。
		if strings.Contains(reply, "她") || strings.Contains(reply, "他") {
			return "role name " + role + " absent from a roleplay reply"
		}
		return ""
	}
}
