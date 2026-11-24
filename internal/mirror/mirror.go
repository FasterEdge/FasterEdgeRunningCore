// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

// Package mirror 实现“集群内分发”：把 release 产物摆成一份可搬迁的目录结构，
// 让只能连局域网（或完全离线）的节点也能装组件。
//
// 设计取向是**可搬迁**：目录里不存任何绝对 URL，只存“仓库 + tag + 文件名”。
// 同一份目录可以被挂在 U 盘上、被 `ferc mirror serve` 用 HTTP 暴露、被 rsync
// 到另一台机器，客户端只是换一个“基址”而已。
//
// 目录结构：
//
//	<base>/ferc-mirror.json                          索引：每个仓库的最新 tag
//	<base>/repos/<owner>/<name>/releases/<tag>/release.json   产物清单
//	<base>/repos/<owner>/<name>/releases/<tag>/<asset>        产物本体
package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/fetch"
)

// SchemaVersion 是镜像目录布局版本。布局变化时递增，避免新客户端把老目录
// 当成新格式去读。
const SchemaVersion = 1

// releaseFileName 是单个 tag 的产物清单文件名。
const releaseFileName = "release.json"

// IndexName 是索引文件名。
const IndexName = "ferc-mirror.json"

// Index 是镜像的总目录。它只记“每个仓库有哪些 tag、哪个是最新”，
// 具体产物清单在各自的 release.json 里。
type Index struct {
	SchemaVersion int       `json:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at"`
	Components    []Entry   `json:"components"`
}

// Entry 是单个仓库在镜像里的条目。
type Entry struct {
	Repo   string   `json:"repo"`
	Latest string   `json:"latest"`
	Tags   []string `json:"tags,omitempty"`
}

// Release 是单个 tag 的产物清单。刻意不含 URL：路径由基址 + 仓库 + tag 推出来，
// 这样索引才能整体搬迁。
type Release struct {
	SchemaVersion int     `json:"schema_version"`
	Repo          string  `json:"repo"`
	Tag           string  `json:"tag"`
	Assets        []Asset `json:"assets"`
}

// Asset 是一个产物。SHA256 在导出时本地计算，客户端可据此做完整性校验，
// 不依赖原仓库是否发布 checksums 文件。
type Asset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

// IndexPath 返回索引文件位置：本地基址给文件路径，远端基址给 URL。
func IndexPath(base string) string { return joinBase(base, IndexName) }

// ReleaseDir 返回某个 tag 的产物目录（仅本地基址有意义）。
func ReleaseDir(base, repo, tag string) string {
	return filepath.Join(base, "repos", filepath.FromSlash(repo), "releases", tag)
}

// ReleasePath 返回某个 tag 的产物清单位置。
func ReleasePath(base, repo, tag string) string {
	return joinBase(base, repoRel(repo, tag)+"/"+releaseFileName)
}

// IsRemote 报告基址是否为 HTTP(S)。
func IsRemote(base string) bool {
	return strings.HasPrefix(base, "http://") || strings.HasPrefix(base, "https://")
}

// Locator 返回某个产物在给定基址下的定位串：远端基址给 URL，本地基址给路径。
// 两种形式都由 Store.Download 统一处理，客户端不需要关心来源。
func Locator(base, repo, tag, name string) string {
	return joinBase(base, repoRel(repo, tag)+"/"+name)
}

// joinBase 把镜像内的相对路径拼到基址上。
//
// 远端基址必须拼成 URL：文件系统路径分隔符会把 `http://host:port` 拆成
// `http:\host:port` 这样的怪路径，取包时表现为“镜像里没这个仓库”。
func joinBase(base, rel string) string {
	if IsRemote(base) {
		return strings.TrimSuffix(base, "/") + "/" + escapeRel(rel)
	}
	return filepath.Join(base, filepath.FromSlash(rel))
}

func repoRel(repo, tag string) string {
	return "repos/" + repo + "/releases/" + tag
}

