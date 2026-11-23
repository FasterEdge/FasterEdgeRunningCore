// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

// Package cli 实现 ferc 的子命令。
//
// 运行核不是一个常驻进程：它每次被调用都做完一件事就退出。因此这里没有
// 事件循环、没有守护逻辑，唯一的“状态”是磁盘上的 ferc-state.json。
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/catalog"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/fetch"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/install"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/platform"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/resolve"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/state"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/supervise"
)

// Version 由构建时注入；默认值表示本地开发构建。
var Version = "dev"

// Env 是 CLI 依赖的外部环境，便于测试替换。
type Env struct {
	Stdout io.Writer
	Stderr io.Writer
	// DefaultRoot 是数据根目录；为空时按平台推导。
	DefaultRoot string
	// Token 是 GitHub token。
	Token string
	// Platform 探测函数。
	Detect func(ctx context.Context) (platform.Info, error)
	// Fetcher 允许测试注入假的 GitHub。
	Fetcher *fetch.Client
	// Supervisor 允许测试注入假的 init 操作。
	Supervisor func(info platform.Info, root string) *supervise.Manager
	// Getenv 读取环境变量。
	Getenv func(string) string
}

func (e *Env) stdout() io.Writer {
	if e.Stdout != nil {
		return e.Stdout
	}
	return os.Stdout
}

func (e *Env) stderr() io.Writer {
	if e.Stderr != nil {
		return e.Stderr
	}
	return os.Stderr
}

// DefaultRoot 返回平台默认的数据根目录。
//
// 选 /opt/FasterEdgeRunningCore 是因为它符合 Linux 的本地软件安装惯例，
// 又不与任何发行版包管理器的目录冲突；Windows 用 ProgramData 以获得
// 对 SYSTEM 账户可见、对普通用户可读的稳定位置。
func DefaultRoot(goos string) string {
	if goos == "windows" {
		pd := os.Getenv("ProgramData")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		return filepath.Join(pd, "FasterEdgeRunningCore")
	}
	return "/opt/FasterEdgeRunningCore"
}

// Run 解析并执行子命令，返回进程退出码。
func Run(ctx context.Context, args []string, env Env) int {
	if env.Getenv == nil {
		env.Getenv = os.Getenv
	}
	if len(args) == 0 {
		printUsage(env.stderr())
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "help", "-h", "--help":
		printUsage(env.stdout())
		return 0
	case "version", "--version":
		fmt.Fprintf(env.stdout(), "ferc %s (%s/%s)\n", Version, runtime.GOOS, runtime.GOARCH)
		return 0
	case "detect":
		return cmdDetect(ctx, rest, env)
	case "list":
		return cmdList(ctx, rest, env)
	case "bootstrap":
		return cmdBootstrap(ctx, rest, env)
	case "install":
		return cmdInstall(ctx, rest, env)
	case "status":
		return cmdStatus(ctx, rest, env)
	case "verify":
		return cmdVerify(ctx, rest, env)
	case "remove":
		return cmdRemove(ctx, rest, env)
	default:
		fmt.Fprintf(env.stderr(), "未知子命令 %q\n\n", cmd)
		printUsage(env.stderr())
		return 2
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `ferc —— FasterEdge 运行核（单点式通用统一管理部署工具）

用法:
  ferc bootstrap [选项]        一键装载默认组件并把节点配置完成
  ferc install  <组件...>      按需装载组件（应用商店式）
  ferc list                    列出可装载组件
  ferc status                  查看本机已装载组件
  ferc verify                  检查已装载组件是否健康
  ferc remove   <组件...>      卸载组件
  ferc detect                  打印平台探测结果
  ferc version                 打印版本

通用选项:
  --root <dir>        数据根目录（默认 `+DefaultRoot(runtime.GOOS)+`）
  --token <token>     GitHub token（默认取 GH_TOKEN / GITHUB_TOKEN）
  --dry-run           只打印计划，不下载不落盘
  --json              机器可读输出
  --catalog <file>    用外部清单覆盖内置清单
  --no-start          只落盘，不注册服务/不托管
  --allow-conflicts   把端口/互斥冲突降级为告警

运行核不是常驻进程：它装完就退出，节点后续由系统 init 或已装载的
进程管理器（DontCrack）托管。
`)
}

