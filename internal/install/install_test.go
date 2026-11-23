package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/catalog"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/fetch"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/platform"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/resolve"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/state"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/supervise"
)

type captureLogger struct{ lines []string }

func (c *captureLogger) Logf(format string, args ...any) {
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

func (c *captureLogger) joined() string { return strings.Join(c.lines, "\n") }

// tarGzOf 构造一个含单个文件的 tar.gz。
func tarGzOf(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// releaseServer 起一个假的 GitHub：给出 release JSON、产物与校验文件。
func releaseServer(t *testing.T, assetName string, archive []byte, withSum bool) *httptest.Server {
	t.Helper()
	sum := sha1.Sum(archive)
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/releases/latest") {
			http.NotFound(w, r)
			return
		}
		assets := []fetch.Asset{
			{Name: assetName, URL: srv.URL + "/dl/" + assetName, Size: int64(len(archive))},
		}
		if withSum {
			assets = append(assets, fetch.Asset{Name: "checksums-sha1.txt", URL: srv.URL + "/dl/checksums-sha1.txt"})
		}
		_ = json.NewEncoder(w).Encode(fetch.Release{Tag: "v1.0.20261124", Assets: assets})
	})
	mux.HandleFunc("/dl/"+assetName, func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	})
	mux.HandleFunc("/dl/checksums-sha1.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), assetName)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func planFor(t *testing.T, cat *catalog.Catalog, names ...string) *resolve.Plan {
	t.Helper()
	plan, err := resolve.Resolve(cat, resolve.Request{
		Want: names,
		Info: platform.Info{GOOS: "linux", GOARCH: "amd64", Arch: "amd64", Init: platform.InitSystemd},
	})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	return plan
}

func TestApplyInstallsTool(t *testing.T) {
	archive := tarGzOf(t, "any2utf8", "BINARY")
	srv := releaseServer(t, "any2utf8-linux-amd64.tar.gz", archive, true)

	cat, _ := catalog.Builtin()
	plan := planFor(t, cat, "any2utf8")

	root := t.TempDir()
	client := fetch.New("")
	client.BaseURL = srv.URL
	inst, err := New(Options{Root: root, Fetcher: client})
	if err != nil {
		t.Fatal(err)
	}
	log := &captureLogger{}
	inst.log = log

	recs, err := inst.Apply(context.Background(), plan)
	if err != nil {
		t.Fatalf("安装失败: %v\n%s", err, log.joined())
	}
	if len(recs) != 1 {
		t.Fatalf("应装 1 个组件，实际 %d", len(recs))
	}
	bin := filepath.Join(root, "bin", "any2utf8")
	b, err := os.ReadFile(bin)
	if err != nil {
		t.Fatalf("可执行文件应落盘: %v", err)
	}
	if string(b) != "BINARY" {
		t.Fatalf("内容 = %q", string(b))
	}
	fi, _ := os.Stat(bin)
	if err := checkExecutable(fi.Mode()); err != nil {
		t.Fatal(err)
	}
	if recs[0].Version != "v1.0.20261124" {
		t.Fatalf("Version = %q", recs[0].Version)
	}
	if !strings.Contains(log.joined(), "已通过 checksums-sha1.txt 校验") {
		t.Fatalf("应报告校验通过\n%s", log.joined())
	}

	// 状态文件必须落盘，且能被下次读到。
	st, err := state.Load(state.Path(root))
	if err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	if _, ok := st.Get("any2utf8"); !ok {
		t.Fatal("状态文件应记录已装组件")
	}

	// 暂存目录必须清理干净。
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ferc-stage-") {
			t.Fatalf("残留暂存目录 %s", e.Name())
		}
	}
}

func TestApplyWithoutSumFileSaysSo(t *testing.T) {
	archive := tarGzOf(t, "any2utf8", "B")
	srv := releaseServer(t, "any2utf8-linux-amd64.tar.gz", archive, false)
	cat, _ := catalog.Builtin()
	plan := planFor(t, cat, "any2utf8")

	root := t.TempDir()
	client := fetch.New("")
	client.BaseURL = srv.URL
	inst, _ := New(Options{Root: root, Fetcher: client})
	log := &captureLogger{}
	inst.log = log
	if _, err := inst.Apply(context.Background(), plan); err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if !strings.Contains(log.joined(), "本次未校验") {
		t.Fatalf("没有校验文件时必须如实说明未校验，不能假装校验过\n%s", log.joined())
	}
}

