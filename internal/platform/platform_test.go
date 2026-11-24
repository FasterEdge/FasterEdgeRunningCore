package platform

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

type fakeFileInfo struct{ dir bool }

func (f fakeFileInfo) Name() string       { return "fake" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return 0o644 }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.dir }
func (f fakeFileInfo) Sys() any           { return nil }

// fakeEnv 构造一台完全虚拟的 Linux 目标机，避免测试依赖宿主机的真实路径。
func fakeEnv(files map[string]string, bins map[string]string) Env {
	return Env{
		GOOS:     "linux",
		GOARCH:   "arm64",
		Hostname: func() (string, error) { return "edge-01", nil },
		LookPath: func(name string) (string, error) {
			if p, ok := bins[name]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
		Stat: func(path string) (os.FileInfo, error) {
			if _, ok := files[path]; ok {
				return fakeFileInfo{}, nil
			}
			return nil, os.ErrNotExist
		},
		ReadFile: func(path string) ([]byte, error) {
			if c, ok := files[path]; ok {
				return []byte(c), nil
			}
			return nil, os.ErrNotExist
		},
		Run: func(_ context.Context, name string, args ...string) (string, error) {
			if name == "uname" && len(args) == 1 && args[0] == "-r" {
				return "6.6.0-edge", nil
			}
			return "", errors.New("unexpected command")
		},
		Privileged: func() bool { return true },
	}
}

func TestDetectSystemd(t *testing.T) {
	env := fakeEnv(map[string]string{
		"/run/systemd/system": "",
		"/etc/os-release":     "NAME=\"Ubuntu\"\nID=ubuntu\nVERSION_ID=\"24.04\"\n",
	}, map[string]string{"apt-get": "/usr/bin/apt-get"})

	info, err := env.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect 失败: %v", err)
	}
	if info.Init != InitSystemd {
		t.Fatalf("Init = %q，期望 systemd", info.Init)
	}
	if !info.Init.Supervised() {
		t.Fatal("systemd 必须被判定为具备保活能力")
	}
	if info.Distro != "ubuntu" || info.DistroVersion != "24.04" {
		t.Fatalf("发行版解析错误: %q %q", info.Distro, info.DistroVersion)
	}
	if info.Arch != "arm64" {
		t.Fatalf("Arch = %q，期望 arm64", info.Arch)
	}
	if info.PackageMgr != "apt-get" {
		t.Fatalf("PackageMgr = %q，期望 apt-get", info.PackageMgr)
	}
	if !info.Privileged {
		t.Fatal("Privileged 应为 true")
	}
}

func TestDetectOpenRC(t *testing.T) {
	env := fakeEnv(map[string]string{}, map[string]string{"rc-service": "/sbin/rc-service", "apk": "/sbin/apk"})
	info, err := env.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect 失败: %v", err)
	}
	if info.Init != InitOpenRC {
		t.Fatalf("Init = %q，期望 openrc", info.Init)
	}
}

func TestDetectNoneNeedsSupervisor(t *testing.T) {
	env := fakeEnv(map[string]string{"/etc/os-release": "ID=scratch\n"}, nil)
	info, err := env.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect 失败: %v", err)
	}
	if info.Init != InitNone {
		t.Fatalf("Init = %q，期望 none", info.Init)
	}
	if info.Init.Supervised() {
		t.Fatal("InitNone 不得被判定为具备保活能力")
	}
}

func TestDetectContainer(t *testing.T) {
	env := fakeEnv(map[string]string{
		"/.dockerenv":         "",
		"/run/systemd/system": "",
	}, nil)
	info, err := env.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect 失败: %v", err)
	}
	if !info.Container {
		t.Fatal("存在 /.dockerenv 时应判定为容器")
	}
}

