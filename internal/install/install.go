// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

// Package install 把 resolve 的计划真正落到磁盘上。
//
// 事务语义：每一步都先在暂存目录里完成“下载 → 校验 → 解包 → 挑出可执行文件”，
// 全部成功后才原子地移动到最终位置并写入状态文件。任何一步失败都会回滚已移动
// 的文件，不会留下“装了一半、状态文件和磁盘对不上”的节点——那种节点比没装
// 更麻烦，因为没人知道它到底在跑什么。
package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/catalog"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/fetch"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/platform"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/resolve"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/state"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/supervise"
)

// Logger 接收安装过程的人类可读日志。
type Logger interface {
	Logf(format string, args ...any)
}

// Options 是安装器配置。
type Options struct {
	// Root 是运行核数据根目录（bin/、cache/、logs/、ferc-state.json 都在其下）。
	Root string
	// Info 是目标平台探测结果，用于写入状态文件，让节点“自述身份”。
	Info platform.Info
	// Fetcher 负责访问 GitHub；可在测试中替换。
	Fetcher *fetch.Client
	// Supervisor 负责注册服务；为 nil 表示只落盘不托管（--no-start）。
	Supervisor *supervise.Manager
	// Token 是 GitHub token，仅在未提供 Fetcher 时用于构造默认客户端。
	Token string
	// DryRun 时不下载不落盘，仅打印将要做什么。
	DryRun bool
	// Log 允许为 nil。
	Log Logger
}

// Installer 执行安装。
type Installer struct {
	opt Options
	st  *state.State
	log Logger
}

func (o Options) logger() Logger {
	if o.Log != nil {
		return o.Log
	}
	return nopLogger{}
}

type nopLogger struct{}

func (nopLogger) Logf(string, ...any) {}

// New 加载状态文件并返回安装器。
func New(opt Options) (*Installer, error) {
	if opt.Root == "" {
		return nil, errors.New("install: 缺少数据根目录")
	}
	st, err := state.Load(state.Path(opt.Root))
	if err != nil {
		return nil, err
	}
	// 状态文件要让节点“自述身份”，否则 status 只能给出空壳。
	info := opt.Info
	if info.GOOS != "" {
		st.GOOS = info.GOOS
	}
	if info.GOARCH != "" {
		st.Arch = info.GOARCH
	}
	if info.Hostname != "" {
		st.Hostname = info.Hostname
	}
	st.Root = opt.Root
	return &Installer{opt: opt, st: st, log: opt.logger()}, nil
}

// State 暴露当前状态，供 CLI 打印。
func (i *Installer) State() *state.State { return i.st }

// BinDir 是组件可执行文件的落盘目录。
func (i *Installer) BinDir() string { return filepath.Join(i.opt.Root, "bin") }

// LogDir 是日志目录。
func (i *Installer) LogDir() string { return filepath.Join(i.opt.Root, "logs") }

// Apply 按计划装载。返回装载后的记录（仅成功的）。
//
// 失败时会把本次已落盘的文件与已注册的服务全部回滚，状态文件保持原样。
func (i *Installer) Apply(ctx context.Context, plan *resolve.Plan) ([]state.Record, error) {
	for _, w := range plan.Warnings {
		i.log.Logf("警告: %s", w)
	}
	if len(plan.Steps) == 0 {
		return nil, nil
	}
	// dry-run 必须真正零副作用，包括不创建目录。
	if !i.opt.DryRun {
		if err := os.MkdirAll(i.BinDir(), 0o755); err != nil {
			return nil, fmt.Errorf("install: 建目录 %s 失败: %w", i.BinDir(), err)
		}
	}

	// staged 记录本次已移动到最终位置的文件，回滚时按逆序删除。
	var (
		staged  []string
		regs    []supervise.Result
		records []state.Record
	)
	rollback := func() {
		for n := len(regs) - 1; n >= 0; n-- {
			if i.opt.Supervisor != nil {
				if err := i.opt.Supervisor.Unregister(ctx, regs[n]); err != nil {
					i.log.Logf("回滚: 撤销服务注册 %s 失败: %v", regs[n].Unit, err)
				}
			}
		}
		for n := len(staged) - 1; n >= 0; n-- {
			if err := os.Remove(staged[n]); err != nil && !errors.Is(err, os.ErrNotExist) {
				i.log.Logf("回滚: 删除 %s 失败: %v", staged[n], err)
			}
		}
	}

	for _, step := range plan.Steps {
		rec, reg, err := i.applyStep(ctx, step)
		if err != nil {
			rollback()
			return nil, err
		}
		if rec.Path != "" {
			staged = append(staged, rec.Path)
		}
		if reg.Unit != "" {
			regs = append(regs, reg)
		}
		records = append(records, rec)
	}

	for _, rec := range records {
		i.st.Put(rec)
	}
	if err := i.st.Save(state.Path(i.opt.Root)); err != nil {
		rollback()
		return nil, fmt.Errorf("install: 写状态文件失败: %w", err)
	}
	return records, nil
}

