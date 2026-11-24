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
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/fetch"
)

// fakeSource 是一份内存里的“上游 release”，用来驱动 Export。
type fakeSource struct {
	tags   map[string]string
	assets map[string]map[string][]byte // repo -> name -> 内容
	fail   map[string]bool              // 取清单就失败的仓库
}

func newFake() *fakeSource {
	return &fakeSource{
		tags:   map[string]string{},
		assets: map[string]map[string][]byte{},
		fail:   map[string]bool{},
	}
}

func (f *fakeSource) add(repo, tag string, files map[string][]byte) {
	f.tags[repo] = tag
	f.assets[repo] = files
}

func (f *fakeSource) LatestRelease(_ context.Context, repo string) (*fetch.Release, error) {
	if f.fail[repo] {
		return nil, os.ErrNotExist
	}
	tag, ok := f.tags[repo]
	if !ok {
		return nil, os.ErrNotExist
	}
	rel := &fetch.Release{Tag: tag}
	names := make([]string, 0, len(f.assets[repo]))
	for n := range f.assets[repo] {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		rel.Assets = append(rel.Assets, fetch.Asset{
			Name: n,
			URL:  "mem://" + repo + "/" + tag + "/" + n,
			Size: int64(len(f.assets[repo][n])),
		})
	}
	return rel, nil
}

func (f *fakeSource) Download(_ context.Context, loc, dst string) (int64, error) {
	// 定位串形如 mem://<owner>/<name>/<tag>/<file>。
	parts := strings.Split(strings.TrimPrefix(loc, "mem://"), "/")
	if len(parts) < 4 {
		return 0, os.ErrNotExist
	}
	repo := parts[0] + "/" + parts[1]
	b, ok := f.assets[repo][parts[len(parts)-1]]
	if !ok {
		return 0, os.ErrNotExist
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		return 0, err
	}
	return int64(len(b)), nil
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ── 定位串 ───────────────────────────────────────────────

func TestLocatorLocalAndRemote(t *testing.T) {
	dir := t.TempDir()
	local := Locator(dir, "FasterEdge/SimpleWebShell", "v1.0.20260101", "simple-web-shell-linux-amd64.tar.gz")
	if IsRemote(local) {
		t.Fatalf("本地基址不该产出远端定位串: %s", local)
	}
	if filepath.Dir(local) != ReleaseDir(dir, "FasterEdge/SimpleWebShell", "v1.0.20260101") {
		t.Fatalf("本地定位串应落在 ReleaseDir 下: %s", local)
	}

	remote := Locator("http://10.0.0.9:8090/", "FasterEdge/SimpleWebShell", "v1.0.20260101", "simple-web-shell-linux-amd64.tar.gz")
	want := "http://10.0.0.9:8090/repos/FasterEdge/SimpleWebShell/releases/v1.0.20260101/simple-web-shell-linux-amd64.tar.gz"
	if remote != want {
		t.Fatalf("远端定位串不对:\n got %s\nwant %s", remote, want)
	}
}

func TestNewMakesLocalBaseAbsolute(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	s := New("some/relative/dir")
	if !filepath.IsAbs(s.Base) {
		t.Fatalf("本地基址应被清成绝对路径: %s", s.Base)
	}
	if got := filepath.Dir(s.Base); got != wd && runtime.GOOS == "windows" {
		// Windows 上只有大小写差异，只要有绝对路径就够。
		t.Logf("基址 %s（工作目录 %s）", s.Base, wd)
	}
	if r := New("https://example.com/mirror"); r.Base != "https://example.com/mirror" {
		t.Fatalf("远端基址应原样保留: %s", r.Base)
	}
}

// ── 架构过滤 ─────────────────────────────────────────────

func TestMatchArchAvoidsStemFalsePositive(t *testing.T) {
	cases := []struct {
		name string
		arch string
		want bool
	}{
		// stem 里带 arm 字样，但架构段是 amd64，不该被 `arm` 匹配上。
		{"arm-tool-linux-amd64.tar.gz", "arm64", false},
		{"dontcrack-linux-arm64.tar.gz", "arm", false},
		{"dontcrack-linux-arm64.tar.gz", "arm64", true},
		{"dontcrack-linux-arm.tar.gz", "arm", true},
		{"ferc-linux-riscv64.tar.gz", "riscv64", true},
		{"ferc-windows-386.zip", "386", true},
		{"ferc-darwin-arm64.tar.gz", "arm64", true},
		{"ferc-linux-amd64.tar.gz", "386", false},
		// 名字里根本没架构段（如 checksums 文件），不匹配任何受限架构。
		{"checksums-md5.txt", "amd64", false},
	}
	for _, c := range cases {
		if got := matchArch(c.name, []string{c.arch}); got != c.want {
			t.Errorf("matchArch(%q, [%q]) = %v, 期望 %v", c.name, c.arch, got, c.want)
		}
	}
	if !matchArch("whatever.txt", nil) {
		t.Error("未限定架构时应全部匹配")
	}
}

// ── tag 比较 ─────────────────────────────────────────────

func TestCompareTags(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v1.0.20260101", "v1.0.20251231", 1},
		{"v1.0.20251231", "v1.0.20260101", -1},
		{"v1.0.20260101", "v1.0.20260101", 0},
		{"V1.0.20260101", "v1.0.20260101", 0},
		// 段数不同：多的更新（1.0.1 > 1.0）。
		{"v1.0.1", "v1.0", 1},
		{"v1.0", "v1.0.1", -1},
		// 数字段按数值比，不按字典序：9 < 10。
		{"v1.0.9", "v1.0.10", -1},
	}
	for _, c := range cases {
		if got := CompareTags(c.a, c.b); got != c.want {
			t.Errorf("CompareTags(%q,%q) = %d, 期望 %d", c.a, c.b, got, c.want)
		}
	}
	if got := LatestOf([]string{"v1.0.20251231", "v1.0.20260101", "v1.0.20251215"}); got != "v1.0.20260101" {
		t.Errorf("LatestOf 挑错了: %s", got)
	}
	if LatestOf(nil) != "" {
		t.Error("空列表应返回空串")
	}
}