func TestDetectCgroupContainer(t *testing.T) {
	env := fakeEnv(map[string]string{
		"/proc/1/cgroup": "12:pids:/kubepods/besteffort/pod123\n",
	}, nil)
	info, err := env.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect 失败: %v", err)
	}
	if !info.Container {
		t.Fatal("cgroup 含 kubepods 时应判定为容器")
	}
}

func TestDetectDarwin(t *testing.T) {
	env := fakeEnv(map[string]string{}, nil)
	env.GOOS = "darwin"
	env.GOARCH = "arm64"
	info, err := env.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect 失败: %v", err)
	}
	if info.Init != InitLaunchd {
		t.Fatalf("Init = %q，期望 launchd", info.Init)
	}
	if !info.Init.Supervised() {
		t.Fatal("launchd 必须被判定为具备保活能力")
	}
}

func TestDetectWindows(t *testing.T) {
	env := fakeEnv(map[string]string{}, map[string]string{"winget": `C:\winget.exe`})
	env.GOOS = "windows"
	env.GOARCH = "amd64"
	info, err := env.Detect(context.Background())
	if err != nil {
		t.Fatalf("Detect 失败: %v", err)
	}
	if info.Init != InitWindowsSchtasks {
		t.Fatalf("Init = %q，期望 windows-schtasks", info.Init)
	}
	if info.Init.Supervised() {
		t.Fatal("Windows 计划任务只能自启不能拉起崩溃进程，不应被判定为具备完整保活能力")
	}
	if !info.Init.Autostart() {
		t.Fatal("计划任务应被判定为具备开机自启能力")
	}
	if info.PackageMgr != "winget" {
		t.Fatalf("PackageMgr = %q，期望 winget", info.PackageMgr)
	}
}

func TestDetectRejectsIncompleteEnv(t *testing.T) {
	_, err := Env{GOOS: "linux"}.Detect(context.Background())
	if err == nil {
		t.Fatal("不完整的 Env 必须返回错误，而不是静默给出错误结论")
	}
}

func TestNormalizeArch(t *testing.T) {
	// arm 的标签由 GOARM 决定，按构建配置在 arch_self_*_test.go 里分别断言。
	// 这里只断言与 GOARM 无关、原样透传的那些 GOARCH。
	cases := map[string]string{
		"386": "386", "amd64": "amd64", "arm64": "arm64", "riscv64": "riscv64",
		"loong64": "loong64", "mips": "mips", "mipsle": "mipsle",
		"mips64": "mips64", "mips64le": "mips64le",
		"ppc64": "ppc64", "ppc64le": "ppc64le", "s390x": "s390x",
		"wasm": "wasm",
	}
	for in, want := range cases {
		if got := NormalizeArch(in); got != want {
			t.Errorf("NormalizeArch(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestNormalizeArchSelf 断言本机自报的架构标签与「按它拼产物名」的规则一致。
// 这里刻意不写死 armv7：arm 的具体标签由 arch_self_armv6_test.go /
// arch_self_armv7_test.go / arch_self_fallback_test.go 按 GOARM 分别断言，
// 写死会让 armv6 设备误判成 armv7，从而下错产物。
func TestNormalizeArchSelf(t *testing.T) {
	armSelfLabelTest(t)
}

func TestSupportsArch(t *testing.T) {
	if !SupportsArch(nil, "arm64") {
		t.Fatal("空支持列表应视为全架构兼容")
	}
	if !SupportsArch([]string{"amd64", "arm64"}, "arm64") {
		t.Fatal("arm64 应在支持列表内")
	}
	if SupportsArch([]string{"amd64"}, "armv7") {
		t.Fatal("armv7 不在支持列表内")
	}
}

func TestParseOSReleaseQuotedAndCommented(t *testing.T) {
	id, ver := parseOSRelease("# comment\nID='debian'\nVERSION_ID=\"12\"\n")
	if id != "debian" || ver != "12" {
		t.Fatalf("解析结果 %q %q，期望 debian 12", id, ver)
	}
	if !strings.Contains(id, "debian") {
		t.Fatal("ID 应去掉引号")
	}
}
