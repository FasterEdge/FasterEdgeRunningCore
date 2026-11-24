// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/catalog"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/platform"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/state"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/supervise"
)

// selfSupervisor 是状态文件里表示“由运行核装载的 DontCrack 托管”的标记值，
// 与 install 写入的记录保持一致。
const selfSupervisor = "self"

// dontCrackSupervisor 是状态文件里表示“由 DontCrack 进程管理器托管”的标记值。
const dontCrackSupervisor = "dontcrack"

// restoreOutcome 是一条记录的恢复结果：既用于终端逐条汇报，也是 --json 的一项。
type restoreOutcome string

const (
	outcomeRestored restoreOutcome = "restored"
	outcomeRunning  restoreOutcome = "already-running"
	outcomeSkipped  restoreOutcome = "skipped"
	outcomeFailed   restoreOutcome = "failed"
	outcomePlanned  restoreOutcome = "planned"
)

// restoreItem 是恢复报告里的一行。
type restoreItem struct {
	Name        string         `json:"name"`
	Kind        string         `json:"kind,omitempty"`
	Supervisor  string         `json:"supervisor,omitempty"`
	Unit        string         `json:"unit,omitempty"`
	ControlPort int            `json:"control_port,omitempty"`
	Outcome     restoreOutcome `json:"outcome"`
	Detail      string         `json:"detail,omitempty"`
}

// restoreReport 是一次恢复的汇总。
type restoreReport struct {
	Restored int           `json:"restored"`
	Skipped  int           `json:"skipped"`
	Failed   int           `json:"failed"`
	Items    []restoreItem `json:"items"`
}

// cmdRestore 重放“谁在托管谁”。
//
// 运行核不常驻，重启后没有任何进程记得组件该怎么起来，开机自启落盘的脚本就是
// 调这一条命令。因此它有两条硬性质：幂等（开机脚本可能被执行多次、运维者也会
// 手工重跑），以及宽容（个别二进制被删掉不该让其它组件的恢复一起失败）。
func cmdRestore(ctx context.Context, args []string, env Env) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
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
	if len(st.Components) == 0 {
		if c.json {
			return writeJSON(env.stdout(), restoreReport{Items: []restoreItem{}})
		}
		fmt.Fprintln(env.stdout(), "没有已装载的组件，无需恢复")
		return 0
	}

	info, err := env.detect(ctx)
	if err != nil {
		fmt.Fprintf(env.stderr(), "探测失败: %v\n", err)
		return 1
	}
	mgr := env.supervisor(info, c.root)
	if mgr == nil {
		fmt.Fprintln(env.stderr(), "恢复失败: 没有可用的托管实现")
		return 1
	}
	// 自托管组件的托管方是状态文件里记下的 DontCrack 二进制，而不是 PATH 里的
	// 同名命令：开机时 PATH 往往很短，只能靠状态文件里的绝对路径。
	if bin := dontCrackBinFromState(st, info.GOOS, c.root); bin != "" {
		mgr.DontCrackBin = bin
	}

	// 按名字排序，让同一份状态文件每次恢复的顺序与报告一致、可对比。
	recs := append([]state.Record(nil), st.Components...)
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].Name < recs[j].Name })

	var (
		report  restoreReport
		updates []state.Record
		offset  int
	)
	for _, rec := range recs {
		item, upd, next := restoreRecord(ctx, &c, mgr, rec, offset)
		offset = next
		report.Items = append(report.Items, item)
		switch item.Outcome {
		case outcomeRestored, outcomePlanned:
			report.Restored++
		case outcomeFailed:
			report.Failed++
		default:
			report.Skipped++
		}
		if upd != nil {
			updates = append(updates, *upd)
		}
	}

	// 重新派生出来的管理器换了进程号（重启后原来的进程早就不在了），必须写回
	// 状态文件；否则之后 ferc remove 既停不掉它，也找不到那个占着控制端口的进程。
	var saveErr error
	if len(updates) > 0 && !c.dryRun {
		for _, u := range updates {
			st.Put(u)
		}
		saveErr = st.Save(state.Path(c.root))
	}

	if c.json {
		if code := writeJSON(env.stdout(), report); code != 0 {
			return code
		}
		if saveErr != nil || report.Failed > 0 {
			return 1
		}
		return 0
	}

	fmt.Fprintf(env.stdout(), "恢复已装载组件（数据根目录 %s，服务托管 %s）\n", c.root, info.Init)
	for _, it := range report.Items {
		fmt.Fprintf(env.stdout(), "%s %s%s\n", restoreMark(it.Outcome), padRight(it.Name, 22), restoreDetail(it))
	}
	if c.dryRun {
		fmt.Fprintln(env.stdout(), "（dry-run，未做任何改动）")
	}
	fmt.Fprintf(env.stdout(), "\n恢复 %d 个组件，%d 个跳过\n", report.Restored, report.Skipped)
	if saveErr != nil {
		fmt.Fprintf(env.stderr(), "写回状态文件失败: %v\n", saveErr)
		return 1
	}
	if report.Failed > 0 {
		fmt.Fprintf(env.stderr(), "%d 个组件恢复失败\n", report.Failed)
		return 1
	}
	return 0
}

