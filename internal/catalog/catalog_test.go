package catalog

import (
	"strings"
	"testing"
)

func TestBuiltinLoads(t *testing.T) {
	c, err := Builtin()
	if err != nil {
		t.Fatalf("内嵌清单必须可加载: %v", err)
	}
	if len(c.Components) == 0 {
		t.Fatal("内嵌清单为空")
	}
	if _, ok := c.Get("simple-web-shell"); !ok {
		t.Fatal("清单里应有 simple-web-shell")
	}
}

func TestBuiltinAssetNamesMatchReleases(t *testing.T) {
	// 这些名字直接决定 run 时能不能下载到东西，必须与各仓 release 的命名一致。
	c, err := Builtin()
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	cases := []struct {
		name, goos, arch, want string
	}{
		{"simple-web-shell", "linux", "arm64", "simple-web-shell-linux-arm64.tar.gz"},
		{"simple-web-shell", "windows", "amd64", "simple-web-shell-windows-amd64.zip"},
		{"mqtt-broker-core", "linux", "amd64", "mqtt-broker-core-linux-amd64.tar.gz"},
		{"dontcrack", "linux", "armv7", "dontcrack-manylinux-linux-armv7.tar.gz"},
		{"dontcrack", "windows", "amd64", "dontcrack-windows-windows-amd64.zip"},
		{"netmap", "darwin", "arm64", "netmap-darwin-arm64.tar.gz"},
		{"fasteredge-doctor", "windows", "arm64", "fasteredge-doctor-windows-arm64.zip"},
	}
	for _, tc := range cases {
		comp, ok := c.Get(tc.name)
		if !ok {
			t.Fatalf("缺少组件 %q", tc.name)
		}
		if got := comp.AssetName(tc.goos, tc.arch); got != tc.want {
			t.Errorf("%s/%s-%s 产物名 = %q，期望 %q", tc.name, tc.goos, tc.arch, got, tc.want)
		}
	}
}

func TestBinaryNames(t *testing.T) {
	c, _ := Builtin()
	web, _ := c.Get("simple-web-shell")
	if got := web.BinaryName("windows"); got != "simple-web-shell.exe" {
		t.Errorf("windows 二进制名 = %q", got)
	}
	if got := web.BinaryName("linux"); got != "simple-web-shell" {
		t.Errorf("linux 二进制名 = %q", got)
	}
	dc, _ := c.Get("dontcrack")
	if got := dc.BinaryName("linux"); got != "dontcrack-manylinux" {
		t.Errorf("dontcrack linux 二进制名 = %q", got)
	}
}

func TestEssentials(t *testing.T) {
	c, _ := Builtin()
	ess := c.Essentials()
	if len(ess) == 0 {
		t.Fatal("一键默认必须至少包含一个组件")
	}
	found := false
	for _, n := range ess {
		if n == "simple-web-shell" {
			found = true
		}
	}
	if !found {
		t.Fatal("一键默认必须包含远程终端 simple-web-shell")
	}
}

