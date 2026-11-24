// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

// Package boot 回答唯一一个问题：为了在开机器上自动跑一次
// `ferc restore --root <root>`，应该往哪里写什么文件、执行什么命令。
//
// 运行核不常驻，它的全部记忆是磁盘上的 ferc-state.json；组件自己由 init 或
// DontCrack 托管。但在没有真正 init 的系统（FasterEdgeOS 只有 BusyBox init 与
// /etc/autorun/*）上，重启后没人把托管关系重放一遍，组件就不会自己回来。
// 本包把“开机自启落在哪里”收敛成一份纯数据方案：它不写文件、不跑命令、
// 不碰网络，因此可以在任意平台上被单元测试，也便于 CLI 先打印再执行。
package boot

import (
	"os"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/platform"
)

// Mechanism 是开机自启的落点。
type Mechanism string

const (
	// MechanismSystemd 用 /etc/systemd/system 下的 oneshot 单元。
	MechanismSystemd Mechanism = "systemd"
	// MechanismOpenRC 用 /etc/init.d 下的 openrc-run 脚本。
	MechanismOpenRC Mechanism = "openrc"
	// MechanismSysV 用 /etc/init.d 下的 LSB 脚本。
	MechanismSysV Mechanism = "sysvinit"
	// MechanismLaunchd 用 /Library/LaunchDaemons 下的 plist。
	MechanismLaunchd Mechanism = "launchd"
	// MechanismSchtasks 用 Windows 计划任务。
	MechanismSchtasks Mechanism = "schtasks"
	// MechanismAutorun 用 /etc/autorun/NN_*.sh（BusyBox init、FasterEdgeOS 约定）。
	// 这类系统按文件名序执行脚本，没有 /etc/init.d，也没有 service 命令。
	MechanismAutorun Mechanism = "autorun"
	// MechanismNone 表示本平台没有任何可用的开机自启机制。
	MechanismNone Mechanism = "none"
)

// 落点常量。导出是为了让 CLI 能把“文件会写到哪里”直接打印给人看，
// 而不是在别处再抄一份字面量。
const (
	// ServiceName 是开机自启单元/脚本的名字，也是 Windows 计划任务名。
	ServiceName = "ferc-restore"
	// LaunchdLabel 是 launchd 的 label，和文件名保持一致。
	LaunchdLabel = "com.fasteredge.ferc-restore"
	// SystemdUnitPath 是 systemd oneshot 单元的落点。
	SystemdUnitPath = "/etc/systemd/system/" + ServiceName + ".service"
	// InitdScriptPath 是 openrc / sysvinit 共用的脚本落点。
	InitdScriptPath = "/etc/init.d/" + ServiceName
	// LaunchdPlistPath 是 launchd plist 的落点。
	LaunchdPlistPath = "/Library/LaunchDaemons/" + LaunchdLabel + ".plist"
	// DefaultAutorunDir 是 autorun 约定目录。
	DefaultAutorunDir = "/etc/autorun"
	// AutorunFileName 是 autorun 脚本名。前缀 30_ 让它严格排在
	// NetworkAutorunFileName 之后：字符串序 "30_..." > "20_..."。
	AutorunFileName = "30_ferc-restore.sh"
	// NetworkAutorunFileName 是网络脚本名，用来钉住“restore 必须晚于网络”。
	NetworkAutorunFileName = "20_network.sh"
)

// File 是一个需要写到磁盘上的文件。Body 是最终字节，调用方原样落盘即可。
type File struct {
	Path string
	Body []byte
	Mode os.FileMode
}

// Command 是一条需要执行的命令。
type Command struct {
	Name string
	Args []string
}

// Plan 是一次“启用/禁用开机自启”的完整方案。
//
// 启用时用 Files + Commands；禁用时用 Remove + RemoveCmd。两组字段总是同时
// 填好，这样调用方既能 install 也能 uninstall，而方案本身仍然是纯数据。
type Plan struct {
	Mechanism Mechanism
	Files     []File
	Commands  []Command
	Remove    []string
	RemoveCmd []Command
	Warnings  []string
}

