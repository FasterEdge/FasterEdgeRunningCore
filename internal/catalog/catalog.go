// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

// Package catalog 维护“应用商店”式的组件清单：每个条目描述一个可被运行核
// 拉取的 FasterEdge 组件——它来自哪个仓、产物叫什么、装完是常驻服务还是
// 一次性命令行工具、需要哪些前置组件、与谁冲突。
//
// 清单默认从仓库内嵌的 catalog.json 读取，也可用外部文件覆盖，因此新增组件
// 不需要重新编译运行核。
package catalog

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

//go:embed catalog.json
var embeddedJSON []byte

// Kind 描述组件被装载后的存在形态。
type Kind string

const (
	// KindService 是常驻服务，崩溃后需要进程保活。
	KindService Kind = "service"
	// KindTool 是一次性命令行工具，运行核不会为它注册保活。
	KindTool Kind = "tool"
	// KindSupervisor 是进程管理器本身。运行核在没有原生保活能力时才装载它，
	// 装载后由它承担其它 service 的保活。
	KindSupervisor Kind = "supervisor"
)

// Component 是清单中的单个组件。
type Component struct {
	Name    string `json:"name"`
	Display string `json:"display"`
	Summary string `json:"summary"`
	Kind    Kind   `json:"kind"`

	// Repos 按 GOOS 给出制品所在仓库（owner/name）。"default" 为兜底。
	Repos map[string]string `json:"repos"`
	// Stems 按 GOOS 给出产物名前缀。"default" 为兜底。
	Stems map[string]string `json:"stems"`
	// Binaries 按 GOOS 给出解包后的可执行文件名。"default" 为兜底。
	Binaries map[string]string `json:"binaries"`
	// Archives 按 GOOS 给出产物格式（tar.gz / zip）。"default" 为 tar.gz。
	Archives map[string]string `json:"archives,omitempty"`

	// Archs 是已发布产物的架构列表；为空表示“全部架构”。
	Archs []string `json:"archs,omitempty"`
	// Ports 是该组件默认监听的端口，用于安装前的占用冲突检查。
	Ports []int `json:"ports,omitempty"`
	// Requires 是硬依赖：缺任意一个都无法正常装载。
	Requires []string `json:"requires,omitempty"`
	// Conflicts 是与本组件互斥的组件名。
	Conflicts []string `json:"conflicts,omitempty"`
	// DefaultArgs 是装载服务时追加的默认启动参数。
	DefaultArgs []string `json:"default_args,omitempty"`
	// ArgsByOS 按 GOOS 追加额外启动参数。
	ArgsByOS map[string][]string `json:"args_by_os,omitempty"`
	// RequiredArgs 是启动必须具备的参数名（如 -key）。缺失时运行核应在
	// 装载前直接报错，而不是装上一个一启动就退出的“常驻服务”。
	RequiredArgs []string `json:"required_args,omitempty"`
	// SecretArgs 是“缺失时应自动生成一个随机值”的必填参数。
	// 它让 `ferc bootstrap` 真正做到一行指令即可用：终端服务的访问密码
	// 由运行核生成并记录，而不是要人先想一个再手写进命令行。
	SecretArgs []string `json:"secret_args,omitempty"`
	// Version 固定版本；为空表示跟随该仓最新 release。
	Version string `json:"version,omitempty"`
	// Essential 标记“一键默认”是否包含该组件。
	Essential bool `json:"essential,omitempty"`
	// Notes 是安装后必须让人知道的前置条件或后续动作。
	Notes string `json:"notes,omitempty"`
}

// Catalog 是一份完整清单。
type Catalog struct {
	SchemaVersion int         `json:"schema_version"`
	Components    []Component `json:"components"`
	byName        map[string]*Component
}

// Load 解析清单 JSON。
func Load(raw []byte) (*Catalog, error) {
	var c Catalog
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("catalog: 解析清单失败: %w", err)
	}
	if c.SchemaVersion != 1 {
		return nil, fmt.Errorf("catalog: 不支持的 schema_version %d", c.SchemaVersion)
	}
	if len(c.Components) == 0 {
		return nil, errors.New("catalog: 清单为空")
	}
	c.byName = make(map[string]*Component, len(c.Components))
	for i := range c.Components {
		comp := &c.Components[i]
		if comp.Name == "" {
			return nil, errors.New("catalog: 存在没有 name 的组件")
		}
		if _, dup := c.byName[comp.Name]; dup {
			return nil, fmt.Errorf("catalog: 组件 %q 重复定义", comp.Name)
		}
		if comp.Kind != KindService && comp.Kind != KindTool && comp.Kind != KindSupervisor {
			return nil, fmt.Errorf("catalog: 组件 %q 的 kind %q 非法", comp.Name, comp.Kind)
		}
		if comp.Display == "" {
			comp.Display = comp.Name
		}
		c.byName[comp.Name] = comp
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Builtin 返回内嵌清单。
func Builtin() (*Catalog, error) { return Load(embeddedJSON) }

func (c *Catalog) validate() error {
	for _, comp := range c.Components {
		for _, dep := range comp.Requires {
			if _, ok := c.byName[dep]; !ok {
				return fmt.Errorf("catalog: 组件 %q 依赖了不存在的 %q", comp.Name, dep)
			}
			if dep == comp.Name {
				return fmt.Errorf("catalog: 组件 %q 依赖了自身", comp.Name)
			}
		}
		for _, con := range comp.Conflicts {
			if _, ok := c.byName[con]; !ok {
				return fmt.Errorf("catalog: 组件 %q 声明了不存在的冲突项 %q", comp.Name, con)
			}
		}
	}
	if err := c.detectCycle(); err != nil {
		return err
	}
	return nil
}

func (c *Catalog) detectCycle() error {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(c.Components))
	var visit func(name string, path []string) error
	visit = func(name string, path []string) error {
		switch color[name] {
		case gray:
			return fmt.Errorf("catalog: 依赖成环: %s", strings.Join(append(path, name), " -> "))
		case black:
			return nil
		}
		color[name] = gray
		comp, ok := c.byName[name]
		if !ok {
			return nil
		}
		for _, dep := range comp.Requires {
			if err := visit(dep, append(path, name)); err != nil {
				return err
			}
		}
		color[name] = black
		return nil
	}
	names := c.Names()
	for _, n := range names {
		if err := visit(n, nil); err != nil {
			return err
		}
	}
	return nil
}

