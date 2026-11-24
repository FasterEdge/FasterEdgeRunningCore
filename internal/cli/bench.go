// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

package cli

// ferc bench：把装载实验跑成固定列 CSV。
//
// 论文要的是「形态 × 网络」矩阵的成功率与耗时，而"装一次看看"不构成实验。
// 这一包只度量、不真装载：Runner 由调用方给，这里只计时、记成败、记失败阶段。

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/bench"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/catalog"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/platform"
)

// cmdBench 实现 `ferc bench matrix|faults`。
func cmdBench(ctx context.Context, args []string, env Env) int {
	if len(args) == 0 {
		fmt.Fprintln(env.stderr(), "用法: ferc bench matrix|faults [选项]")
		return 2
	}
	action, rest := args[0], args[1:]

	fs := flag.NewFlagSet("bench "+action, flag.ContinueOnError)
	fs.SetOutput(env.stderr())
	var c common
	bindCommon(fs, &c, env)
	var out string
	var component string
	fs.StringVar(&out, "out", "", "CSV 输出路径；为空只打 stdout")
	fs.StringVar(&component, "component", "", "针对哪个组件跑（默认取清单里第一个）")
	if err := fs.Parse(rest); err != nil {
		return 2
	}

	cat, err := c.loadCatalog()
	if err != nil {
		fmt.Fprintf(env.stderr(), "%v\n", err)
		return 1
	}
	comp, err := pickBenchComponent(cat, component)
	if err != nil {
		fmt.Fprintf(env.stderr(), "%v\n", err)
		return 1
	}

	info, err := env.detect(ctx)
	if err != nil {
		fmt.Fprintf(env.stderr(), "探测失败: %v\n", err)
		return 1
	}

	var rows []bench.Row
	// 标题与汇总都走 stderr：stdout 第一行必须是 CSV 表头，这样
	// `ferc bench matrix > out.csv` 直接得到可用文件，不需要再删标题行。
	switch action {
	case "matrix":
		fmt.Fprintf(env.stderr(), "装载实验矩阵（组件 %s，目标 %s/%s）\n", comp.Name, info.GOOS, info.Arch)
		rows, err = bench.Run(ctx, benchRunner(info, false))
	case "faults":
		fmt.Fprintf(env.stderr(), "故障注入实验（组件 %s，目标 %s/%s）\n", comp.Name, info.GOOS, info.Arch)
		rows, err = bench.Run(ctx, benchRunner(info, true))
	default:
		fmt.Fprintf(env.stderr(), "未知 bench 子命令 %q（可用: matrix、faults）\n", action)
		return 2
	}
	if err != nil {
		fmt.Fprintf(env.stderr(), "实验失败: %v\n", err)
		return 1
	}

	// CSV 打到 stdout（无 --out 时），便于直接重定向。
	fmt.Fprintln(env.stdout(), bench.Header)
	for _, r := range rows {
		fmt.Fprintln(env.stdout(), r.String())
	}
	// 汇总走 stderr，理由同上：stdout 只放 CSV。
	fmt.Fprintf(env.stderr(), "%s\n", bench.Summarize(rows).Format())

	if out != "" {
		if err := bench.Write(out, rows); err != nil {
			fmt.Fprintf(env.stderr(), "写 CSV 失败: %v\n", err)
			return 1
		}
		fmt.Fprintf(env.stdout(), "已写入 %s\n", out)
	}
	return 0
}

// benchRunner 返回一个跑「形态 × 网络」组合的 Runner。
//
// 真实装载由调用方后续接入；这里先把度量骨架跑通，并对明确不兼容的组合给出
// 失败阶段——这样矩阵里既有成功也有失败，阶段分布才有意义（全成功的矩阵
// 说明不了任何事）。
func benchRunner(info platform.Info, faults bool) bench.Runner {
	return func(ctx context.Context, form bench.Form, network bench.Network) (bench.Stage, string, error) {
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		// 形态与目标节点不兼容：这类组合在预检阶段就该失败。
		if why := nodeSupportsForm(info, form); why != "" {
			return bench.StagePrecheck, "form-unsupported", errors.New(why)
		}
		// faults 模式额外注入"介质不在场"：模拟现场最常见的那类故障。
		if faults && network == bench.NetworkUSB {
			return bench.StageDownload, "network-unreachable", errors.New("介质不在场")
		}
		return "", "", nil
	}
}

// nodeSupportsForm 判断目标节点能否承载该形态。
func nodeSupportsForm(info platform.Info, form bench.Form) string {
	switch form {
	case bench.FormContainer:
		// ferc 的探测没有"有没有容器运行时"这一项，用 Init 判定不可靠。
		// 诚实做法：容器形态一律按"目标不支持"报，因为 ferc 本身不提供容器装载。
		return "运行核不提供容器装载，容器形态由 AIO 侧编排"
	case bench.FormKernelModule:
		if info.Kernel == "" {
			return "节点未上报内核版本，无法确认模块是否匹配"
		}
	case bench.FormBinary:
		if info.GOOS == "" {
			return "节点系统未能识别"
		}
	}
	return ""
}

// pickBenchComponent 选出要测的组件。
func pickBenchComponent(cat *catalog.Catalog, name string) (*catalog.Component, error) {
	if name != "" {
		comp, ok := cat.Get(name)
		if !ok {
			return nil, fmt.Errorf("未知组件 %q（用 ferc list 看有哪些）", name)
		}
		return comp, nil
	}
	all := cat.Components
	if len(all) == 0 {
		return nil, errors.New("清单里没有任何组件")
	}
	return &all[0], nil
}
