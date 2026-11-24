// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

package bench

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHeaderIsFixed(t *testing.T) {
	const want = "form,network,ok,stage,elapsed_ms,error_code"
	if Header != want {
		t.Fatalf("表头必须固定，实际 %q", Header)
	}
}

// 矩阵必须覆盖 4 形态 × 3 网络 = 12 组合。漏一个，论文矩阵就有空格。
func TestMatrixCoversAllCombinations(t *testing.T) {
	m := Matrix()
	if len(m) != 12 {
		t.Fatalf("应有 12 个组合（4 形态 × 3 网络），实际 %d", len(m))
	}
	seen := map[string]bool{}
	for _, r := range m {
		seen[string(r.Form)+"/"+string(r.Network)] = true
	}
	for _, f := range []Form{FormBinary, FormContainer, FormKernelModule, FormArchive} {
		for _, n := range []Network{NetworkOnline, NetworkMirror, NetworkUSB} {
			k := string(f) + "/" + string(n)
			if !seen[k] {
				t.Fatalf("矩阵缺少组合 %s", k)
			}
		}
	}
}

// 全成功时 CSV 应含表头 + 12 行，且 ok 全为 true。
func TestRunAllSucceed(t *testing.T) {
	rows, err := Run(context.Background(), func(context.Context, Form, Network) (Stage, string, error) {
		return "", "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 12 {
		t.Fatalf("应跑 12 行，实际 %d", len(rows))
	}
	for _, r := range rows {
		if !r.OK {
			t.Fatalf("全部应成功: %+v", r)
		}
		if r.Stage != "" || r.ErrorCode != "" {
			t.Fatalf("成功时不该有 stage/code: %+v", r)
		}
	}
	s := Summarize(rows)
	if s.OK != 12 || s.Failed != 0 {
		t.Fatalf("汇总不对: %+v", s)
	}
}

// 失败必须带上阶段与错误编码——只说"失败了"无法回答"败在哪一步"。
func TestRunRecordsStageAndCode(t *testing.T) {
	rows, err := Run(context.Background(), func(_ context.Context, f Form, n Network) (Stage, string, error) {
		if f == FormContainer && n == NetworkUSB {
			return StagePrecheck, "arch-mismatch", context.DeadlineExceeded
		}
		return "", "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var failed Row
	for _, r := range rows {
		if !r.OK {
			failed = r
		}
	}
	if failed.Form != FormContainer || failed.Network != NetworkUSB {
		t.Fatalf("失败的应是 container/usb，实际 %+v", failed)
	}
	if failed.Stage != StagePrecheck {
		t.Fatalf("阶段应为 precheck，实际 %q", failed.Stage)
	}
	if failed.ErrorCode != "arch-mismatch" {
		t.Fatalf("错误编码应为 arch-mismatch，实际 %q", failed.ErrorCode)
	}
}

// Runner 不给 code 时兜底为 unknown，不给 stage 时兜底为 resolve——
// 不能让 CSV 里出现空单元格，那会让下游解析错位。
func TestRunBackfillsStageAndCode(t *testing.T) {
	rows, _ := Run(context.Background(), func(context.Context, Form, Network) (Stage, string, error) {
		return "", "", context.Canceled
	})
	for _, r := range rows {
		if r.OK {
			t.Fatal("应失败")
		}
		if r.Stage != StageResolve {
			t.Fatalf("缺 stage 应兜底为 resolve，实际 %q", r.Stage)
		}
		if r.ErrorCode != "unknown" {
			t.Fatalf("缺 code 应兜底为 unknown，实际 %q", r.ErrorCode)
		}
	}
}

func TestWriteProducesFixedHeader(t *testing.T) {
	rows, _ := Run(context.Background(), func(context.Context, Form, Network) (Stage, string, error) {
		return "", "", nil
	})
	p := filepath.Join(t.TempDir(), "bench.csv")
	if err := Write(p, rows); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) != 13 {
		t.Fatalf("应含表头 + 12 行，实际 %d 行", len(lines))
	}
	if lines[0] != Header {
		t.Fatalf("表头不对: %q", lines[0])
	}
	for _, l := range lines[1:] {
		if n := strings.Count(l, ","); n != 5 {
			t.Fatalf("每行应 6 列，实际 %d: %q", n+1, l)
		}
	}
}

func TestSummarizeCountsStages(t *testing.T) {
	rows := []Row{
		{Form: FormBinary, Network: NetworkOnline, OK: true, ElapsedMs: 100},
		{Form: FormBinary, Network: NetworkMirror, OK: true, ElapsedMs: 300},
		{Form: FormContainer, Network: NetworkUSB, Stage: StagePrecheck, ErrorCode: "x"},
		{Form: FormArchive, Network: NetworkUSB, Stage: StageDownload, ErrorCode: "y"},
	}
	s := Summarize(rows)
	if s.Total != 4 || s.OK != 2 || s.Failed != 2 {
		t.Fatalf("汇总不对: %+v", s)
	}
	if s.AvgMsOK != 200 {
		t.Fatalf("成功平均应为 200ms，实际 %d", s.AvgMsOK)
	}
	if s.ByStage[StagePrecheck] != 1 || s.ByStage[StageDownload] != 1 {
		t.Fatalf("阶段分布不对: %+v", s.ByStage)
	}
}

func TestRunWithoutRunner(t *testing.T) {
	if _, err := Run(context.Background(), nil); err == nil {
		t.Fatal("缺少 Runner 应报错")
	}
}
