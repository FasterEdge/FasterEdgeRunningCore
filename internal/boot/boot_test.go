package boot

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/platform"
)

const (
	plainFerc = "/opt/FasterEdgeRunningCore/ferc"
	plainRoot = "/opt/FasterEdgeRunningCore"
)

func baseOpt() Options {
	return Options{
		Root:          plainRoot,
		FercPath:      plainFerc,
		AutorunDir:    DefaultAutorunDir,
		HasAutorunDir: func(string) bool { return true },
	}
}

func (p Plan) file(t *testing.T, path string) File {
	t.Helper()
	for _, f := range p.Files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("方案里没有文件 %s（实际：%v）", path, paths(p.Files))
	return File{}
}

func paths(files []File) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Path)
	}
	return out
}

func (p Plan) commands() []string {
	out := make([]string, 0, len(p.Commands))
	for _, c := range p.Commands {
		out = append(out, c.Name+" "+strings.Join(c.Args, " "))
	}
	return out
}

func (p Plan) removeCommands() []string {
	out := make([]string, 0, len(p.RemoveCmd))
	for _, c := range p.RemoveCmd {
		out = append(out, c.Name+" "+strings.Join(c.Args, " "))
	}
	return out
}

func mustContain(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("%s 里应包含 %q，实际:\n%s", what, needle, haystack)
	}
}

func mustNotContain(t *testing.T, haystack, needle, what string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Fatalf("%s 里不应包含 %q，实际:\n%s", what, needle, haystack)
	}
}

// ── 落点选择 ─────────────────────────────────────────────────

func TestChooseMechanism(t *testing.T) {
	cases := []struct {
		name string
		goos string
		init platform.InitSystem
		opt  func(*Options)
		want Mechanism
	}{
		{"windows 走计划任务", "windows", platform.InitWindowsSchtasks, nil, MechanismSchtasks},
		{"darwin 走 launchd", "darwin", platform.InitLaunchd, nil, MechanismLaunchd},
		{"systemd", "linux", platform.InitSystemd, nil, MechanismSystemd},
		{"openrc", "linux", platform.InitOpenRC, nil, MechanismOpenRC},
		{"sysvinit", "linux", platform.InitSysV, nil, MechanismSysV},
		{"无 init 但 autorun 目录存在", "linux", platform.InitNone, nil, MechanismAutorun},
		{"无 init 且 autorun 目录不存在", "linux", platform.InitNone, func(o *Options) {
			o.HasAutorunDir = func(string) bool { return false }
		}, MechanismNone},
		{"无 init、目录不存在但允许回退", "linux", platform.InitNone, func(o *Options) {
			o.HasAutorunDir = func(string) bool { return false }
			o.AllowAutorunFallback = true
		}, MechanismAutorun},
		{"无 init 且无法探测", "linux", platform.InitNone, func(o *Options) {
			o.HasAutorunDir = nil
		}, MechanismAutorun},
		{"未知 init 但有 autorun 目录", "linux", "upstart", nil, MechanismAutorun},
		{"未知 goos 不做假设", "freebsd", platform.InitNone, nil, MechanismNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opt := baseOpt()
			if c.opt != nil {
				c.opt(&opt)
			}
			got := Build(c.goos, c.init, opt)
			if got.Mechanism != c.want {
				t.Fatalf("Mechanism = %q，期望 %q", got.Mechanism, c.want)
			}
			// 除了 none，任何方案都必须留下“怎么装”或“写什么文件”之一。
			if c.want != MechanismNone && len(got.Files) == 0 && len(got.Commands) == 0 {
				t.Fatal("方案既没有文件也没有命令，等于什么都没做")
			}
			// 每个方案都要能撤销。
			if c.want != MechanismNone && len(got.Remove) == 0 && len(got.RemoveCmd) == 0 {
				t.Fatal("方案没有撤销方式")
			}
		})
	}
}

func TestHasAutorunDirProbesGivenDir(t *testing.T) {
	var probed []string
	opt := Options{
		Root:       plainRoot,
		FercPath:   plainFerc,
		AutorunDir: "/custom/autorun",
		HasAutorunDir: func(d string) bool {
			probed = append(probed, d)
			return false
		},
	}
	p := Build("linux", platform.InitNone, opt)
	if p.Mechanism != MechanismNone {
		t.Fatalf("探测到目录不存在且未允许回退，应返回 none，实际 %q", p.Mechanism)
	}
	if len(probed) != 1 || probed[0] != "/custom/autorun" {
		t.Fatalf("应按 Options.AutorunDir 探测，实际探测 %v", probed)
	}
}

