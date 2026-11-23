// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

// ferc 是 FasterEdge 运行核的命令行入口。
//
// 设计要点：它不做常驻进程。每次执行完成一件具体的事（探测平台、装载组件、
// 查看状态、卸载组件）就退出，节点后续由系统 init 或已装载的进程管理器托管。
// 这样“一行安装指令”之后就不再有任何本工具自身的守护负担。
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(cli.Run(ctx, os.Args[1:], cli.Env{}))
}