func escapeRel(rel string) string {
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// Store 把一份镜像目录当作组件源。
//
// 它实现与 fetch.Client 相同的两个方法（结构上满足 install 的 Fetcher 接口），
// 因此可以整条链路替换，而装载逻辑一行都不用改。
type Store struct {
	// Base 是镜像基址：HTTP(S) URL，或本地目录路径。
	Base string
	// HTTP 用于远端基址；为 nil 时用带超时的默认客户端。
	HTTP *http.Client
}

// New 返回绑定到某基址的 Store。本地路径会被清成绝对路径，
// 免得换工作目录后失效。
func New(base string) *Store {
	if !IsRemote(base) {
		if abs, err := filepath.Abs(base); err == nil {
			base = abs
		}
	}
	return &Store{Base: base}
}

func (s *Store) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

// LatestRelease 从索引里读某个仓库的最新 tag，再拼成 fetch.Release。
func (s *Store) LatestRelease(ctx context.Context, repo string) (*fetch.Release, error) {
	idx, err := s.readIndex(ctx)
	if err != nil {
		return nil, err
	}
	for _, e := range idx.Components {
		if e.Repo == repo {
			return s.Release(ctx, repo, e.Latest)
		}
	}
	return nil, fmt.Errorf("mirror: 镜像 %s 里没有 %s（可用: %s）",
		s.Base, repo, strings.Join(repoNames(idx), ", "))
}

// Release 读取指定 tag 的产物清单。
func (s *Store) Release(ctx context.Context, repo, tag string) (*fetch.Release, error) {
	b, err := s.readFile(ctx, ReleasePath(s.Base, repo, tag))
	if err != nil {
		return nil, fmt.Errorf("mirror: 读 %s@%s 清单失败: %w", repo, tag, err)
	}
	var rel Release
	if err := json.Unmarshal(b, &rel); err != nil {
		return nil, fmt.Errorf("mirror: 解析 %s@%s 清单失败: %w", repo, tag, err)
	}
	if rel.Tag == "" {
		rel.Tag = tag
	}
	out := &fetch.Release{Tag: rel.Tag}
	for _, a := range rel.Assets {
		out.Assets = append(out.Assets, fetch.Asset{
			Name: a.Name,
			URL:  Locator(s.Base, repo, rel.Tag, a.Name),
			Size: a.Size,
		})
	}
	return out, nil
}

// Download 取回一个产物。loc 可以是 HTTP(S) URL，也可以是本地路径。
func (s *Store) Download(ctx context.Context, loc, dst string) (int64, error) {
	if IsRemote(loc) {
		return s.downloadHTTP(ctx, loc, dst)
	}
	return copyFile(loc, dst)
}

func (s *Store) downloadHTTP(ctx context.Context, loc, dst string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loc, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "FasterEdgeRunningCore")
	resp, err := s.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("mirror: 下载 %s 失败: HTTP %d", loc, resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		os.Remove(dst)
		return n, copyErr
	}
	if closeErr != nil {
		os.Remove(dst)
		return n, closeErr
	}
	return n, nil
}

func (s *Store) readIndex(ctx context.Context) (*Index, error) {
	b, err := s.readFile(ctx, IndexPath(s.Base))
	if err != nil {
		return nil, fmt.Errorf("mirror: 读镜像索引 %s 失败（该目录可能还没导出过）: %w", s.Base, err)
	}
	var idx Index
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, fmt.Errorf("mirror: 解析镜像索引失败: %w", err)
	}
	if idx.SchemaVersion > SchemaVersion {
		return nil, fmt.Errorf("mirror: 镜像格式版本 %d 高于本程序支持的 %d，请升级运行核",
			idx.SchemaVersion, SchemaVersion)
	}
	return &idx, nil
}

