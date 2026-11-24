// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/state"
)

// RestoreOptions 控制一次回归。
type RestoreOptions struct {
	// Base 是快照存放目录。
	Base string
	// ID 是要回归的快照；为空表示最新的一份。
	ID string
	// Root 是当前数据根目录（二进制与状态文件写到这里）。
	Root string
	// Hostname / GOOS / Arch 是当前机器的身份。为空时取本机实测值。
	//
	// 这三项必须允许注入：ferc 完全可能在 Windows 工作站上管理一台 Linux
	// 边缘节点（--catalog / --root 都指向那台机导出的目录），此时若拿
	// runtime.GOOS 做跨平台校验，会把这套正当用法整个挡掉。
	Hostname string
	GOOS     string
	Arch     string
	// Log 接收进度；可为 nil。
	Log func(format string, args ...any)
	// VerifyChecksum 为真时逐字节核对二进制；缺省开启，只有测试会关掉。
	VerifyChecksum bool
}

// RestoreItem 是一个组件的回归结果。
type RestoreItem struct {
	Name     string `json:"name"`
	Version  string `json:"version,omitempty"`
	Outcome  string `json:"outcome"`
	Detail   string `json:"detail,omitempty"`
	SHA256OK bool   `json:"sha256_ok,omitempty"`
}

// RestoreResult 汇报一次回归。
type RestoreResult struct {
	ID        string        `json:"id"`
	CreatedAt string        `json:"created_at,omitempty"`
	Note      string        `json:"note,omitempty"`
	FromHost  string        `json:"from_host,omitempty"`
	Restored  int           `json:"restored"`
	Failed    int           `json:"failed"`
	Items     []RestoreItem `json:"items"`
	// Warnings 是“做了但需要人知道”的事：跨架构、跨主机、快照不完整等。
	Warnings []string `json:"warnings,omitempty"`
}

