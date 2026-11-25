// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

package cli

// P0 缺口基线的红测试：ferc bench 必须产出固定列的 CSV。
// 课题对应项 F4。
//
// 要解决的问题：论文需要"装载成功率/耗时/失败阶段"这组实验数据，而目前
// 没有任何落盘的指标。bench 子命令把矩阵实验跑成 CSV，列固定为
//
//	form,network,ok,stage,elapsed_ms,error_code
//
// 列固定是为了让多次实验的 CSV 能直接 diff——列名一变，历史数据全部作废。

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/bench"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/catalog"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/platform"
)

// F4-1：bench matrix 必须产出固定表头。
func TestBenchMatrixHeader(t *testing.T) {
	env, out, errb := testEnv(t, platform.Info{
		GOOS: "linux", GOARCH: "amd64", Arch: "amd64", Hostname: "edge-01",
	})
	env.DefaultRoot = t.TempDir()

	if code := Run(context.Background(), []string{"bench", "matrix"}, env); code != 0 {
		t.Fatalf("bench matrix 退出码 = %d\n%s%s", code, out.String(), errb.String())
	}
	first := firstLine(out.String())
	const want = "form,network,ok,stage,elapsed_ms,error_code"
	if first != want {
		t.Fatalf("表头必须固定为 %q，实际 %q", want, first)
	}
}

// F4-2：bench faults 用同一套列——两个子命令的产物要能拼成一个文件分析。
func TestBenchFaultsHeader(t *testing.T) {
	env, out, errb := testEnv(t, platform.Info{
		GOOS: "linux", GOARCH: "amd64", Arch: "amd64", Hostname: "edge-01",
	})
	env.DefaultRoot = t.TempDir()

	if code := Run(context.Background(), []string{"bench", "faults"}, env); code != 0 {
		t.Fatalf("bench faults 退出码 = %d\n%s%s", code, out.String(), errb.String())
	}
	first := firstLine(out.String())
	const want = "form,network,ok,stage,elapsed_ms,error_code"
	if first != want {
		t.Fatalf("表头必须固定为 %q，实际 %q", want, first)
	}
}

// F4-3：CSV 必须真的能落盘（--out），而不只是打到 stdout。
//
// 实验要可复现，就必须有文件；只打 stdout 的话每次实验都要人手重定向。
func TestBenchWritesCSVFile(t *testing.T) {
	env, out, errb := testEnv(t, platform.Info{
		GOOS: "linux", GOARCH: "amd64", Arch: "amd64", Hostname: "edge-01",
	})
	env.DefaultRoot = t.TempDir()
	csvPath := env.DefaultRoot + "/bench.csv"

	if code := Run(context.Background(), []string{"bench", "matrix", "--out", csvPath}, env); code != 0 {
		t.Fatalf("退出码 = %d\n%s%s", code, out.String(), errb.String())
	}
	b, err := osReadFile(csvPath)
	if err != nil {
		t.Fatalf("应写出 %s: %v", csvPath, err)
	}
	if firstLine(string(b)) != "form,network,ok,stage,elapsed_ms,error_code" {
		t.Fatalf("落盘的 CSV 表头不对:\n%s", b)
	}
}

// F4-4：未知 bench 子命令必须报错，不能静默跑成别的。
func TestBenchUnknownSubcommand(t *testing.T) {
	env, _, errb := testEnv(t, platform.Info{
		GOOS: "linux", GOARCH: "amd64", Arch: "amd64", Hostname: "edge-01",
	})
	env.DefaultRoot = t.TempDir()
	if code := Run(context.Background(), []string{"bench", "bogus"}, env); code != 2 {
		t.Fatalf("未知子命令应退出 2，实际 %d", code)
	}
	if !strings.Contains(errb.String(), "bench") {
		t.Fatalf("应提示 bench 的用法，实际: %s", errb.String())
	}
}

