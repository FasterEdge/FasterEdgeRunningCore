//go:build windows

package supervise

import (
	"os/exec"
	"syscall"
)

// startDetached 让子进程脱离父进程的控制台，并隐藏窗口。
//
// CREATE_NO_WINDOW 是必须的：边缘现场常用“双击 exe”或任务计划的无窗口上下文
// 运行，若继承父控制台，会弹出黑色 PowerShell/控制台窗口打断操作者。
// CREATE_NEW_PROCESS_GROUP 让子进程不受父进程的 Ctrl+C 影响。
func startDetached(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | 0x08000000, // 0x08000000 = CREATE_NO_WINDOW
		HideWindow:    true,
	}
	return cmd.Start()
}
