// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

// Package fetch 负责从 GitHub Release 取回组件产物，并把它安全地解包出来。
//
// 这里刻意只用标准库：运行核要能在最朴素的目标机上自举，不能要求目标机
// 预装 curl / tar / sha256sum，也不该为此引入外部依赖。
//
// 安全边界：
//   - 解包时拒绝绝对路径、拒绝 .. 逃逸、拒绝符号链接与设备文件；
//   - 单文件与解包总量都有上限，避免恶意 tar 炸弹；
//   - 若 release 提供了 checksums-sha1.txt / SHA256SUMS.txt，则强制校验。
package fetch

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Limits 控制单个产物的解包上限。默认值足够放下任何本项目产物，
// 又能挡住把磁盘写穿的恶意包。
type Limits struct {
	// MaxArchiveBytes 是下载的压缩包上限。
	MaxArchiveBytes int64
	// MaxFileBytes 是解包后单个文件的上限。
	MaxFileBytes int64
	// MaxTotalBytes 是解包后全部文件的总量上限。
	MaxTotalBytes int64
	// MaxEntries 是解包条目数量上限。
	MaxEntries int
}

// DefaultLimits 是保守但够用的默认值。
func DefaultLimits() Limits {
	return Limits{
		MaxArchiveBytes: 512 << 20, // 512 MiB
		MaxFileBytes:    256 << 20, // 256 MiB
		MaxTotalBytes:   1 << 30,   // 1 GiB
		MaxEntries:      4096,
	}
}

// Client 访问 GitHub Release。
type Client struct {
	HTTP    *http.Client
	BaseURL string
	Token   string
	Limits  Limits
}

// New 返回可直接使用的 Client。
func New(token string) *Client {
	return &Client{
		HTTP:    &http.Client{Timeout: 10 * time.Minute},
		BaseURL: "https://api.github.com",
		Token:   token,
		Limits:  DefaultLimits(),
	}
}

// Asset 是 release 里的一个产物。
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Release 是解析后的 release 信息。
type Release struct {
	Tag    string  `json:"tag_name"`
	Assets []Asset `json:"assets"`
}

// FindAsset 按文件名查找产物。
func (r *Release) FindAsset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// LatestRelease 取仓库最新正式 release。
func (c *Client) LatestRelease(ctx context.Context, repo string) (*Release, error) {
	if repo == "" {
		return nil, errors.New("fetch: 仓库名为空")
	}
	url := fmt.Sprintf("%s/repos/%s/releases/latest", strings.TrimSuffix(c.BaseURL, "/"), repo)
	body, err := c.get(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("fetch: 取 %s 最新 release 失败: %w", repo, err)
	}
	defer body.Close()
	var rel Release
	if err := decodeJSON(body, &rel); err != nil {
		return nil, fmt.Errorf("fetch: 解析 %s release 失败: %w", repo, err)
	}
	if rel.Tag == "" {
		return nil, fmt.Errorf("fetch: %s 的最新 release 没有 tag", repo)
	}
	return &rel, nil
}

func (c *Client) get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "FasterEdgeRunningCore")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d %s", resp.StatusCode, resp.Status)
	}
	return resp.Body, nil
}

