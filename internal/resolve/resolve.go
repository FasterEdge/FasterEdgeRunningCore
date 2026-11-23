// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

// Package resolve 把“用户想要的一组组件”变成“可按顺序执行的装载计划”。
//
// 它的全部输出都是纯数据：装载顺序、需要装载的进程管理器、架构与端口的
// 冲突告警。真正的下载与落盘在 install 包，二者分离使计划可以被打印、
// 对比、存档，而不产生任何副作用。
package resolve

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/catalog"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/platform"
)

// SupervisorDecision 说明本次装载是否要引入自托管进程管理器。
type SupervisorDecision string

const (
	// SupervisorNative 表示目标系统自带保活能力，无需自托管。
	SupervisorNative SupervisorDecision = "native"
	// SupervisorInstall 表示必须装载清单里的 supervisor 组件。
	SupervisorInstall SupervisorDecision = "install-supervisor"
	// SupervisorMissing 表示需要进程管理器但清单里没有可用的 supervisor。
	SupervisorMissing SupervisorDecision = "missing-supervisor"
)

// Step 是计划中的一步，对应一个组件的装载。
type Step struct {
	Component *catalog.Component
	Repo      string
	Asset     string
	Binary    string
	Args      []string
	// Reason 说明“为什么这一步会出现”，安装日志会原样打印。
	Reason string
}

// Plan 是一次装载的完整计划。
type Plan struct {
	GOOS       string              `json:"goos"`
	GOARCH     string              `json:"goarch"`
	Init       platform.InitSystem `json:"init"`
	Supervisor SupervisorDecision  `json:"supervisor"`
	Steps      []Step              `json:"steps"`
	Warnings   []string            `json:"warnings"`
}

// Names 返回计划中各组件的装载顺序。
func (p *Plan) Names() []string {
	out := make([]string, 0, len(p.Steps))
	for _, s := range p.Steps {
		out = append(out, s.Component.Name)
	}
	return out
}

// Request 描述一次解析请求。
type Request struct {
	// Want 是用户点名要的组件；为空表示使用清单里 essential 的组件。
	Want []string
	// All 表示装载清单全部组件。
	All bool
	// Info 是目标机探测结果。
	Info platform.Info
	// Installed 是已装载组件名，用于让“依赖已满足”的组件不重复出现在计划里。
	Installed map[string]bool
	// AllowConflicts 把端口/互斥冲突从硬错误降级为告警。
	// 默认是硬错误：装到一半才发现端口占用，比装之前报错更糟。
	AllowConflicts bool
}

// Resolve 生成装载计划。
func Resolve(cat *catalog.Catalog, req Request) (*Plan, error) {
	info := req.Info
	goos := info.GOOS
	if goos == "" {
		return nil, errors.New("resolve: 缺少目标操作系统")
	}
	arch := info.Arch
	if arch == "" {
		arch = platform.NormalizeArch(info.GOARCH)
	}

	want, err := requested(cat, req)
	if err != nil {
		return nil, err
	}

	ordered, err := expand(cat, want, req.Installed)
	if err != nil {
		return nil, err
	}

	plan := &Plan{GOOS: goos, GOARCH: info.GOARCH, Init: info.Init}

	needsSupervisor := !info.Init.Supervised() && hasService(cat, ordered)
	if needsSupervisor {
		sup := supervisorFor(cat, ordered)
		if sup == nil {
			plan.Supervisor = SupervisorMissing
			return plan, fmt.Errorf("resolve: %s 没有原生进程保活能力，但清单中没有可用的 supervisor 组件", info.Init)
		}
		plan.Supervisor = SupervisorInstall
		// 用户可能已经点名 supervisor；此时不能把它再插一遍，否则计划里
		// 会出现两个 dontcrack，第二个还要去抢第一个的端口。
		if !containsName(ordered, sup.Name) {
			ordered = append([]string{sup.Name}, ordered...)
		}
	} else {
		plan.Supervisor = SupervisorNative
	}

	if err := plan.checkConflicts(cat, ordered, req.AllowConflicts); err != nil {
		return plan, err
	}
	plan.fill(cat, ordered, arch, goos, needsSupervisor)
	if needsSupervisor && !plan.hasStep("dontcrack") && !plan.hasSupervisorStep() {
		// 计划号称要装载 supervisor 却没能排进去，等于“答应保活但没保活”，
		// 这比直接失败更危险，必须硬失败。
		return plan, errors.New("resolve: 需要进程保活，但进程管理器无法在当前平台装载")
	}
	return plan, nil
}

func (p *Plan) hasStep(name string) bool {
	for _, s := range p.Steps {
		if s.Component.Name == name {
			return true
		}
	}
	return false
}

func (p *Plan) hasSupervisorStep() bool {
	for _, s := range p.Steps {
		if s.Component.Kind == catalog.KindSupervisor {
			return true
		}
	}
	return false
}

