// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/boot"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/platform"
)

// bootFileStatus 描述一个自启落点文件当前的状态。
type bootFileStatus struct {
	Path             string `json:"path"`
	Mode             string `json:"mode"`
	Exists           bool   `json:"exists"`
	MatchesGenerated bool   `json:"matches_generated"`
}

// bootReport 是 boot 子命令的机器可读输出。
type bootReport struct {
	Mechanism string           `json:"mechanism"`
	Enabled   bool             `json:"enabled"`
	Files     []bootFileStatus `json:"files,omitempty"`
	FileCount int              `json:"file_count"`
	Commands  []string         `json:"commands,omitempty"`
	Warnings  []string         `json:"warnings,omitempty"`
	Detail    string           `json:"detail,omitempty"`
}

// errNoBootMechanism 表示本平台没有可用的开机自启落点。
//
// 它不是“出错”而是“做不到”：调用方（装载流程）必须把它降级为告警，
// 否则一台没有 init 目录、也没有 autorun 约定的机器会因此装不上组件。
var errNoBootMechanism = errors.New("本平台没有可用的开机自启机制")

// cmdBoot 实现 `ferc boot enable|disable|status`。
func cmdBoot(ctx context.Context, args []string, env Env) int {
	if len(args) == 0 {
		fmt.Fprintln(env.stderr(), "用法: ferc boot enable|disable|status [选项]")
		return 2
	}
	action, rest := args[0], args[1:]
	fs := flag.NewFlagSet("boot "+action, flag.ContinueOnError)
	fs.SetOutput(env.stderr())
	var c common
	bindCommon(fs, &c, env)
	if err := fs.Parse(rest); err != nil {
		return 2
	}
	info, err := env.detect(ctx)
	if err != nil {
		fmt.Fprintf(env.stderr(), "探测失败: %v\n", err)
		return 1
	}
	plan := env.bootPlan(info, c.root)

	switch action {
	case "enable":
		return cmdBootEnable(ctx, &c, env, info, plan)
	case "disable":
		return cmdBootDisable(ctx, &c, env, plan)
	case "status":
		return cmdBootStatus(ctx, &c, env, info, plan)
	default:
		fmt.Fprintf(env.stderr(), "未知的 boot 动作 %q，可用: enable / disable / status\n", action)
		return 2
	}
}

func cmdBootEnable(ctx context.Context, c *common, env Env, info platform.Info, plan boot.Plan) int {
	if c.dryRun {
		printBootPlan(env.stdout(), plan)
		fmt.Fprintln(env.stdout(), "\n（dry-run，未做任何改动）")
		return 0
	}
	warns, err := applyBootEnable(ctx, env, plan)
	for _, w := range warns {
		fmt.Fprintf(env.stdout(), "警告: %s\n", w)
	}
	if err != nil {
		fmt.Fprintf(env.stderr(), "开机自启未启用: %v\n", err)
		printBootPlan(env.stderr(), plan)
		return 1
	}
	fmt.Fprintf(env.stdout(), "已启用开机自启（机制 %s），开机时执行 `ferc restore --root %s`\n", plan.Mechanism, c.root)
	if info.Init == platform.InitNone {
		// FasterEdgeOS 这类系统没有真正的 init，落点只有 /etc/autorun；
		// 说清楚这条链才能让运维者知道重启后到底是谁跑了什么。
		fmt.Fprintf(env.stdout(), "本机没有系统 init，自启由 %s 在开机时按文件名序执行\n", boot.DefaultAutorunDir)
	}
	return 0
}

func cmdBootDisable(ctx context.Context, c *common, env Env, plan boot.Plan) int {
	if c.dryRun {
		printBootPlan(env.stdout(), plan)
		fmt.Fprintln(env.stdout(), "（dry-run，未做任何改动）")
		return 0
	}
	warns, err := applyBootDisable(ctx, env, plan)
	for _, w := range warns {
		fmt.Fprintf(env.stdout(), "警告: %s\n", w)
	}
	if err != nil {
		fmt.Fprintf(env.stderr(), "撤销开机自启失败: %v\n", err)
		return 1
	}
	fmt.Fprintln(env.stdout(), "已撤销开机自启：重启后不会再自动重放组件托管关系，需要时手工执行 `ferc restore`")
	return 0
}

