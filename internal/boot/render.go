// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

package boot

import (
	"fmt"
	"os"
	"strings"
)

// 各机制共用的文件权限。
const (
	// FileModeUnit 是 systemd 单元与 launchd plist 的权限：不含密钥，可读即可。
	FileModeUnit os.FileMode = 0o644
	// FileModeScript 是 init 脚本与 autorun 脚本的权限：init 只执行可执行文件，
	// 少了 0755 脚本会被静默跳过，那等于没写。
	FileModeScript os.FileMode = 0o755
)

// ── 命令行 ────────────────────────────────────────────────────

// restoreArgs 返回要写进自启的完整参数（不含 ferc 自身路径）。
//
// 用分离的 "--root <dir>" 而不是 "--root=<dir>"：flag 与 Go 的 flag 包都接受
// 两种形式，而分离形式在各 init 的引号规则下更少歧义。
func restoreArgs(opt Options) []string {
	args := []string{"restore"}
	if opt.Root != "" {
		args = append(args, "--root", opt.Root)
	}
	return args
}

// ── systemd ──────────────────────────────────────────────────

func systemdPlan(opt Options) Plan {
	args := append([]string{opt.FercPath}, restoreArgs(opt)...)
	unit := strings.Join([]string{
		"[Unit]",
		"Description=FasterEdge 运行核：开机重放组件托管关系",
		"After=network-online.target",
		"Wants=network-online.target",
		"",
		"[Service]",
		"# restore 是一次性动作：重放托管关系后立刻退出，不留常驻进程。",
		"Type=oneshot",
		"ExecStart=" + systemdJoin(args),
		"",
		"[Install]",
		"WantedBy=multi-user.target",
		"",
	}, "\n")
	return Plan{
		Mechanism: MechanismSystemd,
		Files:     []File{{Path: SystemdUnitPath, Body: []byte(unit), Mode: FileModeUnit}},
		Commands: []Command{
			{Name: "systemctl", Args: []string{"daemon-reload"}},
			{Name: "systemctl", Args: []string{"enable", ServiceName + ".service"}},
		},
		Remove: []string{SystemdUnitPath},
		RemoveCmd: []Command{
			{Name: "systemctl", Args: []string{"disable", "--now", ServiceName + ".service"}},
			{Name: "systemctl", Args: []string{"daemon-reload"}},
		},
	}
}

// ── openrc ───────────────────────────────────────────────────

func openRCPlan(opt Options) Plan {
	args := restoreArgs(opt)
	var b strings.Builder
	b.WriteString("#!/sbin/openrc-run\n")
	b.WriteString("description=\"FasterEdge 运行核：开机重放组件托管关系\"\n")
	fmt.Fprintf(&b, "command=%s\n", shQuote(opt.FercPath))
	// restore 是一次性动作：跑完就退出，不留常驻进程。
	b.WriteString("command_background=false\n")
	b.WriteString("start_inactive=true\n\n")
	b.WriteString("depend() {\n\tneed net\n}\n\n")
	// 这里不用 command_args：openrc 把它的值按空白切词，引号不会再被解释，
	// 含空格的路径会因此被拆成两段。直接在 start() 里执行已正确引用的命令行，
	// 才能同时兼容 /opt/Fast Edge/ferc 这类路径。
	b.WriteString("start() {\n\tebegin \"重放组件托管关系\"\n")
	fmt.Fprintf(&b, "\t%s %s\n", shQuote(opt.FercPath), shQuoteAll(args))
	b.WriteString("\teend $?\n}\n\n")
	// 没有常驻进程可停，但必须显式给出 stop()：否则 openrc 会去找 pidfile
	// 并把“停不掉”报成失败。
	b.WriteString("stop() {\n\tebegin \"无需停止（一次性任务）\"\n\teend 0\n}\n")
	return Plan{
		Mechanism: MechanismOpenRC,
		Files:     []File{{Path: InitdScriptPath, Body: []byte(b.String()), Mode: FileModeScript}},
		Commands: []Command{
			{Name: "rc-update", Args: []string{"add", ServiceName, "default"}},
		},
		Remove:    []string{InitdScriptPath},
		RemoveCmd: []Command{{Name: "rc-update", Args: []string{"del", ServiceName, "default"}}},
	}
}

