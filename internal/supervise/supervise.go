// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

// Package supervise 负责把装载好的组件托管给“某个进程管理器”。
//
// 托管分两条路：
//   - 系统自带：systemd / openrc / sysvinit / launchd / Windows 计划任务。
//     前四者里只有支持 respawn 的才算“完整保活”，具体判定见 platform.InitSystem。
//   - 自托管：目标系统没有原生 respawn 能力时，由运行核装载 DontCrack，
//     再由 DontCrack 托管其它常驻组件。
//
// 所有外部命令都通过 Exec 注入，文件写入通过 WriteFile 注入，因此每个 init
// 系统的单元文件都能在任意平台上被单元测试，而不需要真的去碰 /etc。
package supervise

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/platform"
)

// Exec 执行一条外部命令。
type Exec func(ctx context.Context, name string, args ...string) (string, error)

// Options 是注册一个服务所需的全部输入。
type Options struct {
	// Unit 是单元名，例如 ferc-simple-web-shell。
	Unit string
	// BinPath 是可执行文件绝对路径。
	BinPath string
	// Args 是启动参数。
	Args []string
	// Dir 是工作目录。
	Dir string
	// LogDir 是日志目录（sysvinit / DontCrack 用）。
	LogDir string
	// Description 是人类可读描述。
	Description string
}

// Result 描述注册结果。
type Result struct {
	// Supervisor 是实际承担托管的一方：systemd / openrc / sysvinit / launchd /
	// schtasks / dontcrack。空表示未托管。
	Supervisor string
	// Unit 是注册出去的单元名。
	Unit string
	// Written 是本次写出的文件（便于卸载时清理）。
	Written []string
	// Warnings 是“做了但需要人知道”的事，例如 schtasks 只能自启不能拉起。
	Warnings []string
}

// Manager 按目标平台注册服务。
type Manager struct {
	GOOS     string
	Init     platform.InitSystem
	Run      Exec
	Write    func(path string, data []byte, perm os.FileMode) error
	MkdirAll func(path string, perm os.FileMode) error
	// DontCrackBin 是已装载的 DontCrack 可执行文件路径；仅在需要自托管时使用。
	DontCrackBin string
	// DontCrackPort 是 DontCrack 的 HTTP 控制端口。
	DontCrackPort int
	// Root 是运行核的数据根目录。
	Root string
}

// New 返回绑定到真实系统的 Manager。
func New(info platform.Info, root string) *Manager {
	return &Manager{
		GOOS:     info.GOOS,
		Init:     info.Init,
		Run:      runExec,
		Write:    writeFile,
		MkdirAll: os.MkdirAll,
		Root:     root,
	}
}

func runExec(ctx context.Context, name string, args ...string) (string, error) {
	if runtime.GOOS == "windows" {
		// 计划任务与 sc 需要通过 cmd 才能稳定拿到退出码。
		cmdArgs := append([]string{"/c", name}, args...)
		return runOne(ctx, "cmd", cmdArgs...)
	}
	return runOne(ctx, name, args...)
}

func runOne(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.Output()
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func writeFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, perm)
}

// Register 按目标平台的 init 系统注册服务并尝试启动。
//
// 判定分两档：
//   - 有 respawn：交给系统自带管理器，最省事；
//   - 只有 autostart：仍走系统自带方式注册开机自启，但必须再挂上一个进程
//     管理器兜住崩溃，否则“崩了就再也不起来”；
//   - 都没有：只能靠自托管进程管理器。
func (m *Manager) Register(ctx context.Context, opt Options) (Result, error) {
	if opt.BinPath == "" {
		return Result{}, errors.New("supervise: 缺少可执行文件路径")
	}
	if m.Init.Respawn() || m.Init.Autostart() {
		return m.registerNative(ctx, opt)
	}
	return m.registerDontCrack(ctx, opt)
}