// common 收集所有子命令共享的选项。
type common struct {
	root           string
	token          string
	dryRun         bool
	json           bool
	catalogPath    string
	noStart        bool
	allowConflicts bool
	timeout        time.Duration
}

func bindCommon(fs *flag.FlagSet, c *common, env Env) {
	def := env.DefaultRoot
	if def == "" {
		def = DefaultRoot(runtime.GOOS)
	}
	fs.StringVar(&c.root, "root", def, "数据根目录")
	fs.StringVar(&c.token, "token", envToken(env), "GitHub token")
	fs.BoolVar(&c.dryRun, "dry-run", false, "只打印计划，不下载不落盘")
	fs.BoolVar(&c.json, "json", false, "机器可读输出")
	fs.StringVar(&c.catalogPath, "catalog", "", "外部清单文件")
	fs.BoolVar(&c.noStart, "no-start", false, "只落盘，不注册服务")
	fs.BoolVar(&c.allowConflicts, "allow-conflicts", false, "把冲突降级为告警")
	fs.DurationVar(&c.timeout, "timeout", 30*time.Minute, "整体超时")
}

func envToken(env Env) string {
	if env.Token != "" {
		return env.Token
	}
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN", "FERC_TOKEN"} {
		if v := env.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func (c *common) loadCatalog() (*catalog.Catalog, error) {
	if c.catalogPath == "" {
		return catalog.Builtin()
	}
	b, err := os.ReadFile(c.catalogPath)
	if err != nil {
		return nil, fmt.Errorf("读清单 %s 失败: %w", c.catalogPath, err)
	}
	return catalog.Load(b)
}

func (env Env) detect(ctx context.Context) (platform.Info, error) {
	if env.Detect != nil {
		return env.Detect(ctx)
	}
	return platform.Default().Detect(ctx)
}

func (env Env) fetcher(c *common) *fetch.Client {
	if env.Fetcher != nil {
		return env.Fetcher
	}
	return fetch.New(c.token)
}

func (env Env) supervisor(info platform.Info, root string) *supervise.Manager {
	if env.Supervisor != nil {
		return env.Supervisor(info, root)
	}
	m := supervise.New(info, root)
	return m
}

// parseFlagsAnywhere 解析允许 flags 与位置参数任意混排的命令行。
//
// Go 的 flag 包在遇到第一个非 flag 参数时就停止解析，于是
// `ferc install any2utf8 --dry-run` 会把 --dry-run 当成组件名。
// 这里反复“解析一段、取出一个位置参数、继续解析剩余”，从而支持
// UNIX 习惯的混排写法，同时保留 flag 包的全部类型检查。
func parseFlagsAnywhere(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		remaining := fs.Args()
		if len(remaining) == 0 {
			return positional, nil
		}
		positional = append(positional, remaining[0])
		rest = remaining[1:]
	}
}

type logger struct{ w io.Writer }

func (l logger) Logf(format string, args ...any) {
	fmt.Fprintf(l.w, format+"\n", args...)
}

func cmdDetect(ctx context.Context, args []string, env Env) int {
	fs := flag.NewFlagSet("detect", flag.ContinueOnError)
	fs.SetOutput(env.stderr())
	var c common
	bindCommon(fs, &c, env)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	info, err := env.detect(ctx)
	if err != nil {
		fmt.Fprintf(env.stderr(), "探测失败: %v\n", err)
		return 1
	}
	if c.json {
		return writeJSON(env.stdout(), info)
	}
	fmt.Fprintf(env.stdout(), "操作系统      %s/%s\n", info.GOOS, info.GOARCH)
	fmt.Fprintf(env.stdout(), "归一化架构    %s\n", info.Arch)
	fmt.Fprintf(env.stdout(), "主机名        %s\n", info.Hostname)
	if info.Distro != "" {
		fmt.Fprintf(env.stdout(), "发行版        %s %s\n", info.Distro, info.DistroVersion)
	}
	if info.Kernel != "" {
		fmt.Fprintf(env.stdout(), "内核          %s\n", info.Kernel)
	}
	fmt.Fprintf(env.stdout(), "服务托管      %s（%s）\n", info.Init, info.InitEvidence)
	fmt.Fprintf(env.stdout(), "开机自启      %s\n", yesNo(info.Init.Autostart()))
	fmt.Fprintf(env.stdout(), "崩溃拉起      %s\n", yesNo(info.Init.Respawn()))
	if !info.Init.Supervised() {
		fmt.Fprintf(env.stdout(), "结论          需要装载自托管进程管理器才能保证节点长期可用\n")
	} else {
		fmt.Fprintf(env.stdout(), "结论          系统自带管理器已能保活，无需额外进程管理器\n")
	}
	if info.PackageMgr != "" {
		fmt.Fprintf(env.stdout(), "包管理器      %s\n", info.PackageMgr)
	}
	if info.Container {
		fmt.Fprintf(env.stdout(), "容器环境      是\n")
	}
	fmt.Fprintf(env.stdout(), "特权          %s\n", yesNo(info.Privileged))
	return 0
}

