package cron

import (
	"os"
	"syscall"
)

// flock 封装（POSIX；与 bgtask 一致不覆盖 Windows 构建）。
// 调度属主锁与文件写互斥都建立在它上面：锁由内核持有，进程死亡
// （含 kill -9）自动释放，无需心跳或 stale 检测。

func flockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

func flockTryExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func flockUnlock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