// ── systemd ─────────────────────────────────────────────────

func TestSystemdUnit(t *testing.T) {
	p := Build("linux", platform.InitSystemd, baseOpt())
	if p.Mechanism != MechanismSystemd {
		t.Fatalf("Mechanism = %q", p.Mechanism)
	}
	f := p.file(t, SystemdUnitPath)
	if f.Mode != 0o644 {
		t.Fatalf("权限 = %o，期望 644", f.Mode)
	}
	body := string(f.Body)
	mustContain(t, body, "Type=oneshot", "unit")
	mustContain(t, body, "After=network-online.target", "unit")
	mustContain(t, body, "ExecStart="+plainFerc+" restore --root "+plainRoot+"\n", "unit")
	mustContain(t, body, "[Install]", "unit")
	mustContain(t, body, "WantedBy=multi-user.target", "unit")
	mustNotContain(t, body, "Restart=always", "unit")

	want := []string{"systemctl daemon-reload", "systemctl enable ferc-restore.service"}
	if got := p.commands(); !equal(got, want) {
		t.Fatalf("命令 = %v，期望 %v", got, want)
	}
	if !contains(p.Remove, SystemdUnitPath) {
		t.Fatalf("撤销时应删除 %s", SystemdUnitPath)
	}
}

// ── openrc ─────────────────────────────────────────────────

func TestOpenRCScript(t *testing.T) {
	p := Build("linux", platform.InitOpenRC, baseOpt())
	f := p.file(t, InitdScriptPath)
	if f.Mode != 0o755 {
		t.Fatalf("权限 = %o，期望 755", f.Mode)
	}
	body := string(f.Body)
	mustContain(t, body, "#!/sbin/openrc-run", "openrc 脚本")
	mustContain(t, body, "command="+plainFerc, "openrc 脚本")
	mustContain(t, body, "depend() {\n\tneed net\n}", "openrc 脚本")
	mustContain(t, body, "restore --root "+plainRoot, "openrc 脚本")
	mustContain(t, body, "start() {", "openrc 脚本")
	mustContain(t, body, "stop() {", "openrc 脚本")

	want := []string{"rc-update add ferc-restore default"}
	if got := p.commands(); !equal(got, want) {
		t.Fatalf("命令 = %v，期望 %v", got, want)
	}
}

// ── sysvinit ───────────────────────────────────────────────

func TestSysVScript(t *testing.T) {
	p := Build("linux", platform.InitSysV, baseOpt())
	f := p.file(t, InitdScriptPath)
	if f.Mode != 0o755 {
		t.Fatalf("权限 = %o，期望 755", f.Mode)
	}
	body := string(f.Body)
	for _, want := range []string{
		"#!/bin/sh",
		"### BEGIN INIT INFO",
		"# Provides:          ferc-restore",
		"# Default-Start:     2 3 4 5",
		"### END INIT INFO",
		"case \"$1\" in",
		"  start)",
		"  stop)",
		"esac",
	} {
		mustContain(t, body, want, "sysvinit 脚本")
	}
	if !strings.Contains(body, plainFerc+" restore --root "+plainRoot) {
		t.Fatalf("脚本没有调用 restore:\n%s", body)
	}

	want := []string{"update-rc.d ferc-restore defaults"}
	if got := p.commands(); !equal(got, want) {
		t.Fatalf("命令 = %v，期望 %v", got, want)
	}
}

// ── launchd ────────────────────────────────────────────────

func TestLaunchdPlist(t *testing.T) {
	p := Build("darwin", platform.InitLaunchd, baseOpt())
	f := p.file(t, LaunchdPlistPath)
	if f.Mode != 0o644 {
		t.Fatalf("权限 = %o，期望 644", f.Mode)
	}
	body := string(f.Body)
	for _, want := range []string{
		`<plist version="1.0">`,
		"<key>Label</key><string>com.fasteredge.ferc-restore</string>",
		"<key>RunAtLoad</key><true/>",
		"<string>" + plainFerc + "</string>",
		"<string>--root</string>",
	} {
		mustContain(t, body, want, "plist")
	}
	want := []string{"launchctl load -w " + LaunchdPlistPath}
	if got := p.commands(); !equal(got, want) {
		t.Fatalf("命令 = %v，期望 %v", got, want)
	}
}

// ── schtasks ───────────────────────────────────────────────