func requested(cat *catalog.Catalog, req Request) ([]string, error) {
	if req.All {
		return cat.Names(), nil
	}
	want := req.Want
	if len(want) == 0 {
		want = cat.Essentials()
		if len(want) == 0 {
			return nil, errors.New("resolve: 未指定组件，且清单里没有标记 essential 的组件")
		}
	}
	for _, n := range want {
		if _, ok := cat.Get(n); !ok {
			return nil, fmt.Errorf("resolve: 未知组件 %q", n)
		}
	}
	return want, nil
}

// expand 按依赖拓扑排序，得到“依赖在前”的装载顺序。
func expand(cat *catalog.Catalog, want []string, installed map[string]bool) ([]string, error) {
	var order []string
	seen := make(map[string]bool, len(want))
	var visit func(name, via string) error
	visit = func(name, via string) error {
		if seen[name] {
			return nil
		}
		comp, ok := cat.Get(name)
		if !ok {
			return fmt.Errorf("resolve: 组件 %q 依赖了不存在的 %q", via, name)
		}
		seen[name] = true
		for _, dep := range comp.Requires {
			if installed[dep] {
				continue
			}
			if err := visit(dep, name); err != nil {
				return err
			}
		}
		order = append(order, name)
		return nil
	}
	for _, name := range want {
		if installed[name] {
			continue
		}
		if err := visit(name, ""); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func hasService(cat *catalog.Catalog, names []string) bool {
	for _, n := range names {
		if comp, ok := cat.Get(n); ok && comp.Kind == catalog.KindService {
			return true
		}
	}
	return false
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func supervisorFor(cat *catalog.Catalog, names []string) *catalog.Component {
	for _, n := range names {
		if comp, ok := cat.Get(n); ok && comp.Kind == catalog.KindSupervisor {
			return comp
		}
	}
	// 用户没有点名 supervisor 时，退回到清单里第一个可用的 supervisor。
	for _, name := range cat.Names() {
		comp, _ := cat.Get(name)
		if comp.Kind == catalog.KindSupervisor {
			c := *comp
			return &c
		}
	}
	return nil
}

// checkConflicts 在下载之前就把端口占用与互斥声明报出来。
// 装到一半才发现服务起不来，比装之前报错更糟；因此默认是硬错误，
// 只有显式 allow 时才降级为告警并继续。
func (p *Plan) checkConflicts(cat *catalog.Catalog, names []string, allow bool) error {
	var problems []string

	owner := make(map[int]string)
	for _, n := range names {
		comp, _ := cat.Get(n)
		for _, port := range comp.Ports {
			if prev, ok := owner[port]; ok && prev != n {
				problems = append(problems, fmt.Sprintf("端口 %d 被 %s 与 %s 同时占用", port, prev, n))
				continue
			}
			owner[port] = n
		}
	}

	chosen := make(map[string]bool, len(names))
	for _, n := range names {
		chosen[n] = true
	}
	for _, n := range names {
		comp, _ := cat.Get(n)
		for _, con := range comp.Conflicts {
			if chosen[con] && n < con { // 只报一次
				problems = append(problems, fmt.Sprintf("%s 与 %s 声明互斥", n, con))
			}
		}
	}

	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	if allow {
		p.Warnings = append(p.Warnings, problems...)
		return nil
	}
	return fmt.Errorf("resolve: 存在冲突（可用 --allow-conflicts 降级为告警）: %s", strings.Join(problems, "; "))
}

func (p *Plan) fill(cat *catalog.Catalog, names []string, arch, goos string, needsSupervisor bool) {
	for _, n := range names {
		comp, _ := cat.Get(n)
		step := Step{
			Component: comp,
			Repo:      comp.Repo(goos),
			Asset:     comp.AssetName(goos, arch),
			Binary:    comp.BinaryName(goos),
			Args:      comp.Args(goos),
			Reason:    "显式请求",
		}
		if needsSupervisor && comp.Kind == catalog.KindSupervisor {
			step.Reason = fmt.Sprintf("%s 不提供进程保活，装载自托管进程管理器", p.Init)
		}
		if !comp.SupportsArch(arch) {
			p.Warnings = append(p.Warnings,
				fmt.Sprintf("%s 未发布 %s 产物（已发布：%s），跳过", comp.Name, arch, strings.Join(comp.Archs, ", ")))
			continue
		}
		if step.Repo == "" {
			p.Warnings = append(p.Warnings,
				fmt.Sprintf("%s 在 %s 上没有对应的制品仓库，跳过", comp.Name, goos))
			continue
		}
		p.Steps = append(p.Steps, step)
	}
	if len(p.Steps) == 0 {
		p.Warnings = append(p.Warnings, "没有可在当前平台装载的组件")
	}
	sort.SliceStable(p.Warnings, func(i, j int) bool { return p.Warnings[i] < p.Warnings[j] })
}
