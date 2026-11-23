// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

// Package state 维护运行核在目标机上的“已装载清单”。
//
// 运行核本身不常驻，唯一的记忆就是这个 JSON 文件：它记录了装了哪些组件、
// 各自落在哪个路径、由谁托管、版本是多少。有了它，运维者才能在不登录本机
// 的前提下知道这台端上到底有什么，也才能做增量升级与卸载。
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// SchemaVersion 是状态文件格式版本。格式变化时递增，读取旧版会明确报错
// 而不是静默按新格式解释。
const SchemaVersion = 1

// Record 描述一个已装载组件。
type Record struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// Kind 是 catalog 里的 kind（service/tool/supervisor）。
	Kind string `json:"kind"`
	// Path 是落盘后的可执行文件绝对路径。
	Path string `json:"path"`
	// Args 是装载时使用的启动参数。
	Args []string `json:"args,omitempty"`
	// Supervisor 记录托管它的组件名：systemd / openrc / launchd / schtasks /
	// dontcrack。为空表示未托管（一次性工具）。
	Supervisor string `json:"supervisor,omitempty"`
	// Unit 是注册到 init 系统里的单元名（systemd unit、launchd label、
	// schtasks 任务名等）。为空表示没有注册。
	Unit string `json:"unit,omitempty"`
	// Ports 记录该组件声明的端口，供后续冲突检查复用。
	Ports []int `json:"ports,omitempty"`
	// SecretFile 是运行核自动生成的凭据文件路径（0600）。为空表示无凭据。
	SecretFile string `json:"secret_file,omitempty"`
	// ControlPort 是自托管进程管理器（DontCrack）的控制端口，仅
	// supervisor=dontcrack 时非零。卸载时靠它让管理器停掉被托管的子进程。
	ControlPort int `json:"control_port,omitempty"`
	// ManagerPID 是自托管进程管理器的进程号（仅 supervisor=dontcrack 时非零）。
	// 运行核不常驻，这是唯一能记住“谁在保活”的地方，卸载时用它清掉孤儿进程。
	ManagerPID  int       `json:"manager_pid,omitempty"`
	InstalledAt time.Time `json:"installed_at"`
}

// State 是整份状态文件。
type State struct {
	SchemaVersion int      `json:"schema_version"`
	GOOS          string   `json:"goos"`
	Arch          string   `json:"arch"`
	Hostname      string   `json:"hostname"`
	Root          string   `json:"root"`
	Components    []Record `json:"components"`
}

// New 返回一份与目标机绑定的空状态。
func New(goos, arch, hostname, root string) *State {
	return &State{SchemaVersion: SchemaVersion, GOOS: goos, Arch: arch, Hostname: hostname, Root: root}
}

// Path 返回状态文件路径。
func Path(root string) string { return filepath.Join(root, "ferc-state.json") }

// Load 读取状态文件。文件不存在时返回一份空状态而非错误——首次运行就是这种情形。
func Load(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &State{SchemaVersion: SchemaVersion}, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("state: 解析 %s 失败: %w", path, err)
	}
	if s.SchemaVersion > SchemaVersion {
		return nil, fmt.Errorf("state: 状态文件版本 %d 高于本程序支持的 %d，请升级运行核", s.SchemaVersion, SchemaVersion)
	}
	return &s, nil
}

// Save 原子地写回状态文件：先写临时文件再 rename，任何中途失败都不会
// 留下半截 JSON 让下次运行读崩。
func (s *State) Save(path string) error {
	s.SchemaVersion = SchemaVersion
	sort.SliceStable(s.Components, func(i, j int) bool { return s.Components[i].Name < s.Components[j].Name })
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".ferc-state-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Get 查找已装载组件。
func (s *State) Get(name string) (Record, bool) {
	for _, r := range s.Components {
		if r.Name == name {
			return r, true
		}
	}
	return Record{}, false
}

// Put 新增或替换一条记录。
func (s *State) Put(rec Record) {
	if rec.InstalledAt.IsZero() {
		rec.InstalledAt = time.Now().UTC()
	}
	for i := range s.Components {
		if s.Components[i].Name == rec.Name {
			s.Components[i] = rec
			return
		}
	}
	s.Components = append(s.Components, rec)
}

// Remove 删除一条记录，返回是否真的删掉了。
func (s *State) Remove(name string) bool {
	for i := range s.Components {
		if s.Components[i].Name == name {
			s.Components = append(s.Components[:i], s.Components[i+1:]...)
			return true
		}
	}
	return false
}

// Names 返回已装载组件名集合，供 resolve 判断依赖是否已满足。
func (s *State) Names() map[string]bool {
	out := make(map[string]bool, len(s.Components))
	for _, r := range s.Components {
		out[r.Name] = true
	}
	return out
}

// NamesSorted 返回按字母序排列的已装载组件名。
func (s *State) NamesSorted() []string {
	out := make([]string, 0, len(s.Components))
	for _, r := range s.Components {
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return out
}