func cmdBootStatus(ctx context.Context, c *common, env Env, info platform.Info, plan boot.Plan) int {
	rep := bootReport{
		Mechanism: string(plan.Mechanism),
		Files:     bootFileStates(plan),
		Commands:  commandLines(plan.Commands),
		Warnings:  plan.Warnings,
	}
	for _, f := range rep.Files {
		if f.Exists {
			rep.FileCount++
		}
	}
	switch {
	case plan.Mechanism == boot.MechanismNone:
		rep.Enabled = false
	case len(plan.Files) > 0:
		rep.Enabled = rep.FileCount > 0
	default:
		// 计划任务这类落点没有文件可查，只能问注册表自己：`/Query` 是只读的。
		_, err := env.run(ctx, "schtasks", "/Query", "/TN", boot.ServiceName)
		rep.Enabled = err == nil
		if !rep.Enabled {
			rep.Detail = "未查询到计划任务 " + boot.ServiceName
		}
	}

	if c.json {
		return writeJSON(env.stdout(), rep)
	}
	fmt.Fprintf(env.stdout(), "数据根目录 %s\n", c.root)
	fmt.Fprintf(env.stdout(), "服务托管   %s\n", info.Init)
	fmt.Fprintf(env.stdout(), "自启机制   %s\n", plan.Mechanism)
	if len(plan.Files) == 0 {
		fmt.Fprintln(env.stdout(), "落点文件   无（该机制靠注册命令而不是文件）")
	}
	for _, f := range rep.Files {
		state := "缺失"
		if f.Exists {
			state = "已就位"
			if !f.MatchesGenerated {
				state = "已就位但内容与当前版本生成的不一致"
			}
		}
		fmt.Fprintf(env.stdout(), "%s%s  %s\n", padRight(state, 40), f.Mode, f.Path)
	}
	if rep.Detail != "" {
		fmt.Fprintf(env.stdout(), "%s\n", rep.Detail)
	}
	fmt.Fprintf(env.stdout(), "结论       %s\n", bootVerdict(rep))
	for _, w := range plan.Warnings {
		fmt.Fprintf(env.stdout(), "警告: %s\n", w)
	}
	return 0
}

// bootVerdict 把“机制可用 + 文件在位”翻译成一句人话。
func bootVerdict(rep bootReport) string {
	switch {
	case rep.Mechanism == string(boot.MechanismNone):
		return "本平台没有可用的自启落点，重启后需手工执行 ferc restore"
	case !rep.Enabled:
		return "尚未启用，重启后组件不会自己回来；执行 `ferc boot enable`"
	default:
		return "已启用，重启后会自动重放组件托管关系"
	}
}

// applyBootEnable 落盘并执行启用命令，返回提示与错误。
//
// 返回的 warnings 永远不为 nil 时才包含“做了但需要人知道”的事；error 只覆盖
// 真正没达成目的的情形（写不了文件、注册命令失败）。装载流程把它降级成告警，
// 因为“自启没落盘”不该让一次成功的组件装载整体失败。
func applyBootEnable(ctx context.Context, env Env, plan boot.Plan) ([]string, error) {
	warns := append([]string(nil), plan.Warnings...)
	if plan.Mechanism == boot.MechanismNone {
		return warns, errNoBootMechanism
	}
	if err := writeBootFiles(plan.Files); err != nil {
		return warns, err
	}
	var errs []error
	for _, cmd := range plan.Commands {
		if _, err := env.run(ctx, cmd.Name, cmd.Args...); err != nil {
			errs = append(errs, fmt.Errorf("%s %s 失败: %w", cmd.Name, strings.Join(cmd.Args, " "), err))
		}
	}
	return warns, errors.Join(errs...)
}

// applyBootDisable 执行撤销命令并删掉落点文件。
//
// 顺序是“先撤销注册、再删文件”：systemctl disable 需要单元还在才能读到它；
// 反过来先删文件，注册信息就成了无主的残留，只能靠手工清理。
// systemd 在文件删除后还需要一次 daemon-reload 才不认那个已消失的单元。
//
// 这里不返回方案里的 Warnings：那些是“启用时会有什么风险”的提示，撤销时
// 没有意义，混进输出只会让人以为撤销本身出了问题。
func applyBootDisable(ctx context.Context, env Env, plan boot.Plan) ([]string, error) {
	var (
		warns []string
		errs  []error
	)
	for _, cmd := range plan.RemoveCmd {
		if _, err := env.run(ctx, cmd.Name, cmd.Args...); err != nil {
			// 撤销是“尽量干净”：注册信息本来就可能不存在（装的时候就没成功过），
			// 把这种情况报成硬失败会让操作者没法把配置收尾。
			warns = append(warns, fmt.Sprintf("%s %s 未成功（可能本来就没注册）: %v",
				cmd.Name, strings.Join(cmd.Args, " "), err))
		}
	}
	for _, p := range plan.Remove {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("删除 %s 失败: %w", p, err))
		}
	}
	if plan.Mechanism == boot.MechanismSystemd {
		if _, err := env.run(ctx, "systemctl", "daemon-reload"); err != nil {
			warns = append(warns, "systemctl daemon-reload 未成功: "+err.Error())
		}
	}
	return warns, errors.Join(errs...)
}

