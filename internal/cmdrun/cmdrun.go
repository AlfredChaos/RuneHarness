// Package cmdrun 是「带护栏的外部命令执行」共享层，pdfdoc 与 officedoc 共用：
//   - Runner 带并发闸（每个工具进程峰值 ~100MB，pod 内存有限）；
//   - 每条命令独立超时，stdout/stderr 各挂一个有界缓冲（子进程写多少都吞，
//     只保留前 N 字节，避免 PDF 全文或 stderr 噪音把内存撑爆）；
//   - 错误统一归类：缺工具 / 超时 / 调用方取消 / 非零退出（带 stderr 供上层分类）。
package cmdrun

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

var (
	// ErrToolsMissing：所需的外部二进制不在 PATH。
	ErrToolsMissing = errors.New("conversion tool not installed")
	// ErrTimeout：整条抽取超时。
	ErrTimeout = errors.New("extraction timed out")
)

const (
	cmdTimeout     = 20 * time.Second // 单条命令的硬上限；总超时由调用方 ctx 叠加
	maxStderrBytes = 8 << 10          // stderr 只留前 8KB：足够分类，又防刷屏
	maxStdoutBytes = 4 << 20          // stdout 4MB：够 80k 字符上限的若干倍
)

// RunFunc 执行外部命令的签名；可注入假实现做离线测试。
type RunFunc func(ctx context.Context, name string, args ...string) (stdout []byte, err error)

// Runner 持并发闸与执行器。
type Runner struct {
	run RunFunc
	sem chan struct{}
}

// New 返回用真实 exec 的 Runner，并发上限 cap。
func New(cap int) *Runner {
	return &Runner{run: Exec, sem: make(chan struct{}, cap)}
}

// NewWith 注入自定义执行器（测试用），并发上限 cap。
func NewWith(run RunFunc, cap int) *Runner {
	return &Runner{run: run, sem: make(chan struct{}, cap)}
}

// Run 排队取闸后执行一条命令。调用方 ctx 取消即不等闸。
func (r *Runner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return r.run(ctx, name, args...)
}

// CmdError 是外部命令的非零退出，带 stderr 供上层分类。
type CmdError struct {
	Name   string
	Code   int
	Stderr string
}

func (e *CmdError) Error() string {
	return fmt.Sprintf("%s exited %d: %s", e.Name, e.Code, strings.TrimSpace(e.Stderr))
}

// capBuffer 只保留前 max 字节、其余丢弃但照常吞掉（避免子进程写满管道阻塞）。
type capBuffer struct {
	buf []byte
	max int
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if room := c.max - len(c.buf); room > 0 {
		c.buf = append(c.buf, p[:min(len(p), room)]...)
	}
	return len(p), nil
}

// Exec 是真实执行路径：LookPath → cmdTimeout → 有界 stdout/stderr。
func Exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	if _, err := exec.LookPath(name); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrToolsMissing, name)
	}
	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, errb := &capBuffer{max: maxStdoutBytes}, &capBuffer{max: maxStderrBytes}
	cmd.Stdout, cmd.Stderr = out, errb
	cmd.WaitDelay = 2 * time.Second // ctx 取消后给管道收尾的时间，避免孙进程占着管道卡死 Wait
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ctx.Err() == nil {
			return nil, &CmdError{Name: name, Code: ee.ExitCode(), Stderr: string(errb.buf)}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return out.buf, nil
}
