package resolve

import (
	"strings"
	"testing"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/catalog"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/platform"
)

func builtin(t *testing.T) *catalog.Catalog {
	t.Helper()
	c, err := catalog.Builtin()
	if err != nil {
		t.Fatalf("加载内嵌清单失败: %v", err)
	}
	return c
}

func TestResolveEssentialsDefaults(t *testing.T) {
	plan, err := Resolve(builtin(t), Request{Info: platform.Info{
		GOOS: "linux", GOARCH: "arm64", Arch: "arm64", Init: platform.InitSystemd,
	}})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(plan.Steps) == 0 {
		t.Fatal("一键默认不能是空计划")
	}
	if plan.Supervisor != SupervisorNative {
		t.Fatalf("systemd 下不应装载自带 supervisor，实际 %q", plan.Supervisor)
	}
	names := plan.Names()
	if !contains(names, "simple-web-shell") {
		t.Fatalf("一键默认必须含远程终端，实际 %v", names)
	}
}

func TestResolveInstallsSupervisorWhenNoInit(t *testing.T) {
	plan, err := Resolve(builtin(t), Request{
		Want: []string{"simple-web-shell"},
		Info: platform.Info{GOOS: "linux", GOARCH: "amd64", Arch: "amd64", Init: platform.InitNone},
	})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if plan.Supervisor != SupervisorInstall {
		t.Fatalf("无 init 时必须装载 supervisor，实际 %q", plan.Supervisor)
	}
	if len(plan.Steps) < 2 {
		t.Fatalf("计划应含 supervisor + 终端，实际 %v", plan.Names())
	}
	if plan.Steps[0].Component.Kind != catalog.KindSupervisor {
		t.Fatalf("supervisor 必须排在最前，实际首步 %q", plan.Steps[0].Component.Name)
	}
	if !strings.Contains(plan.Steps[0].Reason, "进程保活") {
		t.Fatalf("supervisor 步骤应说明原因，实际 %q", plan.Steps[0].Reason)
	}
}

func TestResolveNoSupervisorForToolOnly(t *testing.T) {
	plan, err := Resolve(builtin(t), Request{
		Want: []string{"any2utf8"},
		Info: platform.Info{GOOS: "linux", GOARCH: "amd64", Arch: "amd64", Init: platform.InitNone},
	})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if plan.Supervisor != SupervisorNative {
		t.Fatalf("只装一次性工具时不该引入进程管理器，实际 %q", plan.Supervisor)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("应只有一步，实际 %v", plan.Names())
	}
}

func TestResolveConflictIsHardError(t *testing.T) {
	_, err := Resolve(builtin(t), Request{
		Want: []string{"netmap", "fasteredge2api"},
		Info: platform.Info{GOOS: "linux", GOARCH: "amd64", Arch: "amd64", Init: platform.InitSystemd},
	})
	if err == nil {
		t.Fatal("端口冲突默认必须报错")
	}
	if !strings.Contains(err.Error(), "冲突") {
		t.Fatalf("错误信息应说明冲突: %v", err)
	}
}

func TestResolveConflictDowngraded(t *testing.T) {
	plan, err := Resolve(builtin(t), Request{
		Want:           []string{"netmap", "fasteredge2api"},
		Info:           platform.Info{GOOS: "linux", GOARCH: "amd64", Arch: "amd64", Init: platform.InitSystemd},
		AllowConflicts: true,
	})
	if err != nil {
		t.Fatalf("允许冲突时不应报错: %v", err)
	}
	if len(plan.Warnings) == 0 {
		t.Fatal("降级后必须留下告警，不能静默放过")
	}
}

func TestResolveUnknownComponent(t *testing.T) {
	_, err := Resolve(builtin(t), Request{
		Want: []string{"no-such-thing"},
		Info: platform.Info{GOOS: "linux", GOARCH: "amd64", Arch: "amd64", Init: platform.InitSystemd},
	})
	if err == nil {
		t.Fatal("未知组件必须报错")
	}
}

func TestResolveUnsupportedArchIsWarning(t *testing.T) {
	plan, err := Resolve(builtin(t), Request{
		Want: []string{"any2utf8"},
		Info: platform.Info{GOOS: "linux", GOARCH: "riscv64", Arch: "riscv64", Init: platform.InitSystemd},
	})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(plan.Steps) != 0 {
		t.Fatalf("不支持的架构不该产生装载步骤，实际 %v", plan.Names())
	}
	if len(plan.Warnings) == 0 {
		t.Fatal("跳过必须留下告警")
	}
}

func TestResolveSkipsAlreadyInstalled(t *testing.T) {
	plan, err := Resolve(builtin(t), Request{
		Want:      []string{"simple-web-shell"},
		Info:      platform.Info{GOOS: "linux", GOARCH: "amd64", Arch: "amd64", Init: platform.InitSystemd},
		Installed: map[string]bool{"simple-web-shell": true},
	})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(plan.Steps) != 0 {
		t.Fatalf("已装载的组件不该重复出现在计划里，实际 %v", plan.Names())
	}
}