func TestApplyFailsOnMissingAsset(t *testing.T) {
	archive := tarGzOf(t, "any2utf8", "B")
	srv := releaseServer(t, "any2utf8-linux-amd64.tar.gz", archive, false)
	cat, _ := catalog.Builtin()
	plan := planFor(t, cat, "any2utf8")
	// 故意把产物名改错，模拟 release 命名变了。
	plan.Steps[0].Asset = "any2utf8-linux-riscv64.tar.gz"

	root := t.TempDir()
	client := fetch.New("")
	client.BaseURL = srv.URL
	inst, _ := New(Options{Root: root, Fetcher: client})
	inst.log = &captureLogger{}
	_, err := inst.Apply(context.Background(), plan)
	if err == nil {
		t.Fatal("产物缺失必须报错")
	}
	if !strings.Contains(err.Error(), "没有产物") {
		t.Fatalf("错误应说明缺产物: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "bin", "any2utf8")); !os.IsNotExist(statErr) {
		t.Fatal("失败时不该留下可执行文件")
	}
}

func TestApplyFailsOnChecksumMismatch(t *testing.T) {
	archive := tarGzOf(t, "any2utf8", "B")
	// 服务端给出的校验和与内容不符。
	srv := releaseServer(t, "any2utf8-linux-amd64.tar.gz", archive, false)
	mux := http.NewServeMux()
	var s *httptest.Server
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(fetch.Release{Tag: "v1", Assets: []fetch.Asset{
			{Name: "any2utf8-linux-amd64.tar.gz", URL: s.URL + "/dl/a"},
			{Name: "checksums-sha1.txt", URL: s.URL + "/dl/sum"},
		}})
	})
	mux.HandleFunc("/dl/a", func(w http.ResponseWriter, r *http.Request) { w.Write(archive) })
	mux.HandleFunc("/dl/sum", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "0000000000000000000000000000000000000000  any2utf8-linux-amd64.tar.gz\n")
	})
	s = httptest.NewServer(mux)
	defer s.Close()
	_ = srv

	cat, _ := catalog.Builtin()
	plan := planFor(t, cat, "any2utf8")
	root := t.TempDir()
	client := fetch.New("")
	client.BaseURL = s.URL
	inst, _ := New(Options{Root: root, Fetcher: client})
	inst.log = &captureLogger{}
	_, err := inst.Apply(context.Background(), plan)
	if err == nil {
		t.Fatal("校验和不符必须失败")
	}
	if !strings.Contains(err.Error(), "sha1 不匹配") {
		t.Fatalf("错误应指明校验失败: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "bin", "any2utf8")); !os.IsNotExist(statErr) {
		t.Fatal("校验失败不该留下产物")
	}
}