// ── sysvinit ─────────────────────────────────────────────────

func sysvPlan(opt Options) Plan {
	args := restoreArgs(opt)
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("### BEGIN INIT INFO\n")
	fmt.Fprintf(&b, "# Provides:          %s\n", ServiceName)
	b.WriteString("# Required-Start:    $network $remote_fs\n")
	b.WriteString("# Required-Stop:     $network $remote_fs\n")
	b.WriteString("# Default-Start:     2 3 4 5\n")
	b.WriteString("# Default-Stop:      0 1 6\n")
	b.WriteString("# Short-Description: FasterEdge 运行核开机重放组件托管关系\n")
	b.WriteString("# Description:       重放 ferc-state.json 里的托管关系，使组件重启后自动回来。\n")
	b.WriteString("### END INIT INFO\n\n")
	b.WriteString("# 由 ferc 生成：不要在注释之外改动下面这行，重新生成会覆盖。\n")
	b.WriteString("case \"$1\" in\n")
	fmt.Fprintf(&b, "  start)\n    %s %s\n", shQuote(opt.FercPath), shQuoteAll(args))
	b.WriteString("    ;;\n")
	b.WriteString("  stop)\n    # restore 是一次性任务，没有常驻进程可停。\n    :\n    ;;\n")
	b.WriteString("  restart|force-reload)\n    \"$0\" start\n    ;;\n")
	b.WriteString("  status)\n    # LSB 要求 status 常驻进程时给出退出码；这里没有常驻进程。\n")
	b.WriteString("    echo \"ferc-restore 是一次性任务，没有常驻进程\"\n    ;;\n")
	b.WriteString("  *)\n    echo \"用法: $0 {start|stop|restart|status}\" >&2\n    exit 2\n    ;;\n")
	b.WriteString("esac\n")
	b.WriteString("exit 0\n")
	return Plan{
		Mechanism: MechanismSysV,
		Files:     []File{{Path: InitdScriptPath, Body: []byte(b.String()), Mode: FileModeScript}},
		Commands: []Command{
			{Name: "update-rc.d", Args: []string{ServiceName, "defaults"}},
		},
		Remove: []string{InitdScriptPath},
		// -f：脚本可能已经被手工删掉或本来就不完整，强制摘掉运行级链接。
		RemoveCmd: []Command{{Name: "update-rc.d", Args: []string{"-f", ServiceName, "remove"}}},
	}
}

// ── launchd ──────────────────────────────────────────────────

func launchdPlan(opt Options) Plan {
	args := append([]string{opt.FercPath}, restoreArgs(opt)...)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	fmt.Fprintf(&b, "  <key>Label</key><string>%s</string>\n", xmlEscape(LaunchdLabel))
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, a := range args {
		fmt.Fprintf(&b, "    <string>%s</string>\n", xmlEscape(a))
	}
	b.WriteString("  </array>\n")
	b.WriteString("  <key>RunAtLoad</key><true/>\n")
	// restore 跑完就退出，不需要 KeepAlive；重复拉起只会空转。
	b.WriteString("  <key>LaunchOnlyOnce</key><true/>\n")
	b.WriteString("  <key>StandardOutPath</key><string>/var/log/ferc-restore.log</string>\n")
	b.WriteString("  <key>StandardErrorPath</key><string>/var/log/ferc-restore.err</string>\n")
	b.WriteString("</dict>\n</plist>\n")
	return Plan{
		Mechanism: MechanismLaunchd,
		Files:     []File{{Path: LaunchdPlistPath, Body: []byte(b.String()), Mode: FileModeUnit}},
		Commands:  []Command{{Name: "launchctl", Args: []string{"load", "-w", LaunchdPlistPath}}},
		Remove:    []string{LaunchdPlistPath},
		RemoveCmd: []Command{{Name: "launchctl", Args: []string{"unload", "-w", LaunchdPlistPath}}},
	}
}

// ── schtasks ─────────────────────────────────────────────────