func TestResolveAll(t *testing.T) {
	cat := builtin(t)
	plan, err := Resolve(cat, Request{
		All:  true,
		Info: platform.Info{GOOS: "linux", GOARCH: "arm64", Arch: "arm64", Init: platform.InitSystemd},
	})
	if err != nil {
		// 全量装载默认会撞上同端口冲突，这正是我们希望暴露的行为。
		if !strings.Contains(err.Error(), "冲突") {
			t.Fatalf("全量的失败原因应是冲突: %v", err)
		}
		return
	}
	if len(plan.Steps) != len(cat.Components) {
		t.Fatalf("全量计划步数 = %d，组件数 = %d", len(plan.Steps), len(cat.Components))
	}
}

func TestResolveRequiresGOOS(t *testing.T) {
	if _, err := Resolve(builtin(t), Request{}); err == nil {
		t.Fatal("缺少 GOOS 必须报错")
	}
}

func TestResolveDependencyOrder(t *testing.T) {
	raw := `{"schema_version":1,"components":[
		{"name":"app","kind":"service","requires":["lib"],"ports":[9000],"repos":{"default":"FasterEdge/App"}},
		{"name":"lib","kind":"service","ports":[9001],"repos":{"default":"FasterEdge/Lib"}}]}`
	cat, err := catalog.Load([]byte(raw))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	plan, err := Resolve(cat, Request{
		Want: []string{"app"},
		Info: platform.Info{GOOS: "linux", GOARCH: "amd64", Arch: "amd64", Init: platform.InitSystemd},
	})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := plan.Names(); len(got) != 2 || got[0] != "lib" || got[1] != "app" {
		t.Fatalf("依赖必须排在被依赖者之前，实际 %v", got)
	}
}

func TestResolveMissingSupervisor(t *testing.T) {
	raw := `{"schema_version":1,"components":[
		{"name":"app","kind":"service","ports":[9000]}]}`
	cat, err := catalog.Load([]byte(raw))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	_, err = Resolve(cat, Request{
		Want: []string{"app"},
		Info: platform.Info{GOOS: "linux", GOARCH: "amd64", Arch: "amd64", Init: platform.InitNone},
	})
	if err == nil {
		t.Fatal("需要 supervisor 但清单里没有时必须报错，而不是悄无声息地不保活")
	}
}

func TestResolveNoDuplicateSupervisor(t *testing.T) {
	// 用户显式点名 supervisor，且系统又不具备保活能力时，它只能出现一次。
	plan, err := Resolve(builtin(t), Request{
		Want: []string{"dontcrack", "simple-web-shell"},
		Info: platform.Info{GOOS: "windows", GOARCH: "amd64", Arch: "amd64", Init: platform.InitWindowsSchtasks},
	})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	count := 0
	for _, s := range plan.Steps {
		if s.Component.Kind == catalog.KindSupervisor {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("supervisor 应只出现一次，实际 %d 次: %v", count, plan.Names())
	}
	if plan.Steps[0].Component.Name != "dontcrack" {
		t.Fatalf("supervisor 应排在最前，实际 %v", plan.Names())
	}
}

func TestResolveAutoSecretForRequiredArg(t *testing.T) {
	// SimpleWebShell 没有 -key 会直接退出；resolve 不该让它变硬错误，
	// 而是标记“这个参数需要运行核生成”。
	plan, err := Resolve(builtin(t), Request{
		Want: []string{"simple-web-shell"},
		Info: platform.Info{GOOS: "linux", GOARCH: "amd64", Arch: "amd64", Init: platform.InitSystemd},
	})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("应有 1 步，实际 %v", plan.Names())
	}
	if _, ok := plan.Steps[0].Secrets["-key"]; !ok {
		t.Fatalf("应标记需要生成 -key，实际 %v", plan.Steps[0].Secrets)
	}
}

func TestResolveHardRequiredArgIsError(t *testing.T) {
	raw := `{"schema_version":1,"components":[
		{"name":"app","kind":"service","repos":{"default":"o/r"},"required_args":["-token"]}]}`
	cat, err := catalog.Load([]byte(raw))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	plan, err := Resolve(cat, Request{
		Want: []string{"app"},
		Info: platform.Info{GOOS: "linux", GOARCH: "amd64", Arch: "amd64", Init: platform.InitSystemd},
	})
	if err == nil {
		t.Fatal("缺必填参数且无法自动生成时，必须报错而不是装上一个跑不起来的服务")
	}
	if plan == nil || len(plan.Errors) == 0 {
		t.Fatalf("计划里应记录硬错误，实际 %+v", plan)
	}
}

func TestContainsName(t *testing.T) {
	if !containsName([]string{"a", "b"}, "b") {
		t.Fatal("b 应在列表中")
	}
	if containsName([]string{"a"}, "c") {
		t.Fatal("c 不在列表中")
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