func TestLoadRejectsBadSchema(t *testing.T) {
	if _, err := Load([]byte(`{"schema_version":99,"components":[]}`)); err == nil {
		t.Fatal("不支持的 schema_version 必须报错")
	}
	if _, err := Load([]byte(`{"schema_version":1,"components":[]}`)); err == nil {
		t.Fatal("空清单必须报错")
	}
	if _, err := Load([]byte(`not json`)); err == nil {
		t.Fatal("非 JSON 必须报错")
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	raw := `{"schema_version":1,"components":[{"name":"a","kind":"tool","unknown_field":1}]}`
	if _, err := Load([]byte(raw)); err == nil {
		t.Fatal("未知字段必须报错，避免清单写错却不生效")
	}
}

func TestLoadRejectsDuplicate(t *testing.T) {
	raw := `{"schema_version":1,"components":[{"name":"a","kind":"tool"},{"name":"a","kind":"tool"}]}`
	if _, err := Load([]byte(raw)); err == nil {
		t.Fatal("重复组件必须报错")
	}
}

func TestLoadRejectsBadKind(t *testing.T) {
	raw := `{"schema_version":1,"components":[{"name":"a","kind":"daemon"}]}`
	if _, err := Load([]byte(raw)); err == nil {
		t.Fatal("非法 kind 必须报错")
	}
}

func TestLoadRejectsMissingDependency(t *testing.T) {
	raw := `{"schema_version":1,"components":[{"name":"a","kind":"tool","requires":["ghost"]}]}`
	if _, err := Load([]byte(raw)); err == nil {
		t.Fatal("依赖不存在的组件必须报错")
	}
}

func TestLoadRejectsCycle(t *testing.T) {
	raw := `{"schema_version":1,"components":[
		{"name":"a","kind":"tool","requires":["b"]},
		{"name":"b","kind":"tool","requires":["a"]}]}`
	_, err := Load([]byte(raw))
	if err == nil {
		t.Fatal("依赖成环必须报错")
	}
	if !strings.Contains(err.Error(), "成环") {
		t.Fatalf("错误信息应指明成环: %v", err)
	}
}

func TestNameFilledFromName(t *testing.T) {
	c, err := Load([]byte(`{"schema_version":1,"components":[{"name":"a","kind":"tool"}]}`))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	comp, _ := c.Get("a")
	if comp.Display != "a" {
		t.Errorf("Display 应回退为 name，实际 %q", comp.Display)
	}
}

func TestSupportsArch(t *testing.T) {
	comp := Component{Archs: []string{"amd64"}}
	if !comp.SupportsArch("amd64") {
		t.Fatal("amd64 应在支持列表内")
	}
	if comp.SupportsArch("arm64") {
		t.Fatal("arm64 不在支持列表内")
	}
	all := Component{}
	if !all.SupportsArch("riscv64") {
		t.Fatal("空列表应视为全架构")
	}
}

func TestArgs(t *testing.T) {
	comp := Component{
		DefaultArgs: []string{"-a"},
		ArgsByOS:    map[string][]string{"linux": {"-b"}},
	}
	got := comp.Args("linux")
	if len(got) != 2 || got[0] != "-a" || got[1] != "-b" {
		t.Fatalf("Args(linux) = %v", got)
	}
	if got := comp.Args("windows"); len(got) != 1 || got[0] != "-a" {
		t.Fatalf("Args(windows) = %v", got)
	}
}

func TestTarget(t *testing.T) {
	if got := Target("linux", "arm64"); got != "linux-arm64" {
		t.Fatalf("Target = %q", got)
	}
}

// TestArchsMatchPublishedReleases 把清单里的 archs 钉在“各仓真的发布了什么”上。
// 运行核拿 archs 决定要不要去下载：写多了会让用户以为能装、结果拿到 404，
// 写少了会让能装的环境被跳过，所以两者都必须来自各仓已发布产物的实况。
func TestArchsMatchPublishedReleases(t *testing.T) {
	c, err := Builtin()
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	// 已核实的发布情况：除 dontcrack 外的组件只发 amd64/arm64。
	want := map[string][]string{
		"dontcrack": {"386", "amd64", "arm64", "armv7"},
	}
	allowed := map[string]bool{"386": true, "amd64": true, "arm64": true, "armv7": true}
	for i := range c.Components {
		comp := &c.Components[i]
		archs, ok := want[comp.Name]
		if !ok {
			archs = []string{"amd64", "arm64"}
		}
		if len(comp.Archs) != len(archs) {
			t.Errorf("%s 的 archs = %v，期望 %v（须与已发布产物一致）", comp.Name, comp.Archs, archs)
			continue
		}
		for j, a := range archs {
			if comp.Archs[j] != a {
				t.Errorf("%s 的 archs[%d] = %q，期望 %q", comp.Name, j, comp.Archs[j], a)
			}
		}
		for _, a := range comp.Archs {
			if !allowed[a] {
				t.Errorf("%s 声明了未发布的架构 %q，会让用户以为能装却拿到 404", comp.Name, a)
			}
		}
	}
}
