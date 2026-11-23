//go:build windows

package platform

import (
	"os"
	"path/filepath"
)

// isPrivileged 通过“能否以只读方式打开 SAM 数据库”判断当前进程是否具备管理员等价权限。
//
// 这里刻意不引入 golang.org/x/sys：运行核的定位是零外部依赖的自举工具，
// 必须在任何有 Go 环境（甚至只有预编译产物）的机器上都能构建与运行。
// SAM 对普通用户不可读、对管理员可读，是标准库就能问到的稳定探针，
// 且只做只读打开，不产生任何副作用。
func isPrivileged() bool {
	sysRoot := os.Getenv("SystemRoot")
	if sysRoot == "" {
		sysRoot = `C:\Windows`
	}
	f, err := os.Open(filepath.Join(sysRoot, "System32", "config", "SAM"))
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}