func TestSchtasks(t *testing.T) {
	p := Build("windows", platform.InitWindowsSchtasks, Options{
		Root:     `C:\ProgramData\FasterEdgeRunningCore`,
		FercPath: `C:\Program Files\ferc\ferc.exe`,
	})
	if len(p.Files) != 0 {
		t.Fatalf("schtasks 方案不应写文件，实际 %v", paths(p.Files))
	}
	want := []string{
		`schtasks /Create /F /TN ferc-restore /SC ONSTART /RU SYSTEM /TR "C:\Program Files\ferc\ferc.exe" restore --root C:\ProgramData\FasterEdgeRunningCore`,
	}
	if got := p.commands(); !equal(got, want) {
		t.Fatalf("命令 =\n%v\n期望\n%v", got, want)
	}
	if len(p.RemoveCmd) != 1 || !strings.HasPrefix(p.RemoveCmd[0].Name, "schtasks") {
		t.Fatalf("撤销命令 = %v", p.removeCommands())
	}
	if len(p.Warnings) == 0 {
		t.Fatal("schtasks 只自启不拉起，应给出警告")
	}
}

// ── autorun（关键路径） ────────────────────────────────────

func TestAutorunScript(t *testing.T) {
	p := Build("linux", platform.InitNone, baseOpt())
	if p.Mechanism != MechanismAutorun {
		t.Fatalf("Mechanism = %q", p.Mechanism)
	}
	path := "/etc/autorun/" + AutorunFileName
	f := p.file(t, path)
	if f.Mode != 0o755 {
		t.Fatalf("权限 = %o，期望 755（BusyBox init 会跳过不可执行脚本）", f.Mode)
	}
	body := string(f.Body)
	mustContain(t, body, "#!/bin/sh", "autorun 脚本")
	mustContain(t, body, plainFerc+" restore --root "+plainRoot, "autorun 脚本")
	if len(p.Commands) != 0 {
		t.Fatalf("autorun 不需要注册命令，实际 %v", p.commands())
	}
	if !contains(p.Remove, path) {
		t.Fatalf("撤销时应删除 %s", path)
	}
}

// TestAutorunRunsAfterNetwork 钉住最关键的一条：restore 必须在 network 之后。
// BusyBox init 按文件名字符串序执行，顺序错了就会在网络没起来时探端口、注册服务。
func TestAutorunRunsAfterNetwork(t *testing.T) {
	if !(AutorunFileName > NetworkAutorunFileName) {
		t.Fatalf("%q 必须排在 %q 之后（字符串序）", AutorunFileName, NetworkAutorunFileName)
	}
	// 再确认前缀也真的更大，避免有人改了常量却没改排序假设。
	if AutorunFileName[:2] < NetworkAutorunFileName[:2] {
		t.Fatalf("前缀顺序不对：%q vs %q", AutorunFileName, NetworkAutorunFileName)
	}

	opt := baseOpt()
	opt.AutorunDir = t.TempDir()
	p := Build("linux", platform.InitNone, opt)
	if len(p.Files) != 1 {
		t.Fatalf("应写出一个脚本，实际 %v", paths(p.Files))
	}
	name := filepath.Base(p.Files[0].Path)
	if !(name > NetworkAutorunFileName) {
		t.Fatalf("实际脚本名 %q 未排在 %q 之后", name, NetworkAutorunFileName)
	}
}

func TestAutorunDirOverride(t *testing.T) {
	dir := t.TempDir()
	opt := baseOpt()
	opt.AutorunDir = dir + "/" // 尾部斜杠不应产生双斜杠
	p := Build("linux", platform.InitNone, opt)
	if want := dir + "/" + AutorunFileName; p.Files[0].Path != want {
		t.Fatalf("Path = %q，期望 %q", p.Files[0].Path, want)
	}

	empty := baseOpt()
	empty.AutorunDir = ""
	p = Build("linux", platform.InitNone, empty)
	if want := DefaultAutorunDir + "/" + AutorunFileName; p.Files[0].Path != want {
		t.Fatalf("AutorunDir 为空时应退回默认目录，实际 %q", p.Files[0].Path)
	}
}

// ── none ───────────────────────────────────────────────────