// Download 把产物写进 dst，返回字节数。
// 若产物超过 Limits.MaxArchiveBytes 会在写满之前中止，不会撑爆磁盘。
//
// 重试策略：GitHub 的 release 资产走的是独立的 CDN 域名，实测会偶发 EOF /
// 连接重置；边缘现场的网络更差。因此这里对网络类错误做有限次退避重试，
// 但对 HTTP 4xx（例如 404 = 产物不存在）立即失败，不浪费在必然失败的请求上。
func (c *Client) Download(ctx context.Context, url, dst string) (int64, error) {
	if url == "" {
		return 0, errors.New("fetch: 下载地址为空")
	}
	const attempts = 3
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		n, err := c.downloadOnce(ctx, url, dst)
		if err == nil {
			return n, nil
		}
		lastErr = err
		var statusErr *statusError
		if errors.As(err, &statusErr) && statusErr.Code < 500 {
			return n, err
		}
		var permErr *permanentError
		if errors.As(err, &permErr) {
			return n, permErr
		}
		if ctx.Err() != nil {
			return n, ctx.Err()
		}
		if attempt < attempts {
			select {
			case <-ctx.Done():
				return n, ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
	}
	return 0, fmt.Errorf("fetch: 下载 %s 在 %d 次尝试后仍失败: %w", url, attempts, lastErr)
}

// statusError 携带 HTTP 状态码，便于区分“可重试”与“不该重试”。
type statusError struct {
	Code int
	URL  string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("fetch: 下载 %s 失败: HTTP %d", e.URL, e.Code)
}

func (c *Client) downloadOnce(ctx context.Context, url, dst string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "FasterEdgeRunningCore")
	if c.Token != "" && strings.HasPrefix(url, c.BaseURL) {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, &statusError{Code: resp.StatusCode, URL: url}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	limit := c.Limits.MaxArchiveBytes
	if limit <= 0 {
		limit = DefaultLimits().MaxArchiveBytes
	}
	n, copyErr := io.Copy(f, io.LimitReader(resp.Body, limit+1))
	closeErr := f.Close()
	if copyErr != nil {
		// 半截产物会污染后续校验，必须清掉。
		_ = os.Remove(dst)
		return n, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(dst)
		return n, closeErr
	}
	if n > limit {
		_ = os.Remove(dst)
		// 超限是确定性失败，重试只会浪费时间，因此明确标记为不可重试。
		return n, &permanentError{err: fmt.Errorf("fetch: 产物超过上限 %d 字节，已中止", limit)}
	}
	return n, nil
}

// permanentError 标记“重试也不会成功”的错误，供 Download 跳过退避重试。
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// VerifySHA1 用 release 的 checksums-sha1.txt 校验文件。
// 文件缺失或未列出该产物时返回 ok=false 与 nil error，由调用方决定是否强制。
func VerifySHA1(sumFile, target, name string) (ok bool, err error) {
	want, found, err := lookupSum(sumFile, name)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	f, err := os.Open(target)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha1.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return false, fmt.Errorf("fetch: sha1 不匹配 %s：期望 %s，实际 %s", name, want, got)
	}
	return true, nil
}

// VerifySHA256 用 SHA256SUMS.txt 校验文件。
func VerifySHA256(sumFile, target, name string) (ok bool, err error) {
	want, found, err := lookupSum(sumFile, name)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	f, err := os.Open(target)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return false, fmt.Errorf("fetch: sha256 不匹配 %s：期望 %s，实际 %s", name, want, got)
	}
	return true, nil
}

// lookupSum 在 "digest  name" 或 "digest *name" 格式的校验文件里找目标文件名。
func lookupSum(sumFile, name string) (string, bool, error) {
	f, err := os.Open(sumFile)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	base := path.Base(name)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		gotName := strings.TrimPrefix(fields[len(fields)-1], "*")
		if gotName == name || path.Base(gotName) == base {
			return fields[0], true, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", false, err
	}
	return "", false, nil
}

// Extract 把压缩包解到 dir，返回解出来的文件名（不含目录部分）。
// 只认 .tar.gz / .tgz / .zip；其它格式直接报错而不是猜。
func Extract(archive, dir string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	lower := strings.ToLower(archive)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return extractTarGz(archive, dir)
	case strings.HasSuffix(lower, ".zip"):
		return extractZip(archive, dir)
	default:
		return nil, fmt.Errorf("fetch: 不支持的产物格式 %s", filepath.Base(archive))
	}
}

