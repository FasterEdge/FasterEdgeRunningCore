// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

// Package snapshot 实现节点的环境快照：改坏了可以回归到之前的完好状态。
//
// 装载事务回滚（internal/install）只覆盖“一次 install 中途失败”，命令一退出
// 就失效；快照覆盖的是另一类事故——装载成功之后，运维者或 Agent 改错了端口、
// 删了配置、装了个不兼容的新版本，此时需要把整台端退回到某个已知良好状态。
//
// 快照目录与 internal/mirror 的镜像保持同一套哲学：只记相对位置、不记绝对
// 地址，因此整份目录可以拷到 U 盘、可以 mirror export 带走、也可以在另一台
// 同架构的机器上还原。
package snapshot

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/state"
)

// SchemaVersion 是快照格式版本。格式变化时递增，让旧客户端明确报错而不是
// 按新格式解释出一份错数据。
const SchemaVersion = 1

// MetaName 是快照的元数据文件名。
const MetaName = "snapshot.json"

// StateName 是快照里保存的状态文件副本名。
const StateName = "ferc-state.json"

// BinDirName 是快照里保存可执行文件的子目录名。
const BinDirName = "bin"

// Meta 描述一份快照。
type Meta struct {
	SchemaVersion int       `json:"schema_version"`
	ID            string    `json:"id"`
	CreatedAt     time.Time `json:"created_at"`
	// Note 是运维者留下的备注，例如“升级到 1.0.20261127 之前”。
	Note string `json:"note,omitempty"`
	// Hostname / GOOS / Arch 记录这份快照来自什么样的机器。
	//
	// 还原时用它提醒“这份快照不是在当前机器上打的”：跨架构还原没有意义
	// （二进制根本跑不起来），跨主机还原则要确认端口没被别人占。
	Hostname string `json:"hostname,omitempty"`
	GOOS     string `json:"goos,omitempty"`
	Arch     string `json:"arch,omitempty"`
	// Root 是打快照时的数据根目录，仅作提示；还原时用的是当前 --root。
	Root string `json:"root,omitempty"`
	// Components 是快照收录的组件，含版本与二进制校验和。
	Components []Component `json:"components"`
	// FercVersion 是打快照时运行核自身的版本。
	FercVersion string `json:"ferc_version,omitempty"`
}

// Component 是快照里的一个组件。
type Component struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Kind    string `json:"kind,omitempty"`
	// Bin 是快照里保存的可执行文件名（位于快照的 bin/ 下）。为空表示打快照时
	// 二进制已缺失——快照仍然收录它，让运维者看见“这里本来有个东西”。
	Bin string `json:"bin,omitempty"`
	// SHA256 是二进制内容的校验和；Bin 为空时也为空。还原时逐字节核对，
	// 防止 U 盘拷了一半或者目录被改过。
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

// Summary 是快照的概要信息，供 list 输出与机器读取。
type Summary struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	Note       string    `json:"note,omitempty"`
	Hostname   string    `json:"hostname,omitempty"`
	GOOS       string    `json:"goos,omitempty"`
	Arch       string    `json:"arch,omitempty"`
	Components int       `json:"components"`
	// Missing 是快照里二进制缺失的组件数。非 0 表示这份快照不完整，
	// 还原前必须让人知道，否则会以为回到了完好状态。
	Missing int    `json:"missing"`
	Bytes   int64  `json:"bytes"`
	Path    string `json:"-"`
	Meta    *Meta  `json:"-"`
}

// CreateOptions 控制 Create 的行为。
type CreateOptions struct {
	// Base 是快照存放目录，例如 <root>/snapshots。
	Base string
	// Note 是备注。
	Note string
	// Log 接收逐组件进度；可为 nil。
	Log func(format string, args ...any)
	// Now 返回当前时间；为 nil 用 time.Now。测试可注入以固定快照 ID。
	Now func() time.Time
}

// CreateResult 汇报一次 Create。
type CreateResult struct {
	ID         string
	Dir        string
	Components int
	Missing    int
	Bytes      int64
}

