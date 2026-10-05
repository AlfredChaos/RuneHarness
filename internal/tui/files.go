// files.go —— "@" 文件提及（参照 claude-code 的 @ 补全与 attachment 展开）：
//
//	输入时：光标处的 @token（行首/空白后的 @ 起，空白止；@"..." 引号形态
//	允许空格）实时补全 workspace 内文件与目录，目录项补全为 "@dir/"
//	可继续下钻；选中文件补全为 "@path "。
//	提交时：消息中的全部 @path 引用展开为 <attachments><file> 内容块，
//	附在用户文本之后发给模型；对话区仍显示原始输入。
//
// 文件清单优先取 `git ls-files -co --exclude-standard`（tracked + 未被
// ignore 的 untracked，天然尊重 .gitignore）；非 git 目录回退 WalkDir
// 跳过常见的重目录。清单带 TTL 缓存，避免每击键重建。
package tui

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	fileListTTL    = 3 * time.Second // 文件清单缓存寿命
	fileSuggCap    = 20              // 文件建议条目上限
	attachFileCap  = 32 << 10        // 单文件附件字节上限
	attachTotalCap = 128 << 10       // 附件总量字节上限
	attachDirCap   = 300             // 目录附件的行数上限
	fileWalkCap    = 20000           // 非 git 目录遍历时收集的文件数上限
)

// atTok 是光标处的一个 @ 提及 token。
type atTok struct {
	start  int    // '@' 的 rune 偏移
	end    int    // token 结束（含光标后的延续部分与收尾引号）
	query  string // @ 与光标之间的查询文本（引号形态为引号内文本）
	quoted bool   // @"…" 形态
}

// atToken 提取光标处的 @ token；光标不在 @ token 内返回 false。
// 只认光标前最近的一个 @：该 token 若已在光标前闭合（空白截断或引号
// 收尾），说明光标在 token 外，不再向前找更早的 @。
func atToken(v string, cursor int) (atTok, bool) {
	rs := []rune(v)
	if cursor > len(rs) {
		cursor = len(rs)
	}
	for j := cursor - 1; j >= 0; j-- {
		if rs[j] != '@' {
			continue
		}
		if j > 0 && !unicode.IsSpace(rs[j-1]) {
			continue // email 等：@ 前必须是空白或行首
		}
		body := rs[j+1 : cursor]
		if len(body) > 0 && body[0] == '"' {
			// 引号形态：闭合引号在光标之前则 token 已结束
			tail := string(body[1:])
			if strings.IndexByte(tail, '"') >= 0 {
				return atTok{}, false
			}
			end := cursor + indexRuneOrLen(rs[cursor:], '"')
			if end < len(rs) {
				end++ // 把收尾引号一并纳入替换区间
			}
			return atTok{start: j, end: end, query: tail, quoted: true}, true
		}
		if strings.IndexFunc(string(body), unicode.IsSpace) >= 0 {
			return atTok{}, false // token 在光标前已被空白截断
		}
		end := cursor
		for end < len(rs) && !unicode.IsSpace(rs[end]) {
			end++
		}
		return atTok{start: j, end: end, query: string(body)}, true
	}
	return atTok{}, false
}

// indexRuneOrLen 返回 r 在 rs 中的下标；未命中返回 len(rs)。
func indexRuneOrLen(rs []rune, r rune) int {
	for i := range rs {
		if rs[i] == r {
			return i
		}
	}
	return len(rs)
}

// cursorOffset 返回 textarea 光标在整个输入串中的 rune 偏移。
// LineInfo().CharOffset 是当前行内的 rune 偏移，行号乘以换行补齐。
func (m *Model) cursorOffset() int {
	rs := []rune(m.input.Value())
	row := m.input.Line()
	off := 0
	for off < len(rs) && row > 0 {
		if rs[off] == '\n' {
			row--
		}
		off++
	}
	return off + m.input.LineInfo().CharOffset
}

// setCursorAt 把光标移到 rune 偏移 pos（textarea 按 row/col 定位，逐行换算）。
func (m *Model) setCursorAt(pos int) {
	rs := []rune(m.input.Value())
	row, col := 0, 0
	for i := 0; i < pos && i < len(rs); i++ {
		if rs[i] == '\n' {
			row, col = row+1, 0
		} else {
			col++
		}
	}
	for m.input.Line() < row {
		m.input.CursorDown()
	}
	for m.input.Line() > row {
		m.input.CursorUp()
	}
	m.input.SetCursor(col)
}

// ── 文件清单 ──

// workspaceFiles 返回 workspace 内可引用的文件相对路径（正斜杠），带 TTL 缓存。
func (m *Model) workspaceFiles() []string {
	if m.fileIndex != nil && time.Since(m.fileIndexAt) < fileListTTL {
		return m.fileIndex
	}
	m.fileIndex = listWorkspaceFiles(m.sc.Workspace)
	m.fileIndexAt = time.Now()
	return m.fileIndex
}

