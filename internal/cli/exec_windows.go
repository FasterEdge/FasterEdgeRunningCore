//go:build windows

package cli

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
)

// runHidden 执行一条外部命令并返回去掉首尾空白的标准输出。
//
// CREATE_NO_WINDOW 与 HideWindow 是必须的：边缘现场常用“双击 exe”或在计划
// 任务的无控制台上下文里运行，若子进程继承父控制台，schtasks / systemctl
// 这类命令会闪出一个黑色控制台窗口打断操作者。处理方式与
// internal/supervise 启动进程管理器时完全一致。
func runHidden(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | 0x08000000, // 0x08000000 = CREATE_NO_WINDOW
		HideWindow:    true,
	}
	out, err := cmd.Output()
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}
