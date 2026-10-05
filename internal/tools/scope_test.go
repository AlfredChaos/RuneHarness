package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"runeharness/internal/scope"
)

// scopedCtx 造一个租户工作区为 ws 的 ctx；工具的一切边界都来自它。
func scopedCtx(ws string) context.Context {
	return scope.WithScope(context.Background(),
		scope.Scope{TenantID: "t", SessionID: "s", Workspace: ws})
}

// workspace 内相对路径与根内绝对路径都放行。
func TestResolvePathInsideWorkspace(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := scopedCtx(ws)

	out, err := ReadFile{}.Run(ctx, json.RawMessage(`{"path":"f.txt"}`))
	if err != nil || out != "hi" {
		t.Fatalf("relative read: out=%q err=%v", out, err)
	}
	out, err = ReadFile{}.Run(ctx, json.RawMessage(`{"path":"`+filepath.Join(ws, "f.txt")+`"}`))
	if err != nil || out != "hi" {
		t.Fatalf("absolute-in-root read: out=%q err=%v", out, err)
	}
	if _, err := (ListDir{}).Run(ctx, json.RawMessage(`{"path":"."}`)); err != nil {
		t.Fatalf("list root: %v", err)
	}
}

// 读工具允许访问 workspace 外的路径：绝对路径与 ".." 相对逃逸都放行
// （自主访问），敏感文件由 permission Gate1 拦截而非路径边界。
func TestResolvePathOutsideWorkspace(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "out.txt"), []byte("out"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := scopedCtx(ws)

	// 根外绝对路径
	out, err := ReadFile{}.Run(ctx, json.RawMessage(`{"path":"`+filepath.Join(outside, "out.txt")+`"}`))
	if err != nil || out != "out" {
		t.Fatalf("abs outside read: out=%q err=%v", out, err)
	}
	// ".." 相对逃逸，解析到兄弟目录
	rel, err := filepath.Rel(ws, filepath.Join(outside, "out.txt"))
	if err != nil {
		t.Fatal(err)
	}
	out, err = ReadFile{}.Run(ctx, json.RawMessage(`{"path":"`+rel+`"}`))
	if err != nil || out != "out" {
		t.Fatalf("dotdot read: out=%q err=%v", out, err)
	}
	// 根外目录可列
	if _, err := (ListDir{}).Run(ctx, json.RawMessage(`{"path":"`+outside+`"}`)); err != nil {
		t.Fatalf("list outside: %v", err)
	}
}

// ctx 缺 scope 时 fail closed：任何路径处理都直接报错。
func TestToolsRequireScope(t *testing.T) {
	for name, run := range map[string]func() error{
		"read_file": func() error {
			_, err := ReadFile{}.Run(context.Background(), json.RawMessage(`{"path":"a"}`))
			return err
		},
		"list_dir": func() error {
			_, err := ListDir{}.Run(context.Background(), json.RawMessage(`{}`))
			return err
		},
		"run_command": func() error {
			_, err := RunCommand{}.Run(context.Background(), json.RawMessage(`{"command":"echo x"}`))
			return err
		},
	} {
		if err := run(); err == nil {
			t.Fatalf("%s: missing scope should error", name)
		}
	}
}

// run_command 的工作目录必须是 scope.Workspace。
func TestRunCommandRunsInWorkspace(t *testing.T) {
	ws := t.TempDir()
	out, err := RunCommand{}.Run(scopedCtx(ws), json.RawMessage(`{"command":"pwd"}`))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(out)
	if resolved, _ := filepath.EvalSymlinks(got); resolved != ws && got != ws {
		// macOS /tmp 常是符号链接，容忍解析后等价的路径。
		if realWS, _ := filepath.EvalSymlinks(ws); resolved != realWS {
			t.Fatalf("pwd = %q, want workspace %q", got, ws)
		}
	}
}