// listWorkspaceFiles 枚举 root 下的文件（相对路径）。git 仓库用
// ls-files（含未跟踪文件，尊重 ignore 规则）；否则遍历并跳过重目录。
func listWorkspaceFiles(root string) []string {
	if root == "" {
		return nil
	}
	if out, err := exec.Command("git", "-C", root, "ls-files",
		"-co", "--exclude-standard").Output(); err == nil {
		var files []string
		for _, ln := range strings.Split(string(out), "\n") {
			if ln = strings.TrimSpace(ln); ln != "" {
				files = append(files, filepath.ToSlash(ln))
			}
		}
		return files
	}
	var files []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "dist", "build", "out",
				".next", ".turbo", "__pycache__", "target", ".idea", ".vscode":
				return filepath.SkipDir
			}
			return nil
		}
		if rel, err := filepath.Rel(root, p); err == nil {
			files = append(files, filepath.ToSlash(rel))
		}
		if len(files) >= fileWalkCap {
			return filepath.SkipAll
		}
		return nil
	})
	return files
}

// fileSuggestions 生成 @ 查询的文件/目录建议：空查询列顶层条目；
// 非空按"基名全等 > 基名前缀 > 基名子串 > 路径子串 > 子序列"分级。
func (m *Model) fileSuggestions(query string) []suggestion {
	files := m.workspaceFiles()
	if len(files) == 0 {
		return nil
	}
	q := strings.TrimPrefix(strings.ToLower(query), "./")
	if q == "" {
		return m.topLevelSuggestions()
	}
	type cand struct {
		suggestion
		rank int
	}
	var cands []cand
	push := func(path string, isDir bool) {
		base := path
		if i := strings.LastIndexByte(path, '/'); i >= 0 {
			base = path[i+1:]
		}
		lb, lp := strings.ToLower(base), strings.ToLower(path)
		var rank int
		switch {
		case lb == q:
			rank = 0
		case strings.HasPrefix(lb, q):
			rank = 1
		case strings.Contains(lb, q):
			rank = 2
		case strings.Contains(lp, q):
			rank = 3
		case isSubsequence(lp, q):
			rank = 4
		default:
			return
		}
		label := path
		if isDir {
			label += "/"
		}
		desc := ""
		if isDir {
			desc = "dir"
		}
		cands = append(cands, cand{
			suggestion{id: "file:" + label, kind: suggFile, label: label,
				desc: desc, file: path, isDir: isDir},
			rank})
	}
	for _, f := range files {
		push(f, false)
	}
	// 目录项从文件路径的父目录推导，选中后补全 "@dir/" 继续下钻。
	seen := map[string]bool{}
	for _, f := range files {
		for i := 0; i < len(f); i++ {
			if f[i] != '/' {
				continue
			}
			d := f[:i]
			if !seen[d] {
				seen[d] = true
				push(d, true)
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].rank != cands[j].rank {
			return cands[i].rank < cands[j].rank
		}
		if len(cands[i].label) != len(cands[j].label) {
			return len(cands[i].label) < len(cands[j].label)
		}
		return cands[i].label < cands[j].label
	})
	if len(cands) == 0 {
		return nil
	}
	out := make([]suggestion, 0, min(len(cands), fileSuggCap))
	for i := 0; i < len(cands) && len(out) < fileSuggCap; i++ {
		out = append(out, cands[i].suggestion)
	}
	return out
}

// topLevelSuggestions 列 workspace 顶层条目（目录带 / 后缀），
// 对应 CC 空查询时的 getTopLevelPaths。
func (m *Model) topLevelSuggestions() []suggestion {
	entries, err := os.ReadDir(m.sc.Workspace)
	if err != nil {
		return nil
	}
	var out []suggestion
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		label := e.Name()
		isDir := e.IsDir()
		if isDir {
			label += "/"
		}
		out = append(out, suggestion{
			id: "file:" + label, kind: suggFile, label: label,
			desc: map[bool]string{true: "dir", false: ""}[isDir],
			file: e.Name(), isDir: isDir,
		})
	}
	return out
}

// isSubsequence 判断 needle 是否为 hay 的子序列（fzf 式散匹配兜底）。
func isSubsequence(hay, needle string) bool {
	hr, nr := []rune(hay), []rune(needle)
	j := 0
	for i := 0; i < len(hr) && j < len(nr); i++ {
		if hr[i] == nr[j] {
			j++
		}
	}
	return j == len(nr)
}

// applyFileCompletion 把光标处的 @token 替换为完整引用：
// 文件 → "@path "（含空白的路径自动加引号），目录 → "@dir/" 继续下钻。
func (m *Model) applyFileCompletion(s suggestion) tea.Cmd {
	v := m.input.Value()
	rs := []rune(v)
	tok, ok := atToken(v, m.cursorOffset())
	if !ok {
		return nil
	}
	// 含空白的路径一律 @"..." 引号形态，否则提交解析会在空格处截断。
	quoted := strings.ContainsAny(s.file, " \t")
	rep := "@" + s.file
	switch {
	case s.isDir && quoted:
		rep = "@\"" + s.file + "/\""
	case s.isDir:
		rep += "/"
	case quoted:
		rep = "@\"" + s.file + "\" "
	default:
		rep += " "
	}
	next := string(rs[:tok.start]) + rep + string(rs[tok.end:])
	m.input.SetValue(next)
	// 引号目录的光标落在收尾引号内，@token 保持打开继续出下级建议。
	pos := tok.start + len([]rune(rep))
	if s.isDir && strings.ContainsAny(s.file, " \t") {
		pos--
	}
	m.setCursorAt(pos)
	m.suggFor = ""
	m.updateSuggestions()
	m.fitInput()
	return nil
}