func TestNonePlanWarnsAboutManualRestore(t *testing.T) {
	opt := baseOpt()
	opt.HasAutorunDir = func(string) bool { return false }
	p := Build("linux", platform.InitNone, opt)
	if p.Mechanism != MechanismNone {
		t.Fatalf("Mechanism = %q", p.Mechanism)
	}
	if len(p.Files) != 0 || len(p.Commands) != 0 {
		t.Fatal("none 方案不应写文件或执行命令")
	}
	joined := strings.Join(p.Warnings, "；")
	mustContain(t, joined, "没有可用的开机自启机制", "警告")
	mustContain(t, joined, "ferc restore", "警告")
}

// ── 幂等 ───────────────────────────────────────────────────

func TestBuildIsIdempotent(t *testing.T) {
	opt := Options{
		Root:          "/opt/with space/root",
		FercPath:      "/opt/with space/ferc",
		AutorunDir:    DefaultAutorunDir,
		HasAutorunDir: func(string) bool { return true },
	}
	for _, c := range []struct {
		goos string
		init platform.InitSystem
	}{
		{"linux", platform.InitSystemd},
		{"linux", platform.InitOpenRC},
		{"linux", platform.InitSysV},
		{"darwin", platform.InitLaunchd},
		{"windows", platform.InitWindowsSchtasks},
		{"linux", platform.InitNone},
	} {
		a := Build(c.goos, c.init, opt)
		b := Build(c.goos, c.init, opt)
		if a.Mechanism != b.Mechanism {
			t.Fatalf("%s/%s: 两次机制不同", c.goos, c.init)
		}
		if len(a.Files) != len(b.Files) {
			t.Fatalf("%s/%s: 两次文件数不同", c.goos, c.init)
		}
		for i := range a.Files {
			if a.Files[i].Path != b.Files[i].Path || a.Files[i].Mode != b.Files[i].Mode ||
				string(a.Files[i].Body) != string(b.Files[i].Body) {
				t.Fatalf("%s/%s: 第 %d 个文件两次不一致", c.goos, c.init, i)
			}
		}
		if strings.Join(a.commands(), "|") != strings.Join(b.commands(), "|") {
			t.Fatalf("%s/%s: 两次命令不一致", c.goos, c.init)
		}
		if strings.Join(a.removeCommands(), "|") != strings.Join(b.removeCommands(), "|") {
			t.Fatalf("%s/%s: 两次撤销命令不一致", c.goos, c.init)
		}
		if strings.Join(a.Remove, "|") != strings.Join(b.Remove, "|") {
			t.Fatalf("%s/%s: 两次撤销列表不一致", c.goos, c.init)
		}
		if strings.Join(a.Warnings, "|") != strings.Join(b.Warnings, "|") {
			t.Fatalf("%s/%s: 两次警告不一致", c.goos, c.init)
		}
	}
}

// ── 引用与转义 ──────────────────────────────────────────────

func spaceOpt() Options {
	return Options{
		Root:          "/opt/with space/it's here",
		FercPath:      "/opt/with space/ferc",
		AutorunDir:    DefaultAutorunDir,
		HasAutorunDir: func(string) bool { return true },
	}
}