// Options 是生成方案所需的输入。零值可用：所有字段都有保守的默认行为。
type Options struct {
	// Root 是运行核数据根目录，会被写进 restore 的命令行。
	// 为空时省略 --root，让 restore 用它自己的默认根目录（并给出警告）。
	Root string
	// FercPath 是 ferc 自身的绝对路径。为空时退回 "ferc"，靠 PATH 查找，
	// 这在开机脚本里通常不可靠，因此会给出警告。
	FercPath string
	// AutorunDir 是 autorun 目录，为空时用 DefaultAutorunDir。
	AutorunDir string
	// HasAutorunDir 探测某个目录是否存在。测试与 CLI 各自注入真实实现。
	// 为 nil 表示“无法探测”，此时按 linux 约定直接落盘到 autorun 目录。
	HasAutorunDir func(string) bool
	// AllowAutorunFallback 允许在探测到 autorun 目录不存在时仍然按该约定落盘。
	// FasterEdgeOS 的镜像可能尚未建出该目录，但运行时一定会有（init 会扫它），
	// 因此调用方可以显式打开这个回退。
	AllowAutorunFallback bool
}

// withDefaults 补齐默认值，保证 Build 幂等。
func (o Options) withDefaults() Options {
	if o.AutorunDir == "" {
		o.AutorunDir = DefaultAutorunDir
	}
	if o.FercPath == "" {
		o.FercPath = "ferc"
	}
	return o
}

// Build 生成启用方案。纯函数：同样的输入必然得到同样的字节。
//
// 落点选择顺序：windows → schtasks；darwin → launchd；其余系统看 init：
// systemd / openrc / sysvinit 走各自的单元；没有 init（或无法识别）时走
// autorun 约定，实在没有可用落点就返回 MechanismNone 并说明后果。
func Build(goos string, init platform.InitSystem, opt Options) Plan {
	opt = opt.withDefaults()
	var p Plan
	switch choose(goos, init, opt) {
	case MechanismSystemd:
		p = systemdPlan(opt)
	case MechanismOpenRC:
		p = openRCPlan(opt)
	case MechanismSysV:
		p = sysvPlan(opt)
	case MechanismLaunchd:
		p = launchdPlan(opt)
	case MechanismSchtasks:
		p = schtasksPlan(opt)
	case MechanismAutorun:
		p = autorunPlan(opt)
	default:
		p = nonePlan(opt)
	}
	p.Warnings = append(baseWarnings(opt), p.Warnings...)
	return p
}

// choose 是落点判定，单独抽出来便于测试穷举。
func choose(goos string, init platform.InitSystem, opt Options) Mechanism {
	switch goos {
	case "windows":
		// Windows 没有 init 目录可写，开机自启只有计划任务这一条路。
		return MechanismSchtasks
	case "darwin":
		return MechanismLaunchd
	case "linux":
		switch init {
		case platform.InitSystemd:
			return MechanismSystemd
		case platform.InitOpenRC:
			return MechanismOpenRC
		case platform.InitSysV:
			return MechanismSysV
		}
		return chooseNoInit(opt)
	default:
		// 其它 unix（*BSD 等）没有我们实现的落点：宁可明确说“没有”，
		// 也不要往一个不对的地方写文件假装成功。
		return MechanismNone
	}
}

// chooseNoInit 处理“没有真正 init”的机器：Linux 上优先按 autorun 约定落盘。
func chooseNoInit(opt Options) Mechanism {
	switch {
	case opt.HasAutorunDir == nil:
		// 无法探测，但 init 已经判定为 none：FasterEdgeOS 这类系统只有
		// /etc/autorun，直接按约定落盘比什么都不做更接近可用。
		return MechanismAutorun
	case opt.HasAutorunDir(opt.AutorunDir):
		return MechanismAutorun
	case opt.AllowAutorunFallback:
		return MechanismAutorun
	default:
		return MechanismNone
	}
}

// baseWarnings 汇总“输入不完整”带来的风险，与具体落点无关。
func baseWarnings(opt Options) []string {
	var out []string
	if opt.FercPath == "ferc" {
		out = append(out, "未提供 ferc 自身的绝对路径，开机自启将依赖 PATH 中的 ferc；"+
			"开机脚本里的 PATH 通常很短，建议用 --ferc 之类的方式显式指定绝对路径")
	}
	if opt.Root == "" {
		out = append(out, "未提供数据根目录，命令行里省略了 --root，restore 将使用自己的默认根目录；"+
			"若 ferc-state.json 不在默认位置，开机恢复会找不到托管记录")
	}
	return out
}