func TestApplyDryRunTouchesNothing(t *testing.T) {
	cat, _ := catalog.Builtin()
	plan := planFor(t, cat, "any2utf8")
	root := t.TempDir()
	inst, err := New(Options{Root: root, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	log := &captureLogger{}
	inst.log = log
	recs, err := inst.Apply(context.Background(), plan)
	if err != nil {
		t.Fatalf("dry-run 不该失败: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("dry-run 也应回报将要装什么，实际 %d", len(recs))
	}
	if _, err := os.Stat(filepath.Join(root, "bin")); !os.IsNotExist(err) {
		t.Fatal("dry-run 不该创建 bin 目录")
	}
	if !strings.Contains(log.joined(), "[dry-run]") {
		t.Fatalf("应标注 dry-run\n%s", log.joined())
	}
}

func TestApplyNoSupervisorSkipsRegistration(t *testing.T) {
	archive := tarGzOf(t, "simple-web-shell", "B")
	srv := releaseServer(t, "simple-web-shell-linux-amd64.tar.gz", archive, false)
	cat, _ := catalog.Builtin()
	plan := planFor(t, cat, "simple-web-shell")

	root := t.TempDir()
	client := fetch.New("")
	client.BaseURL = srv.URL
	inst, _ := New(Options{Root: root, Fetcher: client}) // Supervisor 为 nil
	log := &captureLogger{}
	inst.log = log
	recs, err := inst.Apply(context.Background(), plan)
	if err != nil {
		t.Fatalf("安装失败: %v", err)
	}
	if recs[0].Supervisor != "" {
		t.Fatalf("未提供 supervisor 时不该记录托管方，实际 %q", recs[0].Supervisor)
	}
	if !strings.Contains(log.joined(), "--no-start") {
		t.Fatalf("应说明未注册服务\n%s", log.joined())
	}
}

func TestApplyRollsBackOnSecondStepFailure(t *testing.T) {
	// 第一步成功、第二步产物缺失：已落盘的文件必须被清掉。
	archive := tarGzOf(t, "any2utf8", "B")
	srv := releaseServer(t, "any2utf8-linux-amd64.tar.gz", archive, false)
	cat, _ := catalog.Builtin()
	plan := planFor(t, cat, "any2utf8")
	plan.Steps = append(plan.Steps, resolve.Step{
		Component: mustComp(t, cat, "any2pcd"),
		Repo:      "FasterEdge/Any2Pcd",
		Asset:     "does-not-exist.tar.gz",
		Binary:    "any2pcd",
	})

	root := t.TempDir()
	client := fetch.New("")
	client.BaseURL = srv.URL
	inst, _ := New(Options{Root: root, Fetcher: client})
	inst.log = &captureLogger{}
	if _, err := inst.Apply(context.Background(), plan); err == nil {
		t.Fatal("第二步失败必须整体失败")
	}
	if _, err := os.Stat(filepath.Join(root, "bin", "any2utf8")); !os.IsNotExist(err) {
		t.Fatal("回滚应删除第一步落盘的文件")
	}
	st, err := state.Load(state.Path(root))
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Components) != 0 {
		t.Fatalf("失败时状态文件不该记录任何组件，实际 %v", st.NamesSorted())
	}
}

func TestApplyRegistersService(t *testing.T) {
	archive := tarGzOf(t, "simple-web-shell", "B")
	srv := releaseServer(t, "simple-web-shell-linux-amd64.tar.gz", archive, false)
	cat, _ := catalog.Builtin()
	plan := planFor(t, cat, "simple-web-shell")

	root := t.TempDir()
	client := fetch.New("")
	client.BaseURL = srv.URL

	var calls []string
	mgr := &supervise.Manager{
		GOOS: "linux",
		Init: platform.InitSystemd,
		Run: func(_ context.Context, name string, args ...string) (string, error) {
			calls = append(calls, name+" "+strings.Join(args, " "))
			return "", nil
		},
		Write: func(p string, b []byte, perm os.FileMode) error {
			return os.WriteFile(filepath.Join(root, filepath.Base(p)), b, perm)
		},
		MkdirAll: func(p string, perm os.FileMode) error { return os.MkdirAll(p, perm) },
		Root:     root,
	}
	inst, err := New(Options{Root: root, Fetcher: client, Supervisor: mgr})
	if err != nil {
		t.Fatal(err)
	}
	log := &captureLogger{}
	inst.log = log

	recs, err := inst.Apply(context.Background(), plan)
	if err != nil {
		t.Fatalf("安装失败: %v\n%s", err, log.joined())
	}
	if recs[0].Supervisor != "systemd" || recs[0].Unit != "ferc-simple-web-shell" {
		t.Fatalf("托管信息 = %q %q", recs[0].Supervisor, recs[0].Unit)
	}
	if len(calls) == 0 {
		t.Fatal("应调用 systemctl")
	}
	if _, err := os.Stat(filepath.Join(root, "ferc-simple-web-shell.service")); err != nil {
		t.Fatalf("unit 文件应写出: %v", err)
	}
	st, err := state.Load(state.Path(root))
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := st.Get("simple-web-shell")
	if rec.Supervisor != "systemd" {
		t.Fatalf("状态文件应记录托管方，实际 %q", rec.Supervisor)
	}
}

func TestApplySkipsRegistrationForSupervisorKind(t *testing.T) {
	// supervisor 自身不该被别的托管方托管，否则会形成环。
	archive := tarGzOf(t, "dontcrack-manylinux", "B")
	srv := releaseServer(t, "dontcrack-manylinux-linux-amd64.tar.gz", archive, false)
	cat, _ := catalog.Builtin()
	plan, err := resolve.Resolve(cat, resolve.Request{
		Want: []string{"dontcrack"},
		Info: platform.Info{GOOS: "linux", GOARCH: "amd64", Arch: "amd64", Init: platform.InitNone},
	})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	root := t.TempDir()
	client := fetch.New("")
	client.BaseURL = srv.URL
	mgr := &supervise.Manager{
		GOOS:     "linux",
		Init:     platform.InitNone,
		Run:      func(context.Context, string, ...string) (string, error) { return "", nil },
		Write:    func(string, []byte, os.FileMode) error { return nil },
		MkdirAll: func(p string, perm os.FileMode) error { return os.MkdirAll(p, perm) },
		Root:     root,
	}
	inst, err := New(Options{Root: root, Fetcher: client, Supervisor: mgr})
	if err != nil {
		t.Fatal(err)
	}
	inst.log = &captureLogger{}
	recs, applyErr := inst.Apply(context.Background(), plan)
	if applyErr != nil {
		t.Fatalf("安装失败: %v", applyErr)
	}
	if recs[0].Supervisor != "self" {
		t.Fatalf("supervisor 自身应标记 self，实际 %q", recs[0].Supervisor)
	}
}

func TestApplyGeneratesSecretForRequiredArg(t *testing.T) {
	archive := tarGzOf(t, "simple-web-shell", "B")
	srv := releaseServer(t, "simple-web-shell-linux-amd64.tar.gz", archive, false)
	cat, _ := catalog.Builtin()
	plan := planFor(t, cat, "simple-web-shell")
	if len(plan.Steps) != 1 || len(plan.Steps[0].Secrets) == 0 {
		t.Fatalf("计划应标记需要生成凭据: %+v", plan.Steps)
	}

	root := t.TempDir()
	client := fetch.New("")
	client.BaseURL = srv.URL
	inst, err := New(Options{Root: root, Fetcher: client})
	if err != nil {
		t.Fatal(err)
	}
	log := &captureLogger{}
	inst.log = log
	recs, err := inst.Apply(context.Background(), plan)
	if err != nil {
		t.Fatalf("安装失败: %v\n%s", err, log.joined())
	}
	rec := recs[0]
	if rec.SecretFile == "" {
		t.Fatal("应记录凭据文件路径")
	}
	b, err := os.ReadFile(rec.SecretFile)
	if err != nil {
		t.Fatalf("凭据文件应存在: %v", err)
	}
	if !strings.Contains(string(b), "-key=") {
		t.Fatalf("凭据文件应含 -key=，实际 %s", b)
	}
	// 参数里必须真的带上凭据，否则服务启动还是会被拒。
	found := false
	for _, a := range rec.Args {
		if strings.HasPrefix(a, "-key=") {
			found = true
		}
	}
	if !found {
		t.Fatalf("启动参数应含 -key=，实际 %v", rec.Args)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(rec.SecretFile)
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("凭据文件权限应为 0600，实际 %v", fi.Mode().Perm())
		}
	}
	// 凭据不该出现在日志里。
	if strings.Contains(log.joined(), "-key=") {
		t.Fatalf("日志不该泄露凭据\n%s", log.joined())
	}
}

func TestApplyGeneratesDistinctSecrets(t *testing.T) {
	archive := tarGzOf(t, "simple-web-shell", "B")
	srv := releaseServer(t, "simple-web-shell-linux-amd64.tar.gz", archive, false)
	cat, _ := catalog.Builtin()

	read := func() string {
		plan := planFor(t, cat, "simple-web-shell")
		root := t.TempDir()
		client := fetch.New("")
		client.BaseURL = srv.URL
		inst, err := New(Options{Root: root, Fetcher: client})
		if err != nil {
			t.Fatal(err)
		}
		inst.log = &captureLogger{}
		recs, err := inst.Apply(context.Background(), plan)
		if err != nil {
			t.Fatalf("安装失败: %v", err)
		}
		return recs[0].Args[len(recs[0].Args)-1]
	}
	a, b := read(), read()
	if a == b {
		t.Fatalf("两次生成的凭据不该相同: %s", a)
	}
}

func TestApplyRecordsDistinctControlPortsForMultipleServices(t *testing.T) {
	// 没有原生保活能力的系统上，两个服务各要一个 DontCrack 实例。若端口相同，
	// 第二个实例绑不上端口，节点就变成“一个被保活、一个裸奔”而没人知道。
	mux := http.NewServeMux()
	type fake struct {
		repo, asset string
		archive     []byte
	}
	fakes := []fake{
		{"FasterEdge/DontCrack4ManyLinux", "dontcrack-manylinux-linux-amd64.tar.gz", tarGzOf(t, "dontcrack-manylinux", "B")},
		{"FasterEdge/SimpleWebShell", "simple-web-shell-linux-amd64.tar.gz", tarGzOf(t, "simple-web-shell", "B")},
		{"FasterEdge/SimpleTimeService", "simple-time-service-linux-amd64.tar.gz", tarGzOf(t, "simple-time-service", "B")},
	}
	var srv *httptest.Server
	for _, f := range fakes {
		f := f
		mux.HandleFunc("/repos/"+f.repo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(fetch.Release{
				Tag:    "v1.0.20261124",
				Assets: []fetch.Asset{{Name: f.asset, URL: srv.URL + "/dl/" + f.asset, Size: int64(len(f.archive))}},
			})
		})
		mux.HandleFunc("/dl/"+f.asset, func(w http.ResponseWriter, r *http.Request) { w.Write(f.archive) })
	}
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cat, _ := catalog.Builtin()
	plan, err := resolve.Resolve(cat, resolve.Request{
		Want: []string{"simple-web-shell", "simple-time-service"},
		Info: platform.Info{GOOS: "linux", GOARCH: "amd64", Arch: "amd64", Init: platform.InitNone},
	})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	root := t.TempDir()
	var ports []string
	mgr := &supervise.Manager{
		GOOS:         "linux",
		Init:         platform.InitNone,
		DontCrackBin: filepath.Join(root, "bin", "dontcrack-manylinux"),
		Run:          func(context.Context, string, ...string) (string, error) { return "", nil },
		Write:        func(string, []byte, os.FileMode) error { return nil },
		MkdirAll:     func(p string, perm os.FileMode) error { return os.MkdirAll(p, perm) },
		Get:          func(string) error { return nil },
		Spawn: func(_ string, args []string, _ string) (int, error) {
			for i, a := range args {
				if a == "-port" && i+1 < len(args) {
					ports = append(ports, args[i+1])
				}
			}
			return 9000 + len(ports), nil
		},
		Root: root,
	}
	client := fetch.New("")
	client.BaseURL = srv.URL
	inst, err := New(Options{
		Root:       root,
		Info:       platform.Info{GOOS: "linux", GOARCH: "amd64", Init: platform.InitNone},
		Fetcher:    client,
		Supervisor: mgr,
	})
	if err != nil {
		t.Fatal(err)
	}
	log := &captureLogger{}
	inst.log = log
	if _, err := inst.Apply(context.Background(), plan); err != nil {
		t.Fatalf("安装失败: %v\n%s", err, log.joined())
	}
	if len(ports) != 2 || ports[0] == ports[1] {
		t.Fatalf("两个服务应各占一个控制端口，实际 %v", ports)
	}
	st, err := state.Load(state.Path(root))
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, r := range st.Components {
		if r.ControlPort != 0 {
			got = append(got, r.ControlPort)
		}
	}
	if len(got) != 2 || got[0] == got[1] {
		t.Fatalf("状态文件应记录两个不同的控制端口，实际 %v", got)
	}
}

