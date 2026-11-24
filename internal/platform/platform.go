// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

// Package platform 负责识别运行核所处的操作系统、架构与初始化系统。
//
// 运行核需要在“完全未知”的目标机上做决策：用哪个 init 注册服务、该下载哪个
// 架构的产物、是否具备写 /usr/local/bin 的权限。这些判断全部收敛到本包，
// 并通过 Env 抽象注入外部世界，使核心逻辑可在任意平台上被单元测试。
package platform

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
)

// InitSystem 是目标机的服务托管方式。运行核据此决定“用系统自带的进程管理器”
// 还是“自行装载一个进程管理器”。
//
// 这里刻意把“开机自启”与“崩溃拉起”分成两件事：sysvinit 能开机自启但没有
// respawn，Windows 计划任务同样只能自启。只有两者都具备时才算“系统自带进程
// 管理器”，否则仍需外挂一个来兜住崩溃——这正是本工具存在的理由。
type InitSystem string

const (
	// InitSystemd 表示由 systemd 托管（/run/systemd/system 存在）。
	InitSystemd InitSystem = "systemd"
	// InitOpenRC 表示 Alpine/Gentoo 系的 OpenRC。
	InitOpenRC InitSystem = "openrc"
	// InitSysV 表示传统的 /etc/init.d + update-rc.d / chkconfig。
	InitSysV InitSystem = "sysvinit"
	// InitLaunchd 表示 macOS 的 launchd。
	InitLaunchd InitSystem = "launchd"
	// InitWindowsSchtasks 表示 Windows 上只能用计划任务做到开机自启。
	InitWindowsSchtasks InitSystem = "windows-schtasks"
	// InitNone 表示没有任何可用的服务托管，运行核必须自带进程管理器。
	InitNone InitSystem = "none"
)

// Autostart 报告该 init 系统能否让组件开机自启。
func (i InitSystem) Autostart() bool {
	switch i {
	case InitSystemd, InitOpenRC, InitSysV, InitLaunchd, InitWindowsSchtasks:
		return true
	default:
		return false
	}
}

// Respawn 报告该 init 系统能否在进程崩溃后自动拉起。
// 这是判定“是否需要外挂进程管理器”的唯一依据。
func (i InitSystem) Respawn() bool {
	switch i {
	case InitSystemd, InitOpenRC, InitLaunchd:
		return true
	default:
		return false
	}
}

// Supervised 报告该 init 系统是否足以独立承担“进程保活”。
// 注意它不是 Autostart 的同义词：sysvinit 与 Windows 计划任务只自启不拉起。
func (i InitSystem) Supervised() bool { return i.Autostart() && i.Respawn() }

func (i InitSystem) String() string { return string(i) }

// Info 是一台目标机的自供式描述。
type Info struct {
	GOOS          string     `json:"goos"`
	GOARCH        string     `json:"goarch"`
	Arch          string     `json:"arch"` // 归一化架构标签：amd64 / arm64 / armv6 / armv7 / 386 / riscv64 …
	Hostname      string     `json:"hostname"`
	Distro        string     `json:"distro,omitempty"`
	DistroVersion string     `json:"distro_version,omitempty"`
	Kernel        string     `json:"kernel,omitempty"`
	Init          InitSystem `json:"init"`
	InitEvidence  string     `json:"init_evidence,omitempty"`
	Container     bool       `json:"container"`
	Privileged    bool       `json:"privileged"`
	PackageMgr    string     `json:"package_manager,omitempty"`
}

// Env 是运行核访问外部世界的最小端口集合。
type Env struct {
	GOOS     string
	GOARCH   string
	Hostname func() (string, error)
	LookPath func(string) (string, error)
	Stat     func(string) (os.FileInfo, error)
	ReadFile func(string) ([]byte, error)
	// Run 执行一条外部命令并返回其标准输出。失败时返回非 nil error。
	Run func(ctx context.Context, name string, args ...string) (string, error)
	// Privileged 报告当前进程是否具备 root / 管理员等价权限。
	Privileged func() bool
}

// Default 返回绑定到真实操作系统的 Env。
func Default() Env {
	return Env{
		GOOS:       runtime.GOOS,
		GOARCH:     runtime.GOARCH,
		Hostname:   os.Hostname,
		LookPath:   lookPath,
		Stat:       os.Stat,
		ReadFile:   os.ReadFile,
		Run:        runCommand,
		Privileged: isPrivileged,
	}
}

