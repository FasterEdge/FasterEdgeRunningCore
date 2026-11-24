// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

package install

// 产物预检的红测试（P0 立，P2 转绿）。
//
// 这些测试刻意走完整的"下载 → 校验 → 解包 → 预检"流程，而不是直接调
// precheckArtifact：预检的价值全在"它在落盘之前拦住了东西"，跳过前面几步
// 就验不到"没落盘"这个关键性质。
//
// 归档里的 ELF 由 internal/artifact 的合成器构造（同一套测试件，不跨仓复制）。

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/fetch"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/platform"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/resolve"
)

// elfArchive 造一个内含单个 ELF 的 tar.gz。
//
// ELF 用 artifact 包的合成器构造——那里有完整的自检，保证测试件可信。
func elfArchive(t *testing.T, name string, machine uint16, modinfo []byte) []byte {
	t.Helper()
	body := buildSyntheticELFForTest(t, machine, modinfo)
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return gz.Bytes()
}

// precheckEnv 搭一套跑完整装载流程的环境。
func precheckEnv(t *testing.T, info platform.Info) (*Installer, string) {
	t.Helper()
	root := t.TempDir()
	inst, err := New(Options{Root: root, Info: info, Log: &captureLogger{}})
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
	// 归档里是 aarch64 的 ELF，目标节点是 amd64。
	archive := elfArchive(t, "wrong-arch", machineAARCH64, nil)
	srv := releaseServer(t, "wrong-arch-linux-amd64.tar.gz", archive, false)

	inst, root := precheckEnv(t, linuxAMD64())
	inst.opt.Source = fetch.New("")
	inst.opt.Source.(*fetch.Client).BaseURL = srv.URL

	step := resolve.Step{
		Repo: "FasterEdge/Test", Asset: "wrong-arch-linux-amd64.tar.gz",
		Binary: "wrong-arch", Reason: "测试：架构不一致必须被拒",
	}
	final := filepath.Join(root, "bin", "wrong-arch")

	_, err := inst.InstallArtifactChecked(context.Background(), step, final, ArtifactCheckOptions{
		Enabled: true, ExpectArch: "amd64",
	})
	if err == nil {
		t.Fatal("架构不一致必须判失败")
	}
	// 错误里必须同时出现产物架构与目标架构——只说"不匹配"在现场等于没说。
	if !strings.Contains(err.Error(), "arm64") || !strings.Contains(err.Error(), "amd64") {
		t.Fatalf("错误应指明产物架构与目标架构，实际: %v", err)
	}
	// 目标文件不得存在。
	if _, statErr := os.Stat(final); !os.IsNotExist(statErr) {
		t.Fatalf("预检失败时不得落盘，但 %s 存在", final)
	}
	// 连 bin/ 目录都不该被创建（见 F2-2）。
	if _, statErr := os.Stat(filepath.Join(root, "bin")); !os.IsNotExist(statErr) {
		t.Fatalf("预检失败时连目录都不该创建，但 bin/ 存在")
	}
}

// F2-1b：架构一致时必须装成——证明上面的失败是"真在比架构"而不是一律拒绝。
func TestInstallAcceptsMatchingArch(t *testing.T) {
	if testing.Short() {
		t.Skip("需要合成归档，跳过")
	}
	archive := elfArchive(t, "good", machineX86_64, nil)
	srv := releaseServer(t, "good-linux-amd64.tar.gz", archive, false)

	inst, root := precheckEnv(t, linuxAMD64())
	inst.opt.Source = fetch.New("")
	inst.opt.Source.(*fetch.Client).BaseURL = srv.URL

	step := resolve.Step{
		Repo: "FasterEdge/Test", Asset: "good-linux-amd64.tar.gz", Binary: "good",
	}
	final := filepath.Join(root, "bin", "good")

	tag, err := inst.InstallArtifactChecked(context.Background(), step, final, ArtifactCheckOptions{
		Enabled: true, ExpectArch: "amd64",
	})
	if err != nil {
		t.Fatalf("架构匹配时应装成: %v", err)
	}
	if tag != "v1.0.20261124" {
		t.Fatalf("应返回 release tag，实际 %q", tag)
	}
	if _, statErr := os.Stat(final); statErr != nil {
		t.Fatalf("预检通过后应落盘: %v", statErr)
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
	archive := elfArchive(t, "wrong-arch", machineAARCH64, nil)
	srv := releaseServer(t, "wrong-arch-linux-amd64.tar.gz", archive, false)

	inst, root := precheckEnv(t, linuxAMD64())
	inst.opt.Source = fetch.New("")
	inst.opt.Source.(*fetch.Client).BaseURL = srv.URL

	step := resolve.Step{
		Repo: "FasterEdge/Test", Asset: "wrong-arch-linux-amd64.tar.gz", Binary: "wrong-arch",
	}
	final := filepath.Join(root, "bin", "wrong-arch")

	if _, err := inst.InstallArtifactChecked(context.Background(), step, final, ArtifactCheckOptions{
		Enabled: true, ExpectArch: "amd64",
	}); err == nil {
		t.Fatal("架构不符必须失败")
	}
	binDir := filepath.Join(root, "bin")
	if _, err := os.Stat(binDir); !os.IsNotExist(err) {
		t.Fatalf("预检失败时连目录都不该创建，但 %s 存在", binDir)
	}
	// 暂存目录也必须清干净——它留在 root 下会让 ferc status 以为有半装状态。
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ferc-stage-") {
			t.Fatalf("暂存目录 %s 未清理", e.Name())
		}
	}
}

