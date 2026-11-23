//go:build windows

package supervise

import (
	"errors"
	"syscall"
)

const (
	processTerminate = 0x0001
	processSynch     = 0x00100000
	// 结束进程后等它真正退出的上限：Windows 的 TerminateProcess 是异步的，
	// 不等就可能出现“进程已死但可执行文件还被占着”，回滚删文件会 Access denied。
	waitExitMillis = 5000
	// errInvalidParameter 是 OpenProcess 拿到不存在的 PID 时的错误码。
	errInvalidParameter = syscall.Errno(87)
)

// killProcess 用 TerminateProcess 结束进程，并等它真正退出。
//
// Windows 没有 SIGTERM 这种“请你收尾”的软信号，os.Interrupt 只在同控制台组内
// 有效，而这些管理器是我们派生出去的独立进程组，收不到。因此这里直接强杀：
// 卸载场景本来就要把它彻底清掉，而不是等它优雅退出。
func killProcess(pid int) error {
	if pid <= 0 {
		return nil
	}
	h, err := syscall.OpenProcess(processTerminate|processSynch, false, uint32(pid))
	if err != nil {
		// 进程已经不在了就是“已经干净”，不是失败。
		if errors.Is(err, errInvalidParameter) {
			return nil
		}
		return err
	}
	defer syscall.CloseHandle(h)
	if err := syscall.TerminateProcess(h, 1); err != nil {
		return err
	}
	// 等进程对象变成已信号状态：此时句柄（含映像文件）都已释放。
	if _, err := syscall.WaitForSingleObject(h, waitExitMillis); err != nil {
		return err
	}
	return nil
}
