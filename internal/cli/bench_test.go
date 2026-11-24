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
	"os"
	"strings"
	"testing"

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