// Create 打一份快照。
//
// 只存状态文件 + 各组件可执行文件。刻意不存组件自己生成的配置与凭据：那些
// 是运行时产物，存它们会让“回到过去”把后来产生的数据一起倒回去，而运维者
// 想回到的通常是“装了哪些东西、什么版本”这一层。
func Create(st *state.State, statePath string, fercVersion string, opt CreateOptions) (*CreateResult, error) {
	if opt.Base == "" {
		return nil, errors.New("snapshot: 缺少快照目录")
	}
	if st == nil {
		return nil, errors.New("snapshot: 缺少状态")
	}
	logf := opt.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	now := opt.Now
	if now == nil {
		now = time.Now
	}

	createdAt := now().UTC()
	id := uniqueID(opt.Base, createdAt)
	dir := filepath.Join(opt.Base, id)
	if err := os.MkdirAll(filepath.Join(dir, BinDirName), 0o755); err != nil {
		return nil, err
	}

	meta := &Meta{
		SchemaVersion: SchemaVersion,
		ID:            id,
		CreatedAt:     createdAt,
		Note:          opt.Note,
		Hostname:      st.Hostname,
		GOOS:          st.GOOS,
		Arch:          st.Arch,
		Root:          st.Root,
		FercVersion:   fercVersion,
	}

	var total int64
	missing := 0
	// 按名字排序，让同一份状态每次打出的快照字节一致、可对比。
	recs := append([]state.Record(nil), st.Components...)
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].Name < recs[j].Name })

	for _, rec := range recs {
		comp := Component{Name: rec.Name, Version: rec.Version, Kind: rec.Kind}
		data, err := os.ReadFile(rec.Path)
		if err != nil {
			// 二进制不在也照样收录：少了它这份快照不完整，但“这里本来有个组件”
			// 这件事本身有价值，而且 Missing 会让它在 list 里显眼。
			missing++
			logf("  %s 二进制缺失，快照仅记录状态: %v", rec.Name, err)
			meta.Components = append(meta.Components, comp)
			continue
		}
		sum := sha256.Sum256(data)
		comp.SHA256 = hex.EncodeToString(sum[:])
		comp.Size = int64(len(data))
		// 快照里的文件名用组件名加原扩展名：原文件名在不同架构下不同，而一份
		// 快照本来就是给同一台端用的，用组件名更直观，也顺带保住 .exe 后缀。
		comp.Bin = rec.Name + filepath.Ext(rec.Path)
		if err := os.WriteFile(filepath.Join(dir, BinDirName, comp.Bin), data, 0o755); err != nil {
			return nil, fmt.Errorf("snapshot: 保存 %s 失败: %w", rec.Name, err)
		}
		total += comp.Size
		logf("  %s %s（%d 字节）", rec.Name, rec.Version, comp.Size)
		meta.Components = append(meta.Components, comp)
	}

	if err := writeJSON(filepath.Join(dir, MetaName), meta); err != nil {
		return nil, err
	}
	// 状态文件原样复制：它就是节点的安装事实，还原时直接拿它当目标状态。
	stateBytes, err := os.ReadFile(statePath)
	if err != nil {
		return nil, fmt.Errorf("snapshot: 读状态文件失败: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, StateName), stateBytes, 0o644); err != nil {
		return nil, err
	}
	total += int64(len(stateBytes))

	return &CreateResult{
		ID:         id,
		Dir:        dir,
		Components: len(meta.Components),
		Missing:    missing,
		Bytes:      total,
	}, nil
}

// List 列出快照目录里的所有快照，按创建时间从新到旧。
func List(base string) ([]Summary, error) {
	if base == "" {
		return nil, errors.New("snapshot: 缺少快照目录")
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Summary, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		meta, err := loadMeta(filepath.Join(base, e.Name(), MetaName))
		if err != nil {
			// 目录存在但没有合法元数据：跳过而不是让整条 list 失败，现场可能
			// 残留了半截目录（例如创建中途断电）。
			continue
		}
		out = append(out, summaryOf(base, meta))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

// Load 读一份快照的元数据。id 为空表示取最新的一份。
func Load(base, id string) (*Meta, string, error) {
	if id == "" {
		list, err := List(base)
		if err != nil {
			return nil, "", err
		}
		if len(list) == 0 {
			return nil, "", fmt.Errorf("snapshot: %s 里没有任何快照", base)
		}
		return list[0].Meta, list[0].Path, nil
	}
	dir := filepath.Join(base, id)
	meta, err := loadMeta(filepath.Join(dir, MetaName))
	if err != nil {
		return nil, "", fmt.Errorf("snapshot: 读快照 %s 失败: %w", id, err)
	}
	return meta, dir, nil
}

// Delete 删除一份快照。
func Delete(base, id string) error {
	if id == "" {
		return errors.New("snapshot: 缺少快照 ID")
	}
	dir := filepath.Join(base, id)
	if _, err := loadMeta(filepath.Join(dir, MetaName)); err != nil {
		return fmt.Errorf("snapshot: 快照 %s 不存在", id)
	}
	return os.RemoveAll(dir)
}

func loadMeta(p string) (*Meta, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.SchemaVersion > SchemaVersion {
		return nil, fmt.Errorf("快照格式版本 %d 高于本程序支持的 %d，请升级运行核", m.SchemaVersion, SchemaVersion)
	}
	if m.ID == "" {
		return nil, errors.New("快照缺少 ID")
	}
	return &m, nil
}

func summaryOf(base string, m *Meta) Summary {
	s := Summary{
		ID:         m.ID,
		CreatedAt:  m.CreatedAt,
		Note:       m.Note,
		Hostname:   m.Hostname,
		GOOS:       m.GOOS,
		Arch:       m.Arch,
		Components: len(m.Components),
		Path:       filepath.Join(base, m.ID),
		Meta:       m,
	}
	for _, c := range m.Components {
		if c.Bin == "" {
			s.Missing++
			continue
		}
		s.Bytes += c.Size
	}
	return s
}

// newID 生成快照 ID：时间戳 + 一段随机后缀。
//
// 时间戳只精确到秒，而“升级前打一份、装完再打一份”完全可能落在同一秒内，
// 纯靠时间戳会算出同一个 ID、后一份直接把前一份覆盖掉。因此额外混入随机
// 字节；万一仍撞上（概率约 1/2^24），由调用方的“ID 已存在就换一个”兜底。
func newID(t time.Time) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 几乎不会失败；真失败了也别让打快照这件事挂掉，
		// 退化成用纳秒时间戳。
		return t.UTC().Format("20060102-150405") + "-" + strconv.FormatInt(t.UnixNano(), 16)[:6]
	}
	return t.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// uniqueID 返回一个尚未被占用的快照 ID。
func uniqueID(base string, t time.Time) string {
	for i := 0; i < 8; i++ {
		id := newID(t)
		if _, err := os.Stat(filepath.Join(base, id)); err != nil {
			return id
		}
	}
	// 八次都撞上只能说明时钟停了；用纳秒后缀强制区分。
	return t.UTC().Format("20060102-150405") + "-" + strconv.FormatInt(t.UnixNano(), 16)
}

func writeJSON(p string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".ferc-snap-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, p)
}