func TestShaQuote(t *testing.T) {
	cases := map[string]string{
		"plain":         "plain",
		"":              "''",
		"a b":           "'a b'",
		"it's":          `'it'\''s'`,
		"$HOME":         "'$HOME'",
		"a;b":           "'a;b'",
		"`x`":           "'`x`'",
		`back\slash`:    `'back\slash'`,
		"paren(1)":      "'paren(1)'",
		"-key=value":    "-key=value",
		"/opt/root/bin": "/opt/root/bin",
	}
	for in, want := range cases {
		if got := shQuote(in); got != want {
			t.Fatalf("shQuote(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestQuotingWithSpaces(t *testing.T) {
	opt := spaceOpt()

	sd := Build("linux", platform.InitSystemd, opt).Files[0]
	mustContain(t, string(sd.Body),
		`ExecStart="/opt/with space/ferc" restore --root "/opt/with space/it's here"`, "systemd ExecStart")

	if got := systemdQuote(`a"b\c%d`); got != `"a\"b\\c%%d"` {
		t.Fatalf("systemdQuote 转义不对: %q", got)
	}

	orc := Build("linux", platform.InitOpenRC, opt).Files[0]
	mustContain(t, string(orc.Body),
		`'/opt/with space/ferc' restore --root '/opt/with space/it'\''s here'`, "openrc 脚本")

	sv := Build("linux", platform.InitSysV, opt).Files[0]
	mustContain(t, string(sv.Body),
		`'/opt/with space/ferc' restore --root '/opt/with space/it'\''s here'`, "sysvinit 脚本")

	ar := Build("linux", platform.InitNone, opt).Files[0]
	mustContain(t, string(ar.Body),
		`'/opt/with space/ferc' restore --root '/opt/with space/it'\''s here'`, "autorun 脚本")

	la := Build("darwin", platform.InitLaunchd, Options{
		Root:     `/opt/a & b`,
		FercPath: `/opt/x<y>/ferc`,
	}).Files[0]
	body := string(la.Body)
	mustContain(t, body, "<string>/opt/x&lt;y&gt;/ferc</string>", "plist 参数")
	mustContain(t, body, "<string>/opt/a &amp; b</string>", "plist 参数")

	win := Build("windows", platform.InitWindowsSchtasks, Options{
		Root:     `C:\a b`,
		FercPath: `C:\Program Files\ferc\ferc.exe`,
	}).Commands[0]
	want := `"C:\Program Files\ferc\ferc.exe" restore --root "C:\a b"`
	if got := win.Args[len(win.Args)-1]; got != want {
		t.Fatalf("schtasks /TR = %q，期望 %q", got, want)
	}

	// 结尾反斜杠 + 空格：不能吞掉闭合引号。
	if got := winQuote(`C:\dir with space\`); got != `"C:\dir with space\\"` {
		t.Fatalf("winQuote 结尾反斜杠处理不对: %q", got)
	}
}

func TestXMLBodiesAreParseableShape(t *testing.T) {
	body := string(Build("darwin", platform.InitLaunchd, spaceOpt()).Files[0].Body)
	if strings.Count(body, "<dict>") != 1 || strings.Count(body, "</dict>") != 1 {
		t.Fatalf("plist 结构不平衡:\n%s", body)
	}
	if strings.Count(body, "<string>") != strings.Count(body, "</string>") {
		t.Fatalf("plist 的 string 标签不平衡:\n%s", body)
	}
}

// ── 脚本真的能被 sh 执行（有 sh 时） ────────────────────────

func requireSh(t *testing.T) string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("本机没有 sh，跳过脚本执行测试")
	}
	return sh
}

func TestScriptsAreValidShellSyntax(t *testing.T) {
	sh := requireSh(t)
	for _, c := range []struct {
		goos string
		init platform.InitSystem
	}{
		{"linux", platform.InitOpenRC},
		{"linux", platform.InitSysV},
		{"linux", platform.InitNone},
	} {
		for _, f := range Build(c.goos, c.init, spaceOpt()).Files {
			dir := t.TempDir()
			path := filepath.Join(dir, filepath.Base(f.Path))
			if err := os.WriteFile(path, f.Body, f.Mode); err != nil {
				t.Fatalf("写临时脚本失败: %v", err)
			}
			// openrc 脚本以 #!/sbin/openrc-run 开头，sh -n 只做语法检查，不执行。
			if out, err := exec.Command(sh, "-n", path).CombinedOutput(); err != nil {
				t.Fatalf("%s 语法错误: %v\n%s\n%s", f.Path, err, out, f.Body)
			}
		}
	}
}

// TestAutorunPassesArgumentsIntact 用假的 ferc 端到端验证引用：路径含空格与
// 单引号时，脚本仍应把 "--root <root>" 原样交给 ferc，而不是拆成两段。
func TestAutorunPassesArgumentsIntact(t *testing.T) {
	sh := requireSh(t)
	dir := t.TempDir()
	fercDir := filepath.Join(dir, "with space")
	if err := os.MkdirAll(fercDir, 0o755); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "argv.txt")
	fercPath := filepath.Join(fercDir, "ferc")
	fake := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shQuote(record) + "\n"
	if err := os.WriteFile(fercPath, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(dir, "data root")
	opt := Options{
		Root:          root,
		FercPath:      fercPath,
		AutorunDir:    filepath.Join(dir, "autorun"),
		HasAutorunDir: func(string) bool { return true },
	}
	p := Build("linux", platform.InitNone, opt)
	script := filepath.Join(dir, "30_ferc-restore.sh")
	if err := os.WriteFile(script, p.Files[0].Body, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(sh, script).CombinedOutput(); err != nil {
		t.Fatalf("执行 autorun 脚本失败: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("假 ferc 没被调用: %v", err)
	}
	got := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	want := []string{"restore", "--root", root}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("ferc 收到参数 %q，期望 %q", got, want)
	}
}

// ── 小工具 ──────────────────────────────────────────────────

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
