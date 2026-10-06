package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"runeharness/internal/scope"
)

const (
	// maxReadChars 是 read_file / list_dir 单次返回的字符上限：这两个工具在
	// 请求侧限额、不进回填层的 spill（plan §4.2 第 2 步），超出部分给续读坐标。
	maxReadChars = 50_000
	// defaultReadLines 是 read_file 未指定 limit 时的行数上限。
	defaultReadLines = 2000
	// BlobScheme 是回填层 / 卸载层落库结果的虚拟路径前缀：read_file 识别后改走
	// Store 查询，不碰文件系统（plan §4.2 取回通道）。
	BlobScheme = "blob://"
)

// BlobReader 按 ref 读取当前会话（及其祖先会话）落库的 blob；
// session_id 取自 ctx 的 scope，模型无法在参数里指定别人的会话。
type BlobReader interface {
	LoadBlob(ctx context.Context, ref string) (string, error)
}

// resolvePath 解析模型给的路径：相对路径锚定 scope.Workspace，绝对路径与
// ".." 越界一律放行——读工具允许自主访问任意文件（敏感文件由 permission
// Gate1 的 ForbiddenPaths 拦截）。写操作不出现在读工具里；将来的写工具若
// 需要 workspace 边界，须自行做包含性检查，不要复用本函数。
func resolvePath(ctx context.Context, p string) (string, error) {
	sc, err := scope.FromContext(ctx)
	if err != nil {
		return "", err
	}
	if sc.Workspace == "" {
		return "", errors.New("scope: empty workspace")
	}
	full := filepath.Clean(p)
	if !filepath.IsAbs(full) {
		full = filepath.Join(filepath.Clean(sc.Workspace), full)
	}
	return full, nil
}

// cutRunes 把 s 截到不超过 n 字节，且不切断 UTF-8 字符。
func cutRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// page 按行切出 [offset, offset+limit) 的内容，合计不超过 maxReadChars；
// 没读完时追加续读坐标，模型据此翻页。offset 从 1 起。
func page(text string, offset, limit int) (string, error) {
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	total := len(lines)
	if total == 0 {
		return "", nil
	}
	if offset > total {
		return "", fmt.Errorf("offset %d is beyond end of file (%d lines)", offset, total)
	}
	var b strings.Builder
	end := offset - 1
	for end < total && end-(offset-1) < limit {
		ln := lines[end]
		if b.Len()+len(ln) > maxReadChars {
			if b.Len() == 0 { // 单行超长（压缩过的文件）：截断这一行
				b.WriteString(cutRunes(ln, maxReadChars))
				end++
				fmt.Fprintf(&b, "\n[line %d truncated at %d chars]", end, maxReadChars)
			}
			break
		}
		b.WriteString(ln)
		end++
	}
	if end < total {
		fmt.Fprintf(&b, "\n[showing lines %d-%d of %d; continue with offset=%d]", offset, end, total, end+1)
	}
	return b.String(), nil
}

func unmarshalArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// ListDir 列出目录内容，目录名带 / 后缀。
type ListDir struct{}

func (ListDir) Spec() Spec {
	return Spec{
		Name:        "list_dir",
		Description: "List entries under a directory; directory names end with /",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "directory path, defaults to \".\""},
			},
		},
	}
}

func (ListDir) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", err
	}
	if args.Path == "" {
		args.Path = "."
	}
	dir, err := resolvePath(ctx, args.Path)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.Name())
		if e.IsDir() {
			b.WriteByte('/')
		}
		b.WriteByte('\n')
	}
	out := b.String()
	if len(out) > maxReadChars {
		out = cutRunes(out, maxReadChars) + fmt.Sprintf("\n[listing truncated at %d chars]", maxReadChars)
	}
	return out, nil
}

// ReadFile 读取文本文件内容，按行分页；path 以 blob:// 开头时读取落库的
// 工具结果全文（Blobs 为 nil 时该形态不可用）。
type ReadFile struct {
	Blobs BlobReader
}

func (ReadFile) Spec() Spec {
	return Spec{
		Name: "read_file",
		Description: "Read a UTF-8 text file and return its content. Large files are paged: " +
			"the output ends with the next offset when more lines remain. " +
			"A path like blob://<ref> reads a stored tool result referenced in the conversation.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string"},
				"offset": map[string]any{"type": "integer", "description": "1-based line to start from; default 1"},
				"limit":  map[string]any{"type": "integer", "description": "max lines to return; default 2000"},
			},
			"required": []string{"path"},
		},
	}
}

func (r ReadFile) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", err
	}
	if args.Path == "" {
		return "", errors.New("path is required")
	}
	if args.Offset < 1 {
		args.Offset = 1
	}
	if args.Limit < 1 {
		args.Limit = defaultReadLines
	}
	if ref, ok := strings.CutPrefix(args.Path, BlobScheme); ok {
		if r.Blobs == nil {
			return "", errors.New("blob storage is not available")
		}
		text, err := r.Blobs.LoadBlob(ctx, ref)
		if err != nil {
			return "", err
		}
		return page(text, args.Offset, args.Limit)
	}
	p, err := resolvePath(ctx, args.Path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return page(string(data), args.Offset, args.Limit)
}
