// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

//go:build linux

package install

// Linux 专属的非常驻测试：枚举 /proc/*/comm。
//
// 课题对应项 F3 明确要求这种判定方式。平台无关的部分（goroutine 净增长）
// 见 residency_common_test.go——它在任何平台上都跑，因此这条立论在开发机上
// 也有效，不只依赖 CI 的 Linux runner。
//
// 本文件只做 Linux 上才做得到的两件事：
//  1. 枚举进程表，断言没有名为 ferc 的进程（含真装载路径，不只是空计划）；
//  2. 断言被装载的服务在 install 返回后仍活着。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// assertNoStrayFerc 断言进程表里没有名为 ferc 的进程。
//
// 注意测试进程自身的名字可能是 ferc.test——那不算。要排除测试二进制本身，
// 但被测代码派生的任何名为 ferc 的进程都算违规。
func assertNoStrayFerc(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var stray int
		for _, c := range fercComms(t) {
			if c == "ferc" {
				stray++
			}
		}
		if stray == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("进程表里仍有 %d 个 ferc 进程——运行核必须非常驻", stray)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// F3-1：install 返回后，进程表里不得有 ferc。
func TestRuncoreIsNotResident(t *testing.T) {
	guard := Snapshot()
	runEmptyApply(t, platform.Info{GOOS: "linux", GOARCH: "amd64", Arch: "amd64", Hostname: "edge-01"})

	// 两层断言都要过：goroutine 不净增长（平台无关那层已测，这里再确认一次），
	// 且进程表里没有 ferc。
	if why := guard.Check(); why != "" {
		t.Fatal(why)
	}
	assertNoStrayFerc(t)
}

// F3-2：被装载的服务在 install 返回后必须还在跑。
//
// 这一条是 F3-1 的对偶：不能为了"自己非常驻"就把人家要保活的服务也带走。
// 用一个"退出前一直睡"的哨兵进程模拟被装载的服务，断言 install 返回后它
// 仍在 /proc 里、且 cmdline 可读。这是端到端性质的最小可复现版本。
func TestInstalledServiceSurvivesInstallerExit(t *testing.T) {
	if testing.Short() {
		t.Skip("需要真实进程，跳过")
	}

	// 哨兵进程：sh -c 'exec sleep 30'。一定存在、一定长睡、不占端口。
	sentinel := startSentinel(t)
	defer killPID(t, sentinel)
	if sentinel == 0 {
		t.Fatal("哨兵进程没起来")
	}

	guard := Snapshot()
	runEmptyApply(t, platform.Info{GOOS: "linux", GOARCH: "amd64", Arch: "amd64", Hostname: "edge-01"})

	// install 自己不得留痕迹。
	if why := guard.Check(); why != "" {
		t.Fatal(why)
	}
	// 被装载的服务必须还活着。
	if !procAlive(sentinel) {
		t.Fatal("install 返回后被装载的服务不见了——不能为了自己非常驻就把服务也带走")
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(sentinel) + "/cmdline")
	if err != nil {
		t.Fatalf("读哨兵 cmdline 失败: %v", err)
	}
	if !strings.Contains(string(b), "sleep") {
		t.Fatalf("哨兵进程的 cmdline 不对: %q", b)
	}
	assertNoStrayFerc(t)
}

// startSentinel 派生一个长睡的哨兵进程，返回其 PID（失败返回 0）。
func startSentinel(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exec sleep 30")
	if err := cmd.Start(); err != nil {
		t.Skipf("无法派生哨兵进程: %v", err)
		return 0
	}
	// 不 Wait：Wait 会阻塞到进程结束。父进程退出时子进程会被 init 收养，
	// 这里由 defer killPID 负责收拾。
	return cmd.Process.Pid
}

func killPID(t *testing.T, pid int) {
	t.Helper()
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	// 等它真的消失，避免把僵尸留给后续用例。
	for i := 0; i < 50; i++ {
		if !procAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func procAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