// writeBootFiles 按方案落盘。MkdirAll + WriteFile + 显式 Chmod：
// WriteFile 不会改动已存在文件的权限，而升级前留下的 0644 脚本会被 init 静默
// 跳过——那等于“写成功但永远不执行”，必须显式收紧到方案要求的位。
func writeBootFiles(files []boot.File) error {
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
			return fmt.Errorf("建目录 %s 失败: %w", filepath.Dir(f.Path), err)
		}
		if err := os.WriteFile(f.Path, f.Body, f.Mode); err != nil {
			return fmt.Errorf("写 %s 失败: %w", f.Path, err)
		}
		if err := os.Chmod(f.Path, f.Mode); err != nil && runtime.GOOS != "windows" {
			return fmt.Errorf("设置 %s 权限失败: %w", f.Path, err)
		}
	}
	return nil
}

// bootFileStates 核对每个落点文件是否存在、内容是否与当前生成的一致。
func bootFileStates(plan boot.Plan) []bootFileStatus {
	out := make([]bootFileStatus, 0, len(plan.Files))
	for _, f := range plan.Files {
		st := bootFileStatus{Path: f.Path, Mode: fmt.Sprintf("%04o", f.Mode.Perm())}
		if b, err := os.ReadFile(f.Path); err == nil {
			st.Exists = true
			st.MatchesGenerated = string(b) == string(f.Body)
		}
		out = append(out, st)
	}
	return out
}

func commandLines(cmds []boot.Command) []string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, c.Name+" "+strings.Join(c.Args, " "))
	}
	return out
}

// printBootPlan 打印“会写到哪里、会执行什么”，dry-run 与失败时都直接可用。
func printBootPlan(w io.Writer, plan boot.Plan) {
	fmt.Fprintf(w, "开机自启方案（机制 %s）\n", plan.Mechanism)
	for _, f := range plan.Files {
		fmt.Fprintf(w, "  写入 %s（%04o）\n", f.Path, f.Mode.Perm())
	}
	for _, c := range plan.Commands {
		fmt.Fprintf(w, "  执行 %s %s\n", c.Name, strings.Join(c.Args, " "))
	}
	for _, r := range plan.Remove {
		fmt.Fprintf(w, "  撤销时删除 %s\n", r)
	}
	for _, c := range plan.RemoveCmd {
		fmt.Fprintf(w, "  撤销时执行 %s %s\n", c.Name, strings.Join(c.Args, " "))
	}
	for _, warn := range plan.Warnings {
		fmt.Fprintf(w, "  警告: %s\n", warn)
	}
}

// enableBootAfterInstall 让“装完就重启”也能自己回来。
//
// 它必须在装载成功之后调用：托管关系已经写进 ferc-state.json，自启脚本才
// 有东西可重放。任何失败都只降级成告警——自启没落盘不等于组件没装好，
// 而把已经装好的节点判成失败会让操作者去回滚一件其实成功的事。
// w 是告警的去处：--json 时调用方传 stderr，免得混进机器可读的输出里。
func enableBootAfterInstall(ctx context.Context, c *common, env Env, info platform.Info, w io.Writer) {
	if c.dryRun || c.noStart || c.noBoot {
		return
	}
	plan := env.bootPlan(info, c.root)
	warns, err := applyBootEnable(ctx, env, plan)
	for _, warn := range warns {
		fmt.Fprintf(w, "警告: %s\n", warn)
	}
	if err != nil {
		fmt.Fprintf(w, "警告: 开机自启未启用（不影响本次装载）: %v\n", err)
		fmt.Fprintf(w, "  需要时可手工执行 `ferc boot enable --root %s`\n", c.root)
		return
	}
	fmt.Fprintf(w, "开机自启已启用（%s），重启后会自动执行 `ferc restore --root %s`\n",
		plan.Mechanism, c.root)
}
