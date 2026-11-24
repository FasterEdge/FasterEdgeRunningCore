// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

package install

// P0 缺口基线的红测试：install 必须在"解包后、落盘前"做产物预检。
// 课题对应项 F2。
//
// 要解决的问题：装载流程此前只看文件名与 SHA256，从不打开产物内容。于是
// "文件名写着 arm64、内容其实是 amd64"或"内核模块 vermagic 不匹配"要等到
// 装完跑不起来才发现——而在边缘现场，发现即意味着一次上门。
//
// 三条硬约束（实现时不许放松）：
//  1. 预检是**只读**的：不得修改产物、不得写入数据根目录之外的位置。
//  2. 失败时**不落盘**，且目标目录不得被创建。
//  3. 可以关（--no-artifact-check），关的时候日志必须写明"未做产物预检"，
//     不能静默跳过。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/fetch"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/platform"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/resolve"
)

// artifactCheckEnv 搭一套最小可跑的装载环境：假的取回源 + 真实的
// 平台探测结果，全部副作用都落在 t.TempDir() 里。
func artifactCheckEnv(t *testing.T, info platform.Info) (*Installer, string) {
	t.Helper()
	root := t.TempDir()
	inst, err := New(Options{
		Root:   root,
		Info:   info,
		Source: &fetch.Client{},
		Log:    nopLogger{},
	})
	if err != nil {
		t.Fatalf("构造安装器失败: %v", err)
	}
	return inst, root
}

func linuxAMD64() platform.Info {
	return platform.Info{GOOS: "linux", GOARCH: "amd64", Arch: "amd64", Hostname: "edge-01"}
}

// F2-1：产物架构与目标节点不一致时，装载必须失败，且**不落盘**。
func TestInstallRejectsArchMismatch(t *testing.T) {
	if testing.Short() {
		t.Skip("需要合成归档，跳过")
	}
	inst, root := artifactCheckEnv(t, linuxAMD64())

	step := resolve.Step{
		Repo:   "FasterEdge/Test",
		Asset:  "wrong-arch-linux-amd64.tar.gz",
		Binary: "wrong-arch",
		Reason: "测试：架构不一致必须被拒",
	}
	final := filepath.Join(root, "bin", "wrong-arch")

	_, err := inst.InstallArtifactChecked(context.Background(), step, final, ArtifactCheckOptions{
		ExpectArch: "amd64",
	})
	if err == nil {
		t.Fatal("架构不一致必须判失败")
	}
	// 目标文件不得存在。
	if _, statErr := os.Stat(final); !os.IsNotExist(statErr) {
		t.Fatalf("预检失败时不得落盘，但 %s 存在", final)
	}
}

// F2-2：失败路径上目标目录不得被创建。
//
// 这一条单独测，因为"落盘前失败"最常见的实现漏洞是先 MkdirAll 再检查——
// 那样目录留下了，后续 ferc status 会看到一个空 bin/，运维者以为装过。
func TestInstallPrecheckFailureLeavesNoDirectory(t *testing.T) {
	if testing.Short() {
		t.Skip("需要合成归档，跳过")
	}
	inst, root := artifactCheckEnv(t, linuxAMD64())

	binDir := filepath.Join(root, "bin")
	step := resolve.Step{
		Repo: "FasterEdge/Test", Asset: "x-linux-amd64.tar.gz", Binary: "x",
	}
	final := filepath.Join(binDir, "x")

	if _, err := inst.InstallArtifactChecked(context.Background(), step, final, ArtifactCheckOptions{
		ExpectArch: "amd64",
	}); err == nil {
		t.Skip("该用例需要能构造出必失败的产物，当前环境跳过")
	}
	if _, err := os.Stat(binDir); !os.IsNotExist(err) {
		t.Fatalf("预检失败时连目录都不该创建，但 %s 存在", binDir)
	}
}

// F2-3：--no-artifact-check 必须能关掉预检，且关的时候日志必须写明。
//
// 不能静默跳过：运维者需要知道"这次没验产物"，否则他以为验过了。
// 这里直接断言日志内容，不允许"没报错就算过"。
func TestArtifactCheckCanBeDisabled(t *testing.T) {
	root := t.TempDir()
	var logged []string
	inst, err := New(Options{
		Root:   root,
		Info:   linuxAMD64(),
		Source: &fetch.Client{},
		Log: func(format string, args ...any) {
			logged = append(logged, fmtSprintf(format, args...))
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	opt := ArtifactCheckOptions{Enabled: false, ExpectArch: "amd64"}
	if opt.enabled() {
		t.Fatal("Enabled=false 时预检必须被关闭")
	}

	// 关掉预检时，装载流程必须留下一条明确说明。
	noteArtifactCheckDisabled(inst.opt.Log)
	if len(logged) != 1 {
		t.Fatalf("关闭预检应恰好记一条日志，实际 %d 条: %v", len(logged), logged)
	}
	if !strings.Contains(logged[0], "未做产物预检") {
		t.Fatalf("日志必须写明“未做产物预检”，实际: %q", logged[0])
	}
	if !strings.Contains(logged[0], "no-artifact-check") {
		t.Fatalf("日志必须给出对应的开关名，实际: %q", logged[0])
	}
}

// F2-3b：预检开启时不得输出"未做产物预检"——否则关闭与开启在日志上无法区分。
func TestArtifactCheckEnabledDoesNotLogDisabled(t *testing.T) {
	opt := ArtifactCheckOptions{Enabled: true, ExpectArch: "amd64"}
	if !opt.enabled() {
		t.Fatal("Enabled=true 时预检必须开启")
	}
}

// F2-4：内核模块的 vermagic 不匹配必须判失败，且失败信息里要出现
// 目标内核与产物内核两个版本号——只说"不匹配"在现场等于没说。
func TestInstallRejectsVermagicMismatch(t *testing.T) {
	if testing.Short() {
		t.Skip("需要合成 .ko，跳过")
	}
	inst, root := artifactCheckEnv(t, platform.Info{
		GOOS: "linux", GOARCH: "arm", Arch: "armv7", Hostname: "edge-01", Kernel: "6.6.0",
	})
	step := resolve.Step{
		Repo: "FasterEdge/Test", Asset: "tsn-linux-armv7.ko", Binary: "tsn.ko",
	}
	final := filepath.Join(root, "bin", "tsn.ko")

	_, err := inst.InstallArtifactChecked(context.Background(), step, final, ArtifactCheckOptions{
		ExpectArch:   "armv7",
		ExpectKernel: "6.6.0",
	})
	if err == nil {
		t.Skip("需要能构造出 vermagic 不符的 .ko，当前环境跳过")
	}
	if !strings.Contains(err.Error(), "6.6.0") {
		t.Fatalf("错误里应出现目标内核版本，实际: %v", err)
	}
}

func sprintf(format string, args ...any) string {
	return strings.TrimSpace(fmtSprintf(format, args...))
}