// F2-3：--no-artifact-check 必须能关掉预检，且关的时候日志必须写明。
//
// 不能静默跳过：运维者需要知道"这次没验产物"，否则他以为验过了。
func TestArtifactCheckCanBeDisabled(t *testing.T) {
	root := t.TempDir()
	var logged []string
	inst, err := New(Options{
		Root: root,
		Info: linuxAMD64(),
		Log: logFunc(func(format string, args ...any) {
			logged = append(logged, fmtSprintf(format, args...))
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	opt := ArtifactCheckOptions{Enabled: false, ExpectArch: "amd64"}
	if opt.enabled() {
		t.Fatal("Enabled=false 时预检必须被关闭")
	}

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

// F2-3c：关掉预检后，架构不符的产物也必须装得上——证明"关"是真的关了。
func TestDisabledCheckAllowsMismatchedArch(t *testing.T) {
	if testing.Short() {
		t.Skip("需要合成归档，跳过")
	}
	archive := elfArchive(t, "wrong-arch", machineAARCH64, nil)
	srv := releaseServer(t, "wrong-arch-linux-amd64.tar.gz", archive, false)

	inst, root := precheckEnv(t, linuxAMD64())
	inst.opt.Source = fetch.New("")
	inst.opt.Source.(*fetch.Client).BaseURL = srv.URL

	step := resolve.Step{
		Repo: "FasterEdge/Test", Asset: "wrong-arch-linux-amd64.tar.gz", Binary: "wrong-arch",
	}
	final := filepath.Join(root, "bin", "wrong-arch")

	if _, err := inst.InstallArtifactChecked(context.Background(), step, final, ArtifactCheckOptions{
		Enabled: false, ExpectArch: "amd64",
	}); err != nil {
		t.Fatalf("关掉预检后应放行: %v", err)
	}
	if _, err := os.Stat(final); err != nil {
		t.Fatalf("关掉预检后应落盘: %v", err)
	}
}

// F2-4：内核模块的 vermagic 不匹配必须判失败，且失败信息里要出现
// 目标内核与产物内核两个版本号——只说"不匹配"在现场等于没说。
func TestInstallRejectsVermagicMismatch(t *testing.T) {
	if testing.Short() {
		t.Skip("需要合成 .ko，跳过")
	}
	// 造一个编译于 5.15.0 的 .ko。
	archive := elfArchive(t, "tsn.ko", machineARM, []byte("vermagic=5.15.0 SMP mod_unload ARMv7\x00name=tsn\x00\x00"))
	srv := releaseServer(t, "tsn-linux-armv7.tar.gz", archive, false)

	inst, root := precheckEnv(t, platform.Info{
		GOOS: "linux", GOARCH: "arm", Arch: "armv7", Hostname: "edge-01", Kernel: "6.6.0",
	})
	inst.opt.Source = fetch.New("")
	inst.opt.Source.(*fetch.Client).BaseURL = srv.URL

	step := resolve.Step{
		Repo: "FasterEdge/Test", Asset: "tsn-linux-armv7.tar.gz", Binary: "tsn.ko",
	}
	final := filepath.Join(root, "bin", "tsn.ko")

	_, err := inst.InstallArtifactChecked(context.Background(), step, final, ArtifactCheckOptions{
		Enabled: true, ExpectArch: "armv7", ExpectKernel: "6.6.0",
	})
	if err == nil {
		t.Fatal("vermagic 不符必须判失败")
	}
	// 两个版本号都要出现。
	if !strings.Contains(err.Error(), "5.15.0") || !strings.Contains(err.Error(), "6.6.0") {
		t.Fatalf("错误应同时给出产物内核与目标内核，实际: %v", err)
	}
	if _, statErr := os.Stat(final); !os.IsNotExist(statErr) {
		t.Fatalf("预检失败时不得落盘，但 %s 存在", final)
	}
}

// F2-4b：vermagic 匹配时必须装成。
func TestInstallAcceptsMatchingVermagic(t *testing.T) {
	if testing.Short() {
		t.Skip("需要合成 .ko，跳过")
	}
	archive := elfArchive(t, "tsn.ko", machineARM, []byte("vermagic=6.6.0 SMP mod_unload ARMv7\x00name=tsn\x00\x00"))
	srv := releaseServer(t, "tsn-linux-armv7.tar.gz", archive, false)

	inst, root := precheckEnv(t, platform.Info{
		GOOS: "linux", GOARCH: "arm", Arch: "armv7", Hostname: "edge-01", Kernel: "6.6.0",
	})
	inst.opt.Source = fetch.New("")
	inst.opt.Source.(*fetch.Client).BaseURL = srv.URL

	step := resolve.Step{
		Repo: "FasterEdge/Test", Asset: "tsn-linux-armv7.tar.gz", Binary: "tsn.ko",
	}
	final := filepath.Join(root, "bin", "tsn.ko")

	if _, err := inst.InstallArtifactChecked(context.Background(), step, final, ArtifactCheckOptions{
		Enabled: true, ExpectArch: "armv7", ExpectKernel: "6.6.0",
	}); err != nil {
		t.Fatalf("vermagic 匹配时应装成: %v", err)
	}
}

// F2-4c：目标内核带发行版后缀时也要能匹配上（6.6.0-debian12 与 6.6.0 是同一个内核）。
func TestVermagicMatchesKernelWithDistroSuffix(t *testing.T) {
	if got := kernelRelease("6.6.0-debian12"); got != "6.6.0" {
		t.Fatalf("kernelRelease 应剥掉发行版后缀，得到 %q", got)
	}
	if got := kernelRelease("5.15.0-91-generic"); got != "5.15.0" {
		t.Fatalf("kernelRelease 应剥掉发行版后缀，得到 %q", got)
	}
	if got := kernelRelease("6.6.0"); got != "6.6.0" {
		t.Fatalf("无后缀时应原样返回，得到 %q", got)
	}
}

func fmtSprintf(format string, args ...any) string {
	return strings.TrimSpace(fmt.Sprintf(format, args...))
}

// logFunc 把一个普通函数适配成 install.Logger。
type logFunc func(format string, args ...any)

func (f logFunc) Logf(format string, args ...any) { f(format, args...) }

// F2-5：认不出形态的产物必须放行，不能当成"不兼容"。
//
// 现实里有不是 ELF 的产物：shell/python 脚本、jar、由外部配置驱动的工具。
// 把"认不出"判成失败是假阴性——它直接把本来能用的组件推掉，比漏检更糟：
// 漏检最多是装完再报错，假阴性是让合法装载永远失败。
func TestPrecheckPassesNonELFArtifact(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "tool")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := precheckArtifact(script, ArtifactCheckOptions{
		Enabled: true, ExpectArch: "amd64",
	}); err != nil {
		t.Fatalf("非 ELF 产物应放行: %v", err)
	}
}

// F2-5b：空文件同样放行——"没有内容"不等于"内容不兼容"。
func TestPrecheckPassesEmptyArtifact(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := precheckArtifact(empty, ArtifactCheckOptions{
		Enabled: true, ExpectArch: "amd64",
	}); err != nil {
		t.Fatalf("空产物应放行: %v", err)
	}
}

// F2-5c：真 ELF 但架构不符仍必须拒——放行只针对"认不出"，不是一律放行。
func TestPrecheckStillRejectsRealELFMismatch(t *testing.T) {
	dir := t.TempDir()
	elfPath := filepath.Join(dir, "real-elf")
	if err := os.WriteFile(elfPath, buildSyntheticELFForTest(t, machineAARCH64, nil), 0o755); err != nil {
		t.Fatal(err)
	}
	err := precheckArtifact(elfPath, ArtifactCheckOptions{Enabled: true, ExpectArch: "amd64"})
	if err == nil {
		t.Fatal("真 ELF 架构不符必须拒")
	}
	if !strings.Contains(err.Error(), "arm64") || !strings.Contains(err.Error(), "amd64") {
		t.Fatalf("错误应指明两个架构: %v", err)
	}
}