func yesNo(b bool) string {
	if b {
		return "是"
	}
	return "否"
}

func cmdList(ctx context.Context, args []string, env Env) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(env.stderr())
	var c common
	bindCommon(fs, &c, env)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cat, err := c.loadCatalog()
	if err != nil {
		fmt.Fprintf(env.stderr(), "%v\n", err)
		return 1
	}
	info, err := env.detect(ctx)
	if err != nil {
		fmt.Fprintf(env.stderr(), "探测失败: %v\n", err)
		return 1
	}
	st, _ := state.Load(state.Path(c.root))
	installed := map[string]string{}
	if st != nil {
		for _, r := range st.Components {
			installed[r.Name] = r.Version
		}
	}
	if c.json {
		type row struct {
			Name      string   `json:"name"`
			Display   string   `json:"display"`
			Kind      string   `json:"kind"`
			Summary   string   `json:"summary"`
			Essential bool     `json:"essential"`
			Installed string   `json:"installed_version,omitempty"`
			Supported bool     `json:"supported_on_this_host"`
			Archs     []string `json:"archs,omitempty"`
			Ports     []int    `json:"ports,omitempty"`
			Notes     string   `json:"notes,omitempty"`
		}
		var rows []row
		for _, name := range cat.Names() {
			comp, _ := cat.Get(name)
			rows = append(rows, row{
				Name: comp.Name, Display: comp.Display, Kind: string(comp.Kind),
				Summary: comp.Summary, Essential: comp.Essential,
				Installed: installed[comp.Name],
				Supported: comp.SupportsArch(info.Arch) && comp.Repo(info.GOOS) != "",
				Archs:     comp.Archs, Ports: comp.Ports, Notes: comp.Notes,
			})
		}
		return writeJSON(env.stdout(), rows)
	}

	fmt.Fprintf(env.stdout(), "%s%s%s%s\n",
		padRight("组件", 22), padRight("类型", 12), padRight("状态", 16), "说明")
	for _, name := range cat.Names() {
		comp, _ := cat.Get(name)
		status := "可装载"
		if v, ok := installed[name]; ok {
			status = "已装 " + v
		}
		if !comp.SupportsArch(info.Arch) {
			status = "不支持 " + info.Arch
		}
		if comp.Repo(info.GOOS) == "" {
			status = "不支持 " + info.GOOS
		}
		mark := ""
		if comp.Essential {
			mark = " *"
		}
		fmt.Fprintf(env.stdout(), "%s%s%s%s%s\n",
			padRight(comp.Name, 22), padRight(string(comp.Kind), 12),
			padRight(status, 16), comp.Summary, mark)
	}
	fmt.Fprintf(env.stdout(), "\n* 表示 `ferc bootstrap` 默认装载；本机 %s/%s\n", info.GOOS, info.Arch)
	return 0
}

func cmdBootstrap(ctx context.Context, args []string, env Env) int {
	return runInstall(ctx, "bootstrap", args, env, true)
}

func cmdInstall(ctx context.Context, args []string, env Env) int {
	return runInstall(ctx, "install", args, env, false)
}