// ── 端到端：导出 → 用 Store 当源装回来 ────────────────────

func TestExportThenStoreRoundTrip(t *testing.T) {
	src := newFake()
	body := []byte("fake tarball bytes")
	src.add("FasterEdge/SimpleWebShell", "v1.0.20260101", map[string][]byte{
		"simple-web-shell-linux-amd64.tar.gz": body,
		"simple-web-shell-linux-arm64.tar.gz": []byte("arm64"),
		"simple-web-shell-windows-amd64.zip":  []byte("win"),
		"checksums-md5.txt":                   []byte("md5"),
		"release.json":                        []byte("{}"),
	})

	base := t.TempDir()
	res, err := Export(context.Background(), src, ExportOptions{
		Base:  base,
		Repos: []string{"FasterEdge/SimpleWebShell"},
		Archs: []string{"amd64"},
	})
	if err != nil {
		t.Fatalf("Export 失败: %v", err)
	}
	if len(res.Repos) != 1 || res.Repos[0].Tag != "v1.0.20260101" {
		t.Fatalf("导出结果不对: %+v", res.Repos)
	}
	// 只导 amd64 的两个产物（linux-amd64 + windows-amd64），checksums/release.json 不算产物。
	if len(res.Repos[0].Assets) != 2 {
		t.Fatalf("应导出 2 个 amd64 产物，实际 %v", res.Repos[0].Assets)
	}

	// release.json 里必须带本地算出的 SHA256，且不含绝对 URL。
	rb, err := os.ReadFile(ReleasePath(base, "FasterEdge/SimpleWebShell", "v1.0.20260101"))
	if err != nil {
		t.Fatalf("读 release.json 失败: %v", err)
	}
	if strings.Contains(string(rb), "mem://") || strings.Contains(string(rb), "http") {
		t.Fatalf("release.json 不该存 URL（否则无法整体搬迁）:\n%s", rb)
	}
	var rel Release
	if err := json.Unmarshal(rb, &rel); err != nil {
		t.Fatal(err)
	}
	for _, a := range rel.Assets {
		if a.SHA256 == "" {
			t.Errorf("%s 缺少 SHA256", a.Name)
		}
		if a.Name == "simple-web-shell-linux-amd64.tar.gz" && a.SHA256 != sum(body) {
			t.Errorf("SHA256 不对: %s vs %s", a.SHA256, sum(body))
		}
	}

	// 用 Store 把镜像当成组件源，装回来的字节必须和原文件一致。
	st := New(base)
	got, err := st.LatestRelease(context.Background(), "FasterEdge/SimpleWebShell")
	if err != nil {
		t.Fatalf("Store 取清单失败: %v", err)
	}
	if got.Tag != "v1.0.20260101" {
		t.Fatalf("Store 给的 tag 不对: %s", got.Tag)
	}
	asset, ok := got.FindAsset("simple-web-shell-linux-amd64.tar.gz")
	if !ok {
		t.Fatalf("Store 清单里没有 linux-amd64: %+v", got.Assets)
	}
	dst := filepath.Join(t.TempDir(), asset.Name)
	if _, err := st.Download(context.Background(), asset.URL, dst); err != nil {
		t.Fatalf("Store 取产物失败: %v", err)
	}
	back, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != string(body) {
		t.Fatalf("取回的字节和原文件不一致: %q vs %q", back, body)
	}
}

