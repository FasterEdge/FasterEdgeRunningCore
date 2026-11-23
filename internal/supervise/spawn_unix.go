//go:build !windows

package supervise

import (
	"os/exec"
	"syscall"
)

// startDetached 让子进程脱离当前会话：Setsid 之后它不再是终端的前台进程组，
// 父进程退出（ferc 装完就退出）也不会连带杀掉它。
func startDetached(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}
