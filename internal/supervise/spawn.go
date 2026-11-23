package supervise

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// spawnDetached 启动一个长期运行的后台进程并立即返回它的进程号。
//
// 与 Exec 的区别是“不等它退出”：进程管理器（DontCrack）本来就永不退出，
// 用 Exec 会把装载流程卡到超时。这里把 stdout/stderr 重定向到日志文件，
// 并给出一段短暂的存活观察窗口——如果进程立刻退出，就说明参数错了或端口
// 被占用，应当报错而不是默默留下一个死掉的服务。
func spawnDetached(name string, args []string, logPath string) (int, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return 0, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(name, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := startDetached(cmd); err != nil {
		logFile.Close()
		return 0, err
	}
	pid := cmd.Process.Pid
	// 父进程不再持有子进程的句柄，日志文件可由子进程独占。
	logFile.Close()

	// 观察窗口：进程若在 500ms 内就退出，多半是参数错误或端口冲突。
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case err := <-waitCh:
		tail := tailFile(logPath, 800)
		if err != nil {
			return 0, fmt.Errorf("%s 启动后立即退出: %w；日志尾部: %s", filepath.Base(name), err, tail)
		}
		return 0, fmt.Errorf("%s 启动后立即正常退出，不符合常驻预期；日志尾部: %s", filepath.Base(name), tail)
	case <-time.After(500 * time.Millisecond):
		return pid, nil
	}
}

// tailFile 取文件末尾若干字节，用于把启动失败原因直接摆在报错里，
// 免得还要人再去翻日志。
func tailFile(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return "（无法读取日志: " + err.Error() + "）"
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "（无法读取日志大小）"
	}
	size := fi.Size()
	offset := size - n
	if offset < 0 {
		offset = 0
	}
	buf := make([]byte, size-offset)
	if _, err := f.ReadAt(buf, offset); err != nil && err.Error() != "EOF" {
		return "（读取日志失败: " + err.Error() + "）"
	}
	s := string(buf)
	if len(s) == 0 {
		return "（空）"
	}
	return s
}