func TestExportIsIncrementalAcrossRepos(t *testing.T) {
	src := newFake()
	src.add("a/one", "v1.0.20260101", map[string][]byte{"one-linux-amd64.tar.gz": []byte("1")})
	src.add("b/two", "v1.0.20260101", map[string][]byte{"two-linux-amd64.tar.gz": []byte("2")})
	base := t.TempDir()

	if _, err := Export(context.Background(), src, ExportOptions{
		Base: base, Repos: []string{"a/one"}, Archs: []string{"amd64"},
	}); err != nil {
		t.Fatal(err)
	}
	// 第二次只导 b/two：a/one 的条目必须还在索引里。
	if _, err := Export(context.Background(), src, ExportOptions{
		Base: base, Repos: []string{"b/two"}, Archs: []string{"amd64"},
	}); err != nil {
		t.Fatal(err)
	}
	idx, err := readIndexFile(IndexPath(base))
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Components) != 2 {
		t.Fatalf("增量导出后索引应含 2 个仓库，实际 %+v", idx.Components)
	}
}

func TestExportRecordsWarningsInsteadOfAborting(t *testing.T) {
	src := newFake()
	src.add("a/good", "v1.0.20260101", map[string][]byte{"good-linux-amd64.tar.gz": []byte("x")})
	src.fail["a/bad"] = true

	res, err := Export(context.Background(), src, ExportOptions{
		Base: t.TempDir(), Repos: []string{"a/bad", "a/good"}, Archs: []string{"amd64"},
	})
	if err != nil {
		t.Fatalf("单个仓库失败不该让整次导出失败: %v", err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "a/bad") {
		t.Fatalf("应给出 a/bad 的告警: %+v", res.Warnings)
	}
	if len(res.Repos) != 1 || res.Repos[0].Repo != "a/good" {
		t.Fatalf("a/good 应导出成功: %+v", res.Repos)
	}
}

func TestStoreMissingRepoErrorMentionsAvailable(t *testing.T) {
	src := newFake()
	src.add("a/one", "v1.0.20260101", map[string][]byte{"one-linux-amd64.tar.gz": []byte("1")})
	base := t.TempDir()
	if _, err := Export(context.Background(), src, ExportOptions{
		Base: base, Repos: []string{"a/one"}, Archs: []string{"amd64"},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := New(base).LatestRelease(context.Background(), "a/nope")
	if err == nil {
		t.Fatal("取不存在的仓库应报错")
	}
	// 报错要告诉运维“镜像里到底有什么”，否则现场只能挨个目录翻。
	if !strings.Contains(err.Error(), "a/one") {
		t.Fatalf("错误里应列出可用仓库: %v", err)
	}
}

// ── 远端镜像（Serve + 远端 Store） ───────────────────────

func TestServeThenRemoteStore(t *testing.T) {
	src := newFake()
	src.add("a/one", "v1.0.20260101", map[string][]byte{"one-linux-amd64.tar.gz": []byte("payload")})
	base := t.TempDir()
	if _, err := Export(context.Background(), src, ExportOptions{
		Base: base, Repos: []string{"a/one"}, Archs: []string{"amd64"},
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan string, 1)
	go func() {
		_ = Serve(ctx, ServeOptions{
			Base: base,
			Addr: "127.0.0.1:0",
			// 监听 :0 让内核分配端口，避免和别的测试抢固定端口。
			OnListen: func(addr string) {
				select {
				case started <- addr:
				default:
				}
			},
		})
	}()

	var addr string
	select {
	case addr = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("镜像服务没在 5s 内启动")
	}

	remote := New("http://" + addr)
	rel, err := remote.LatestRelease(context.Background(), "a/one")
	if err != nil {
		t.Fatalf("远端 Store 取清单失败: %v", err)
	}
	if rel.Tag != "v1.0.20260101" || len(rel.Assets) != 1 {
		t.Fatalf("远端清单不对: %+v", rel)
	}
	dst := filepath.Join(t.TempDir(), rel.Assets[0].Name)
	if _, err := remote.Download(context.Background(), rel.Assets[0].URL, dst); err != nil {
		t.Fatalf("远端取产物失败: %v", err)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "payload" {
		t.Fatalf("远端取回内容不对: %q", b)
	}
}

// ── Chain 回落 ───────────────────────────────────────────

func TestChainFallsBackAndPrefersMirrorForLocalPaths(t *testing.T) {
	src := newFake()
	src.add("a/one", "v1.0.20260101", map[string][]byte{"one-linux-amd64.tar.gz": []byte("payload")})
	base := t.TempDir()
	if _, err := Export(context.Background(), src, ExportOptions{
		Base: base, Repos: []string{"a/one"}, Archs: []string{"amd64"},
	}); err != nil {
		t.Fatal(err)
	}

	dead := &failSource{}
	var logs []string
	chain := NewChain(
		[]string{"wall", "mirror"},
		[]fetch.Source{dead, New(base)},
		func(f string, a ...any) { logs = append(logs, f) },
	)

	rel, err := chain.LatestRelease(context.Background(), "a/one")
	if err != nil {
		t.Fatalf("第一个源失败时应回落到第二个: %v", err)
	}
	if dead.calls != 1 {
		t.Errorf("应尝试过第一个源: %d", dead.calls)
	}
	if len(logs) == 0 {
		t.Error("回落时应记一条日志")
	}
	dst := filepath.Join(t.TempDir(), rel.Assets[0].Name)
	if _, err := chain.Download(context.Background(), rel.Assets[0].URL, dst); err != nil {
		t.Fatalf("下载应回落到镜像源: %v", err)
	}
}

func TestChainAllSourcesFail(t *testing.T) {
	chain := NewChain([]string{"a", "b"}, []fetch.Source{&failSource{}, &failSource{}}, nil)
	if _, err := chain.LatestRelease(context.Background(), "x/y"); err == nil {
		t.Fatal("全部源失败应报错")
	}
	if _, err := NewChain(nil, nil, nil).LatestRelease(context.Background(), "x/y"); err == nil {
		t.Fatal("空链应报错")
	}
}

type failSource struct{ calls int }

func (f *failSource) LatestRelease(context.Context, string) (*fetch.Release, error) {
	f.calls++
	return nil, os.ErrDeadlineExceeded
}

func (f *failSource) Download(context.Context, string, string) (int64, error) {
	f.calls++
	return 0, os.ErrDeadlineExceeded
}

// ── Serve 的输入校验 ─────────────────────────────────────

func TestServeRejectsNonMirrorDir(t *testing.T) {
	if err := Serve(context.Background(), ServeOptions{Base: t.TempDir()}); err == nil {
		t.Fatal("没有索引文件的目录应被拒绝")
	}
	if err := Serve(context.Background(), ServeOptions{}); err == nil {
		t.Fatal("缺基址应被拒绝")
	}
}

func TestStoreRejectsNewerSchema(t *testing.T) {
	base := t.TempDir()
	b, err := json.Marshal(Index{SchemaVersion: SchemaVersion + 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(IndexPath(base), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(base).LatestRelease(context.Background(), "a/one"); err == nil {
		t.Fatal("镜像格式版本高于本程序时应报错，而不是读出错数据")
	} else if !strings.Contains(err.Error(), "升级") {
		t.Fatalf("错误应提示升级运行核: %v", err)
	}
}
