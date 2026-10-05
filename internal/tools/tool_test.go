package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type okTool struct{}

func (okTool) Spec() Spec { return Spec{Name: "ok"} }
func (okTool) Run(context.Context, json.RawMessage) (string, error) {
	return "fine", nil
}

type failTool struct{}

func (failTool) Spec() Spec { return Spec{Name: "fail"} }
func (failTool) Run(context.Context, json.RawMessage) (string, error) {
	return "", errors.New("nope")
}

type panicTool struct{}

func (panicTool) Spec() Spec { return Spec{Name: "panics"} }
func (panicTool) Run(context.Context, json.RawMessage) (string, error) {
	panic("kaboom")
}

// Call 必须把成功与失败区分开：IsError 是给 hook/UI 的结构化标记，
// "error:" 前缀是给模型的文本约定，两者要同时成立。
func TestCallMarksIsError(t *testing.T) {
	reg := NewRegistry(okTool{}, failTool{}, panicTool{})
	cases := []struct {
		name   string
		isErr  bool
		prefix bool
	}{
		{"ok", false, false},
		{"fail", true, true},
		{"panics", true, true},
		{"missing", true, true},
	}
	for _, c := range cases {
		res := reg.Call(context.Background(), c.name, json.RawMessage(`{}`))
		if res.IsError != c.isErr {
			t.Fatalf("Call(%q).IsError = %v, want %v", c.name, res.IsError, c.isErr)
		}
		if strings.HasPrefix(res.Output, "error:") != c.prefix {
			t.Fatalf("Call(%q).Output = %q, error-prefix mismatch", c.name, res.Output)
		}
	}
}

// timeout_seconds 应让长命令在指定时间被 kill。
func TestRunCommandTimeoutSeconds(t *testing.T) {
	start := time.Now()
	out, err := RunCommand{}.Run(scopedCtx(t.TempDir()),
		json.RawMessage(`{"command":"sleep 5","timeout_seconds":1}`))
	if err != nil {
		t.Fatalf("Run error = %v（超时是输出里的 exit error，不是 Go error）", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("took %v, timeout_seconds=1 not applied", elapsed)
	}
	if !strings.Contains(out, "exit error") {
		t.Fatalf("out = %q, want killed-command exit error", out)
	}
}

// 省略与越界的 timeout_seconds 都回落到默认值/上限，命令正常跑完。
func TestRunCommandTimeoutFallback(t *testing.T) {
	for _, raw := range []string{
		`{"command":"echo hi"}`,
		`{"command":"echo hi","timeout_seconds":0}`,
		`{"command":"echo hi","timeout_seconds":9999}`,
	} {
		out, err := RunCommand{}.Run(scopedCtx(t.TempDir()), json.RawMessage(raw))
		if err != nil {
			t.Fatalf("Run(%s) error = %v", raw, err)
		}
		if !strings.Contains(out, "hi") {
			t.Fatalf("Run(%s) out = %q, want 'hi'", raw, out)
		}
	}
}
