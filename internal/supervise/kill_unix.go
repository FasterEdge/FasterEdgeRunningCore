//go:build !windows

package supervise

import (
	"errors"
	"syscall"
	"time"
)

// killProcess 先发 SIGTERM 让进程自己收尾，超时再 SIGKILL。
//
// 自托管管理器不是本进程的子进程，没法 Wait，只能靠 signal 0 轮询它是否还在。
func killProcess(pid int) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