// restoreRecord 把一条记录重新交回托管方，返回结果与需要写回的状态（未变则为 nil）。
// 返回的 offset 供下一个自托管组件分配控制端口。
func restoreRecord(ctx context.Context, c *common, mgr *supervise.Manager, rec state.Record, offset int) (restoreItem, *state.Record, int) {
	item := restoreItem{Name: rec.Name, Kind: rec.Kind, Supervisor: rec.Supervisor, Unit: rec.Unit, ControlPort: rec.ControlPort}

	switch {
	case rec.Kind == string(catalog.KindTool):
		// 一次性工具装载后没有托管关系，恢复时无事可做。
		return skippedRestore(item, "一次性工具，无需保活"), nil, offset
	case rec.Supervisor == "":
		return skippedRestore(item, "装载时未托管（--no-start），无需恢复"), nil, offset
	case rec.Supervisor == selfSupervisor && rec.Kind == string(catalog.KindSupervisor):
		// 进程管理器自身没有可注册的常驻单元：托管实例是装载服务时按服务派生的。
		// 这里只核对二进制还在不在——它不在，后面所有自托管服务都会失败，先把话说清楚。
		// 其它 kind 被标成 self 时不在这里短路，交给下面按 init 重新注册。
		if _, err := os.Stat(rec.Path); err != nil {
			return skippedRestore(item, "进程管理器二进制缺失: "+err.Error()), nil, offset
		}
		return skippedRestore(item, "进程管理器二进制已就位，托管实例随各服务重新派生"), nil, offset
	}

	if _, err := os.Stat(rec.Path); err != nil {
		return skippedRestore(item, "二进制缺失，跳过: "+err.Error()), nil, offset
	}

	m := mgr
	portOffset := offset
	if rec.Supervisor == dontCrackSupervisor && rec.ControlPort > 0 {
		// 已经在跑就先别再派生一个：同一端口上的第二个实例必然绑不上，而“重复
		// 执行不报错”是恢复的基本要求（开机脚本可能被手工跑第二次）。
		if managerHealthy(m, rec.ControlPort) {
			item.Outcome = outcomeRunning
			item.Detail = fmt.Sprintf("控制端口 %d 上的管理器仍在运行，跳过", rec.ControlPort)
			return item, nil, offset
		}
		// 沿用状态文件里记下的端口：重启后重新派生的实例应回到原来的地址，
		// 否则告警规则与防火墙放行都会指向一个没人监听的端口。
		cp := *m
		cp.DontCrackPort = rec.ControlPort
		m = &cp
		portOffset = 0
	}

	unit := rec.Unit
	if unit == "" {
		unit = "ferc-" + rec.Name
	}
	opt := supervise.Options{
		Unit:        unit,
		BinPath:     rec.Path,
		Args:        rec.Args,
		Dir:         filepath.Dir(rec.Path),
		LogDir:      filepath.Join(c.root, "logs", rec.Name),
		Description: rec.Name,
		PortOffset:  portOffset,
		Ports:       rec.Ports,
	}

	if c.dryRun {
		item.Outcome = outcomePlanned
		item.Detail = "将重新注册到 " + rec.Supervisor
		return item, nil, offset
	}

	res, err := m.Register(ctx, opt)
	if err != nil {
		// launchd 的 `load -w` 对一个已加载的 plist 会直接报错退出，openrc 的
		// rc-update 也可能把“已经加过”说成失败。这类“本来就已完成”的回复不算
		// 恢复失败，否则重复执行 restore 会一路红字，把真正的故障埋掉。
		if alreadyRegistered(err) {
			item.Outcome = outcomeRunning
			item.Detail = "托管关系已存在: " + err.Error()
			return item, nil, offset
		}
		item.Outcome = outcomeFailed
		item.Detail = err.Error()
		return item, nil, offset
	}
	item.Outcome = outcomeRestored
	item.Supervisor = res.Supervisor
	item.Unit = res.Unit
	item.ControlPort = res.ControlPort
	notes := append([]string(nil), res.Warnings...)
	if res.Supervisor != rec.Supervisor {
		// 状态文件记的托管方与这台机器现在能提供的不一致（例如原本 systemd 的
		// 镜像换成了 FasterEdgeOS）：Register 会按当前能力重新选择，但这件事必须
		// 说出来，否则运维者会以为组件还在原来那个管理器手里。
		notes = append(notes, fmt.Sprintf("托管方由 %s 改为 %s", rec.Supervisor, res.Supervisor))
	}
	if len(notes) > 0 {
		item.Detail = strings.Join(notes, "；")
	}
	upd := rec
	upd.Supervisor = res.Supervisor
	upd.Unit = res.Unit
	upd.ControlPort = res.ControlPort
	upd.ManagerPID = res.ManagerPID
	if res.Supervisor == dontCrackSupervisor {
		offset++
	}
	return item, &upd, offset
}

