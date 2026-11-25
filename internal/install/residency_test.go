// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

// 非常驻性质的公共支撑（无平台标签，Windows/macOS/Linux 都编译）。
//
// 拆分理由：课题要求"枚举 /proc/*/comm"是 Linux 专属判定，但"install 不得留下
// 属于自己的进程或 goroutine"这条性质与平台无关。把平台无关的部分放在这里，
// 它就能在开发机上同样被测到；否则这条立论在最常跑测试的机器上等于没测，
// 只在 CI 的 Linux runner 上才生效。
//
// 平台无关的测试见 residency_common_test.go；
// Linux 专属的 /proc 枚举与哨兵进程见 residency_linux_test.go。
package install

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/platform"
)

// ResidentGuard 在 install 前后各取一次 goroutine 快照，用于判断 install
// 是否留下了自己的常驻痕迹。
//
// 判据只有一层：goroutine 数不得净增长。真常驻必然要有人"看着"被装载的东西，
// 而"看着"就是 goroutine。ferc 的设计是装完就退出，因此 Apply 返回时不该有
// 净增长的 goroutine。
//
// 其他候选判据（活跃 ticker 数、打开的 fd 数）Go 没有公开 API 能可靠拿到，
// 因此不作为断言依据——用一个不可靠的判据做断言，比没有断言更糟：它会在
// 偶发时刻假红，而假红会让人学会忽略整条测试。
type ResidentGuard struct {
	before int
}

// Snapshot 取一次快照。
func Snapshot() ResidentGuard {
	return ResidentGuard{before: runtime.NumGoroutine()}
}

// Check 断言 install 前后没有净增长的 goroutine。
//
// 返回空字符串表示通过，否则给出人可读的原因。刻意允许 +1 余量：测试框架
// 自己、GC、以及 runtime 的后台任务都可能短暂增减一两个，用"完全相等"做
// 断言会在偶发时刻假红。
func (g ResidentGuard) Check() string {
	// 等一会儿：install 返回时可能有 goroutine 正在正常收尾。
	// 真常驻不会因为等 2s 就消失，正常收尾的会。
	deadline := time.Now().Add(2 * time.Second)
	for {
		now := runtime.NumGoroutine()
		if now <= g.before+1 {
			return ""
		}
		if time.Now().After(deadline) {
			return "install 返回后 goroutine 数净增长：" +
				itoa(now) + " → 基准 " + itoa(g.before) +
				"（运行核必须非常驻，不得留下后台 goroutine）"
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// runEmptyApply 跑一次空计划的完整 Apply。
//
// 空计划也要走完整个 Apply：装载逻辑必须自己返回，不能留下后台 goroutine。
func runEmptyApply(t *testing.T, info platform.Info) {
	t.Helper()
	root := t.TempDir()
	inst, err := New(Options{Root: root, Info: info, Log: nopLogger{}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := inst.Apply(ctx, nil); err != nil {
		t.Fatalf("空计划应直接成功: %v", err)
	}
}

// hostInfo 返回本机的 platform.Info（探测不到的字段留空）。
//
// 测试只需要 GOOS/GOARCH/Arch：Apply 的空计划路径不读 Init、Kernel 等字段。
func hostInfo() platform.Info {
	return platform.Info{
		GOOS:   runtime.GOOS,
		GOARCH: runtime.GOARCH,
		Arch:   platform.NormalizeArch(runtime.GOARCH),
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