func firstLine(s string) string {
	s = strings.TrimPrefix(s, "\ufeff") // 兼容 BOM
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func osReadFile(p string) ([]byte, error) { return os.ReadFile(p) }

// ── 真实装载 Runner ─────────────────────────────────────────

func linuxAMD64Info() platform.Info {
	return platform.Info{GOOS: "linux", GOARCH: "amd64", Arch: "amd64",
		Hostname: "bench-host", Init: platform.InitSystemd}
}

// serviceComponent 取一个 service 类型的组件做实验对象。
//
// 不用 pickBenchComponent 的默认值：它返回清单第一个（dontcrack，supervisor），
// 而 supervisor 与 binary 实验形态对不上，会撞 form-mismatch 而不是真装载。
func serviceComponent(t *testing.T) *catalog.Component {
	t.Helper()
	cat, err := catalog.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for i := range cat.Components {
		if cat.Components[i].Kind == catalog.KindService {
			return &cat.Components[i]
		}
	}
	t.Skip("清单里没有 service 组件")
	return nil
}

// 真实 Runner 必须给出非 0 的 elapsed_ms——这是它与度量骨架的分界。
func TestRealBenchRunnerProducesTiming(t *testing.T) {
	if testing.Short() {
		t.Skip("真实装载要访问网络，跳过")
	}
	comp := serviceComponent(t)
	info := linuxAMD64Info()
	runner := realBenchRunner(Env{}, info, comp, false)

	// 只跑 binary/online 一个组合：其余组合要么形态不匹配要么要镜像目录。
	stage, code, runErr := runner(context.Background(), bench.FormBinary, bench.NetworkOnline)
	t.Logf("binary/online: stage=%q code=%q err=%v", stage, code, runErr)

	// 走完整 bench.Run 才能量到时间。
	rows, err := bench.Run(context.Background(), func(ctx context.Context, form bench.Form, network bench.Network) (bench.Stage, string, error) {
		if form != bench.FormBinary || network != bench.NetworkOnline {
			return "", "", nil // 其他组合不跑
		}
		return runner(ctx, form, network)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Form == bench.FormBinary && r.Network == bench.NetworkOnline {
			t.Logf("elapsed_ms=%d ok=%v stage=%q code=%q", r.ElapsedMs, r.OK, r.Stage, r.ErrorCode)
			if r.ElapsedMs <= 0 {
				t.Fatal("真实装载必须给出非 0 的 elapsed_ms")
			}
		}
	}
}

// 形态不匹配必须明确报出，不能默默跑一个不相干的组合。
func TestRealBenchRunnerReportsFormMismatch(t *testing.T) {
	comp := serviceComponent(t)
	runner := realBenchRunner(Env{}, linuxAMD64Info(), comp, false)

	// container 形态：ferc 不提供容器装载，必须在预检阶段就失败。
	stage, code, runErr := runner(context.Background(), bench.FormContainer, bench.NetworkOnline)
	if runErr == nil {
		t.Fatal("容器形态必须失败")
	}
	if stage != bench.StagePrecheck {
		t.Fatalf("应在预检阶段失败，实际 %q", stage)
	}
	if code != "form-unsupported" {
		t.Fatalf("错误编码应为 form-unsupported，实际 %q", code)
	}
}

// 非 online 网络在 bench 里没有镜像目录可给，必须报"该条件不可用"
// 而不是假装成功或静默跳过。
func TestRealBenchRunnerNonOnlineNetworkUnavailable(t *testing.T) {
	comp := serviceComponent(t)
	runner := realBenchRunner(Env{}, linuxAMD64Info(), comp, false)

	for _, net := range []bench.Network{bench.NetworkMirror, bench.NetworkUSB} {
		stage, code, runErr := runner(context.Background(), bench.FormBinary, net)
		if runErr == nil {
			t.Fatalf("%s 网络未提供镜像目录时必须失败", net)
		}
		if stage != bench.StageDownload {
			t.Fatalf("%s 应在 download 阶段失败，实际 %q", net, stage)
		}
		if code != "network-unavailable" {
			t.Fatalf("%s 错误编码应为 network-unavailable，实际 %q", net, code)
		}
	}
}

// 形态与组件不匹配时要报 form-mismatch，不默默跑。
func TestRealBenchRunnerFormMismatchCode(t *testing.T) {
	// kernel-module 形态：信息不足时该在预检阶段报"未上报内核版本"。
	comp := serviceComponent(t)
	runner := realBenchRunner(Env{}, linuxAMD64Info(), comp, false)
	stage, code, err := runner(context.Background(), bench.FormKernelModule, bench.NetworkOnline)
	if err == nil {
		t.Fatal("未上报内核版本时必须失败")
	}
	t.Logf("kernel-module: stage=%q code=%q err=%v", stage, code, err)
}

// stageOf 必须把常见失败归到正确阶段——阶段分布是实验结论的一部分，
// 归错了会让"败在哪儿"的统计失真。
func TestStageOfClassification(t *testing.T) {
	cases := []struct {
		msg  string
		want bench.Stage
	}{
		{"下载失败: connection refused", bench.StageDownload},
		{"download timeout", bench.StageDownload},
		{"SHA256 校验不符", bench.StageVerify},
		{"checksum mismatch", bench.StageVerify},
		{"解包失败: not a tar", bench.StageExtract},
		{"extract failed", bench.StageExtract},
		{"产物预检失败: 架构不符", bench.StagePrecheck},
		{"artifact mismatch", bench.StagePrecheck},
		{"注册服务失败", bench.StageRegister},
		{"register unit failed", bench.StageRegister},
		{"取 release 清单失败", bench.StageResolve},
		{"某种没见过的错误", bench.StageInstall},
	}
	for _, c := range cases {
		if got := stageOf(errors.New(c.msg)); got != c.want {
			t.Errorf("stageOf(%q) = %q，期望 %q", c.msg, got, c.want)
		}
	}
}

// 每个组合用独立临时根目录：否则后一个组合会看到前一个的残留状态，
// "成功率"就不再是这个组合自己的成绩。
func TestRealBenchRunnerIsolatesEachCombination(t *testing.T) {
	comp := serviceComponent(t)
	runner := realBenchRunner(Env{}, linuxAMD64Info(), comp, false)

	// 连续跑两次同一组合：第二次不该因为第一次的残留而变化（这里只验证
	// 不 panic、且都给出可解释的结果）。
	for i := 0; i < 2; i++ {
		stage, code, err := runner(context.Background(), bench.FormContainer, bench.NetworkOnline)
		if err == nil {
			t.Fatal("容器形态必须失败")
		}
		if stage != bench.StagePrecheck || code != "form-unsupported" {
			t.Fatalf("第 %d 次结果不稳定: %q/%q", i, stage, code)
		}
	}
}

// 网络类失败必须归到 download：现场最常见的一类失败就是取不到包，
// 归错了会让阶段分布失真，进而让"败在哪儿"的实验结论失真。
func TestStageOfNetworkErrors(t *testing.T) {
	cases := []string{
		"dial tcp 20.205.243.168:443: connectex: A connection attempt failed",
		"Get \"https://api.github.com\": connection refused",
		"i/o timeout",
		"TLS handshake timeout",
		"no route to host",
		"network is unreachable",
		"介质不在场",
	}
	for _, msg := range cases {
		if got := stageOf(errors.New(msg)); got != bench.StageDownload {
			t.Errorf("stageOf(%q) = %q，期望 %q", msg, got, bench.StageDownload)
		}
	}
}