func skippedRestore(item restoreItem, detail string) restoreItem {
	item.Outcome = outcomeSkipped
	item.Detail = detail
	return item
}

// managerHealthy 问一句“这个控制端口上还有人在答吗”。
// 探测函数由 supervise.Manager 注入，测试里可以替换。
func managerHealthy(m *supervise.Manager, port int) bool {
	if m.Get == nil || port <= 0 {
		return false
	}
	return m.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port)) == nil
}

// alreadyRegistered 识别各 init 的“已经做过了”报错文本。
//
// 这是启发式判断：宁可把一条已经生效的托管关系误判成“已存在”（结果正确、
// 只是措辞保守），也不要把重复执行 restore 的正常情形报成故障。
func alreadyRegistered(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// 只认“已经做过”的措辞。端口被占用之类的 “already in use” 不属于这一类：
	// 那是真的没起来，必须报成失败。
	for _, marker := range []string{
		"already exists",
		"already enabled",
		"already loaded",
		"already registered",
		"already running",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// dontCrackBinFromState 找出本机已装载的进程管理器路径。
//
// 优先用状态文件里 supervisor=self 的那条记录——那是安装时真实落盘的位置；
// 状态文件里没有（例如由更早的版本写入）时退回清单推导的默认路径。
func dontCrackBinFromState(st *state.State, goos, root string) string {
	for _, r := range st.Components {
		if r.Supervisor == selfSupervisor && r.Kind == string(catalog.KindSupervisor) && r.Path != "" {
			return r.Path
		}
	}
	cat, err := catalog.Builtin()
	if err != nil {
		return ""
	}
	name := dontCrackBinary(cat, platform.Info{GOOS: goos})
	if name == "" {
		return ""
	}
	return filepath.Join(root, "bin", name)
}

func restoreMark(o restoreOutcome) string {
	switch o {
	case outcomeRestored:
		return "ok  "
	case outcomeRunning:
		return "run "
	case outcomePlanned:
		return "plan"
	case outcomeSkipped:
		return "skip"
	default:
		return "FAIL"
	}
}

// restoreDetail 拼出一行的后半段：托管方、单元/端口，最后是给人看的原因。
func restoreDetail(it restoreItem) string {
	parts := make([]string, 0, 4)
	if it.Supervisor != "" && !isSelfMarker(it.Supervisor) {
		parts = append(parts, padRight(it.Supervisor, 12))
	}
	if it.Unit != "" && it.Supervisor != dontCrackSupervisor {
		parts = append(parts, padRight(it.Unit, 26))
	}
	if it.ControlPort > 0 && it.Supervisor == dontCrackSupervisor {
		parts = append(parts, padRight(fmt.Sprintf("控制端口 %d", it.ControlPort), 14))
	}
	if it.Detail != "" {
		parts = append(parts, it.Detail)
	}
	return strings.Join(parts, " ")
}

func isSelfMarker(s string) bool {
	return s == selfSupervisor || s == dontCrackSupervisor
}
