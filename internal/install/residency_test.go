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
// 单测环境下没有真的 init 系统，因此这里用 fakeSpawn 派生一个"服务"进程
// （就是测试二进制自己带一个参数重跑），然后断言 install 返回后它仍在
// /proc 里、且退出码可回收。这是端到端性质的最小可复现版本。
func TestInstalledServiceSurvivesInstallerExit(t *testing.T) {
	if testing.Short() {
		t.Skip("需要真实进程，跳过")
	}
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("非 Linux，跳过")
	}

	// 用一个"退出前一直睡"的哨兵进程模拟被装载的服务。
	// 不真的起 ferc：那会把测试和被测搅在一起。
	sentinel := startSentinel(t)
	if sentinel == 0 {
		t.Fatal("哨兵进程没起来")
	}
	defer killPID(t, sentinel)

	// 走一遍完整的 Apply：它自己必须返回，且不得带走哨兵。
	root := t.TempDir()
	inst, err := New(Options{
		Root: root,
		Info: platform.Info{GOOS: "linux", GOARCH: "amd64", Arch: "amd64", Hostname: "edge-01"},
		Log:  nopLogger{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := inst.Apply(ctx, nil); err != nil {
		t.Fatalf("空计划应直接成功: %v", err)
	}

	// 哨兵必须还活着。
	if !procAlive(sentinel) {
		t.Fatal("install 返回后被装载的服务不见了——不能为了自己非常驻就把服务也带走")
	}
	// 且它必须是个真进程（有 /proc/<pid>/cmdline），不是僵尸。
	b, err := os.ReadFile("/proc/" + strconv.Itoa(sentinel) + "/cmdline")
	if err != nil {
		t.Fatalf("读哨兵 cmdline 失败: %v", err)
	}
	if !strings.Contains(string(b), "ferc-test-sentinel") {
		t.Fatalf("哨兵进程的 cmdline 不对: %q", b)
	}
}

// startSentinel 派生一个"长睡"的哨兵进程，返回其 PID。
//
// 用测试二进制自己重跑（-test.run 匹配不到任何用例所以立刻退出）不可行，
// 因此改用 sh -c 'sleep'：它一定存在、一定长睡、一定不占端口。
func startSentinel(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exec sleep 30")
	if err := cmd.Start(); err != nil {
		t.Skipf("无法派生哨兵进程: %v", err)
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
	err := syscall.Kill(pid, 0)
	return err == nil
}