func schtasksPlan(opt Options) Plan {
	args := append([]string{opt.FercPath}, restoreArgs(opt)...)
	return Plan{
		Mechanism: MechanismSchtasks,
		Commands: []Command{{
			Name: "schtasks",
			Args: []string{
				"/Create", "/F",
				"/TN", ServiceName,
				"/SC", "ONSTART",
				"/RU", "SYSTEM",
				"/TR", winCommandLine(args),
			},
		}},
		RemoveCmd: []Command{{
			Name: "schtasks",
			Args: []string{"/Delete", "/TN", ServiceName, "/F"},
		}},
		Warnings: []string{
			"计划任务只能做到“开机启动一次”，不负责拉起崩溃的进程；" +
				"常驻组件的崩溃拉起仍由 ferc 装载时注册的托管方负责",
		},
	}
}

// ── autorun（BusyBox init / FasterEdgeOS） ────────────────────

func autorunPlan(opt Options) Plan {
	args := restoreArgs(opt)
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# 由 ferc 写入。BusyBox init 按文件名序执行 /etc/autorun/*.sh，\n")
	b.WriteString("# 本脚本排在 " + NetworkAutorunFileName + " 之后：restore 要探端口、注册服务，\n")
	b.WriteString("# 网络还没起来就跑它会连不上任何东西。\n")
	b.WriteString("# 不用 exec：后面的 autorun 脚本还要继续执行。\n")
	fmt.Fprintf(&b, "%s %s\n", shQuote(opt.FercPath), shQuoteAll(args))
	path := autorunPath(opt)
	return Plan{
		Mechanism: MechanismAutorun,
		Files:     []File{{Path: path, Body: []byte(b.String()), Mode: FileModeScript}},
		// autorun 没有注册命令：init 每次开机扫目录执行，写完文件就算装好。
		Remove: []string{path},
	}
}

// autorunPath 把脚本放到约定目录里，并保证文件名排在网络脚本之后。
func autorunPath(opt Options) string {
	dir := strings.TrimRight(opt.AutorunDir, `/\`)
	if dir == "" {
		dir = DefaultAutorunDir
	}
	return dir + "/" + AutorunFileName
}

// ── none ─────────────────────────────────────────────────────

func nonePlan(Options) Plan {
	return Plan{
		Mechanism: MechanismNone,
		Warnings: []string{
			"本平台没有可用的开机自启机制（既无 systemd / openrc / sysvinit / launchd，" +
				"也未提供 /etc/autorun 目录），重启后需手工执行一次 ferc restore --root <root>，" +
				"否则已装载的组件不会自己回来",
		},
	}
}

// ── 引用与转义 ───────────────────────────────────────────────

// shQuote 用单引号包住一段文本，供 sh / BusyBox ash / openrc-run 解释。
// 单引号内除单引号本身外一切字符都是字面量，因此只需把 ' 换成 '\”。
func shQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\"'`$\\&|;<>()*?[]{}!#~^") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shQuoteAll 对一组参数逐个引用后拼接。
func shQuoteAll(args []string) string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		out = append(out, shQuote(a))
	}
	return strings.Join(out, " ")
}

// systemdJoin 按 ExecStart 的规则拼接命令行。
//
// systemd 先按空白切分、再解引号，所以含空格/引号/反斜杠/美元号的参数必须
// 用双引号包起来，且内部的 \ 与 " 要各自转义；% 还要写成 %% 才是字面量
// （否则会被当成 specifier 展开）。
func systemdJoin(args []string) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, systemdQuote(a))
	}
	return strings.Join(parts, " ")
}

func systemdQuote(s string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`).Replace(s)
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\n\"'\\$%") {
		return escaped
	}
	return `"` + escaped + `"`
}

// xmlEscape 转义 XML 文本节点里的 & < > 与引号，避免 plist 解析失败。
func xmlEscape(s string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	).Replace(s)
}

// winCommandLine 拼出 Windows 计划任务 /TR 用的命令行：可执行文件总带双引号，
// 参数里含空格或引号的再用双引号包起来，内层引号按 CommandLineToArgvW 的
// 规则用 \" 转义。
func winCommandLine(args []string) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, winQuote(a))
	}
	return strings.Join(parts, " ")
}

func winQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"") {
		return s
	}
	inner := strings.ReplaceAll(s, `"`, `\"`)
	// 结尾的反斜杠会和闭合引号粘成 \"，必须加倍。
	if strings.HasSuffix(inner, `\`) {
		inner += `\`
	}
	return `"` + inner + `"`
}