// Unregister 撤销注册。
func (m *Manager) Unregister(ctx context.Context, res Result) error {
	var errs []error
	switch res.Supervisor {
	case "systemd":
		if _, err := m.Run(ctx, "systemctl", "disable", "--now", res.Unit); err != nil {
			errs = append(errs, err)
		}
		if err := os.Remove(unitPath(res.Unit)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
		if _, err := m.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			errs = append(errs, err)
		}
	case "openrc":
		if _, err := m.Run(ctx, "rc-service", res.Unit, "stop"); err != nil {
			errs = append(errs, err)
		}
		if _, err := m.Run(ctx, "rc-update", "del", res.Unit, "default"); err != nil {
			errs = append(errs, err)
		}
	case "sysvinit":
		if _, err := m.Run(ctx, "service", res.Unit, "stop"); err != nil {
			errs = append(errs, err)
		}
	case "launchd":
		if _, err := m.Run(ctx, "launchctl", "unload", launchdPath(res.Unit)); err != nil {
			errs = append(errs, err)
		}
	case "schtasks":
		if _, err := m.Run(ctx, "schtasks", "/Delete", "/TN", res.Unit, "/F"); err != nil {
			errs = append(errs, err)
		}
	case "dontcrack":
		// DontCrack 由外部 HTTP 接口管理；这里只清理我们写的启动配置。
	}
	for _, p := range res.Written {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func unitPath(unit string) string {
	return filepath.Join("/etc/systemd/system", unit+".service")
}

func launchdPath(label string) string {
	return filepath.Join("/Library/LaunchDaemons", label+".plist")
}

// registerNative 走系统自带的 init。
func (m *Manager) registerNative(ctx context.Context, opt Options) (Result, error) {
	switch m.Init {
	case platform.InitSystemd:
		return m.registerSystemd(ctx, opt)
	case platform.InitOpenRC:
		return m.registerOpenRC(ctx, opt)
	case platform.InitLaunchd:
		return m.registerLaunchd(ctx, opt)
	case platform.InitSysV:
		return m.registerSysV(ctx, opt)
	case platform.InitWindowsSchtasks:
		return m.registerSchtasks(ctx, opt)
	default:
		return m.registerDontCrack(ctx, opt)
	}
}

// registerSchtasks 用计划任务做开机自启，并额外用 DontCrack 兜住崩溃。
// 只注册计划任务等于“能起来但崩了没人管”，与论文里“节点以后几乎不用管”
// 的要求不符，所以这里必须双保险。
func (m *Manager) registerSchtasks(ctx context.Context, opt Options) (Result, error) {
	res, err := m.registerDontCrack(ctx, opt)
	if err != nil {
		return Result{}, err
	}
	if _, err := m.Run(ctx, "schtasks", renderSchtasksCommand(opt)...); err != nil {
		res.Warnings = append(res.Warnings, "计划任务注册失败，开机自启可能未生效: "+err.Error())
		return res, nil
	}
	res.Warnings = append(res.Warnings, "已注册计划任务开机自启；崩溃拉起由 DontCrack 负责")
	return res, nil
}

func (m *Manager) registerSystemd(ctx context.Context, opt Options) (Result, error) {
	unit := opt.Unit
	path := unitPath(unit)
	body := renderSystemdUnit(unit, opt)
	if err := m.Write(path, []byte(body), 0o644); err != nil {
		return Result{}, fmt.Errorf("supervise: 写 systemd unit 失败: %w", err)
	}
	res := Result{Supervisor: "systemd", Unit: unit, Written: []string{path}}
	if _, err := m.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return res, fmt.Errorf("supervise: systemctl daemon-reload 失败: %w", err)
	}
	if _, err := m.Run(ctx, "systemctl", "enable", "--now", unit); err != nil {
		return res, fmt.Errorf("supervise: systemctl enable --now %s 失败: %w", unit, err)
	}
	return res, nil
}

// renderSystemdUnit 生成 unit 文件内容。抽成纯函数便于断言。
func renderSystemdUnit(unit string, opt Options) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	fmt.Fprintf(&b, "Description=%s\n", orDefault(opt.Description, unit))
	b.WriteString("After=network-online.target\nWants=network-online.target\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	if opt.Dir != "" {
		fmt.Fprintf(&b, "WorkingDirectory=%s\n", opt.Dir)
	}
	fmt.Fprintf(&b, "ExecStart=%s\n", execLine(opt.BinPath, opt.Args))
	b.WriteString("Restart=always\n")
	b.WriteString("RestartSec=5s\n")
	b.WriteString("KillMode=mixed\n")
	b.WriteString("TimeoutStopSec=10s\n\n")
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")
	return b.String()
}