func runInstall(ctx context.Context, name string, args []string, env Env, fromEssentials bool) int {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(env.stderr())
	var c common
	bindCommon(fs, &c, env)
	var all bool
	fs.BoolVar(&all, "all", false, "装载清单里的全部组件")
	want, err := parseFlagsAnywhere(fs, args)
	if err != nil {
		return 2
	}
	if !fromEssentials && !all && len(want) == 0 {
		fmt.Fprintf(env.stderr(), "请指定要装载的组件，或使用 `ferc bootstrap` 一键装载\n")
		return 2
	}

	cat, err := c.loadCatalog()
	if err != nil {
		fmt.Fprintf(env.stderr(), "%v\n", err)
		return 1
	}
	info, err := env.detect(ctx)
	if err != nil {
		fmt.Fprintf(env.stderr(), "探测失败: %v\n", err)
		return 1
	}
	if !info.Privileged && c.noStart == false && !c.dryRun {
		fmt.Fprintf(env.stderr(),
			"提示: 当前进程不是 root/管理员，注册服务很可能失败；可加 --no-start 只落盘\n")
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	st, err := state.Load(state.Path(c.root))
	if err != nil {
		fmt.Fprintf(env.stderr(), "%v\n", err)
		return 1
	}

	plan, err := resolve.Resolve(cat, resolve.Request{
		Want:           want,
		All:            all,
		Info:           info,
		Installed:      st.Names(),
		AllowConflicts: c.allowConflicts,
	})
	if err != nil {
		fmt.Fprintf(env.stderr(), "%v\n", err)
		if plan != nil {
			printPlanWarnings(env.stderr(), plan)
		}
		return 1
	}

	if c.json {
		if c.dryRun {
			return writeJSON(env.stdout(), plan)
		}
	}
	if !c.json {
		printPlan(env.stdout(), plan)
	}
	if len(plan.Steps) == 0 {
		if !c.json {
			fmt.Fprintln(env.stdout(), "没有需要装载的组件。")
		}
		return 0
	}

	var sup *supervise.Manager
	if !c.noStart {
		sup = env.supervisor(info, c.root)
		sup.DontCrackBin = filepath.Join(c.root, "bin", dontCrackBinary(cat, info))
	}
	inst, err := install.New(install.Options{
		Root:       c.root,
		Info:       info,
		Fetcher:    env.fetcher(&c),
		Supervisor: sup,
		DryRun:     c.dryRun,
		Log:        logger{w: env.stdout()},
	})
	if err != nil {
		fmt.Fprintf(env.stderr(), "%v\n", err)
		return 1
	}

	recs, err := inst.Apply(ctx, plan)
	if err != nil {
		fmt.Fprintf(env.stderr(), "装载失败: %v\n", err)
		return 1
	}
	if c.json {
		return writeJSON(env.stdout(), recs)
	}
	fmt.Fprintf(env.stdout(), "\n完成：装载 %d 个组件，数据根目录 %s\n", len(recs), c.root)
	if c.dryRun {
		fmt.Fprintln(env.stdout(), "（dry-run，未做任何改动）")
		return 0
	}
	printNextSteps(env.stdout(), info, recs, c.root)
	return 0
}

// dontCrackBinary 从清单里查 supervisor 组件的二进制名，避免在 CLI 里硬编码。
func dontCrackBinary(cat *catalog.Catalog, info platform.Info) string {
	for _, name := range cat.Names() {
		comp, _ := cat.Get(name)
		if comp.Kind == catalog.KindSupervisor && comp.Repo(info.GOOS) != "" {
			return comp.BinaryName(info.GOOS)
		}
	}
	return ""
}

func printPlan(w io.Writer, plan *resolve.Plan) {
	fmt.Fprintf(w, "装载计划（%s/%s，服务托管 %s）\n", plan.GOOS, plan.GOARCH, plan.Init)
	switch plan.Supervisor {
	case resolve.SupervisorInstall:
		fmt.Fprintln(w, "  进程保活: 系统不具备，将装载自托管进程管理器")
	case resolve.SupervisorNative:
		fmt.Fprintln(w, "  进程保活: 使用系统自带管理器")
	}
	for i, s := range plan.Steps {
		fmt.Fprintf(w, "  %d. %-22s %-10s %s\n", i+1, s.Component.Name, s.Component.Kind, s.Asset)
		if len(s.Args) > 0 {
			fmt.Fprintf(w, "       参数: %s\n", strings.Join(s.Args, " "))
		}
	}
	printPlanWarnings(w, plan)
}

func printPlanWarnings(w io.Writer, plan *resolve.Plan) {
	for _, warn := range plan.Warnings {
		fmt.Fprintf(w, "  警告: %s\n", warn)
	}
}

func printNextSteps(w io.Writer, info platform.Info, recs []state.Record, root string) {
	var services []state.Record
	for _, r := range recs {
		if r.Kind == string(catalog.KindService) {
			services = append(services, r)
		}
	}
	if len(services) == 0 {
		return
	}
	fmt.Fprintln(w, "\n后续动作：")
	for _, r := range services {
		for _, p := range r.Ports {
			fmt.Fprintf(w, "  - %s 监听端口 %d\n", r.Name, p)
		}
		if r.SecretFile != "" {
			fmt.Fprintf(w, "  - %s 的访问凭据由 ferc 随机生成，见 %s（0600，仅本机可读）\n",
				r.Name, r.SecretFile)
		}
	}
	fmt.Fprintf(w, "  - 已装载清单: %s\n", state.Path(root))
}

func cmdStatus(ctx context.Context, args []string, env Env) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(env.stderr())
	var c common
	bindCommon(fs, &c, env)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	st, err := state.Load(state.Path(c.root))
	if err != nil {
		fmt.Fprintf(env.stderr(), "%v\n", err)
		return 1
	}
	if c.json {
		return writeJSON(env.stdout(), st)
	}
	if len(st.Components) == 0 {
		fmt.Fprintf(env.stdout(), "本机尚未装载任何组件（状态文件 %s）\n", state.Path(c.root))
		return 0
	}
	root := st.Root
	if root == "" {
		root = c.root
	}
	arch := st.Arch
	if arch == "" {
		arch = "-"
	}
	host := st.Hostname
	if host == "" {
		host = "-"
	}
	fmt.Fprintf(env.stdout(), "数据根目录 %s\n", root)
	fmt.Fprintf(env.stdout(), "平台       %s/%s\n", orDash(st.GOOS), arch)
	fmt.Fprintf(env.stdout(), "主机名     %s\n\n", host)
	fmt.Fprintf(env.stdout(), "%s%s%s%s%s\n",
		padRight("组件", 22), padRight("类型", 12), padRight("版本", 16),
		padRight("托管", 12), "路径")
	for _, r := range st.Components {
		sup := r.Supervisor
		if sup == "" {
			sup = "-"
		}
		fmt.Fprintf(env.stdout(), "%s%s%s%s%s\n",
			padRight(r.Name, 22), padRight(r.Kind, 12),
			padRight(r.Version, 16), padRight(sup, 12), r.Path)
		if r.SecretFile != "" {
			fmt.Fprintf(env.stdout(), "  %s凭据: %s\n", strings.Repeat(" ", 20), r.SecretFile)
		}
		if r.ControlPort > 0 {
			fmt.Fprintf(env.stdout(), "  %s控制端口: %d\n", strings.Repeat(" ", 20), r.ControlPort)
		}
	}
	return 0
}

