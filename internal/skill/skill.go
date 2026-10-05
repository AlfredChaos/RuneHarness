// Package skill 在启动时扫描技能目录，把每个 SKILL.md 的 frontmatter
// 元数据（name/description）聚合为目录文本，供注入 system prompt。
// 这是渐进式披露的第一层：目录常驻、正文按需——目录条目携带 SKILL.md
// 路径，模型判断任务相关时用既有 read_file 自取全文，无需专用加载工具。
package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// manifestName 是每个技能子目录中的清单文件名。
const manifestName = "SKILL.md"

// descLimit 是目录里单条描述的字符上限（按 rune 计），对齐 CC 的 listing
// 截断（description ≤1536）：防止单条超长描述把常驻 prompt 撑爆。
const descLimit = 1536

// Meta 是一个技能的元数据：SKILL.md frontmatter 摘要 + 正文入口路径。
type Meta struct {
	Name        string // frontmatter name；缺省退回目录名
	Description string // frontmatter description；缺省退回正文首个非空行
	Path        string // SKILL.md 绝对路径——模型按需加载正文的入口
}

// Scan 扫描 dir 的直接子目录，为每个含 SKILL.md 的子目录生成一条 Meta，
// 顺序按目录名（os.ReadDir 保序）。dir 缺失或不可读时返回 nil——技能
// 目录是可选增强而非启动前提；单个条目损坏只跳过该条目，不影响其余。
func Scan(dir string) []Meta {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Meta
	for _, e := range entries {
		// 不能用 e.IsDir()：DirEntry 的类型位不跟随符号链接，
		// 链接到目录的 skill（~/.agents/skills 的常见形态）会被漏掉。
		// os.Stat 跟随链接；坏链/链环报错即跳过。
		if info, err := os.Stat(filepath.Join(dir, e.Name())); err != nil || !info.IsDir() {
			continue
		}
		manifest := filepath.Join(dir, e.Name(), manifestName)
		raw, err := os.ReadFile(manifest)
		if err != nil {
			continue
		}
		m := parseMeta(string(raw), e.Name())
		m.Path = manifest
		out = append(out, m)
	}
	return out
}

// Catalog 把技能目录渲染为 system prompt 片段；空列表返回空串。
// 每行一个技能并附正文路径：描述用于模型判断相关性，路径用于命中后
// 按需加载，正文不进 system prompt。
func Catalog(skills []Meta) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("任务与下列某项技能匹配时，必须先用 read_file 读取其 SKILL.md 获取完整说明，再开始执行：")
	for _, s := range skills {
		fmt.Fprintf(&b, "\n- %s: %s (%s)", s.Name, clipRunes(s.Description, descLimit), s.Path)
	}
	return b.String()
}

// clipRunes 把 s 截断到 max 个 rune，超出时补省略号。
func clipRunes(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// parseMeta 从 SKILL.md 原文提取 name/description。
// frontmatter 需以首行 "---" 开始、下一个 "---" 行结束；只识别顶格的
// "key: value" 标量行并剥掉成对引号——嵌套缩进行与多行标量标记（>|）
// 一律跳过，按缺省处理。这是刻意的最小解析，不是完整 YAML。
func parseMeta(raw, dirName string) Meta {
	lines := strings.Split(raw, "\n")
	bodyStart := 0
	var name, desc string
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i := 1; i < len(lines); i++ {
			line := lines[i]
			if strings.TrimSpace(line) == "---" {
				bodyStart = i + 1
				break
			}
			if line == "" || line[0] == ' ' || line[0] == '\t' {
				continue // 空行与缩进行（上层键的子结构）不算顶层字段
			}
			key, val, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			val = strings.TrimSpace(val)
			if val == "" || val[0] == '>' || val[0] == '|' {
				continue // 空值或 YAML 多行标量标记，按缺省处理
			}
			switch key {
			case "name":
				name = unquote(val)
			case "description":
				desc = unquote(val)
			}
		}
	}
	if name == "" {
		name = dirName
	}
	if desc == "" {
		desc = firstLine(lines[bodyStart:])
	}
	return Meta{Name: name, Description: desc}
}

// firstLine 返回正文首个非空行，剥掉 markdown 标题的 '#' 前缀。
func firstLine(lines []string) string {
	for _, l := range lines {
		if l = strings.TrimSpace(l); l != "" {
			return strings.TrimSpace(strings.TrimLeft(l, "#"))
		}
	}
	return ""
}

// unquote 剥掉成对的单/双引号；不处理 YAML 转义细节。
func unquote(s string) string {
	if len(s) >= 2 &&
		(s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}
