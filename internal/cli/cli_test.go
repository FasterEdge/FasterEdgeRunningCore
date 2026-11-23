package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/platform"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/state"
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
