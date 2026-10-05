package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"runeharness/internal/scope"
)

// maxToolOutput 截断工具返回内容，避免超长输出撑爆上下文。
const maxToolOutput = 8000

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

func truncate(s string) string {
	if len(s) > maxToolOutput {
		return s[:maxToolOutput] + "\n...[truncated]"
	}
	return s
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
	return truncate(b.String()), nil
}

// ReadFile 读取文本文件内容。
type ReadFile struct{}

func (ReadFile) Spec() Spec {
	return Spec{
		Name:        "read_file",
		Description: "Read a UTF-8 text file and return its content",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
			},
			"required": []string{"path"},
		},
	}
}

func (ReadFile) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", err
	}
	if args.Path == "" {
		return "", errors.New("path is required")
	}
	p, err := resolvePath(ctx, args.Path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return truncate(string(data)), nil
}