func (m *Manager) registerOpenRC(ctx context.Context, opt Options) (Result, error) {
	path := filepath.Join("/etc/init.d", opt.Unit)
	body := renderOpenRCScript(opt.Unit, opt)
	if err := m.Write(path, []byte(body), 0o755); err != nil {
		return Result{}, fmt.Errorf("supervise: 写 openrc 脚本失败: %w", err)
	}
	res := Result{Supervisor: "openrc", Unit: opt.Unit, Written: []string{path}}
	if _, err := m.Run(ctx, "rc-update", "add", opt.Unit, "default"); err != nil {
		return res, fmt.Errorf("supervise: rc-update add 失败: %w", err)
	}
	if _, err := m.Run(ctx, "rc-service", opt.Unit, "start"); err != nil {
		return res, fmt.Errorf("supervise: rc-service start 失败: %w", err)
	}
	return res, nil
}

func renderOpenRCScript(unit string, opt Options) string {
	var b strings.Builder
	b.WriteString("#!/sbin/openrc-run\n")
	fmt.Fprintf(&b, "name=%q\n", orDefault(opt.Description, unit))
	fmt.Fprintf(&b, "command=%q\n", opt.BinPath)
	if len(opt.Args) > 0 {
		fmt.Fprintf(&b, "command_args=%q\n", strings.Join(opt.Args, " "))
	}
	if opt.Dir != "" {
		fmt.Fprintf(&b, "directory=%q\n", opt.Dir)
	}
	b.WriteString("supervisor=\"supervise-daemon\"\n")
	b.WriteString("respawn_delay=5\n")
	b.WriteString("respawn_max=0\n\n")
	b.WriteString("depend() {\n\tneed net\n}\n")
	return b.String()
}

func (m *Manager) registerLaunchd(ctx context.Context, opt Options) (Result, error) {
	label := opt.Unit
	path := launchdPath(label)
	body := renderLaunchdPlist(label, opt)
	if err := m.Write(path, []byte(body), 0o644); err != nil {
		return Result{}, fmt.Errorf("supervise: 写 launchd plist 失败: %w", err)
	}
	res := Result{Supervisor: "launchd", Unit: label, Written: []string{path}}
	if _, err := m.Run(ctx, "launchctl", "load", "-w", path); err != nil {
		return res, fmt.Errorf("supervise: launchctl load 失败: %w", err)
	}
	return res, nil
}

func renderLaunchdPlist(label string, opt Options) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	fmt.Fprintf(&b, "  <key>Label</key><string>%s</string>\n", label)
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	fmt.Fprintf(&b, "    <string>%s</string>\n", opt.BinPath)
	for _, a := range opt.Args {
		fmt.Fprintf(&b, "    <string>%s</string>\n", a)
	}
	b.WriteString("  </array>\n")
	if opt.Dir != "" {
		fmt.Fprintf(&b, "  <key>WorkingDirectory</key><string>%s</string>\n", opt.Dir)
	}
	b.WriteString("  <key>RunAtLoad</key><true/>\n")
	b.WriteString("  <key>KeepAlive</key><true/>\n")
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

func (m *Manager) registerSysV(ctx context.Context, opt Options) (Result, error) {
	// sysvinit 本身没有 respawn，只能自启。因此除了写 init 脚本，还要外挂
	// DontCrack 兜住崩溃——这正是“没有原生进程管理器就自动装一个”的含义。
	path := filepath.Join("/etc/init.d", opt.Unit)
	body := renderSysVScript(opt.Unit, opt)
	if err := m.Write(path, []byte(body), 0o755); err != nil {
		return Result{}, fmt.Errorf("supervise: 写 sysvinit 脚本失败: %w", err)
	}
	res, err := m.registerDontCrack(ctx, opt)
	if err != nil {
		return Result{Supervisor: "sysvinit", Unit: opt.Unit, Written: []string{path}}, err
	}
	res.Written = append(res.Written, path)
	res.Warnings = append(res.Warnings, "sysvinit 无 respawn，已额外用 DontCrack 兜住崩溃")
	if _, err := m.Run(ctx, "update-rc.d", opt.Unit, "defaults"); err != nil {
		res.Warnings = append(res.Warnings, "update-rc.d 未成功，开机自启可能未生效: "+err.Error())
	}
	return res, nil
}