// Detect 探测目标机。
func (e Env) Detect(ctx context.Context) (Info, error) {
	if e.GOOS == "" {
		e.GOOS = runtime.GOOS
	}
	if e.GOARCH == "" {
		e.GOARCH = runtime.GOARCH
	}
	if e.Stat == nil || e.ReadFile == nil || e.LookPath == nil || e.Run == nil || e.Hostname == nil {
		return Info{}, errors.New("platform: 不完整的 Env，缺少探测所需的函数")
	}
	info := Info{
		GOOS:   e.GOOS,
		GOARCH: e.GOARCH,
		Arch:   NormalizeArch(e.GOARCH),
		Init:   InitNone,
	}
	host, err := e.Hostname()
	if err != nil {
		return Info{}, fmt.Errorf("platform: 读取主机名失败: %w", err)
	}
	info.Hostname = host
	if e.Privileged != nil {
		info.Privileged = e.Privileged()
	}

	switch e.GOOS {
	case "linux":
		e.fillLinux(&info)
	case "darwin":
		info.Init = InitLaunchd
		info.InitEvidence = "darwin 默认使用 launchd"
		if out, err := e.Run(ctx, "uname", "-r"); err == nil {
			info.Kernel = strings.TrimSpace(out)
		}
	case "windows":
		info.Init = InitWindowsSchtasks
		info.InitEvidence = "Windows 仅有计划任务可做开机自启，崩溃拉起需外挂进程管理器"
		info.PackageMgr = firstExisting(e.LookPath, "winget", "choco", "scoop")
	default:
		info.Init = InitNone
		info.InitEvidence = "未知操作系统，无服务托管能力"
	}

	info.Container = e.detectContainer()
	return info, nil
}

func (e Env) fillLinux(info *Info) {
	if b, err := e.ReadFile("/etc/os-release"); err == nil {
		info.Distro, info.DistroVersion = parseOSRelease(string(b))
	}
	if out, err := e.Run(context.Background(), "uname", "-r"); err == nil {
		info.Kernel = strings.TrimSpace(out)
	}
	info.PackageMgr = firstExisting(e.LookPath, "apt-get", "dnf", "yum", "apk", "pacman", "zypper")
	info.Init, info.InitEvidence = e.detectLinuxInit()
}

func (e Env) detectLinuxInit() (InitSystem, string) {
	// systemd 的判定不要依赖 systemctl 可执行文件：容器镜像里常有二进制
	// 但没有真正作为 PID 1 运行，/run/systemd/system 才是可靠探针。
	if _, err := e.Stat("/run/systemd/system"); err == nil {
		return InitSystemd, "/run/systemd/system 存在"
	}
	if _, err := e.Stat("/run/openrc"); err == nil {
		return InitOpenRC, "/run/openrc 存在"
	}
	if _, err := e.LookPath("rc-service"); err == nil {
		return InitOpenRC, "rc-service 在 PATH 中"
	}
	if fi, err := e.Stat("/sbin/init"); err == nil && !fi.IsDir() {
		if _, err := e.Stat("/etc/init.d"); err == nil {
			return InitSysV, "/sbin/init + /etc/init.d 存在"
		}
	}
	if _, err := e.Stat("/etc/init.d"); err == nil {
		return InitSysV, "/etc/init.d 存在"
	}
	return InitNone, "未发现 systemd / openrc / sysvinit"
}

func (e Env) detectContainer() bool {
	for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := e.Stat(p); err == nil {
			return true
		}
	}
	if b, err := e.ReadFile("/proc/1/cgroup"); err == nil {
		s := string(b)
		for _, marker := range []string{"docker", "containerd", "kubepods", "lxc", "podman"} {
			if strings.Contains(s, marker) {
				return true
			}
		}
	}
	return false
}

func parseOSRelease(s string) (id, version string) {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch k {
		case "ID":
			id = v
		case "VERSION_ID":
			version = v
		}
	}
	if id == "" {
		if name := osReleaseName(s); name != "" {
			id = name
		}
	}
	return id, version
}

func osReleaseName(s string) string {
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && k == "NAME" {
			return strings.ToLower(strings.Trim(strings.TrimSpace(v), `"'`))
		}
	}
	return ""
}

func firstExisting(lookPath func(string) (string, error), names ...string) string {
	for _, n := range names {
		if _, err := lookPath(n); err == nil {
			return n
		}
	}
	return ""
}

// NormalizeArch 把 Go 的 GOARCH 映射到本项目产物命名所用的架构标签。
//
// 它是「GOARCH → 产物命名架构标签」的唯一入口。除 arm 外，GOARCH 与产物标签
// 完全同名，原样返回；arm 需要按 GOARM 再细分，因为 armv6 与 armv7 的产物
// 互不通用，笼统给 armv7 会让 armv6 设备下载到跑不起来的二进制。
func NormalizeArch(goarch string) string {
	if goarch == "arm" {
		return armArchLabel()
	}
	return goarch
}

// SupportsArch 报告给定组件支持的架构集合是否包含目标架构。
func SupportsArch(supported []string, arch string) bool {
	if len(supported) == 0 {
		return true
	}
	for _, a := range supported {
		if a == arch {
			return true
		}
	}
	return false
}
