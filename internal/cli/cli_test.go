package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/boot"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/catalog"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/fetch"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/platform"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/state"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/supervise"
)

type fakeDetect struct {
	info platform.Info
	err  error
}

func (f fakeDetect) detect(context.Context) (platform.Info, error) { return f.info, f.err }

func testEnv(t *testing.T, info platform.Info) (Env, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	out, errb := &bytes.Buffer{}, &bytes.Buffer{}
	env := Env{
		Stdout:      out,
		Stderr:      errb,
		DefaultRoot: t.TempDir(),
		Getenv:      func(string) string { return "" },
		Detect:      func(context.Context) (platform.Info, error) { return info, nil },
	}
	return env, out, errb
}

func linuxInfo(init platform.InitSystem) platform.Info {
	return platform.Info{
		GOOS: "linux", GOARCH: "amd64", Arch: "amd64",
		Hostname: "edge-01", Init: init, Distro: "debian", DistroVersion: "12",
		Kernel: "6.6.0", PackageMgr: "apt-get", Privileged: true,
	}
}

func TestRunNoArgs(t *testing.T) {
	env, _, errb := testEnv(t, linuxInfo(platform.InitSystemd))
	if code := Run(context.Background(), nil, env); code != 2 {
		t.Fatalf("无参数退出码 = %d，期望 2", code)
	}
	if !strings.Contains(errb.String(), "用法") {
		t.Fatalf("应打印用法，实际 %q", errb.String())
	}
}

func TestRunUnknownCommand(t *testing.T) {
	env, _, errb := testEnv(t, linuxInfo(platform.InitSystemd))
	if code := Run(context.Background(), []string{"nope"}, env); code != 2 {
		t.Fatalf("未知命令退出码 = %d", code)
	}
	if !strings.Contains(errb.String(), "未知子命令") {
		t.Fatalf("应提示未知子命令: %q", errb.String())
	}
}

func TestVersion(t *testing.T) {
	env, out, _ := testEnv(t, linuxInfo(platform.InitSystemd))
	if code := Run(context.Background(), []string{"version"}, env); code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	if !strings.Contains(out.String(), "ferc") {
		t.Fatalf("输出 = %q", out.String())
	}
}

func TestDetectText(t *testing.T) {
	env, out, _ := testEnv(t, linuxInfo(platform.InitSystemd))
	if code := Run(context.Background(), []string{"detect"}, env); code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	s := out.String()
	for _, want := range []string{"linux/amd64", "systemd", "开机自启      是", "崩溃拉起      是", "无需额外进程管理器"} {
		if !strings.Contains(s, want) {
			t.Errorf("输出缺少 %q\n%s", want, s)
		}
	}
}

func TestDetectNoInitSaysNeedsSupervisor(t *testing.T) {
	env, out, _ := testEnv(t, linuxInfo(platform.InitNone))
	if code := Run(context.Background(), []string{"detect"}, env); code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	if !strings.Contains(out.String(), "需要装载自托管进程管理器") {
		t.Fatalf("无 init 时必须明确说明需要进程管理器\n%s", out.String())
	}
}

func TestDetectJSON(t *testing.T) {
	env, out, _ := testEnv(t, linuxInfo(platform.InitSystemd))
	if code := Run(context.Background(), []string{"detect", "--json"}, env); code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	var got platform.Info
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("不是合法 JSON: %v\n%s", err, out.String())
	}
	if got.Init != platform.InitSystemd {
		t.Fatalf("Init = %q", got.Init)
	}
}

func TestListText(t *testing.T) {
	env, out, _ := testEnv(t, linuxInfo(platform.InitSystemd))
	if code := Run(context.Background(), []string{"list"}, env); code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	s := out.String()
	if !strings.Contains(s, "simple-web-shell") {
		t.Fatalf("应列出远程终端\n%s", s)
	}
	if !strings.Contains(s, "可装载") {
		t.Fatalf("应显示可装载状态\n%s", s)
	}
}

func TestListJSON(t *testing.T) {
	env, out, _ := testEnv(t, linuxInfo(platform.InitSystemd))
	if code := Run(context.Background(), []string{"list", "--json"}, env); code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("不是合法 JSON: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("JSON 列表为空")
	}
}

func TestListShowsUnsupportedArch(t *testing.T) {
	info := linuxInfo(platform.InitSystemd)
	info.GOARCH, info.Arch = "riscv64", "riscv64"
	env, out, _ := testEnv(t, info)
	if code := Run(context.Background(), []string{"list"}, env); code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	if !strings.Contains(out.String(), "不支持 riscv64") {
		t.Fatalf("应标注不支持的架构\n%s", out.String())
	}
}

func TestInstallRequiresComponent(t *testing.T) {
	env, _, errb := testEnv(t, linuxInfo(platform.InitSystemd))
	if code := Run(context.Background(), []string{"install"}, env); code != 2 {
		t.Fatalf("退出码 = %d，期望 2", code)
	}
	if !strings.Contains(errb.String(), "请指定要装载的组件") {
		t.Fatalf("应提示需要组件: %q", errb.String())
	}
}