func renderSysVScript(unit string, opt Options) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("### BEGIN INIT INFO\n")
	fmt.Fprintf(&b, "# Provides:          %s\n", unit)
	b.WriteString("# Required-Start:    $network\n")
	b.WriteString("# Required-Stop:     $network\n")
	b.WriteString("# Default-Start:     2 3 4 5\n")
	b.WriteString("# Default-Stop:      0 1 6\n")
	b.WriteString("### END INIT INFO\n\n")
	fmt.Fprintf(&b, "PIDFILE=/var/run/%s.pid\n", unit)
	fmt.Fprintf(&b, "BIN=%q\n", opt.BinPath)
	args := ""
	if len(opt.Args) > 0 {
		args = " " + shellQuoteAll(opt.Args)
	}
	fmt.Fprintf(&b, "ARGS=%q\n", strings.TrimSpace(args))
	b.WriteString(`
case "$1" in
  start)
    "$BIN" $ARGS &
    echo $! > "$PIDFILE"
    ;;
  stop)
    [ -f "$PIDFILE" ] && kill "$(cat "$PIDFILE")" 2>/dev/null
    rm -f "$PIDFILE"
    ;;
  restart)
    "$0" stop; sleep 1; "$0" start
    ;;
esac
exit 0
`)
	return b.String()
}

// registerDontCrack 让 DontCrack 托管目标组件，并产出可直接执行的命令行。
func (m *Manager) registerDontCrack(ctx context.Context, opt Options) (Result, error) {
	if m.DontCrackBin == "" {
		return Result{}, errors.New("supervise: 当前系统没有可用的进程保活能力，且未装载进程管理器")
	}
	port := m.DontCrackPort
	if port == 0 {
		port = 11883
	}
	logDir := opt.LogDir
	if logDir == "" {
		logDir = filepath.Join(m.Root, "logs")
	}
	if m.MkdirAll != nil {
		if err := m.MkdirAll(logDir, 0o755); err != nil {
			return Result{}, fmt.Errorf("supervise: 建日志目录失败: %w", err)
		}
	}
	args := []string{
		"-path", opt.BinPath,
		"-auto-restart", "-max-retries", "-1",
		"-start-now",
		"-file-log", "-log-path", logDir,
		"-port", fmt.Sprint(port),
	}
	if len(opt.Args) > 0 {
		// DontCrack 的 -args 遇到以 - 开头的值会被 flag 包吞并，必须用等号形式。
		args = append(args, "-args="+strings.Join(opt.Args, " "))
	}
	if _, err := m.Run(ctx, m.DontCrackBin, args...); err != nil {
		return Result{}, fmt.Errorf("supervise: 启动进程管理器托管 %s 失败: %w", opt.Unit, err)
	}
	return Result{Supervisor: "dontcrack", Unit: opt.Unit}, nil
}

// renderSchtasksCommand 生成 Windows 计划任务注册命令。
// 计划任务只能保证开机自启，不能保证崩溃拉起，因此同时要求 DontCrack 兜底。
func renderSchtasksCommand(opt Options) []string {
	bind := fmt.Sprintf(`\"%s\" %s`, opt.BinPath, shellQuoteAll(opt.Args))
	return []string{
		"/Create", "/F",
		"/TN", opt.Unit,
		"/SC", "ONSTART",
		"/RU", "SYSTEM",
		"/TR", bind,
	}
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func execLine(bin string, args []string) string {
	if len(args) == 0 {
		return bin
	}
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, systemdEscape(bin))
	for _, a := range args {
		parts = append(parts, systemdEscape(a))
	}
	return strings.Join(parts, " ")
}

// systemdEscape 按 systemd 的 ExecStart 规则给参数加引号。
func systemdEscape(s string) string {
	if s == "" || strings.ContainsAny(s, " \t\"'\\$") {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
	}
	return s
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\"'$`\\&|;<>()*?[]#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func shellQuoteAll(xs []string) string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, shellQuote(x))
	}
	return strings.Join(out, " ")
}