func mustComp(t *testing.T, cat *catalog.Catalog, name string) *catalog.Component {
	t.Helper()
	c, ok := cat.Get(name)
	if !ok {
		t.Fatalf("缺少组件 %s", name)
	}
	return c
}

func TestFindBinary(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "tool"), []byte("x"), 0o755)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "tool"), []byte("y"), 0o755)

	got, err := findBinary(dir, []string{"sub/tool", "tool"}, "tool")
	if err != nil {
		t.Fatalf("应找到: %v", err)
	}
	if filepath.Dir(got) != dir {
		t.Fatalf("应优先取顶层同名文件，实际 %s", got)
	}
}

func TestFindBinaryFallsBackToSubdir(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "bin"), 0o755)
	os.WriteFile(filepath.Join(dir, "bin", "tool"), []byte("x"), 0o755)
	got, err := findBinary(dir, []string{"bin/tool"}, "tool")
	if err != nil {
		t.Fatalf("应在子目录里找到: %v", err)
	}
	if filepath.Base(filepath.Dir(got)) != "bin" {
		t.Fatalf("路径 = %s", got)
	}
}

func TestFindBinaryMissing(t *testing.T) {
	dir := t.TempDir()
	if _, err := findBinary(dir, []string{"other"}, "tool"); err == nil {
		t.Fatal("找不到时必须报错")
	}
}

func TestMoveFileCrossDir(t *testing.T) {
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "a")
	os.WriteFile(src, []byte("data"), 0o644)
	dst := filepath.Join(t.TempDir(), "sub", "a")
	if err := moveFile(src, dst); err != nil {
		t.Fatalf("移动失败: %v", err)
	}
	b, err := os.ReadFile(dst)
	if err != nil || string(b) != "data" {
		t.Fatalf("内容 = %q err=%v", string(b), err)
	}
	fi, _ := os.Stat(dst)
	if err := checkExecutable(fi.Mode()); err != nil {
		t.Fatal(err)
	}
}

// checkExecutable 在 Unix 上断言可执行位。Windows 的权限位由 ACL 决定，
// os.Chmod 只能切换只读位，因此在那里不做断言。
func checkExecutable(mode os.FileMode) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if mode.Perm()&0o111 == 0 {
		return fmt.Errorf("应具备可执行位，实际 %v", mode)
	}
	return nil
}