func (i *Installer) applyStep(ctx context.Context, step resolve.Step) (state.Record, supervise.Result, error) {
	comp := step.Component
	i.log.Logf("装载 %s（%s）: %s", comp.Name, comp.Kind, step.Reason)
	if i.opt.DryRun {
		i.log.Logf("  [dry-run] 下载 %s", step.Asset)
		return state.Record{Name: comp.Name, Kind: string(comp.Kind), Version: "dry-run", Ports: comp.Ports}, supervise.Result{}, nil
	}

	binDir := i.BinDir()
	finalPath := filepath.Join(binDir, step.Binary)

	ver, err := i.installBinary(ctx, step, finalPath)
	if err != nil {
		return state.Record{}, supervise.Result{}, fmt.Errorf("install: 装载 %s 失败: %w", comp.Name, err)
	}
	i.log.Logf("  已就位 %s (%s)", finalPath, ver)

	rec := state.Record{
		Name:    comp.Name,
		Kind:    string(comp.Kind),
		Version: ver,
		Path:    finalPath,
		Args:    step.Args,
		Ports:   comp.Ports,
	}

	if i.opt.Supervisor == nil {
		i.log.Logf("  未注册服务（--no-start）")
		return rec, supervise.Result{}, nil
	}

	// supervisor 自身不需要被别人托管，它本身就是托管者。
	if comp.Kind == catalog.KindSupervisor {
		rec.Supervisor = "self"
		return rec, supervise.Result{}, nil
	}
	if comp.Kind == catalog.KindTool {
		i.log.Logf("  一次性工具，不注册保活")
		return rec, supervise.Result{}, nil
	}

	res, err := i.opt.Supervisor.Register(ctx, supervise.Options{
		Unit:        "ferc-" + comp.Name,
		BinPath:     finalPath,
		Args:        step.Args,
		Dir:         binDir,
		LogDir:      filepath.Join(i.LogDir(), comp.Name),
		Description: comp.Display,
	})
	for _, w := range res.Warnings {
		i.log.Logf("  提示: %s", w)
	}
	if err != nil {
		return state.Record{}, res, fmt.Errorf("install: 注册 %s 服务失败: %w", comp.Name, err)
	}
	rec.Supervisor = res.Supervisor
	rec.Unit = res.Unit
	i.log.Logf("  托管方 %s，单元 %s", res.Supervisor, res.Unit)
	return rec, res, nil
}

