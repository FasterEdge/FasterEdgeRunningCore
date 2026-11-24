// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/fetch"
)

// ExportOptions 控制一次“把 release 抄进镜像目录”的行为。
type ExportOptions struct {
	// Base 是导出目标目录。
	Base string
	// Repos 要导出的仓库（owner/name）。为空表示按 catalog 推导。
	Repos []string
	// Archs 限定只导出这些架构标签的产物（如 amd64、armv7）；为空表示全部。
	Archs []string
	// Log 接收逐产物进度；可为 nil。
	Log func(format string, args ...any)
}

// ExportResult 汇报一次导出的结果。
type ExportResult struct {
	// Repos 是成功导出的仓库，含 tag 与产物数。
	Repos []ExportEntry
	// Warnings 是“跳过但需要人知道”的事。
	Warnings []string
}

// ExportEntry 是单个仓库的导出结果。
type ExportEntry struct {
	Repo   string
	Tag    string
	Assets []string
}

// Export 把若干仓库的最新 release 抄进镜像目录。
//
// 关键点：产物写完当场算 SHA256 并记进 release.json。原仓库有的发 checksums、
// 有的完全不发（DontCrack4Windows 就没有），镜像自己补上这一层，客户端才能
// 对“从邻居那儿拿到的包”做完整性校验。
func Export(ctx context.Context, src fetch.Source, opt ExportOptions) (*ExportResult, error) {
	if opt.Base == "" {
		return nil, fmt.Errorf("mirror: 缺少导出目录")
	}
	if len(opt.Repos) == 0 {
		return nil, fmt.Errorf("mirror: 没有要导出的仓库")
	}
	logf := opt.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if err := os.MkdirAll(opt.Base, 0o755); err != nil {
		return nil, err
	}

	res := &ExportResult{}
	idx := &Index{SchemaVersion: SchemaVersion, GeneratedAt: time.Now().UTC()}
	// 允许增量导出：先读旧索引，把本次没涉及的仓库原样留下。
	if old, err := readIndexFile(IndexPath(opt.Base)); err == nil {
		idx.Components = old.Components
	}

	for _, repo := range opt.Repos {
		rel, err := src.LatestRelease(ctx, repo)
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s 取最新 release 失败，跳过: %v", repo, err))
			continue
		}
		entry, err := exportOne(ctx, src, opt, rel, repo, logf)
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s 导出失败，跳过: %v", repo, err))
			continue
		}
		res.Repos = append(res.Repos, entry)
		latest := rel.Tag
		if latest == "" {
			latest = entry.Tag
		}
		idx.Components = upsertEntry(idx.Components, Entry{Repo: repo, Latest: latest})
	}

	sort.Slice(idx.Components, func(i, j int) bool { return idx.Components[i].Repo < idx.Components[j].Repo })
	if err := writeJSON(IndexPath(opt.Base), idx); err != nil {
		return res, fmt.Errorf("mirror: 写索引失败: %w", err)
	}
	return res, nil
}

func exportOne(ctx context.Context, src fetch.Source, opt ExportOptions, rel *fetch.Release, repo string,
	logf func(string, ...any)) (ExportEntry, error) {
	tag := rel.Tag
	if tag == "" {
		return ExportEntry{}, fmt.Errorf("release 没有 tag")
	}
	dir := ReleaseDir(opt.Base, repo, tag)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ExportEntry{}, err
	}
	out := Release{SchemaVersion: SchemaVersion, Repo: repo, Tag: tag}
	for _, a := range rel.Assets {
		if isReleaseFile(a.Name) {
			continue
		}
		if !matchArch(a.Name, opt.Archs) {
			continue
		}
		dst := filepath.Join(dir, a.Name)
		logf("  取 %s %s", repo, a.Name)
		if _, err := src.Download(ctx, a.URL, dst); err != nil {
			return ExportEntry{}, fmt.Errorf("下载 %s 失败: %w", a.Name, err)
		}
		sum, size, err := sha256Of(dst)
		if err != nil {
			return ExportEntry{}, err
		}
		out.Assets = append(out.Assets, Asset{Name: a.Name, Size: size, SHA256: sum})
	}
	if len(out.Assets) == 0 {
		return ExportEntry{}, fmt.Errorf("release %s 里没有匹配的产物", tag)
	}
	sort.Slice(out.Assets, func(i, j int) bool { return out.Assets[i].Name < out.Assets[j].Name })
	if err := writeJSON(ReleasePath(opt.Base, repo, tag), out); err != nil {
		return ExportEntry{}, err
	}
	names := make([]string, 0, len(out.Assets))
	for _, a := range out.Assets {
		names = append(names, a.Name)
	}
	return ExportEntry{Repo: repo, Tag: tag, Assets: names}, nil
}

// matchArch 判断产物名是否属于请求的架构集合。
//
// 产物名形如 `<stem>-<goos>-<arch>.<ext>`，因此只认倒数第二段里的架构标签，
// 避免把 `simple-web-shell` 这种 stem 里的词误当架构。
func matchArch(assetName string, archs []string) bool {
	if len(archs) == 0 {
		return true
	}
	for _, a := range archs {
		// 用 "-<arch>." 匹配，既避开 stem 误判，也不要求后缀固定。
		if hasArchSegment(assetName, a) {
			return true
		}
	}
	return false
}

func hasArchSegment(name, arch string) bool {
	for i := 0; i+len(arch)+1 <= len(name); i++ {
		if name[i] != '-' {
			continue
		}
		if name[i+1:i+1+len(arch)] != arch {
			continue
		}
		next := i + 1 + len(arch)
		if next < len(name) && name[next] == '.' {
			return true
		}
	}
	return false
}

func sha256Of(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func upsertEntry(list []Entry, e Entry) []Entry {
	for i := range list {
		if list[i].Repo == e.Repo {
			if e.Latest != "" && CompareTags(e.Latest, list[i].Latest) >= 0 {
				list[i].Latest = e.Latest
			}
			if e.Tags != nil {
				list[i].Tags = e.Tags
			}
			return list
		}
	}
	return append(list, e)
}

func readIndexFile(p string) (*Index, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var idx Index
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, err
	}
	return &idx, nil
}

// writeJSON 原子写：先写临时文件再 rename，中途失败不会留下半截 JSON。
func writeJSON(p string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".ferc-mirror-*.json")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
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
	return os.Rename(name, p)
}