func TestInstallDryRun(t *testing.T) {
	root := t.TempDir()
	env, out, _ := testEnv(t, linuxInfo(platform.InitSystemd))
	env.DefaultRoot = root
	code := Run(context.Background(), []string{"install", "any2utf8", "--dry-run"}, env)
	if code != 0 {
		t.Fatalf("退出码 = %d\n%s", code, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "装载计划") || !strings.Contains(s, "any2utf8") {
		t.Fatalf("应打印计划\n%s", s)
	}
	if !strings.Contains(s, "dry-run") {
		t.Fatalf("应标注 dry-run\n%s", s)
	}
	if _, err := os.Stat(filepath.Join(root, "bin")); !os.IsNotExist(err) {
		t.Fatal("dry-run 不该创建目录")
	}
}

func TestBootstrapDryRunPlansSupervisorWhenNoInit(t *testing.T) {
	root := t.TempDir()
	env, out, _ := testEnv(t, linuxInfo(platform.InitNone))
	env.DefaultRoot = root
	if code := Run(context.Background(), []string{"bootstrap", "--dry-run"}, env); code != 0 {
		t.Fatalf("退出码 = %d\n%s", code, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "将装载自托管进程管理器") {
		t.Fatalf("无 init 时计划里必须说明会装进程管理器\n%s", s)
	}
	if !strings.Contains(s, "dontcrack") {
		t.Fatalf("计划里应出现 dontcrack\n%s", s)
	}
}

func TestBootstrapWithSystemdNoSupervisor(t *testing.T) {
	env, out, _ := testEnv(t, linuxInfo(platform.InitSystemd))
	env.DefaultRoot = t.TempDir()
	if code := Run(context.Background(), []string{"bootstrap", "--dry-run"}, env); code != 0 {
		t.Fatalf("退出码 = %d\n%s", code, out.String())
	}
	if strings.Contains(out.String(), "将装载自托管进程管理器") {
		t.Fatalf("systemd 下不该装进程管理器\n%s", out.String())
	}
}

func TestInstallConflictIsError(t *testing.T) {
	env, _, errb := testEnv(t, linuxInfo(platform.InitSystemd))
	env.DefaultRoot = t.TempDir()
	code := Run(context.Background(), []string{"install", "netmap", "fasteredge2api", "--dry-run"}, env)
	if code != 1 {
		t.Fatalf("冲突应返回 1，实际 %d\n%s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "冲突") {
		t.Fatalf("应说明冲突: %q", errb.String())
	}
}

func TestInstallConflictAllowed(t *testing.T) {
	env, out, _ := testEnv(t, linuxInfo(platform.InitSystemd))
	env.DefaultRoot = t.TempDir()
	code := Run(context.Background(),
		[]string{"install", "netmap", "fasteredge2api", "--dry-run", "--allow-conflicts"}, env)
	if code != 0 {
		t.Fatalf("允许冲突时应成功，实际 %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "警告") {
		t.Fatalf("降级后必须留下告警\n%s", out.String())
	}
}

func TestStatusEmpty(t *testing.T) {
	env, out, _ := testEnv(t, linuxInfo(platform.InitSystemd))
	env.DefaultRoot = t.TempDir()
	if code := Run(context.Background(), []string{"status"}, env); code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	if !strings.Contains(out.String(), "尚未装载") {
		t.Fatalf("空状态应明确说明\n%s", out.String())
	}
}

func TestStatusListsInstalled(t *testing.T) {
	root := t.TempDir()
	st := state.New("linux", "amd64", "edge-01", root)
	st.Put(state.Record{Name: "simple-web-shell", Kind: "service", Version: "v1",
		Path: filepath.Join(root, "bin", "simple-web-shell"), Supervisor: "systemd", Unit: "ferc-simple-web-shell"})
	if err := st.Save(state.Path(root)); err != nil {
		t.Fatal(err)
	}
	env, out, _ := testEnv(t, linuxInfo(platform.InitSystemd))
	env.DefaultRoot = root
	if code := Run(context.Background(), []string{"status"}, env); code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	s := out.String()
	if !strings.Contains(s, "simple-web-shell") || !strings.Contains(s, "systemd") {
		t.Fatalf("应列出组件与托管方\n%s", s)
	}
}

func TestStatusJSON(t *testing.T) {
	root := t.TempDir()
	st := state.New("linux", "amd64", "edge-01", root)
	st.Put(state.Record{Name: "a", Kind: "tool", Path: "/x"})
	st.Save(state.Path(root))
	env, out, _ := testEnv(t, linuxInfo(platform.InitSystemd))
	env.DefaultRoot = root
	if code := Run(context.Background(), []string{"status", "--json"}, env); code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	var got state.State
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("不是合法 JSON: %v", err)
	}
	if len(got.Components) != 1 {
		t.Fatalf("Components = %d", len(got.Components))
	}
}

func TestVerifyAllGood(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	exe := filepath.Join(bin, "tool")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := state.New("linux", "amd64", "h", root)
	st.Put(state.Record{Name: "tool", Kind: "tool", Path: exe})
	st.Save(state.Path(root))

	env, out, _ := testEnv(t, linuxInfo(platform.InitSystemd))
	env.DefaultRoot = root
	if code := Run(context.Background(), []string{"verify"}, env); code != 0 {
		t.Fatalf("退出码 = %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "全部 1 个组件检查通过") {
		t.Fatalf("应报告通过\n%s", out.String())
	}
}

func TestVerifyDetectsMissingFile(t *testing.T) {
	root := t.TempDir()
	st := state.New("linux", "amd64", "h", root)
	st.Put(state.Record{Name: "gone", Kind: "tool", Path: filepath.Join(root, "bin", "gone")})
	st.Save(state.Path(root))

	env, out, errb := testEnv(t, linuxInfo(platform.InitSystemd))
	env.DefaultRoot = root
	if code := Run(context.Background(), []string{"verify"}, env); code != 1 {
		t.Fatalf("缺文件应返回 1，实际 %d", code)
	}
	if !strings.Contains(out.String(), "FAIL") {
		t.Fatalf("应标 FAIL\n%s", out.String())
	}
	if !strings.Contains(errb.String(), "1 个组件检查失败") {
		t.Fatalf("应汇总失败数: %q", errb.String())
	}
}

func TestVerifyJSONExitCode(t *testing.T) {
	root := t.TempDir()
	st := state.New("linux", "amd64", "h", root)
	st.Put(state.Record{Name: "gone", Kind: "tool", Path: filepath.Join(root, "bin", "gone")})
	st.Save(state.Path(root))
	env, out, _ := testEnv(t, linuxInfo(platform.InitSystemd))
	env.DefaultRoot = root
	if code := Run(context.Background(), []string{"verify", "--json"}, env); code != 1 {
		t.Fatalf("JSON 模式下缺文件也应返回 1，实际 %d", code)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("不是合法 JSON: %v", err)
	}
}

func TestRemoveRequiresName(t *testing.T) {
	env, _, errb := testEnv(t, linuxInfo(platform.InitSystemd))
	if code := Run(context.Background(), []string{"remove"}, env); code != 2 {
		t.Fatalf("退出码 = %d", code)
	}
	if !strings.Contains(errb.String(), "请指定要卸载的组件") {
		t.Fatalf("应提示: %q", errb.String())
	}
}

func TestRemoveTool(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	exe := filepath.Join(bin, "any2utf8")
	os.WriteFile(exe, []byte("x"), 0o755)
	st := state.New("linux", "amd64", "h", root)
	st.Put(state.Record{Name: "any2utf8", Kind: "tool", Path: exe})
	st.Save(state.Path(root))

	env, out, _ := testEnv(t, linuxInfo(platform.InitSystemd))
	env.DefaultRoot = root
	if code := Run(context.Background(), []string{"remove", "any2utf8"}, env); code != 0 {
		t.Fatalf("退出码 = %d\n%s", code, out.String())
	}
	if _, err := os.Stat(exe); !os.IsNotExist(err) {
		t.Fatal("文件应被删除")
	}
	after, _ := state.Load(state.Path(root))
	if len(after.Components) != 0 {
		t.Fatalf("状态文件应清空，实际 %v", after.NamesSorted())
	}
}

func TestRemoveUnknownIsSkipped(t *testing.T) {
	env, _, errb := testEnv(t, linuxInfo(platform.InitSystemd))
	env.DefaultRoot = t.TempDir()
	if code := Run(context.Background(), []string{"remove", "ghost"}, env); code != 0 {
		t.Fatalf("未装载的组件应跳过而非失败，实际 %d", code)
	}
	if !strings.Contains(errb.String(), "未装载") {
		t.Fatalf("应提示未装载: %q", errb.String())
	}
}

func TestRemoveWarnsDependents(t *testing.T) {
	raw := `{"schema_version":1,"components":[
		{"name":"app","kind":"tool","requires":["lib"],"repos":{"default":"o/r"}},
		{"name":"lib","kind":"tool","repos":{"default":"o/r"}}]}`
	catPath := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(catPath, []byte(raw), 0o644)

	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	libPath := filepath.Join(bin, "lib")
	appPath := filepath.Join(bin, "app")
	os.WriteFile(libPath, []byte("x"), 0o755)
	os.WriteFile(appPath, []byte("x"), 0o755)
	st := state.New("linux", "amd64", "h", root)
	st.Put(state.Record{Name: "lib", Kind: "tool", Path: libPath})
	st.Put(state.Record{Name: "app", Kind: "tool", Path: appPath})
	st.Save(state.Path(root))

	env, _, errb := testEnv(t, linuxInfo(platform.InitSystemd))
	env.DefaultRoot = root
	if code := Run(context.Background(), []string{"remove", "lib", "--catalog", catPath}, env); code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	if !strings.Contains(errb.String(), "app 依赖被移除的 lib") {
		t.Fatalf("应提示依赖者\n%s", errb.String())
	}
}

func TestBadCatalogPath(t *testing.T) {
	env, _, errb := testEnv(t, linuxInfo(platform.InitSystemd))
	env.DefaultRoot = t.TempDir()
	code := Run(context.Background(), []string{"list", "--catalog", filepath.Join(t.TempDir(), "nope.json")}, env)
	if code != 1 {
		t.Fatalf("退出码 = %d", code)
	}
	if !strings.Contains(errb.String(), "读清单") {
		t.Fatalf("应说明读清单失败: %q", errb.String())
	}
}

func TestDefaultRoot(t *testing.T) {
	if got := DefaultRoot("linux"); got != "/opt/FasterEdgeRunningCore" {
		t.Fatalf("linux 默认目录 = %q", got)
	}
	if got := DefaultRoot("windows"); !strings.Contains(got, "FasterEdgeRunningCore") {
		t.Fatalf("windows 默认目录 = %q", got)
	}
}

func TestEnvTokenPrecedence(t *testing.T) {
	called := ""
	env := Env{Getenv: func(k string) string {
		called = k
		if k == "GH_TOKEN" {
			return "tok"
		}
		return ""
	}}
	if got := envToken(env); got != "tok" {
		t.Fatalf("envToken = %q，期望取 GH_TOKEN", got)
	}
	if called != "GH_TOKEN" {
		t.Fatalf("应优先查 GH_TOKEN，实际查了 %q", called)
	}
	env.Token = "explicit"
	if got := envToken(env); got != "explicit" {
		t.Fatalf("显式 token 应优先，实际 %q", got)
	}
}

func TestWriteJSONErrorOnUnserializable(t *testing.T) {
	var buf bytes.Buffer
	if code := writeJSON(&buf, make(chan int)); code != 1 {
		t.Fatalf("不可序列化应返回 1，实际 %d", code)
	}
}

func TestPrintPlanIncludesArgs(t *testing.T) {
	env, out, _ := testEnv(t, linuxInfo(platform.InitNone))
	env.DefaultRoot = t.TempDir()
	Run(context.Background(), []string{"bootstrap", "--dry-run"}, env)
	if !strings.Contains(out.String(), "参数:") {
		t.Fatalf("有默认参数时应打印出来\n%s", out.String())
	}
}

func TestCatBootstrappedHasWebShell(t *testing.T) {
	env, out, _ := testEnv(t, linuxInfo(platform.InitSystemd))
	env.DefaultRoot = t.TempDir()
	Run(context.Background(), []string{"bootstrap", "--dry-run"}, env)
	if !strings.Contains(out.String(), "simple-web-shell") {
		t.Fatalf("一键默认必须含远程终端\n%s", out.String())
	}
}

func TestInstallMissingGHTokenStillWorks(t *testing.T) {
	// 没有 token 不该阻止 dry-run；真实下载在单测里用假 server 覆盖。
	env, _, _ := testEnv(t, linuxInfo(platform.InitSystemd))
	env.DefaultRoot = t.TempDir()
	if code := Run(context.Background(), []string{"install", "any2utf8", "--dry-run"}, env); code != 0 {
		t.Fatalf("无 token 的 dry-run 应成功，实际 %d", code)
	}
}

var _ = fmt.Sprintf

// ── restore / boot 的测试辅助 ────────────────────────────────

// tarGz 构造一个只含单个文件的 tar.gz，供装载流程解包。
func tarGz(t *testing.T, name, body string) []byte {
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
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// memSource 是只返回一个产物、并把产物写到本地的假取回源：装载流程可以跑通，
// 但完全不接触网络。
type memSource struct {
	asset   string
	archive []byte
}

func (m memSource) LatestRelease(context.Context, string) (*fetch.Release, error) {
	return &fetch.Release{Tag: "v9.9.9", Assets: []fetch.Asset{
		{Name: m.asset, URL: "mem://" + m.asset, Size: int64(len(m.archive))},
	}}, nil
}

func (m memSource) Download(_ context.Context, _, dst string) (int64, error) {
	if err := os.WriteFile(dst, m.archive, 0o600); err != nil {
		return 0, err
	}
	return int64(len(m.archive)), nil
}

// recordManager 返回一个所有副作用都被拦在内存里的托管器。
//
// 恢复与装载测试都不能真的写 /etc、也不能真的派生进程：那既会污染测试机，
// 又会留下没人收拾的后台进程。这里把 Write / Run / Spawn / MkdirAll 全部换成
// 记录器，测试只断言“调了什么”。
type recordManager struct {
	init   platform.InitSystem
	runs   []string
	spawns []string
	health map[int]error
	// runErr 让外部命令全部失败，用于覆盖“注册已经存在”与“注册失败”两条分支。
	runErr error
	memFS
}

type memFS struct {
	written map[string][]byte
	dirs    []string
}

func newRecordManager(init platform.InitSystem) *recordManager {
	return &recordManager{
		init:   init,
		health: map[int]error{},
		memFS:  memFS{written: map[string][]byte{}},
	}
}

func (r *recordManager) manager(root string) *supervise.Manager {
	return &supervise.Manager{
		GOOS: "linux",
		Init: r.init,
		Run: func(_ context.Context, name string, args ...string) (string, error) {
			r.runs = append(r.runs, name+" "+strings.Join(args, " "))
			return "", r.runErr
		},
		// Spawn 被调用就说明真的想派生进程：返回一个假进程号，并让该控制端口
		// 从此“有人应答”，模拟派生出来的管理器已经起来。
		Spawn: func(name string, args []string, _ string) (int, error) {
			r.spawns = append(r.spawns, name+" "+strings.Join(args, " "))
			for i, a := range args {
				if a == "-port" && i+1 < len(args) {
					if port, err := strconv.Atoi(args[i+1]); err == nil {
						delete(r.health, port)
					}
				}
			}
			return 4242, nil
		},
		Write: func(path string, data []byte, _ os.FileMode) error {
			r.written[path] = data
			return nil
		},
		MkdirAll: func(path string, _ os.FileMode) error {
			r.dirs = append(r.dirs, path)
			return nil
		},
		// Get 默认返回“有人在答”；把某个端口放进 health 就能模拟“管理器已经不在”。
		Get: func(url string) error {
			for port, err := range r.health {
				if strings.Contains(url, ":"+strconv.Itoa(port)+"/") {
					return err
				}
			}
			return nil
		},
		Post: func(string) error { return nil },
		Kill: func(int) error { return nil },
		Root: root,
	}
}

// bootPlanFor 生成真实的开机自启方案，只把落点指到临时目录：
// 内容仍由 boot 包生成（因此测的是真内容），位置在测试机可写的目录里。
func bootPlanFor(goos string, init platform.InitSystem, autorunDir, ferc string) func(platform.Info, string) boot.Plan {
	return func(_ platform.Info, root string) boot.Plan {
		return boot.Build(goos, init, boot.Options{
			Root:          root,
			FercPath:      ferc,
			AutorunDir:    autorunDir,
			HasAutorunDir: func(string) bool { return true },
		})
	}
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

// ── restore ─────────────────────────────────────────────────

func TestRestoreEmptyState(t *testing.T) {
	env, out, _ := testEnv(t, linuxInfo(platform.InitSystemd))
	env.DefaultRoot = t.TempDir()
	if code := Run(context.Background(), []string{"restore"}, env); code != 0 {
		t.Fatalf("空状态应返回 0，实际 %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "没有已装载的组件，无需恢复") {
		t.Fatalf("应说明无需恢复\n%s", out.String())
	}
}

func TestRestoreEmptyStateJSON(t *testing.T) {
	env, out, _ := testEnv(t, linuxInfo(platform.InitSystemd))
	env.DefaultRoot = t.TempDir()
	if code := Run(context.Background(), []string{"restore", "--json"}, env); code != 0 {
		t.Fatalf("退出码 = %d", code)
	}
	var rep restoreReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("不是合法 JSON: %v\n%s", err, out.String())
	}
	if rep.Restored != 0 || len(rep.Items) != 0 {
		t.Fatalf("空状态报告应为空: %+v", rep)
	}
}

func TestRestoreSkipsMissingBinary(t *testing.T) {
	root := t.TempDir()
	st := state.New("linux", "amd64", "edge-01", root)
	st.Put(state.Record{Name: "gone", Kind: "service", Version: "v1",
		Path: filepath.Join(root, "bin", "gone"), Supervisor: "systemd", Unit: "ferc-gone"})
	if err := st.Save(state.Path(root)); err != nil {
		t.Fatal(err)
	}

	rec := newRecordManager(platform.InitSystemd)
	env, out, _ := testEnv(t, linuxInfo(platform.InitSystemd))
	env.DefaultRoot = root
	env.Supervisor = func(platform.Info, string) *supervise.Manager { return rec.manager(root) }

	if code := Run(context.Background(), []string{"restore"}, env); code != 0 {
		t.Fatalf("缺二进制应跳过而非失败，实际 %d\n%s", code, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "二进制缺失") {
		t.Fatalf("应说明二进制缺失\n%s", s)
	}
	if !strings.Contains(s, "恢复 0 个组件，1 个跳过") {
		t.Fatalf("汇总应统计跳过数\n%s", s)
	}
	if len(rec.runs) != 0 || len(rec.written) != 0 {
		t.Fatalf("缺失的二进制不该触发任何注册动作: runs=%v written=%v", rec.runs, rec.written)
	}
}

func TestRestoreRegistersNativeService(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	exe := filepath.Join(bin, "simple-web-shell")
	os.WriteFile(exe, []byte("x"), 0o755)
	st := state.New("linux", "amd64", "edge-01", root)
	st.Put(state.Record{Name: "simple-web-shell", Kind: "service", Version: "v1", Path: exe,
		Supervisor: "systemd", Unit: "ferc-simple-web-shell", Ports: []int{8878}})
	st.Save(state.Path(root))

	rec := newRecordManager(platform.InitSystemd)
	env, out, errb := testEnv(t, linuxInfo(platform.InitSystemd))
	env.DefaultRoot = root
	env.Supervisor = func(platform.Info, string) *supervise.Manager { return rec.manager(root) }

	if code := Run(context.Background(), []string{"restore"}, env); code != 0 {
		t.Fatalf("退出码 = %d\n%s%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "恢复 1 个组件，0 个跳过") {
		t.Fatalf("汇总不对\n%s", out.String())
	}
	unit, ok := rec.written[filepath.Join("/etc/systemd/system", "ferc-simple-web-shell.service")]
	if !ok {
		t.Fatalf("应重新写出 systemd 单元，实际写了 %v", rec.written)
	}
	if !strings.Contains(string(unit), "ExecStart="+exe) {
		t.Fatalf("单元里应指向状态文件记录的路径:\n%s", unit)
	}
	if !containsLine(rec.runs, "systemctl enable --now ferc-simple-web-shell") {
		t.Fatalf("应重新启用单元，实际命令 %v", rec.runs)
	}
	if len(rec.spawns) != 0 {
		t.Fatalf("原生 init 下不该派生进程管理器: %v", rec.spawns)
	}
}

func TestRestoreDryRunTouchesNothing(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	exe := filepath.Join(bin, "simple-web-shell")
	os.WriteFile(exe, []byte("x"), 0o755)
	st := state.New("linux", "amd64", "h", root)
	st.Put(state.Record{Name: "simple-web-shell", Kind: "service", Path: exe,
		Supervisor: "systemd", Unit: "ferc-simple-web-shell"})
	st.Save(state.Path(root))

	rec := newRecordManager(platform.InitSystemd)
	env, out, _ := testEnv(t, linuxInfo(platform.InitSystemd))
	env.DefaultRoot = root
	env.Supervisor = func(platform.Info, string) *supervise.Manager { return rec.manager(root) }

	if code := Run(context.Background(), []string{"restore", "--dry-run"}, env); code != 0 {
		t.Fatalf("退出码 = %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "将重新注册") || !strings.Contains(out.String(), "dry-run") {
		t.Fatalf("dry-run 应只打印计划\n%s", out.String())
	}
	if len(rec.runs) != 0 || len(rec.written) != 0 {
		t.Fatalf("dry-run 不该有任何动作: runs=%v written=%v", rec.runs, rec.written)
	}
}

// TestRestoreIdempotent 覆盖重复执行：自托管的管理器还在跑时不能再派生一个，
// 否则第二个实例会因为控制端口被占而启动失败。
func TestRestoreIdempotent(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	exe := filepath.Join(bin, "simple-web-shell")
	os.WriteFile(exe, []byte("x"), 0o755)
	dontcrack := filepath.Join(bin, "dontcrack-manylinux")
	os.WriteFile(dontcrack, []byte("x"), 0o755)

	st := state.New("linux", "amd64", "h", root)
	st.Put(state.Record{Name: "dontcrack", Kind: "supervisor", Path: dontcrack, Supervisor: "self"})
	st.Put(state.Record{Name: "simple-web-shell", Kind: "service", Path: exe,
		Supervisor: "dontcrack", Unit: "ferc-simple-web-shell", ControlPort: 11883, ManagerPID: 999})
	st.Save(state.Path(root))
	before, _ := os.ReadFile(state.Path(root))

	rec := newRecordManager(platform.InitNone)
	env, out, errb := testEnv(t, linuxInfo(platform.InitNone))
	env.DefaultRoot = root
	env.Supervisor = func(_ platform.Info, r string) *supervise.Manager {
		m := rec.manager(r)
		m.DontCrackBin = dontcrack
		return m
	}

	for i := 0; i < 2; i++ {
		out.Reset()
		if code := Run(context.Background(), []string{"restore"}, env); code != 0 {
			t.Fatalf("第 %d 次执行应成功，实际 %d\n%s%s", i+1, code, out.String(), errb.String())
		}
		if !strings.Contains(out.String(), "仍在运行") {
			t.Fatalf("第 %d 次执行应识别出管理器还在运行\n%s", i+1, out.String())
		}
	}
	if len(rec.spawns) != 0 {
		t.Fatalf("管理器还活着时不该再派生一个: %v", rec.spawns)
	}
	after, _ := os.ReadFile(state.Path(root))
	if string(before) != string(after) {
		t.Fatalf("什么都没改变时不该重写状态文件\n前:\n%s\n后:\n%s", before, after)
	}
}

// TestRestoreSelfHostedDerivesManager 覆盖“重启后管理器已不在”的情形：
// 必须重新派生，并把换过的进程号写回状态，否则后续 remove 收拾不干净。
func TestRestoreSelfHostedDerivesManager(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	exe := filepath.Join(bin, "simple-web-shell")
	os.WriteFile(exe, []byte("x"), 0o755)
	dontcrack := filepath.Join(bin, "dontcrack-manylinux")
	os.WriteFile(dontcrack, []byte("x"), 0o755)

	st := state.New("linux", "amd64", "h", root)
	st.Put(state.Record{Name: "dontcrack", Kind: "supervisor", Path: dontcrack, Supervisor: "self"})
	st.Put(state.Record{Name: "simple-web-shell", Kind: "service", Path: exe,
		Supervisor: "dontcrack", Unit: "ferc-simple-web-shell", ControlPort: 11883})
	st.Save(state.Path(root))

	rec := newRecordManager(platform.InitNone)
	rec.health[11883] = os.ErrNotExist // 端口上没人应答
	env, out, errb := testEnv(t, linuxInfo(platform.InitNone))
	env.DefaultRoot = root
	env.Supervisor = func(_ platform.Info, r string) *supervise.Manager {
		m := rec.manager(r)
		m.DontCrackBin = dontcrack
		return m
	}

	if code := Run(context.Background(), []string{"restore"}, env); code != 0 {
		t.Fatalf("退出码 = %d\n%s%s", code, out.String(), errb.String())
	}
	if len(rec.spawns) != 1 {
		t.Fatalf("应重新派生一个管理器，实际 %v", rec.spawns)
	}
	if !strings.Contains(rec.spawns[0], "-port 11883") {
		t.Fatalf("派生时应沿用状态文件里记下的控制端口: %v", rec.spawns[0])
	}
	after, err := state.Load(state.Path(root))
	if err != nil {
		t.Fatal(err)
	}
	got, ok := after.Get("simple-web-shell")
	if !ok {
		t.Fatal("状态文件里应保留该组件")
	}
	if got.ManagerPID != 4242 {
		t.Fatalf("新的管理器进程号应写回状态，实际 %d", got.ManagerPID)
	}
}

func TestRestoreManagerMissingBinaryIsSkipped(t *testing.T) {
	root := t.TempDir()
	st := state.New("linux", "amd64", "h", root)
	st.Put(state.Record{Name: "dontcrack", Kind: "supervisor",
		Path: filepath.Join(root, "bin", "dontcrack-manylinux"), Supervisor: "self"})
	st.Save(state.Path(root))

	rec := newRecordManager(platform.InitNone)
	env, out, _ := testEnv(t, linuxInfo(platform.InitNone))
	env.DefaultRoot = root
	env.Supervisor = func(_ platform.Info, r string) *supervise.Manager { return rec.manager(r) }

	if code := Run(context.Background(), []string{"restore"}, env); code != 0 {
		t.Fatalf("退出码 = %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "进程管理器二进制缺失") {
		t.Fatalf("应说明管理器缺失\n%s", out.String())
	}
}

// TestRestoreAlreadyRegisteredIsNotFailure 覆盖重复执行 restore 的正常情形：
// launchd 的 load -w 对已加载的 plist 会报错，那不该把幂等的恢复说成失败。
func TestRestoreAlreadyRegisteredIsNotFailure(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	exe := filepath.Join(bin, "svc")
	os.WriteFile(exe, []byte("x"), 0o755)
	st := state.New("linux", "amd64", "h", root)
	st.Put(state.Record{Name: "svc", Kind: "service", Path: exe, Supervisor: "launchd", Unit: "com.x.svc"})
	st.Save(state.Path(root))

	rec := newRecordManager(platform.InitLaunchd)
	rec.runErr = errors.New("launchctl: Load failed: service already loaded")
	env, out, errb := testEnv(t, linuxInfo(platform.InitLaunchd))
	env.DefaultRoot = root
	env.Supervisor = func(_ platform.Info, r string) *supervise.Manager { return rec.manager(r) }

	if code := Run(context.Background(), []string{"restore"}, env); code != 0 {
		t.Fatalf("“已加载”不该算失败，实际 %d\n%s%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "托管关系已存在") {
		t.Fatalf("应说明托管关系已存在\n%s", out.String())
	}
}

// TestRestoreRealFailureIsReported 覆盖真的失败：必须返回非零并逐条说清原因。
func TestRestoreRealFailureIsReported(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	exe := filepath.Join(bin, "svc")
	os.WriteFile(exe, []byte("x"), 0o755)
	st := state.New("linux", "amd64", "h", root)
	st.Put(state.Record{Name: "svc", Kind: "service", Path: exe, Supervisor: "systemd", Unit: "ferc-svc"})
	st.Save(state.Path(root))

	rec := newRecordManager(platform.InitSystemd)
	rec.runErr = errors.New("systemctl: Access denied")
	env, out, errb := testEnv(t, linuxInfo(platform.InitSystemd))
	env.DefaultRoot = root
	env.Supervisor = func(_ platform.Info, r string) *supervise.Manager { return rec.manager(r) }

	if code := Run(context.Background(), []string{"restore"}, env); code != 1 {
		t.Fatalf("真失败应返回 1，实际 %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "FAIL") || !strings.Contains(errb.String(), "1 个组件恢复失败") {
		t.Fatalf("应标 FAIL 并汇总失败数\n%s%s", out.String(), errb.String())
	}
}

// ── boot ─────────────────────────────────────────────────────

func TestBootPlanUsesAutorunWhenNoInit(t *testing.T) {
	env, _, _ := testEnv(t, linuxInfo(platform.InitNone))
	plan := env.bootPlan(linuxInfo(platform.InitNone), "/opt/FasterEdgeRunningCore")
	if plan.Mechanism != boot.MechanismAutorun {
		t.Fatalf("无 init 时必须落到 autorun，实际 %s", plan.Mechanism)
	}
	if len(plan.Files) != 1 || plan.Files[0].Path != boot.DefaultAutorunDir+"/"+boot.AutorunFileName {
		t.Fatalf("autorun 落点不对: %+v", plan.Files)
	}
	if plan.Files[0].Mode != boot.FileModeScript {
		t.Fatalf("脚本必须有可执行位，实际 %04o", plan.Files[0].Mode)
	}
}

func TestBootEnableWritesPlanFiles(t *testing.T) {
	root := t.TempDir()
	autorunDir := filepath.Join(t.TempDir(), "autorun")

	env, out, errb := testEnv(t, linuxInfo(platform.InitNone))
	env.DefaultRoot = root
	env.Boot = bootPlanFor("linux", platform.InitNone, autorunDir, "/opt/ferc")
	env.Run = func(context.Context, string, ...string) (string, error) { return "", nil }

	if code := Run(context.Background(), []string{"boot", "enable"}, env); code != 0 {
		t.Fatalf("退出码 = %d\n%s%s", code, out.String(), errb.String())
	}
	path := filepath.Join(autorunDir, boot.AutorunFileName)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("应写出 %s: %v", path, err)
	}
	body := string(b)
	for _, want := range []string{"#!/bin/sh", "/opt/ferc restore", "--root"} {
		if !strings.Contains(body, want) {
			t.Fatalf("脚本内容缺少 %q:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "'"+root+"'") {
		t.Fatalf("自启脚本里的数据根目录应被引用住，避免含空格的路径被拆开:\n%s", body)
	}
	// Windows 的文件权限只有只读位，os.Chmod 无法表达 0755；真正的脚本权限
	// 语义只在 unix 上有意义，因此这条断言只在非 Windows 上生效。
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != boot.FileModeScript {
			t.Fatalf("脚本权限 = %04o，期望 %04o", fi.Mode().Perm(), boot.FileModeScript)
		}
	}
}

func TestBootDisableRemovesFile(t *testing.T) {
	root := t.TempDir()
	autorunDir := filepath.Join(t.TempDir(), "autorun")
	env, out, _ := testEnv(t, linuxInfo(platform.InitNone))
	env.DefaultRoot = root
	env.Boot = bootPlanFor("linux", platform.InitNone, autorunDir, "/opt/ferc")
	env.Run = func(context.Context, string, ...string) (string, error) { return "", nil }

	if code := Run(context.Background(), []string{"boot", "enable"}, env); code != 0 {
		t.Fatalf("enable 退出码 = %d\n%s", code, out.String())
	}
	if code := Run(context.Background(), []string{"boot", "disable"}, env); code != 0 {
		t.Fatalf("disable 退出码 = %d\n%s", code, out.String())
	}
	if _, err := os.Stat(filepath.Join(autorunDir, boot.AutorunFileName)); !os.IsNotExist(err) {
		t.Fatal("disable 后脚本应被删除")
	}
}

func TestBootStatusReportsMissingThenReady(t *testing.T) {
	root := t.TempDir()
	autorunDir := filepath.Join(t.TempDir(), "autorun")
	env, out, _ := testEnv(t, linuxInfo(platform.InitNone))
	env.DefaultRoot = root
	env.Boot = bootPlanFor("linux", platform.InitNone, autorunDir, "/opt/ferc")
	env.Run = func(context.Context, string, ...string) (string, error) { return "", nil }

	if code := Run(context.Background(), []string{"boot", "status"}, env); code != 0 {
		t.Fatalf("退出码 = %d\n%s", code, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "autorun") || !strings.Contains(s, "尚未启用") {
		t.Fatalf("status 应报告机制与未启用\n%s", s)
	}

	Run(context.Background(), []string{"boot", "enable"}, env)
	out.Reset()
	if code := Run(context.Background(), []string{"boot", "status"}, env); code != 0 {
		t.Fatalf("退出码 = %d\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "已启用") || !strings.Contains(out.String(), boot.AutorunFileName) {
		t.Fatalf("enable 之后 status 应报告已启用\n%s", out.String())
	}
}

func TestBootEnableDryRunWritesNothing(t *testing.T) {
	root := t.TempDir()
	autorunDir := filepath.Join(t.TempDir(), "autorun")
	env, out, _ := testEnv(t, linuxInfo(platform.InitNone))
	env.DefaultRoot = root
	env.Boot = bootPlanFor("linux", platform.InitNone, autorunDir, "/opt/ferc")
	env.Run = func(context.Context, string, ...string) (string, error) {
		t.Fatal("dry-run 不该执行外部命令")
		return "", nil
	}
	if code := Run(context.Background(), []string{"boot", "enable", "--dry-run"}, env); code != 0 {
		t.Fatalf("退出码 = %d\n%s", code, out.String())
	}
	if _, err := os.Stat(autorunDir); !os.IsNotExist(err) {
		t.Fatal("dry-run 不该创建目录或文件")
	}
}

func TestBootStatusProbesSchtasksWhenNoFiles(t *testing.T) {
	root := t.TempDir()
	winInfo := platform.Info{GOOS: "windows", GOARCH: "amd64", Arch: "amd64",
		Hostname: "edge-win", Init: platform.InitWindowsSchtasks, Privileged: true}
	env, out, _ := testEnv(t, winInfo)
	env.DefaultRoot = root
	// Windows 的计划任务没有落点文件，只能靠 schtasks /Query 判断是否已注册。
	called := false
	env.Run = func(_ context.Context, name string, args ...string) (string, error) {
		called = true
		if name != "schtasks" || len(args) < 3 || args[0] != "/Query" || args[1] != "/TN" || args[2] != boot.ServiceName {
			t.Fatalf("应该查询计划任务 %s，实际 %s %v", boot.ServiceName, name, args)
		}
		return "", nil
	}
	if code := Run(context.Background(), []string{"boot", "status"}, env); code != 0 {
		t.Fatalf("退出码 = %d\n%s", code, out.String())
	}
	if !called {
		t.Fatal("无落点文件时应查询计划任务状态")
	}
	if !strings.Contains(out.String(), "schtasks") || !strings.Contains(out.String(), "已启用") {
		t.Fatalf("查询成功应判定已启用\n%s", out.String())
	}
}

// ── 装载流程里的自启 ──────────────────────────────────────────

// installFixture 组一份可以真实跑通的装载环境：产物来自内存里的假取回源，
// 因此既能覆盖“装载 → 自启”这条链，又不碰网络、不派生进程。
func installFixture(t *testing.T) (Env, *bytes.Buffer, *bytes.Buffer, *recordManager, string) {
	t.Helper()
	cat, err := catalog.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	comp, ok := cat.Get("any2utf8")
	if !ok {
		t.Fatal("内置清单应含 any2utf8")
	}
	// 选一次性工具：它不注册服务，因此整条流程不会派生任何进程。
	archive := tarGz(t, comp.BinaryName("linux"), "BINARY")

	root := t.TempDir()
	autorunDir := filepath.Join(t.TempDir(), "autorun")
	rec := newRecordManager(platform.InitNone)
	env, out, errb := testEnv(t, linuxInfo(platform.InitNone))
	env.DefaultRoot = root
	env.Source = memSource{asset: comp.AssetName("linux", "amd64"), archive: archive}
	env.Supervisor = func(_ platform.Info, r string) *supervise.Manager { return rec.manager(r) }
	env.Boot = bootPlanFor("linux", platform.InitNone, autorunDir, "/opt/ferc")
	env.Run = func(context.Context, string, ...string) (string, error) { return "", nil }
	return env, out, errb, rec, autorunDir
}

func TestInstallEnablesBootAfterSuccess(t *testing.T) {
	env, out, errb, rec, autorunDir := installFixture(t)
	if code := Run(context.Background(), []string{"install", "any2utf8"}, env); code != 0 {
		t.Fatalf("退出码 = %d\n%s%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "开机自启已启用") {
		t.Fatalf("装载成功后应落盘自启\n%s", out.String())
	}
	b, err := os.ReadFile(filepath.Join(autorunDir, boot.AutorunFileName))
	if err != nil {
		t.Fatalf("应写出 autorun 脚本: %v", err)
	}
	if !strings.Contains(string(b), "restore") {
		t.Fatalf("自启脚本应调用 restore:\n%s", b)
	}
	if len(rec.spawns) != 0 {
		t.Fatalf("一次性工具不该派生任何进程: %v", rec.spawns)
	}
}

func TestInstallNoBootSkipsAutostart(t *testing.T) {
	env, out, errb, _, autorunDir := installFixture(t)
	if code := Run(context.Background(), []string{"install", "any2utf8", "--no-boot"}, env); code != 0 {
		t.Fatalf("退出码 = %d\n%s%s", code, out.String(), errb.String())
	}
	if strings.Contains(out.String(), "开机自启已启用") {
		t.Fatalf("--no-boot 下不该启用自启\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(autorunDir, boot.AutorunFileName)); !os.IsNotExist(err) {
		t.Fatal("--no-boot 下不该写出自启脚本")
	}
}

func TestInstallSurvivesBootFailure(t *testing.T) {
	env, out, errb, _, _ := installFixture(t)
	// 方案本身不可用（没有 init 目录也不允许回退）：装载必须照旧成功。
	env.Boot = func(platform.Info, string) boot.Plan {
		return boot.Plan{Mechanism: boot.MechanismNone}
	}
	if code := Run(context.Background(), []string{"install", "any2utf8"}, env); code != 0 {
		t.Fatalf("自启失败不该拖垮装载，实际 %d\n%s%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "开机自启未启用") {
		t.Fatalf("应把自启失败降级为告警\n%s", out.String())
	}
}

func TestUsageMentionsRestoreAndBoot(t *testing.T) {
	env, out, _ := testEnv(t, linuxInfo(platform.InitSystemd))
	Run(context.Background(), []string{"help"}, env)
	for _, want := range []string{"ferc restore", "ferc boot", "--no-boot"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("用法里应有 %q\n%s", want, out.String())
		}
	}
}
