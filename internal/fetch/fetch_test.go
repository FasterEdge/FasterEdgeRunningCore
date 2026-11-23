package fetch

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeTarGz 构造一个 tar.gz，entries 里的 name 直接写进归档，用于测试逃逸防护。
func makeTarGz(t *testing.T, entries map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.tar.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func makeSymlinkTarGz(t *testing.T, name, target string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "link.tar.gz")
	f, _ := os.Create(p)
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: name, Linkname: target, Mode: 0o777, Typeflag: tar.TypeSymlink}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	f.Close()
	return p
}

func TestExtractTarGz(t *testing.T) {
	dir := t.TempDir()
	archive := makeTarGz(t, map[string]string{
		"simple-web-shell": "binary",
		"README.md":        "docs",
		"sub/nested.txt":   "nested",
	})
	out, err := Extract(archive, dir)
	if err != nil {
		t.Fatalf("解包失败: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("解出 %d 个文件，期望 3: %v", len(out), out)
	}
	b, err := os.ReadFile(filepath.Join(dir, "simple-web-shell"))
	if err != nil {
		t.Fatalf("读解出的文件失败: %v", err)
	}
	if string(b) != "binary" {
		t.Fatalf("内容 = %q", string(b))
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "nested.txt")); err != nil {
		t.Fatalf("嵌套文件未解出: %v", err)
	}
}

func TestExtractRejectsTraversal(t *testing.T) {
	for _, evil := range []string{"../evil", "/etc/passwd", "a/../../evil"} {
		dir := t.TempDir()
		archive := makeTarGz(t, map[string]string{evil: "x"})
		if _, err := Extract(archive, dir); err == nil {
			t.Fatalf("条目 %q 必须被拒绝", evil)
		}
	}
}

func TestExtractRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	archive := makeSymlinkTarGz(t, "link", "/etc/passwd")
	if _, err := Extract(archive, dir); err == nil {
		t.Fatal("符号链接条目必须被拒绝")
	}
}

func TestExtractZip(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(t.TempDir(), "a.zip")
	f, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create("tool.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("MZ")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := Extract(archive, dir)
	if err != nil {
		t.Fatalf("解包失败: %v", err)
	}
	if len(out) != 1 || out[0] != "tool.exe" {
		t.Fatalf("解出 %v", out)
	}
}

func TestExtractZipRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(t.TempDir(), "evil.zip")
	f, _ := os.Create(archive)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("../evil.txt")
	w.Write([]byte("x"))
	zw.Close()
	f.Close()
	if _, err := Extract(archive, dir); err == nil {
		t.Fatal("zip 路径逃逸必须被拒绝")
	}
}

func TestExtractRejectsUnknownFormat(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(t.TempDir(), "a.rar")
	os.WriteFile(p, []byte("x"), 0o644)
	if _, err := Extract(p, dir); err == nil {
		t.Fatal("未知格式必须报错")
	}
}

func TestSafeEntryName(t *testing.T) {
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"ok.txt", false},
		{"dir/ok.txt", false},
		{"./ok.txt", false},
		{"../bad", true},
		{"/abs", true},
		{"C:/x", true},
		{"a\\..\\..\\bad", true},
	}
	for _, tc := range cases {
		_, skip, err := safeEntryName(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q 应被拒绝", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q 不该报错: %v", tc.in, err)
		}
		if skip {
			t.Errorf("%q 不该被跳过", tc.in)
		}
	}
}

func TestLookupSum(t *testing.T) {
	dir := t.TempDir()
	sum := filepath.Join(dir, "checksums-sha1.txt")
	os.WriteFile(sum, []byte("abc123  simple-web-shell-linux-amd64.tar.gz\ndef456 *other.zip\n"), 0o644)
	got, found, err := lookupSum(sum, "simple-web-shell-linux-amd64.tar.gz")
	if err != nil || !found || got != "abc123" {
		t.Fatalf("lookupSum = %q %v %v", got, found, err)
	}
	got, found, err = lookupSum(sum, "other.zip")
	if err != nil || !found || got != "def456" {
		t.Fatalf("带星号的行应支持: %q %v %v", got, found, err)
	}
	_, found, err = lookupSum(sum, "missing.tgz")
	if err != nil || found {
		t.Fatalf("未列出的文件应返回 not found: %v %v", found, err)
	}
}

func TestVerifySHA1AndSHA256(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "f.bin")
	os.WriteFile(target, []byte("hello"), 0o644)

	h1 := sha1.Sum([]byte("hello"))
	sum1 := filepath.Join(dir, "s1.txt")
	os.WriteFile(sum1, []byte(hex.EncodeToString(h1[:])+"  f.bin\n"), 0o644)
	ok, err := VerifySHA1(sum1, target, "f.bin")
	if err != nil || !ok {
		t.Fatalf("sha1 校验应通过: %v %v", ok, err)
	}

	h2 := sha256.Sum256([]byte("hello"))
	sum2 := filepath.Join(dir, "s2.txt")
	os.WriteFile(sum2, []byte(hex.EncodeToString(h2[:])+"  f.bin\n"), 0o644)
	ok, err = VerifySHA256(sum2, target, "f.bin")
	if err != nil || !ok {
		t.Fatalf("sha256 校验应通过: %v %v", ok, err)
	}

	// 篡改后必须报错，而不是返回 ok=false 让调用方误以为"没有校验信息"。
	os.WriteFile(target, []byte("tampered"), 0o644)
	if _, err := VerifySHA1(sum1, target, "f.bin"); err == nil {
		t.Fatal("内容不符必须报错")
	}
}

