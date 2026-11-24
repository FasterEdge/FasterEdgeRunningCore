// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

//go:build linux

package install

// P0 缺口基线的红测试：运行核必须非常驻。
// 课题对应项 F3。
//
// 这是本项目的立论之本：ferc 装完就退出，节点由系统 init 或已装载的进程
// 管理器托管。如果 install 返回后 ferc 自己还赖在进程表里，那"单点式部署
// 工具"就退化成了又一个守护进程——后续 Agent 接管时面对的会是一个说不清
// 谁在管谁的进程树。
//
// 用 //go:build linux 限定：课题要求"枚举 /proc/*/comm"，这是 Linux 特有的
// 判定方式。在别的平台上只能跳过，而跳过不能冒充通过，因此让测试根本不在
// 那些平台上编译。

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/platform"
)

// fercComms 返回当前进程表里所有 /proc/<pid>/comm 的值。
func fercComms(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Skipf("读 /proc 失败，跳过: %v", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "comm"))
		if err != nil {
			continue
		}
		out = append(out, strings.TrimSpace(string(b)))
	}
	return out
}

// F3-1：install 返回后，进程表里不得有 ferc。
//
// 注意测试进程自身的名字可能是 ferc.test——那不算。要排除测试二进制本身，
// 但被测代码派生的任何名为 ferc 的进程都算违规。
func TestRuncoreIsNotResident(t *testing.T) {
	root := t.TempDir()
	inst, err := New(Options{
		Root:   root,
		Info:   platform.Info{GOOS: "linux", GOARCH: "amd64", Arch: "amd64", Hostname: "edge-01"},
		Source: nil,
		Log:    nopLogger{},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 用一个空计划跑完整个 Apply 流程：装载逻辑必须自己返回，
	// 不能留下后台 goroutine 常驻。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := inst.Apply(ctx, nil); err != nil {
		t.Fatalf("空计划应直接成功: %v", err)
	}

	// 给可能的延迟退出一点时间，但不等太久——真常驻的话等也没用。
	deadline := time.Now().Add(3 * time.Second)
	for {
		comms := fercComms(t)
		var stray []string
		for _, c := range comms {
			if c == "ferc" {
				stray = append(stray, c)
			}
		}
		if len(stray) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("install 返回后进程表里仍有 %d 个 ferc 进程——运行核必须非常驻", len(stray))
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// F3-2：被装载的服务在 install 返回后必须还在跑。
//
// 这一条是 F3-1 的对偶：不能为了"自己非常驻"就把人家要保活的服务也带走。
// 单测环境下没有真的 init 系统，因此这里断言的是"Apply 返回时记录里带上了
// 托管信息"——真正的进程存亡由 e2e 覆盖。
func TestInstalledServiceSurvivesInstallerExit(t *testing.T) {
	if testing.Short() {
		t.Skip("需要真实服务进程，跳过")
	}
	// 空实现：P1 补上真实断言。此处留 t.Skip 而不是空过，
	// 避免"仅导入不报错"冒充通过。
	t.Skip("P1 实现：需真实派生一个服务并断言 install 返回后它仍在 /proc 中")
}
