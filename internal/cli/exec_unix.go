//go:build !windows

package cli

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// runHidden 执行一条外部命令并返回去掉首尾空白的标准输出。
//
// unix 上没有弹窗问题，但仍要收走标准输出：systemctl 之类的命令成功时也会
// 打印一行，混进 ferc 自己的输出里会让人分不清哪句是 ferc 说的。
func runHidden(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}