// cmdVerify 检查已装载组件的文件存在性是“可执行”的。
// 它刻意不去猜端口、不去解析日志：那是 FasterEdgeDoctor 的职责，
// 运行核只回答“我装的东西还在不在、还跑不跑得起来”。
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func cmdVerify(ctx context.Context, args []string, env Env) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(env.stderr())
	var c common
	bindCommon(fs, &c, env)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	st, err := state.Load(state.Path(c.root))
	if err != nil {
		fmt.Fprintf(env.stderr(), "%v\n", err)
		return 1
	}
	type result struct {
		Name   string `json:"name"`
		Path   string `json:"path"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail,omitempty"`
	}
	var results []result
	bad := 0
	for _, r := range st.Components {
		res := result{Name: r.Name, Path: r.Path, OK: true}
		fi, statErr := os.Stat(r.Path)
		switch {
		case statErr != nil:
			res.OK = false
			res.Detail = "文件不存在: " + statErr.Error()
		case fi.IsDir():
			res.OK = false
			res.Detail = "路径是目录"
		case runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 == 0:
			res.OK = false
			res.Detail = "缺少可执行位"
		}
		if !res.OK {
			bad++
		}
		results = append(results, res)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })

	if c.json {
		code := writeJSON(env.stdout(), results)
		if code != 0 {
			return code
		}
		if bad > 0 {
			return 1
		}
		return 0
	}
	if len(results) == 0 {
		fmt.Fprintln(env.stdout(), "没有需要检查的组件。")
		return 0
	}
	for _, r := range results {
		mark := "ok  "
		if !r.OK {
			mark = "FAIL"
		}
		line := mark + " " + padRight(r.Name, 22) + r.Path
		if r.Detail != "" {
			line += "  (" + r.Detail + ")"
		}
		fmt.Fprintln(env.stdout(), line)
	}
	if bad > 0 {
		fmt.Fprintf(env.stderr(), "\n%d 个组件检查失败\n", bad)
		return 1
	}
	fmt.Fprintf(env.stdout(), "\n全部 %d 个组件检查通过\n", len(results))
	return 0
}