// ── 提交时的 @ 展开 ──

// parseMentions 提取文本中的全部 @path 引用：@path 与 @"quoted path" 两种
// 形态；@ 前必须是空白或行首（email 不算）。返回去重后的路径列表。
func parseMentions(s string) []string {
	rs := []rune(s)
	var out []string
	seen := map[string]bool{}
	for i := 0; i < len(rs); i++ {
		if rs[i] != '@' || (i > 0 && !unicode.IsSpace(rs[i-1])) {
			continue
		}
		j := i + 1
		var p string
		if j < len(rs) && rs[j] == '"' {
			j++
			k := j
			for k < len(rs) && rs[k] != '"' {
				k++
			}
			p = string(rs[j:k])
			j = k + 1 // 跳过收尾引号（缺失时到串尾）
		} else {
			k := j
			for k < len(rs) && !unicode.IsSpace(rs[k]) && rs[k] != '"' {
				k++
			}
			p = string(rs[j:k])
			j = k
		}
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
		i = j - 1
	}
	return out
}

// expandMentions 把 content 里的 @path 引用展开为尾部 <attachments> 块；
// 返回展开后文本与逐条结果标签（供对话区留痕）。workspace 为空时不展开。
func (m *Model) expandMentions(content string) (string, []string) {
	paths := parseMentions(content)
	if len(paths) == 0 || m.sc.Workspace == "" {
		return content, nil
	}
	var b strings.Builder
	b.WriteString(content)
	b.WriteString("\n\n<attachments>\n")
	var labels []string
	total := 0
	for _, p := range paths {
		body, label := m.readAttachment(p, attachTotalCap-total)
		total += len(body)
		fmt.Fprintf(&b, "<file path=%q>\n%s\n</file>\n", p, body)
		labels = append(labels, label)
	}
	b.WriteString("</attachments>")
	return b.String(), labels
}

// readAttachment 读取单个 @ 引用的内容：目录给递归清单，文件给正文
// （截断标注），错误内联为 [error: …]。返回正文与展示标签。
func (m *Model) readAttachment(p string, budget int) (string, string) {
	full, err := resolveMention(m.sc.Workspace, p)
	if err != nil {
		return "[error: " + err.Error() + "]", p + " ✘"
	}
	info, err := os.Stat(full)
	if err != nil {
		return "[error: " + err.Error() + "]", p + " ✘"
	}
	if info.IsDir() {
		var lines []string
		filepath.WalkDir(full, func(fp string, d os.DirEntry, err error) error {
			if err != nil || len(lines) >= attachDirCap {
				return filepath.SkipAll
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "node_modules", "vendor", "dist", "build",
					".next", "__pycache__":
					return filepath.SkipDir
				}
				return nil
			}
			if rel, err := filepath.Rel(full, fp); err == nil {
				lines = append(lines, filepath.ToSlash(rel))
			}
			return nil
		})
		body := strings.Join(lines, "\n")
		if len(lines) >= attachDirCap {
			body += "\n...[listing truncated]"
		}
		return body, p + "/ (" + fmt.Sprintf("%d entries", len(lines)) + ")"
	}
	if budget <= 0 {
		return "[omitted: attachment budget exhausted]", p + " (over budget)"
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return "[error: " + err.Error() + "]", p + " ✘"
	}
	// 二进制嗅探：头部含 NUL 按二进制处理，不注入内容
	if i := min(len(data), 512); strings.IndexByte(string(data[:i]), 0) >= 0 {
		return "[binary file omitted]", p + " (binary)"
	}
	trunc := ""
	if len(data) > attachFileCap {
		data = data[:attachFileCap]
		trunc = " [truncated]"
	}
	if len(data) > budget {
		data = data[:max(budget, 0)]
		trunc = " [truncated: total budget]"
	}
	return string(data) + trunc, p + trunc
}

// resolveMention 解析 @ 路径：相对路径锚定 workspace，绝对路径与 ".."
// 越界放行——附件是用户亲自敲的路径，与读工具同口径的自主访问。
func resolveMention(root, p string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("empty workspace")
	}
	full := filepath.Clean(p)
	if !filepath.IsAbs(full) {
		full = filepath.Join(filepath.Clean(root), full)
	}
	return full, nil
}

// splitAttachments 把发送内容拆为正文与附件块（按展开时插入的分隔标记），
// 返回正文与附件条数——会话回放时不重放整段文件内容。
func splitAttachments(content string) (string, int) {
	i := strings.Index(content, "\n\n<attachments>")
	if i < 0 {
		return content, 0
	}
	n := strings.Count(content[i:], "<file ")
	return content[:i], n
}