// Names 返回按字母序排列的全部组件名。
func (c *Catalog) Names() []string {
	out := make([]string, 0, len(c.Components))
	for _, comp := range c.Components {
		out = append(out, comp.Name)
	}
	sort.Strings(out)
	return out
}

// Get 按名字取组件。
func (c *Catalog) Get(name string) (*Component, bool) {
	comp, ok := c.byName[name]
	return comp, ok
}

// Essentials 返回“一键默认”应当装载的组件名。
func (c *Catalog) Essentials() []string {
	var out []string
	for _, comp := range c.Components {
		if comp.Essential {
			out = append(out, comp.Name)
		}
	}
	sort.Strings(out)
	return out
}

func lookup(m map[string]string, goos, fallback string) string {
	if v, ok := m[goos]; ok && v != "" {
		return v
	}
	if v, ok := m["default"]; ok && v != "" {
		return v
	}
	return fallback
}

// Repo 返回该组件在指定操作系统上对应的制品仓库。
func (comp *Component) Repo(goos string) string { return lookup(comp.Repos, goos, "") }

// ArchiveExt 返回该组件在指定操作系统上的产物后缀（tar.gz 或 zip）。
//
// 缺省按各仓 release workflow 的既有约定推导：windows 出 zip，其余出 tar.gz。
// 显式配置 archives 时以配置为准（例如某仓将来给 windows 也发 tar.gz）。
func (comp *Component) ArchiveExt(goos string) string {
	def := "tar.gz"
	if goos == "windows" {
		def = "zip"
	}
	if v, ok := comp.Archives[goos]; ok && v != "" {
		return v
	}
	if v, ok := comp.Archives["default"]; ok && v != "" {
		return v
	}
	return def
}

// Target 返回产物命名所用的目标三元组，例如 linux-arm64。
func Target(goos, arch string) string { return goos + "-" + arch }

// AssetName 返回该组件在指定平台上的产物文件名。
func (comp *Component) AssetName(goos, arch string) string {
	stem := lookup(comp.Stems, goos, comp.Name)
	return fmt.Sprintf("%s-%s.%s", stem, Target(goos, arch), comp.ArchiveExt(goos))
}

// BinaryName 返回该组件在指定操作系统上解包后的可执行文件名。
func (comp *Component) BinaryName(goos string) string {
	def := comp.Name
	if goos == "windows" {
		def += ".exe"
	}
	return lookup(comp.Binaries, goos, def)
}

// Args 返回该组件在指定操作系统上装载服务时使用的启动参数。
func (comp *Component) Args(goos string) []string {
	out := make([]string, 0, len(comp.DefaultArgs)+len(comp.ArgsByOS[goos]))
	out = append(out, comp.DefaultArgs...)
	out = append(out, comp.ArgsByOS[goos]...)
	return out
}

// MissingRequiredArgs 返回缺失的必填参数名。
//
// 这是“装上一个一启动就退出的服务”的防线：SimpleWebShell 没有 -key 会直接
// 退出，如果运行核照样把它注册成常驻服务，节点表面上装好了、实际什么都没跑，
// 而这恰恰是最难排查的失败模式。
func (comp *Component) MissingRequiredArgs(args []string) []string {
	if len(comp.RequiredArgs) == 0 {
		return nil
	}
	present := make(map[string]bool, len(args))
	for _, a := range args {
		// 同时接受 "-key value" 与 "-key=value" 两种写法。
		if k, _, ok := strings.Cut(a, "="); ok {
			present[k] = true
		} else {
			present[a] = true
		}
	}
	var missing []string
	for _, req := range comp.RequiredArgs {
		if !present[req] {
			missing = append(missing, req)
		}
	}
	return missing
}

// SupportsArch 报告该组件是否发布了目标架构的产物。
func (comp *Component) SupportsArch(arch string) bool {
	if len(comp.Archs) == 0 {
		return true
	}
	for _, a := range comp.Archs {
		if a == arch {
			return true
		}
	}
	return false
}