func cmdRemove(ctx context.Context, args []string, env Env) int {
	fs := flag.NewFlagSet("remove", flag.ContinueOnError)
	fs.SetOutput(env.stderr())
	var c common
	bindCommon(fs, &c, env)
	names, err := parseFlagsAnywhere(fs, args)
	if err != nil {
		return 2
	}
	if len(names) == 0 {
		fmt.Fprintln(env.stderr(), "请指定要卸载的组件")
		return 2
	}
	cat, err := c.loadCatalog()
	if err != nil {
		fmt.Fprintf(env.stderr(), "%v\n", err)
		return 1
	}
	st, err := state.Load(state.Path(c.root))
	if err != nil {
		fmt.Fprintf(env.stderr(), "%v\n", err)
		return 1
	}
	info, err := env.detect(ctx)
	if err != nil {
		fmt.Fprintf(env.stderr(), "探测失败: %v\n", err)
		return 1
	}
	var mgr *supervise.Manager
	if !c.noStart {
		mgr = env.supervisor(info, c.root)
	}

	var failed []string
	for _, name := range names {
		rec, ok := st.Get(name)
		if !ok {
			fmt.Fprintf(env.stderr(), "%s 未装载，跳过\n", name)
			continue
		}
		if mgr != nil && rec.Unit != "" && rec.Supervisor != "self" {
			if err := mgr.Unregister(ctx, supervise.Result{
				Supervisor:  rec.Supervisor,
				Unit:        rec.Unit,
				ControlPort: rec.ControlPort,
				ManagerPID:  rec.ManagerPID,
			}); err != nil {
				fmt.Fprintf(env.stderr(), "撤销 %s 的服务注册失败: %v\n", name, err)
				failed = append(failed, name)
				continue
			}
		}
		if rec.Path != "" {
			if err := os.Remove(rec.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(env.stderr(), "删除 %s 失败: %v\n", name, err)
				failed = append(failed, name)
				continue
			}
		}
		// 自动生成的凭据是运行核创建的，卸载时就该一并清掉；否则会在
		// secrets 目录里留下一堆没人认识、也没人敢删的密码文件。
		if rec.SecretFile != "" {
			if err := os.Remove(rec.SecretFile); err != nil && !errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(env.stderr(), "删除 %s 的凭据文件失败: %v\n", name, err)
			}
		}
		// 被卸载组件的依赖者若还在，需要提示：它们可能已经无法工作。
		for _, other := range st.Components {
			comp, ok := cat.Get(other.Name)
			if !ok || other.Name == name {
				continue
			}
			for _, dep := range comp.Requires {
				if dep == name {
					fmt.Fprintf(env.stderr(), "提示: %s 依赖被移除的 %s，可能已无法正常工作\n", other.Name, name)
				}
			}
		}
		st.Remove(name)
		fmt.Fprintf(env.stdout(), "已卸载 %s\n", name)
	}
	if !c.dryRun {
		if err := st.Save(state.Path(c.root)); err != nil {
			fmt.Fprintf(env.stderr(), "写状态文件失败: %v\n", err)
			return 1
		}
	}
	if len(failed) > 0 {
		fmt.Fprintf(env.stderr(), "%d 个组件卸载失败\n", len(failed))
		return 1
	}
	return 0
}

func writeJSON(w io.Writer, v any) int {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(w, "序列化失败: %v\n", err)
		return 1
	}
	fmt.Fprintln(w, string(b))
	return 0
}