func TestVerifyNotListed(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "f.bin")
	os.WriteFile(target, []byte("hello"), 0o644)
	sum := filepath.Join(dir, "s.txt")
	os.WriteFile(sum, []byte("deadbeef  other.bin\n"), 0o644)
	ok, err := VerifySHA1(sum, target, "f.bin")
	if err != nil {
		t.Fatalf("未列出时不该报错: %v", err)
	}
	if ok {
		t.Fatal("未列出时 ok 应为 false，由调用方决定是否强制")
	}
}

func TestLatestReleaseAndDownload(t *testing.T) {
	payload := []byte("artifact-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			_ = json.NewEncoder(w).Encode(Release{
				Tag: "v1.0.20261124",
				Assets: []Asset{
					{Name: "a.tar.gz", URL: "http://" + r.Host + "/dl/a.tar.gz", Size: int64(len(payload))},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/dl/a.tar.gz"):
			w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New("")
	c.BaseURL = srv.URL
	c.HTTP = srv.Client()

	rel, err := c.LatestRelease(context.Background(), "FasterEdge/SimpleWebShell")
	if err != nil {
		t.Fatalf("取 release 失败: %v", err)
	}
	if rel.Tag != "v1.0.20261124" {
		t.Fatalf("tag = %q", rel.Tag)
	}
	asset, ok := rel.FindAsset("a.tar.gz")
	if !ok {
		t.Fatal("应找到 a.tar.gz")
	}
	dst := filepath.Join(t.TempDir(), "a.tar.gz")
	n, err := c.Download(context.Background(), asset.URL, dst)
	if err != nil {
		t.Fatalf("下载失败: %v", err)
	}
	if n != int64(len(payload)) {
		t.Fatalf("下载 %d 字节，期望 %d", n, len(payload))
	}
	got, _ := os.ReadFile(dst)
	if string(got) != string(payload) {
		t.Fatalf("内容 = %q", string(got))
	}
}

func TestLatestReleaseHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := New("")
	c.BaseURL = srv.URL
	c.HTTP = srv.Client()
	if _, err := c.LatestRelease(context.Background(), "o/r"); err == nil {
		t.Fatal("HTTP 500 必须报错")
	}
}

func TestDownloadEnforcesLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", 1024)))
	}))
	defer srv.Close()
	c := New("")
	c.BaseURL = srv.URL
	c.HTTP = srv.Client()
	c.Limits.MaxArchiveBytes = 16
	dst := filepath.Join(t.TempDir(), "big.bin")
	if _, err := c.Download(context.Background(), srv.URL+"/x", dst); err == nil {
		t.Fatal("超过上限必须报错")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("超限的下载文件应被删除，不能留下半个产物")
	}
}

func TestDownloadRetriesTransientEOF(t *testing.T) {
	// release 资产走独立 CDN，实测会偶发 EOF；必须重试而不是直接失败。
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			// 声明长度但不写够，制造中途断开。
			w.Header().Set("Content-Length", "100")
			w.Write([]byte("partial"))
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("需要 Hijacker")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatal(err)
			}
			conn.Close()
			return
		}
		w.Write([]byte("payload"))
	}))
	defer srv.Close()
	c := New("")
	c.BaseURL = srv.URL
	c.HTTP = srv.Client()
	dst := filepath.Join(t.TempDir(), "f.bin")
	n, err := c.Download(context.Background(), srv.URL+"/x", dst)
	if err != nil {
		t.Fatalf("瞬时中断应被重试后成功: %v", err)
	}
	if n != 7 {
		t.Fatalf("下载 %d 字节，期望 7", n)
	}
	if hits < 2 {
		t.Fatalf("应至少重试一次，实际请求 %d 次", hits)
	}
}

func TestDownloadNoRetryOn404(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := New("")
	c.BaseURL = srv.URL
	c.HTTP = srv.Client()
	if _, err := c.Download(context.Background(), srv.URL+"/missing", filepath.Join(t.TempDir(), "x")); err == nil {
		t.Fatal("404 必须报错")
	}
	if hits != 1 {
		t.Fatalf("4xx 不该重试，实际请求 %d 次", hits)
	}
}

func TestLatestReleaseRequiresRepo(t *testing.T) {
	if _, err := New("").LatestRelease(context.Background(), ""); err == nil {
		t.Fatal("空仓库名必须报错")
	}
}

func TestDownloadRequiresURL(t *testing.T) {
	if _, err := New("").Download(context.Background(), "", filepath.Join(t.TempDir(), "x")); err == nil {
		t.Fatal("空地址必须报错")
	}
}

func TestFindAssetMissing(t *testing.T) {
	rel := Release{Assets: []Asset{{Name: "a"}}}
	if _, ok := rel.FindAsset("b"); ok {
		t.Fatal("不存在的产物不该被找到")
	}
}
