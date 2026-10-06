package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"runeharness/internal/scope"
	"time"
)

// RunCommand 通过 sh -c 执行 shell 命令，返回合并的 stdout+stderr。
// 注意：这赋予模型在本机执行任意命令的能力，不要对不可信输入开放。
type RunCommand struct{}

const (
	// defaultCmdTimeout 是未指定 timeout_seconds 时的命令超时。
	defaultCmdTimeout = 30 * time.Second
	// maxCmdTimeout 是 timeout_seconds 的上限，防模型给出失控的大值。
	maxCmdTimeout = 10 * time.Minute
)

func (RunCommand) Spec() Spec {
	return Spec{
		Name: "run_command",
		Description: "Run a shell command via sh -c and return combined stdout+stderr. " +
			"Optional timeout_seconds bounds execution (default 30, max 600) — " +
			"raise it for builds/tests and other long-running commands",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":         map[string]any{"type": "string"},
				"timeout_seconds": map[string]any{"type": "integer", "description": "seconds before the command is killed; default 30, capped at 600"},
			},
			"required": []string{"command"},
		},
	}
}

func (RunCommand) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := unmarshalArgs(raw, &args); err != nil {
		return "", err
	}
	if args.Command == "" {
		return "", errors.New("command is required")
	}
	// 命令落在租户工作区：scope 缺失即拒绝（不信任裸跑）。
	// 注意 Dir 只定工作目录，cd/绝对路径仍可逃逸——云端真隔离靠部署沙箱。
	sc, err := scope.FromContext(ctx)
	if err != nil {
		return "", err
	}
	if sc.Workspace == "" {
		return "", errors.New("scope: empty workspace")
	}
	timeout := defaultCmdTimeout
	if args.TimeoutSeconds > 0 {
		timeout = min(time.Duration(args.TimeoutSeconds)*time.Second, maxCmdTimeout)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", args.Command)
	cmd.Dir = sc.Workspace
	out, err := cmd.CombinedOutput()
	// 不在工具侧截断：超限输出由回填层落 blob 换指针（plan §4.2）。
	res := string(out)
	if err != nil {
		res += "\nexit error: " + err.Error()
	}
	return res, nil
}
