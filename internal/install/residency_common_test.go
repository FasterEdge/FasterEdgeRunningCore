// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

package install

// 平台无关的非常驻测试。
//
// 这条**没有** //go:build linux：课题要求"枚举 /proc/*/comm"是 Linux 专属的
// 判定方式，但"install 不得留下属于自己的进程或 goroutine"这条性质与平台无关。
// 抽出来之后它就能在 Windows/macOS 开发机上同样被测到——否则这条立论在最
// 常跑测试的机器上等于没测，只在 CI 的 Linux runner 上才生效。
//
// Linux 专属的 /proc 枚举见 residency_linux_test.go。

import (
	"context"
	"testing"
)

// F3-1a：install 返回后不得留下后台 goroutine。
//
// 这是"非常驻"最基础的可跨平台判据：真常驻必然要有人看着被装载的东西，
// 而"看着"就是 goroutine。ferc 的设计是装完就退出，因此 Apply 返回时不该有
// 净增长的 goroutine。
func TestApplyLeavesNoGoroutine(t *testing.T) {
	guard := Snapshot()
	runEmptyApply(t, hostInfo())
	if why := guard.Check(); why != "" {
		t.Fatal(why)
	}
}

// F3-1b：反复 Apply 也不得累积 goroutine。
//
// 单次不泄漏不代表多次不泄漏：某些bug（如每次 new 一个 ticker 而不 Stop）
// 只在第二次调用后显形。循环 20 次把这类问题放大到能看见。
func TestApplyRepeatedlyDoesNotLeakGoroutines(t *testing.T) {
	guard := Snapshot()
	for i := 0; i < 20; i++ {
		runEmptyApply(t, hostInfo())
	}
	if why := guard.Check(); why != "" {
		t.Fatalf("第 20 次 Apply 后: %s", why)
	}
}

// F3-1c：空计划本身必须直接成功——这是上面两条能跑起来的前提。
func TestEmptyApplySucceeds(t *testing.T) {
	runEmptyApply(t, hostInfo())
}

// F3-1d：Snapshot/Check 自身要能用——防止支撑代码坏了却让测试假通过。
func TestResidentGuardDetectsGrowth(t *testing.T) {
	guard := Snapshot()
	stop := make(chan struct{})
	// 起若干个真正常驻的 goroutine：Check 必须报出来。
	//
	// 起 3 个而不是 1 个：Check 刻意允许 +1 余量（测试框架与 runtime 自己可能
	// 短暂增减一两个），只起 1 个会正好落在余量内，从而让这条守护测试假通过。
	const n = 3
	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		go func() {
			<-stop
			done <- struct{}{}
		}()
	}
	if why := guard.Check(); why == "" {
		close(stop)
		for i := 0; i < n; i++ {
			<-done
		}
		t.Fatal("有多个常驻 goroutine 时 Check 必须报出来，否则这条测试是空的")
	}
	close(stop)
	for i := 0; i < n; i++ {
		<-done
	}
}

// nil plan 必须被当作"什么都没要装"，不能 panic。
//
// 这条是实跑逼出来的：原实现在 Apply 第一行就解引用 plan.Warnings，传 nil 直接
// 崩。而它一直藏着，因为唯一传 nil 的测试带 //go:build linux，从没在开发机上
// 跑过——"只有 CI 跑的测试"等于没测。
func TestApplyNilPlanDoesNotPanic(t *testing.T) {
	root := t.TempDir()
	inst, err := New(Options{Root: root, Info: hostInfo(), Log: nopLogger{}})
	if err != nil {
		t.Fatal(err)
	}
	recs, err := inst.Apply(context.Background(), nil)
	if err != nil {
		t.Fatalf("nil 计划应直接成功: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("nil 计划不该产出记录，实际 %d 条", len(recs))
	}
}