func extractTarGz(archive, dir string) ([]string, error) {
	f, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	lim := DefaultLimits()
	var (
		out     []string
		total   int64
		entries int
	)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		entries++
		if entries > lim.MaxEntries {
			return nil, fmt.Errorf("fetch: 解包条目超过 %d 条，已中止", lim.MaxEntries)
		}
		name, skip, err := safeEntryName(hdr.Name)
		if err != nil {
			return nil, err
		}
		if skip {
			continue
		}
		if strings.Contains(hdr.Name, "\x00") {
			return nil, errors.New("fetch: 条目名含 NUL 字节")
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
				return nil, err
			}
			continue
		case tar.TypeSymlink, tar.TypeLink:
			// 工程上不需要链接；拒绝以彻底消除通过链接写出 dir 的可能性。
			return nil, fmt.Errorf("fetch: 拒绝符号链接条目 %q", hdr.Name)
		case tar.TypeReg:
		default:
			continue
		}
		if hdr.Size > lim.MaxFileBytes {
			return nil, fmt.Errorf("fetch: 条目 %q 超过单文件上限 %d 字节", hdr.Name, lim.MaxFileBytes)
		}
		total += hdr.Size
		if total > lim.MaxTotalBytes {
			return nil, fmt.Errorf("fetch: 解包总量超过 %d 字节，已中止", lim.MaxTotalBytes)
		}
		dst := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		if err := writeLimited(dst, tr, hdr.Size, hdr.Mode); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, nil
}

func extractZip(archive, dir string) ([]string, error) {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	lim := DefaultLimits()
	var (
		out   []string
		total int64
	)
	if len(zr.File) > lim.MaxEntries {
		return nil, fmt.Errorf("fetch: 解包条目超过 %d 条，已中止", lim.MaxEntries)
	}
	for _, zf := range zr.File {
		name, skip, err := safeEntryName(zf.Name)
		if err != nil {
			return nil, err
		}
		if skip {
			continue
		}
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
				return nil, err
			}
			continue
		}
		if zf.FileInfo().Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("fetch: 拒绝符号链接条目 %q", zf.Name)
		}
		if int64(zf.UncompressedSize64) > lim.MaxFileBytes {
			return nil, fmt.Errorf("fetch: 条目 %q 超过单文件上限 %d 字节", zf.Name, lim.MaxFileBytes)
		}
		total += int64(zf.UncompressedSize64)
		if total > lim.MaxTotalBytes {
			return nil, fmt.Errorf("fetch: 解包总量超过 %d 字节，已中止", lim.MaxTotalBytes)
		}
		rc, err := zf.Open()
		if err != nil {
			return nil, err
		}
		dst := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			rc.Close()
			return nil, err
		}
		err = writeLimited(dst, rc, int64(zf.UncompressedSize64), int64(zf.Mode().Perm()))
		rc.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, nil
}

// safeEntryName 把条目名归一化为 dir 下的相对路径，并拒绝任何逃逸。
func safeEntryName(raw string) (name string, skip bool, err error) {
	if raw == "" {
		return "", true, nil
	}
	cleaned := path.Clean(strings.ReplaceAll(raw, "\\", "/"))
	if cleaned == "." {
		return "", true, nil
	}
	if path.IsAbs(cleaned) || strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return "", false, fmt.Errorf("fetch: 拒绝越界路径 %q", raw)
	}
	if vol := filepath.VolumeName(cleaned); vol != "" {
		return "", false, fmt.Errorf("fetch: 拒绝带盘符的路径 %q", raw)
	}
	if strings.Contains(cleaned, ":") {
		// 打包产物里不该出现冒号；在 Windows 上它还会被当成数据流。
		return "", false, fmt.Errorf("fetch: 拒绝含冒号的路径 %q", raw)
	}
	return cleaned, false, nil
}

func writeLimited(dst string, r io.Reader, size int64, mode int64) error {
	perm := os.FileMode(mode).Perm()
	if perm == 0 {
		perm = 0o644
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	// 即便头部声明的大小被伪造，也按声明大小截断，避免无限写入。
	n, copyErr := io.Copy(f, io.LimitReader(r, size))
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n != size {
		return fmt.Errorf("fetch: 条目 %q 声明 %d 字节，实际 %d 字节", filepath.Base(dst), size, n)
	}
	return nil
}
