// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

package mirror

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

// ServeOptions 控制把镜像目录挂在 HTTP 上的行为。
type ServeOptions struct {
	// Base 是镜像目录。
	Base string
	// Addr 监听地址，例如 0.0.0.0:8090 或 :8090。
	Addr string
	// Log 接收启动信息；可为 nil。
	Log func(format string, args ...any)
	// OnListen 在开始服务前收到实际监听地址（监听 :0 时尤其有用）；可为 nil。
	OnListen func(addr string)
}

// Serve 在 addr 上把镜像目录当静态站点提供，直到 ctx 取消。
//
// 这就是“一台能出网的节点给一片只能连局域网的节点当上游”的落点：上游节点
// 先 `ferc mirror export` 出目录，再 `ferc mirror serve`，下游节点
// `ferc install --from http://<上游>:8090` 即可。不需要额外装 nginx，
// 也不需要下游有任何凭据。
func Serve(ctx context.Context, opt ServeOptions) error {
	if opt.Base == "" {
		return errors.New("mirror: 缺少镜像目录")
	}
	if fi, err := os.Stat(IndexPath(opt.Base)); err != nil || fi.IsDir() {
		return fmt.Errorf("mirror: %s 不是有效的镜像目录（缺少 %s）", opt.Base, IndexName)
	}
	addr := opt.Addr
	if addr == "" {
		addr = ":8090"
	}
	logf := opt.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir(opt.Base)))
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("mirror: 监听 %s 失败: %w", addr, err)
	}
	if opt.OnListen != nil {
		opt.OnListen(ln.Addr().String())
	}
	logf("镜像服务已启动，监听 %s，基址 http://%s/", ln.Addr().String(), hostPort(ln.Addr()))

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// hostPort 把监听地址转成客户端能用的 host:port。
// 监听 0.0.0.0 / :: 时给出本机第一个非环回地址，方便直接复制粘贴。
func hostPort(addr net.Addr) string {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return addr.String()
	}
	if !tcp.IP.IsUnspecified() {
		return addr.String()
	}
	if ip := firstLocalIP(); ip != "" {
		return net.JoinHostPort(ip, strconv.Itoa(tcp.Port))
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(tcp.Port))
}

func firstLocalIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
			continue
		}
		return ipnet.IP.String()
	}
	return ""
}
