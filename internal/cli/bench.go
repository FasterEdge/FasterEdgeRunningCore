// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

package cli

// ferc bench：把装载实验跑成固定列 CSV。
//
// 论文要的是「形态 × 网络」矩阵的成功率与耗时，而"装一次看看"不构成实验。
// 度量在 internal/bench（只计时、记成败、记失败阶段）；真实装载由本文件的
// realBenchRunner 用 internal/install 跑完整流程。

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/bench"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/catalog"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/fetch"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/install"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/mirror"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/platform"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/resolve"
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
	var realLoad bool
	fs.StringVar(&out, "out", "", "CSV 输出路径；为空只打 stdout")
	fs.StringVar(&component, "component", "", "针对哪个组件跑（默认取清单里第一个）")
	// --real 才真装载：默认只度量骨架（elapsed_ms 全 0），因为真装载要下载、
	// 要注册服务、要起进程，在没准备好的环境上跑会留下一堆现场状态。
	fs.BoolVar(&realLoad, "real", false, "真装载（走完整 install 流程，耗时与失败阶段为实测）")
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
	//
	// 默认走度量骨架，加 --real 才真装载：真装载要下载、注册服务、起进程，
	// 在没准备好的环境上跑会留下一堆现场状态，不该是默认行为。
	runner := benchRunner(info, action == "faults")
	if realLoad {
		runner = realBenchRunner(env, info, comp, action == "faults")
		fmt.Fprintf(env.stderr(), "真实装载模式：走完整 install 流程，耗时与失败阶段为实测\n")
	} else {
		fmt.Fprintf(env.stderr(), "度量骨架模式（未加 --real）：elapsed_ms 全为 0，只验证矩阵可跑\n")
	}

	switch action {
	case "matrix":
		fmt.Fprintf(env.stderr(), "装载实验矩阵（组件 %s，目标 %s/%s）\n", comp.Name, info.GOOS, info.Arch)
		rows, err = bench.Run(ctx, runner)
	case "faults":
		fmt.Fprintf(env.stderr(), "故障注入实验（组件 %s，目标 %s/%s）\n", comp.Name, info.GOOS, info.Arch)
		rows, err = bench.Run(ctx, runner)
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

// realBenchRunner 用真实装载流程跑一个「形态 × 网络」组合。
//
// 与度量骨架的区别：这里真的走 install.New + resolve + Apply，因此
// elapsed_ms 是真实耗时、失败阶段是真实卡住的地方。论文实验一的数据来自它。
//
// 每个组合用独立的临时数据根目录：装载有副作用（写 bin/、注册服务、起进程），
// 复用同一个 root 会让后一个组合看到前一个的残留状态，"成功率"就不再是
// 这个组合自己的成绩。
func realBenchRunner(env Env, info platform.Info, comp *catalog.Component, faults bool) bench.Runner {
	return func(ctx context.Context, form bench.Form, network bench.Network) (bench.Stage, string, error) {
		if err := ctx.Err(); err != nil {
			return "", "", err
		}
		// 检查顺序刻意按"前提的强弱"排，越基础的越先判：
		//   网络 > 形态兼容 > 组件与实验形态是否对得上
		// 网络在最前是因为它是物理前提：介质不在场时，讨论"这个组件适配吗"
		// 没有意义，先答"能不能取到包"才是现场最想知道的。
		src := benchSource(network)
		if src == nil {
			return bench.StageDownload, "network-unavailable",
				fmt.Errorf("网络条件 %s 当前不可用", network)
		}
		// 形态与目标节点不兼容：这类组合在预检阶段就该失败。
		if why := nodeSupportsForm(info, form); why != "" {
			return bench.StagePrecheck, "form-unsupported", errors.New(why)
		}
		// 组件形态与实验矩阵的形态不一致时跳过：矩阵的 form 是"这次实验要
		// 验证哪种装载形态"，而 comp 是清单里某个具体组件，两者的 kind 未必
		// 对得上。对不上就明确报出来，不要默默跑一个不相干的组合。
		if !formMatchesComponent(form, comp) {
			return bench.StageResolve, "form-mismatch",
				fmt.Errorf("组件 %s 的形态 %s 与实验形态 %s 不一致", comp.Name, comp.Kind, form)
		}

		root, err := os.MkdirTemp("", "ferc-bench-")
		if err != nil {
			return bench.StageResolve, "tempdir", err
		}
		defer os.RemoveAll(root)

		plan, err := resolve.Resolve(mustCatalog(comp), resolve.Request{
			Want: []string{comp.Name},
			Info: info,
		})
		if err != nil {
			return bench.StageResolve, "resolve-failed", err
		}

		inst, err := install.New(install.Options{
			Root:   root,
			Info:   info,
			Source: src,
			Log:    logger{w: io.Discard},
			ArtifactCheck: install.ArtifactCheckOptions{
				// 实验要量的是"装载全流程"，预检是其中一环，跟着真实配置走。
				Enabled: !faults,
			},
		})
		if err != nil {
			return bench.StageResolve, "installer", err
		}

		recs, err := inst.Apply(ctx, plan)
		if err != nil {
			return stageOf(err), "apply-failed", err
		}
		if len(recs) == 0 {
			return bench.StageResolve, "empty-plan", errors.New("计划为空")
		}
		return "", "", nil
	}
}

// benchSource 按网络条件选源。
//
// online 走 GitHub；mirror/usb 需要外部提供镜像目录，bench 本身不猜路径，
// 因此未指定时返回 nil，由调用方按"该条件不可用"记账。
func benchSource(network bench.Network) fetch.Source {
	switch network {
	case bench.NetworkOnline:
		return fetch.New("")
	default:
		return nil
	}
}

// formMatchesComponent 判断组件形态是否与实验形态匹配。
func formMatchesComponent(form bench.Form, comp *catalog.Component) bool {
	switch form {
	case bench.FormBinary, bench.FormArchive:
		// 清单里 binary/archive 都表现为"解包即可执行文件"，归为一类。
		return comp.Kind == catalog.KindService || comp.Kind == catalog.KindTool
	case bench.FormKernelModule:
		return comp.Kind == catalog.KindService // 清单暂无独立 kernel-module kind
	default:
		return false
	}
}

// mustCatalog 取组件所在的清单；bench 只用内置清单。
func mustCatalog(comp *catalog.Component) *catalog.Catalog {
	cat, err := catalog.Builtin()
	if err != nil {
		return &catalog.Catalog{Components: []catalog.Component{*comp}}
	}
	return cat
}

// stageOf 从错误文本推断失败阶段。
//
// install 的错误不带结构化阶段，这里按关键词归类。归类不了就归到 install——
// 最保守的猜测（"就是装的时候坏的"），总比把失败算到更后面的阶段去误导
// 实验结论。
//
// 网络类关键词排在**最前**：现场最常见的一类失败就是取不到包（DNS 挂、
// 代理不通、介质不在场），而它们的错误文本长得五花八门（connectex、
// connection refused、no route to host、TLS handshake、timeout…）。不优先
// 识别的话，一次网络中断会被归成 resolve 或 install，阶段分布就失真了。
func stageOf(err error) bench.Stage {
	msg := strings.ToLower(err.Error())
	switch {
	case isNetworkErr(msg):
		return bench.StageDownload
	case strings.Contains(msg, "下载"), strings.Contains(msg, "download"):
		return bench.StageDownload
	case strings.Contains(msg, "校验"), strings.Contains(msg, "checksum"), strings.Contains(msg, "sha"):
		return bench.StageVerify
	case strings.Contains(msg, "解包"), strings.Contains(msg, "extract"):
		return bench.StageExtract
	case strings.Contains(msg, "预检"), strings.Contains(msg, "artifact"):
		return bench.StagePrecheck
	case strings.Contains(msg, "注册"), strings.Contains(msg, "register"), strings.Contains(msg, "托管"):
		return bench.StageRegister
	case strings.Contains(msg, "release"), strings.Contains(msg, "清单"):
		return bench.StageResolve
	default:
		return bench.StageInstall
	}
}

// isNetworkErr 识别"取不到包"一类的错误。
//
// 覆盖 Go 网络栈与常见代理/TLS 失败的英文措辞。中文错误（如"介质不在场"）
// 由调用方直接给出 StageDownload，不走这里。
func isNetworkErr(msg string) bool {
	for _, k := range []string{
		"connectex", "connection refused", "connection reset", "no route to host",
		"network is unreachable", "network unreachable", "i/o timeout", "timeout",
		"tls handshake", "handshake", "dial tcp", "no such host", "eof",
		"proxy", "unreachable", "medium not present", "介质不在场",
	} {
		if strings.Contains(msg, k) {
			return true
		}
	}
	return false
}

var _ = mirror.New
var _ = filepath.Join
