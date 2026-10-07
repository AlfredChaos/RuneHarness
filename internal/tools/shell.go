package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"runeharness/internal/scope"
)

// BGSpawner 抽象后台任务的启动能力（internal/bgtask.Manager 实现）。
// 接口声明在消费方一侧，避免 tools→bgtask 与 bgtask→tools 的循环依赖。
type BGSpawner interface {
	Spawn(ctx context.Context, in BGSpawn) (BGTask, error)
}

// BGSpawn 是一次后台启动请求。
type BGSpawn struct {
	Command     string
	Description string        // 通知与列表展示用短描述；空则取命令首部
	Timeout     time.Duration // 单次运行上限；<=0 由实现方取默认值
}

// BGTask 是启动成功后的任务句柄摘要。
type BGTask struct {
	ID         string
	OutputPath string
}

// RunCommand 通过 sh -c 执行 shell 命令，返回合并的 stdout+stderr。
// 注意：这赋予模型在本机执行任意命令的能力，不要对不可信输入开放。
//
// run_in_background=true 时改走 BG 后台通道：进程组独立、输出落盘、
// 立即返回任务 id，完成事件以 <task_notification> 形式进入对话。
type RunCommand struct {
	BG BGSpawner // nil → 不支持 run_in_background
}

const (
	// defaultCmdTimeout 是未指定 timeout_seconds 时的命令超时。
	defaultCmdTimeout = 30 * time.Second
	// maxCmdTimeout 是 timeout_seconds 的上限，防模型给出失控的大值。
	maxCmdTimeout = 10 * time.Minute
	// bgCmdTimeout 是后台任务的默认与上限时长：后台进程无人看守，
	// 失控代价高于前台，故默认即上限（模型只能缩短不能放宽）。
	bgCmdTimeout = 30 * time.Minute
)

func (RunCommand) Spec() Spec {
	return Spec{
		Name: "run_command",
		Description: "Run a shell command via sh -c and return combined stdout+stderr. " +
			"Optional timeout_seconds bounds execution (default 30, max 600; background tasks " +
			"default/max 1800). For long-running commands set run_in_background=true to return " +
			"immediately with a task id — completion arrives later as a <task_notification> message. " +
			"Use run_in_background only when you don't need the result right away.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":         map[string]any{"type": "string"},
				"timeout_seconds": map[string]any{"type": "integer", "description": "seconds before the command is killed; default 30, capped at 600 (1800 for background)"},
				"run_in_background": map[string]any{"type": "boolean",
					"description": "run detached in the background; returns a task id instead of waiting. Do not poll for the result — you will be notified"},
				"description": map[string]any{"type": "string",
					"description": "short label describing a background command (shown in notifications); defaults to the command"},
			},
			"required": []string{"command"},
		},
	}
}

func (r RunCommand) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		Command         string `json:"command"`
		TimeoutSeconds  int    `json:"timeout_seconds"`
		RunInBackground bool   `json:"run_in_background"`
		Description     string `json:"description"`
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
	if args.RunInBackground {
		if r.BG == nil {
			return "", errors.New("run_in_background is not configured")
		}
		timeout := bgCmdTimeout
		if args.TimeoutSeconds > 0 {
			timeout = min(time.Duration(args.TimeoutSeconds)*time.Second, bgCmdTimeout)
		}
		bt, err := r.BG.Spawn(ctx, BGSpawn{
			Command: args.Command, Description: args.Description, Timeout: timeout,
		})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Command running in background with ID: %s.\n"+
			"Output is being written to: %s\n"+
			"You will be notified when it completes — do not poll. "+
			"Use read_file on the output path to check progress, "+
			"task_list to list tasks, task_kill to stop it.", bt.ID, bt.OutputPath), nil
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