// Restore 把节点退回到指定快照。
//
// 顺序是刻意安排的：
//  1. 先核对快照自身完整、与当前机器兼容（架构不一致直接拒绝）；
//  2. 再把快照里的二进制逐个还原到 <root>/bin，并逐字节校验；
//  3. 最后用快照自带的状态文件**整体替换**当前状态文件。
//
// 第 3 步是整体替换而不是逐条合并：合并会让“回归”变成“打补丁”，那些在快照
// 之后才装上去的组件会留下来，节点既不是过去也不是现在。整体替换才能保证
// “回到那个状态”这句话是真的。
//
// 托管关系的重建不在这里做：调用方（CLI）在拿到结果后接着跑一遍 restore
// 逻辑即可，两边都不重复实现“怎么把服务注册回去”。
func Restore(ctx context.Context, opt RestoreOptions) (*RestoreResult, error) {
	if opt.Base == "" || opt.Root == "" {
		return nil, errors.New("snapshot: 回归需要快照目录与数据根目录")
	}
	logf := opt.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}

	meta, dir, err := Load(opt.Base, opt.ID)
	if err != nil {
		return nil, err
	}
	res := &RestoreResult{
		ID:        meta.ID,
		CreatedAt: meta.CreatedAt.UTC().Format("2006-01-02 15:04:05Z"),
		Note:      meta.Note,
		FromHost:  meta.Hostname,
		Items:     []RestoreItem{},
	}

	// 快照自身完整吗？二进制缺失的组件还原不了，必须说出来。
	var missing []string
	for _, c := range meta.Components {
		if c.Bin == "" {
			missing = append(missing, c.Name)
		}
	}
	if len(missing) > 0 {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"快照 %s 有 %d 个组件的二进制缺失（%s），它们只会回到状态记录、不会恢复可执行文件",
			meta.ID, len(missing), joinNames(missing)))
	}

	// 架构不一致就别硬来：二进制根本跑不起来，还原完只会得到一堆起不来的服务。
	if meta.GOOS != "" && meta.Arch != "" {
		cur := archPair{GOOS: opt.GOOS, Arch: opt.Arch}
		if cur.GOOS == "" || cur.Arch == "" {
			cur = currentArch()
		}
		if meta.GOOS != cur.GOOS || meta.Arch != cur.Arch {
			return nil, fmt.Errorf(
				"snapshot: 快照来自 %s/%s，当前目标是 %s/%s，跨平台回归没有意义（请换对应平台的快照）",
				meta.GOOS, meta.Arch, cur.GOOS, cur.Arch)
		}
	}

	binDir := filepath.Join(opt.Root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return nil, err
	}

	for _, c := range meta.Components {
		if c.Bin == "" {
			res.Items = append(res.Items, RestoreItem{
				Name: c.Name, Version: c.Version, Outcome: "skipped",
				Detail: "快照里没有这个组件的二进制",
			})
			continue
		}
		src := filepath.Join(dir, BinDirName, c.Bin)
		dst := filepath.Join(binDir, c.Bin)
		item := RestoreItem{Name: c.Name, Version: c.Version, Outcome: "restored"}

		data, err := os.ReadFile(src)
		if err != nil {
			item.Outcome = "failed"
			item.Detail = "读快照里的二进制失败: " + err.Error()
			res.Failed++
			res.Items = append(res.Items, item)
			continue
		}
		if c.SHA256 != "" {
			sum := sha256.Sum256(data)
			got := hex.EncodeToString(sum[:])
			if got != c.SHA256 {
				item.Outcome = "failed"
				item.Detail = "校验和不符，快照可能已损坏"
				res.Failed++
				res.Items = append(res.Items, item)
				continue
			}
			item.SHA256OK = true
		}
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			item.Outcome = "failed"
			item.Detail = "写二进制失败: " + err.Error()
			res.Failed++
			res.Items = append(res.Items, item)
			continue
		}
		res.Restored++
		logf("  还原 %s %s", c.Name, c.Version)
		res.Items = append(res.Items, item)
	}

	if res.Failed > 0 {
		return res, fmt.Errorf("snapshot: %d 个组件还原失败，状态文件未改动", res.Failed)
	}

	// 二进制全部就位后才动状态文件：中途失败不会把节点留在一个“文件是新的、
	// 状态是旧的”中间态。
	src := filepath.Join(dir, StateName)
	b, err := os.ReadFile(src)
	if err != nil {
		return res, fmt.Errorf("snapshot: 读快照状态文件失败: %w", err)
	}
	// 快照里的 Root 是打快照时那台机器的，可能和当前 --root 不同；换成当前的，
	// 否则后续 remove/restore 会按旧路径找东西。
	if meta.Root != "" && meta.Root != opt.Root {
		b = rewriteRoot(b, opt.Root)
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"快照的数据根目录是 %s，已改写为当前的 %s", meta.Root, opt.Root))
	}
	dst := state.Path(opt.Root)
	if err := writeFileAtomic(dst, b); err != nil {
		return res, fmt.Errorf("snapshot: 写状态文件失败: %w", err)
	}
	logf("  状态文件已回归到快照 %s", meta.ID)

	// 跨主机还原要提醒：端口可能已被别的进程占着。
	curHost := opt.Hostname
	if curHost == "" {
		curHost = currentHostname()
	}
	if meta.Hostname != "" && meta.Hostname != curHost {
		res.Warnings = append(res.Warnings, fmt.Sprintf(
			"快照打点在主机 %s 上，当前是 %s；若两端同时运行，端口会冲突", meta.Hostname, curHost))
	}
	return res, nil
}

// rewriteRoot 把状态文件里的 root 字段换成新值。
//
// 只改 Root 一个字段，不动 components 里的 path——组件路径指向 <root>/bin，
// 而当前 root 下的 bin 刚刚被快照的二进制覆盖，两者一致。
func rewriteRoot(b []byte, root string) []byte {
	var s state.State
	if err := jsonUnmarshal(b, &s); err != nil {
		return b
	}
	s.Root = root
	out, err := jsonMarshalIndent(&s)
	if err != nil {
		return b
	}
	return out
}

func writeFileAtomic(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".ferc-restore-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, p)
}

func joinNames(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += "、"
		}
		out += v
	}
	return out
}

func currentHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}