func (s *Store) readFile(ctx context.Context, p string) ([]byte, error) {
	if !IsRemote(p) {
		return os.ReadFile(p)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "FasterEdgeRunningCore")
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// Chain 按顺序尝试多个源，第一个成功的胜出。
//
// 这是“在线优先、失败回落局域网/离线介质”的落点：边缘现场的网络时通时断，
// 让 ferc 在 GitHub 不可达时自动去邻居节点或 U 盘上取包，比让人手工换命令可靠。
//
// 一次装载里，LatestRelease 与随后的 Download 必须落在**同一个源**上：镜像源
// 给出的产物地址是本地路径或镜像 URL，拿它去问 GitHub 只会得到一条无意义的
// 失败。因此这里记住每个仓库“谁答上了”，下载时优先问那个源。
type Chain struct {
	Sources []namedSource
	// Log 接收“某个源失败了、改用下一个”的说明；可为 nil。
	Log func(format string, args ...any)

	mu   sync.Mutex
	pick map[string]int
}

// namedSource 让日志能说清是哪个源失败了。
type namedSource struct {
	name string
	src  fetch.Source
}

// NewChain 构造解析链；names 与 sources 一一对应。
func NewChain(names []string, sources []fetch.Source, logf func(string, ...any)) *Chain {
	c := &Chain{Log: logf, pick: map[string]int{}}
	for i, s := range sources {
		name := "source"
		if i < len(names) {
			name = names[i]
		}
		c.Sources = append(c.Sources, namedSource{name: name, src: s})
	}
	return c
}

// LatestRelease 依次询问各个源，并记住答上来的那个。
func (c *Chain) LatestRelease(ctx context.Context, repo string) (*fetch.Release, error) {
	if len(c.Sources) == 0 {
		return nil, errors.New("mirror: 没有可用的组件源")
	}
	var errs []error
	for i, s := range c.Sources {
		rel, err := s.src.LatestRelease(ctx, repo)
		if err == nil {
			c.mu.Lock()
			if c.pick == nil {
				c.pick = map[string]int{}
			}
			c.pick[repo] = i
			c.mu.Unlock()
			return rel, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", s.name, err))
		if c.Log != nil {
			c.Log("源 %s 取 %s 失败，换下一个: %v", s.name, repo, err)
		}
	}
	return nil, fmt.Errorf("mirror: 所有组件源都失败: %w", errors.Join(errs...))
}

// Download 先问“答上了这个产物所属仓库的那个源”，再按顺序试其余源。
func (c *Chain) Download(ctx context.Context, loc, dst string) (int64, error) {
	order := c.order(loc)
	var errs []error
	for _, s := range order {
		n, err := s.src.Download(ctx, loc, dst)
		if err == nil {
			return n, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", s.name, err))
		if c.Log != nil {
			c.Log("源 %s 下载失败，重试下一个: %v", s.name, err)
		}
	}
	return 0, fmt.Errorf("mirror: 所有组件源都下载失败: %w", errors.Join(errs...))
}

// order 返回下载时要尝试的源顺序：命中的那个源排在最前。
//
// 产物地址本身决定了它来自哪儿（HTTP URL 还是本地路径），所以这里优先按
// “地址形态”筛一遍，避免把本地路径丢给 GitHub 客户端白跑一趟。
func (c *Chain) order(loc string) []namedSource {
	remote := strings.HasPrefix(loc, "http://") || strings.HasPrefix(loc, "https://")
	var first, rest []namedSource
	for _, s := range c.Sources {
		if _, ok := s.src.(*Store); ok == !remote {
			first = append(first, s)
			continue
		}
		rest = append(rest, s)
	}
	return append(first, rest...)
}

func repoNames(idx *Index) []string {
	out := make([]string, 0, len(idx.Components))
	for _, e := range idx.Components {
		out = append(out, e.Repo)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return []string{"（空）"}
	}
	return out
}

func copyFile(src, dst string) (int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		os.Remove(dst)
		return n, copyErr
	}
	if closeErr != nil {
		os.Remove(dst)
		return n, closeErr
	}
	return n, nil
}

// CompareTags 比较两个 tag 的“新旧”，用于在导出时挑出最新。
//
// 本项目的 tag 形如 v1.0.20261123，本质是点分的数字串。这里逐段按数字比，
// 数字段相同再按字符串兜底，既不引入 semver 依赖，也能正确处理日期段回绕。
func CompareTags(a, b string) int {
	as := splitTag(a)
	bs := splitTag(b)
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, xerr := strconv.Atoi(as[i])
		y, yerr := strconv.Atoi(bs[i])
		if xerr == nil && yerr == nil {
			if x != y {
				if x < y {
					return -1
				}
				return 1
			}
			continue
		}
		if c := strings.Compare(as[i], bs[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	}
	return 0
}

func splitTag(tag string) []string {
	t := strings.TrimPrefix(strings.TrimPrefix(tag, "v"), "V")
	return strings.Split(t, ".")
}

// LatestOf 返回 tag 列表里最新的那个。
func LatestOf(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	out := tags[0]
	for _, t := range tags[1:] {
		if CompareTags(t, out) > 0 {
			out = t
		}
	}
	return out
}

// isReleaseFile 报告某个文件名是否就是产物清单本身（它不算产物）。
func isReleaseFile(name string) bool { return name == "release.json" }
