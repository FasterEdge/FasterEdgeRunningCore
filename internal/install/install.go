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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
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
	// Root 是运行核数据根目录（bin/、logs/、ferc-state.json 都在其下）。
	Root string
	// Info 是目标平台探测结果，用于写入状态文件，让节点“自述身份”。
	Info platform.Info
	// Source 负责取回产物清单与产物本体；可为 nil（此时按 Token 构造 GitHub 客户端）。
	// 抽成接口是为了让镜像目录 / HTTP 镜像也能驱动同一份装载逻辑。
	Source fetch.Source
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
	// selfHostedCount 记录本次已经起了几个自托管进程管理器实例：DontCrack 一个
	// 实例只托管一个进程、只占一个控制端口，第 n 个服务必须用基准端口 + n。
	selfHostedCount int
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
		rec, reg, err := i.applyStep(ctx, step, i.selfHostedCount)
		if err != nil {
			// 失败的那一步可能已经把二进制放到最终位置了（例如服务注册失败），
			// 因此也要纳入回滚，否则会留下一个谁都不认领的可执行文件。
			if rec.Path != "" {
				staged = append(staged, rec.Path)
			}
			if reg.Unit != "" {
				regs = append(regs, reg)
			}
			rollback()
			return nil, err
		}
		if rec.Supervisor == "dontcrack" {
			// 自托管管理器一个实例只占一个控制端口，下一个服务要用下一个端口。
			i.selfHostedCount++
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

func (i *Installer) applyStep(ctx context.Context, step resolve.Step, portOffset int) (state.Record, supervise.Result, error) {
	comp := step.Component
	i.log.Logf("装载 %s（%s）: %s", comp.Name, comp.Kind, step.Reason)
	if i.opt.DryRun {
		i.log.Logf("  [dry-run] 下载 %s", step.Asset)
		if len(step.Secrets) > 0 {
			i.log.Logf("  [dry-run] 会为 %s 生成随机凭据并写入 0600 文件", strings.Join(sortedKeys(step.Secrets), ", "))
		}
		return state.Record{Name: comp.Name, Kind: string(comp.Kind), Version: "dry-run", Ports: comp.Ports}, supervise.Result{}, nil
	}

	args := append([]string(nil), step.Args...)
	secretFile := ""
	if len(step.Secrets) > 0 {
		var err error
		args, secretFile, err = i.materializeSecrets(comp, args, step.Secrets)
		if err != nil {
			return state.Record{}, supervise.Result{}, fmt.Errorf("install: 为 %s 生成凭据失败: %w", comp.Name, err)
		}
		i.log.Logf("  已生成访问凭据，见 %s", secretFile)
	}

	binDir := i.BinDir()
	finalPath := filepath.Join(binDir, step.Binary)

	ver, err := i.installBinary(ctx, step, finalPath)
	if err != nil {
		return state.Record{}, supervise.Result{}, fmt.Errorf("install: 装载 %s 失败: %w", comp.Name, err)
	}
	i.log.Logf("  已就位 %s (%s)", finalPath, ver)

	rec := state.Record{
		Name:       comp.Name,
		Kind:       string(comp.Kind),
		Version:    ver,
		Path:       finalPath,
		Args:       args,
		Ports:      comp.Ports,
		SecretFile: secretFile,
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
		Args:        args,
		Dir:         binDir,
		LogDir:      filepath.Join(i.LogDir(), comp.Name),
		Description: comp.Display,
		PortOffset:  portOffset,
		Ports:       comp.Ports,
	})
	for _, w := range res.Warnings {
		i.log.Logf("  提示: %s", w)
	}
	if err != nil {
		// 连 rec 一起返回：这一步的二进制已经落盘了，必须让调用方有机会回滚，
		// 否则失败后磁盘上会留下一个没人认领的可执行文件。
		return rec, res, fmt.Errorf("install: 注册 %s 服务失败: %w", comp.Name, err)
	}
	rec.Supervisor = res.Supervisor
	rec.Unit = res.Unit
	rec.ControlPort = res.ControlPort
	rec.ManagerPID = res.ManagerPID
	if res.ControlPort > 0 {
		i.log.Logf("  托管方 %s，控制端口 %d", res.Supervisor, res.ControlPort)
	} else {
		i.log.Logf("  托管方 %s，单元 %s", res.Supervisor, res.Unit)
	}
	return rec, res, nil
}

// materializeSecrets 为缺失的必填凭据生成随机值，返回补齐后的参数与凭据文件路径。
//
// 之所以落一个 0600 文件而不是把密码塞进日志：运行核装完就退出，操作者需要一个
// 之后还能查到的地方；而日志通常权限更宽、也更容易被复制走。
func (i *Installer) materializeSecrets(comp *catalog.Component, args []string, need map[string]string) ([]string, string, error) {
	secretDir := filepath.Join(i.opt.Root, "secrets")
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		return args, "", err
	}
	out := append([]string(nil), args...)
	var lines []string
	for _, name := range sortedKeys(need) {
		value, err := randomToken()
		if err != nil {
			return args, "", err
		}
		// 同时兼容 -key value 与 -key=value：统一用等号形式，避免参数吞并。
		out = append(out, name+"="+value)
		lines = append(lines, name+"="+value)
	}
	path := filepath.Join(secretDir, comp.Name+".secret")
	body := "# 由 ferc 自动生成，仅供 " + comp.Name + " 使用\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return args, "", err
	}
	// WriteFile 对已存在文件不改权限，这里显式收紧，避免沿用历史宽松权限。
	if err := os.Chmod(path, 0o600); err != nil && runtime.GOOS != "windows" {
		return args, "", err
	}
	return out, path, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// randomToken 生成 192 位随机十六进制串作为访问凭据。
func randomToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
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

func (i *Installer) fetcher() fetch.Source {
	if i.opt.Source != nil {
		return i.opt.Source
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
