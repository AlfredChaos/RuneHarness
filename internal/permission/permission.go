// Package permission 实现工具级权限判定（Permission Desk）。
// 每次工具调用执行前依次过三道闸：
//
//	Gate 1: 硬拒绝名单（危险命令/敏感路径），任何模式都拦不住
//	Gate 2: 规则匹配（safe 工具、safe 命令前缀）
//	Gate 3: 用户确认（由调用方阻塞等待前台答复，本包不负责）
package permission

import (
	"encoding/json"
	"strings"
)

// Verdict 是一次工具调用的判定结果。
type Verdict int

const (
	// Allow 直接放行。
	Allow Verdict = iota
	// Ask 需要前台向用户确认。
	Ask
	// Deny 硬拒绝：不执行、不询问。
	Deny
)

// Config 是权限名单的集中配置。当前在启动代码中硬编码，
// 待其他功能稳定后再迁移到配置文件加载。
//
// 注意：黑名单式的权限控制只在个人、有限场景下有效——本质是用
// "已知的坏"的枚举去防御"可能的坏"，防事故，不防攻击。已知失效场景：
//  1. 黑名单可绕过：命令变体（rm -r -f）、shell 展开、编码载荷，
//     枚举永远落后于构造；
//  2. 语境失明：同一命令因目标/意图不同风险迥异，字符串规则看不到上下文；
//  3. 确认疲劳：高频弹窗训练用户无脑批准，闸门 3 名存实亡；
//  4. 无用户可问：headless / 子 agent 场景下 ask 永远等不到答复；
//  5. 组合攻击与注入：逐调用判定看不到跨步意图，也看不到工具结果里
//     藏着的恶意指令。
type Config struct {
	// SafeTools 是始终放行的只读工具名。
	SafeTools []string
	// SafeCommandPrefixes 是 run_command 中可放行的命令前缀（按词首匹配）。
	SafeCommandPrefixes []string
	// ForbiddenPatterns 命中即硬拒绝的子串，作用于调用主体（命令文本或路径）。
	ForbiddenPatterns []string
	// ForbiddenPaths 命中即硬拒绝的路径片段；对 run_command 的命令文本同样生效，
	// 防止用 cat .env 绕过 read_file 的防护。
	ForbiddenPaths []string
}

// Desk 持有一份不可变配置，对每次调用给出判定。纯函数，无副作用。
type Desk struct {
	cfg Config
}

func New(cfg Config) *Desk { return &Desk{cfg: cfg} }

// Check 判定一次调用，返回 (判定, 原因)。reason 供 UI 展示和回填给模型的错误说明。
func (d *Desk) Check(name string, args json.RawMessage) (Verdict, string) {
	subject := subjectOf(args)

	// Gate 1: 硬拒绝名单
	for _, p := range d.cfg.ForbiddenPatterns {
		if strings.Contains(subject, p) {
			return Deny, "matches forbidden pattern: " + p
		}
	}
	for _, p := range d.cfg.ForbiddenPaths {
		if matchPath(subject, p) {
			return Deny, "path matches forbidden pattern: " + p
		}
	}

	// Gate 2: 规则匹配
	if name == "run_command" {
		return d.checkCommand(subject)
	}
	for _, t := range d.cfg.SafeTools {
		if name == t {
			return Allow, ""
		}
	}
	return Ask, "tool not in safe list"
}

// checkCommand 判定 shell 命令。
// 含 shell 元字符时前缀白名单失效："ls && rm -rf x" 不能因 ls 前缀放行。
// 误伤（如 echo "a>b"）可接受——保守方向是多问一次。
func (d *Desk) checkCommand(cmd string) (Verdict, string) {
	cmd = strings.TrimSpace(cmd)
	if strings.ContainsAny(cmd, "&|;`$><") {
		return Ask, "command contains shell operators"
	}
	for _, p := range d.cfg.SafeCommandPrefixes {
		if cmd == p || strings.HasPrefix(cmd, p+" ") {
			return Allow, ""
		}
	}
	return Ask, "command not in safe list"
}

// subjectOf 提取调用参数中用于规则匹配的主体：
// 优先 command，其次 path，兜底取参数原文。
func subjectOf(args json.RawMessage) string {
	var v struct {
		Command string `json:"command"`
		Path    string `json:"path"`
	}
	if json.Unmarshal(args, &v) == nil {
		if v.Command != "" {
			return v.Command
		}
		if v.Path != "" {
			return v.Path
		}
	}
	return string(args)
}

// matchPath 匹配路径片段：整段结尾命中（.env、id.pem），或作为中间目录命中（.aws/）。
// .env 不会误伤 .env.example——前者是结尾命中，后者尾部是 .example。
func matchPath(subject, pat string) bool {
	return strings.HasSuffix(subject, pat) || strings.Contains(subject, pat+"/")
}