// installBinary 下载、校验、解包，并把可执行文件原子放到 finalPath。
func (i *Installer) installBinary(ctx context.Context, step resolve.Step, finalPath string) (string, error) {
	work, err := os.MkdirTemp(filepath.Join(i.opt.Root), ".ferc-stage-")
	if err != nil {
		return "", fmt.Errorf("建暂存目录失败: %w", err)
	}
	defer os.RemoveAll(work)

	rel, err := i.fetcher().LatestRelease(ctx, step.Repo)
	if err != nil {
		return "", err
	}
	asset, ok := rel.FindAsset(step.Asset)
	if !ok {
		return "", fmt.Errorf("%s 的 release %s 里没有产物 %q（可用：%s）",
			step.Repo, rel.Tag, step.Asset, assetNames(rel))
	}

	arcPath := filepath.Join(work, step.Asset)
	if _, err := i.fetcher().Download(ctx, asset.URL, arcPath); err != nil {
		return "", err
	}

	note, err := i.verify(ctx, rel, step.Asset, work, arcPath)
	if err != nil {
		return "", err
	}
	if note != "" {
		i.log.Logf("  %s", note)
	}

	unpacked := filepath.Join(work, "unpacked")
	files, err := fetch.Extract(arcPath, unpacked)
	if err != nil {
		return "", err
	}
	src, err := findBinary(unpacked, files, step.Binary)
	if err != nil {
		return "", err
	}
	if err := moveFile(src, finalPath); err != nil {
		return "", err
	}
	return rel.Tag, nil
}

// verify 尽量用 release 自带的校验文件核对产物；没有校验文件时不算失败，
// 但会明确说明“本次没有校验”，不假装校验过。
func (i *Installer) verify(ctx context.Context, rel *fetch.Release, assetName, work, arcPath string) (string, error) {
	for _, cand := range []struct {
		name string
		fn   func(sumFile, target, name string) (bool, error)
	}{
		{"SHA256SUMS.txt", fetch.VerifySHA256},
		{"checksums-sha256.txt", fetch.VerifySHA256},
		{"checksums-sha1.txt", fetch.VerifySHA1},
	} {
		asset, ok := rel.FindAsset(cand.name)
		if !ok {
			continue
		}
		p := filepath.Join(work, cand.name)
		if _, err := i.fetcher().Download(ctx, asset.URL, p); err != nil {
			// 校验文件拿不到就直接判失败：不能因为"校验文件下载失败"而跳过校验。
			return "", fmt.Errorf("下载校验文件 %s 失败: %w", cand.name, err)
		}
		matched, err := cand.fn(p, arcPath, assetName)
		if err != nil {
			return "", err
		}
		if matched {
			return "已通过 " + cand.name + " 校验", nil
		}
	}
	return "release 未提供覆盖本产物的校验文件，本次未校验", nil
}

func assetNames(rel *fetch.Release) string {
	names := make([]string, 0, len(rel.Assets))
	for _, a := range rel.Assets {
		names = append(names, a.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func (i *Installer) fetcher() *fetch.Client {
	if i.opt.Fetcher != nil {
		return i.opt.Fetcher
	}
	return fetch.New(i.opt.Token)
}

// findBinary 在解包结果里定位可执行文件。
// 优先精确同名，其次同目录下唯一可执行文件，最后退化为“任意同名（含子目录）”。
func findBinary(root string, files []string, want string) (string, error) {
	var exact []string
	var baseMatch []string
	for _, f := range files {
		full := filepath.Join(root, f)
		if filepath.Base(f) == want {
			if filepath.Dir(f) == "." {
				exact = append(exact, full)
			} else {
				baseMatch = append(baseMatch, full)
			}
		}
	}
	sort.Strings(exact)
	if len(exact) > 0 {
		return exact[0], nil
	}
	sort.Strings(baseMatch)
	if len(baseMatch) > 0 {
		return baseMatch[0], nil
	}
	// 有些产物里的可执行文件略去了扩展名差异（例如 Windows 上少写 .exe）。
	stem := strings.TrimSuffix(want, ".exe")
	for _, f := range files {
		if strings.TrimSuffix(filepath.Base(f), ".exe") == stem {
			return filepath.Join(root, f), nil
		}
	}
	return "", fmt.Errorf("产物里找不到可执行文件 %q（解出 %d 个文件，例如 %s）", want, len(files), sample(files))
}

func sample(files []string) string {
	if len(files) == 0 {
		return "无"
	}
	n := len(files)
	if n > 5 {
		n = 5
	}
	return strings.Join(files[:n], ", ")
}

// moveFile 跨盘安全地移动文件并赋予可执行权限。
func moveFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return os.Chmod(dst, 0o755)
	}
	// 跨设备时 rename 会失败，退化为复制。
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Remove(src)
}
