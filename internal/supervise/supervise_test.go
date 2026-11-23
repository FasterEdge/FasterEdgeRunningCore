package supervise

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/platform"
)

type fakeRun struct {
	calls []string
	err   map[string]error
}

func (f *fakeRun) exec(_ context.Context, name string, args ...string) (string, error) {
	line := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, line)
	if e, ok := f.err[line]; ok {
		return "", e
	}
	for k, e := range f.err {
		if strings.HasPrefix(line, k) {
			return "", e
		}
	}
	return "", nil
}

func newManager(t *testing.T, init platform.InitSystem, dir string) (*Manager, *fakeRun) {
	t.Helper()
	run := &fakeRun{err: map[string]error{}}
	m := &Manager{
		GOOS: "linux",
		Init: init,
		Run:  run.exec,
		Write: func(p string, b []byte, perm os.FileMode) error {
			return os.WriteFile(filepath.Join(dir, filepath.Base(p)), b, perm)
		},
		MkdirAll: func(p string, perm os.FileMode) error { return os.MkdirAll(p, perm) },
		Root:     dir,
	}
	return m, run
}

func opts() Options {
	return Options{
		Unit:        "ferc-simple-web-shell",
		BinPath:     "/opt/ferc/bin/simple-web-shell",
		Args:        []string{"-key", "s3cret", "-port", "8878"},
		Dir:         "/opt/ferc/bin",
		Description: "SimpleWebShell 远程终端",
	}
}

func TestRenderSystemdUnit(t *testing.T) {
	body := renderSystemdUnit("ferc-simple-web-shell", opts())
	for _, want := range []string{
		"Description=SimpleWebShell 远程终端",
		"ExecStart=/opt/ferc/bin/simple-web-shell -key s3cret -port 8878",
		"Restart=always",
		"WantedBy=multi-user.target",
		"WorkingDirectory=/opt/ferc/bin",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("unit 缺少 %q\n%s", want, body)
		}
	}
	if strings.Contains(body, "Restart=on-failure") {
		t.Error("应使用 Restart=always：进程正常退出后也需要被拉起")
	}
}

func TestRenderSystemdUnitQuotesArgs(t *testing.T) {
	o := opts()
	o.Args = []string{"-args=a b"}
	body := renderSystemdUnit("u", o)
	if !strings.Contains(body, `ExecStart=/opt/ferc/bin/simple-web-shell "-args=a b"`) {
		t.Fatalf("含空格的参数必须加引号\n%s", body)
	}
}

func TestRegisterSystemd(t *testing.T) {
	dir := t.TempDir()
	m, run := newManager(t, platform.InitSystemd, dir)
	res, err := m.Register(context.Background(), opts())
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if res.Supervisor != "systemd" {
		t.Fatalf("Supervisor = %q", res.Supervisor)
	}
	if len(run.calls) < 2 {
		t.Fatalf("应调用 daemon-reload 与 enable --now，实际 %v", run.calls)
	}
	if !hasCall(run.calls, "systemctl enable --now ferc-simple-web-shell") {
		t.Fatalf("缺少 enable --now，实际 %v", run.calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "ferc-simple-web-shell.service")); err != nil {
		t.Fatalf("unit 文件应写出: %v", err)
	}
}

func TestRegisterSystemdEnableFailureIsError(t *testing.T) {
	dir := t.TempDir()
	m, run := newManager(t, platform.InitSystemd, dir)
	run.err["systemctl enable --now"] = errors.New("denied")
	if _, err := m.Register(context.Background(), opts()); err == nil {
		t.Fatal("enable 失败必须报错，不能假装注册成功")
	}
}

func TestRegisterOpenRC(t *testing.T) {
	dir := t.TempDir()
	m, run := newManager(t, platform.InitOpenRC, dir)
	res, err := m.Register(context.Background(), opts())
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if res.Supervisor != "openrc" {
		t.Fatalf("Supervisor = %q", res.Supervisor)
	}
	if !hasCall(run.calls, "rc-update add ferc-simple-web-shell default") {
		t.Fatalf("缺少 rc-update add，实际 %v", run.calls)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "ferc-simple-web-shell"))
	if !strings.Contains(string(b), "supervise-daemon") {
		t.Fatalf("openrc 脚本应使用 supervise-daemon 才能 respawn\n%s", b)
	}
	if !strings.Contains(string(b), "respawn_max=0") {
		t.Fatal("应设置无限重试")
	}
}

func TestRegisterLaunchd(t *testing.T) {
	dir := t.TempDir()
	m, run := newManager(t, platform.InitLaunchd, dir)
	res, err := m.Register(context.Background(), opts())
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if res.Supervisor != "launchd" {
		t.Fatalf("Supervisor = %q", res.Supervisor)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "ferc-simple-web-shell.plist"))
	if !strings.Contains(string(b), "<key>KeepAlive</key><true/>") {
		t.Fatalf("plist 必须 KeepAlive\n%s", b)
	}
	if len(run.calls) == 0 {
		t.Fatal("应调用 launchctl load")
	}
}

func TestRegisterSysVAlsoInstallsDontCrack(t *testing.T) {
	dir := t.TempDir()
	m, run := newManager(t, platform.InitSysV, dir)
	m.DontCrackBin = "/opt/ferc/bin/DontCrack"
	res, err := m.Register(context.Background(), opts())
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if res.Supervisor != "dontcrack" {
		t.Fatalf("sysvinit 无 respawn，真正托管方应是 DontCrack，实际 %q", res.Supervisor)
	}
	if !hasPrefix(run.calls, "/opt/ferc/bin/DontCrack -path") {
		t.Fatalf("应启动 DontCrack，实际 %v", run.calls)
	}
	if !hasCall(run.calls, "update-rc.d ferc-simple-web-shell defaults") {
		t.Fatalf("应注册开机自启，实际 %v", run.calls)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("降级到 DontCrack 必须留下告警")
	}
}

func TestRegisterDontCrackRequiresBin(t *testing.T) {
	dir := t.TempDir()
	m, _ := newManager(t, platform.InitNone, dir)
	if _, err := m.Register(context.Background(), opts()); err == nil {
		t.Fatal("没有进程管理器可执行文件时必须报错，不能静默不托管")
	}
}

func TestRegisterDontCrackArgsUseEqualsForm(t *testing.T) {
	dir := t.TempDir()
	m, run := newManager(t, platform.InitNone, dir)
	m.DontCrackBin = "/bin/DontCrack"
	o := opts()
	o.Args = []string{"-mode", "normal"}
	if _, err := m.Register(context.Background(), o); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	joined := strings.Join(run.calls, "\n")
	// DontCrack 的 flag 包会把以 - 开头的 -args 值吞并，必须用等号形式。
	if !strings.Contains(joined, "-args=-mode normal") {
		t.Fatalf("必须使用 -args= 等号形式\n%s", joined)
	}
}

func TestRegisterDontCrackUsesSpawn(t *testing.T) {
	// DontCrack 永不退出，必须走 Spawn（不等它），否则装载会被卡到超时。
	dir := t.TempDir()
	m, run := newManager(t, platform.InitNone, dir)
	m.DontCrackBin = "/bin/DontCrack"
	var spawned int
	var gotArgs []string
	m.Spawn = func(name string, args []string, logPath string) (int, error) {
		spawned++
		gotArgs = append([]string{name}, args...)
		return 4242, nil
	}
	res, err := m.Register(context.Background(), opts())
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if res.ManagerPID != 4242 {
		t.Fatalf("应记录管理器进程号，实际 %d", res.ManagerPID)
	}
	if spawned != 1 {
		t.Fatalf("应通过 Spawn 启动一次，实际 %d 次", spawned)
	}
	if len(run.calls) != 0 {
		t.Fatalf("有 Spawn 时不该走阻塞式 Run，实际调用了 %v", run.calls)
	}
	if gotArgs[0] != "/bin/DontCrack" {
		t.Fatalf("第一个参数应是可执行文件，实际 %v", gotArgs)
	}
}

func TestRegisterDontCrackAllocatesDistinctPorts(t *testing.T) {
	// 一个 DontCrack 实例只托管一个进程、只占一个控制端口。两个服务若用同一
	// 端口，第二个实例启动即失败，节点就变成“一个保活、一个裸奔”。
	dir := t.TempDir()
	m, _ := newManager(t, platform.InitNone, dir)
	m.DontCrackBin = "/bin/DontCrack"
	var ports []string
	m.Spawn = func(name string, args []string, logPath string) (int, error) {
		for i, a := range args {
			if a == "-port" && i+1 < len(args) {
				ports = append(ports, args[i+1])
			}
		}
		return 1000 + len(ports), nil
	}
	for offset := 0; offset < 3; offset++ {
		o := opts()
		o.PortOffset = offset
		if _, err := m.Register(context.Background(), o); err != nil {
			t.Fatalf("注册失败: %v", err)
		}
	}
	want := []string{"11883", "11884", "11885"}
	if strings.Join(ports, ",") != strings.Join(want, ",") {
		t.Fatalf("控制端口应递增，期望 %v，实际 %v", want, ports)
	}
}

func TestRegisterDontCrackFailsWhenControlPortNotReady(t *testing.T) {
	// DontCrack 绑不上端口时只打日志、进程并不退出；若不探端口就会误报成功。
	dir := t.TempDir()
	m, _ := newManager(t, platform.InitNone, dir)
	m.DontCrackBin = "/bin/DontCrack"
	m.Spawn = func(string, []string, string) (int, error) { return 4321, nil }
	m.Get = func(string) error { return errors.New("connection refused") }
	var killed int
	m.Kill = func(pid int) error { killed = pid; return nil }
	if _, err := m.Register(context.Background(), opts()); err == nil {
		t.Fatal("控制端口未就绪时必须报错")
	}
	if killed != 4321 {
		t.Fatalf("失败时应结束已派生的管理器进程，实际 %d", killed)
	}
}

func TestRegisterDontCrackFailsWhenManagedProcessIsNotUp(t *testing.T) {
	// 管理器活着、被托管进程反复秒退（例如端口被占）时，不能报“安装成功”：
	// 必须硬失败，并回收刚派生的管理器，让事务式安装连带回滚。
	dir := t.TempDir()
	m, _ := newManager(t, platform.InitNone, dir)
	m.DontCrackBin = "/bin/DontCrack"
	m.Spawn = func(string, []string, string) (int, error) { return 4321, nil }
	m.StartGrace = 100 * time.Millisecond
	m.Get = func(url string) error {
		if strings.HasSuffix(url, "/healthz") {
			return errors.New("503")
		}
		return nil
	}
	var killed int
	m.Kill = func(pid int) error { killed = pid; return nil }
	o := opts()
	o.Ports = []int{8878}
	_, err := m.Register(context.Background(), o)
	if err == nil {
		t.Fatal("被托管进程没起来时必须硬失败")
	}
	if !strings.Contains(err.Error(), "8878") {
		t.Fatalf("报错应点出组件端口，实际 %v", err)
	}
	if killed != 4321 {
		t.Fatalf("失败时应回收已派生的管理器，实际 %d", killed)
	}
}

func TestUnregisterDontCrackStopsManager(t *testing.T) {
	dir := t.TempDir()
	m, _ := newManager(t, platform.InitNone, dir)
	var posted []string
	m.Get = func(string) error { return nil } // /healthz 说子进程在跑
	m.Post = func(url string) error { posted = append(posted, url); return nil }
	var killed int
	m.Kill = func(pid int) error { killed = pid; return nil }
	res := Result{Supervisor: "dontcrack", Unit: "ferc-simple-web-shell", ControlPort: 11884, ManagerPID: 777}
	if err := m.Unregister(context.Background(), res); err != nil {
		t.Fatalf("卸载失败: %v", err)
	}
	if len(posted) != 1 || !strings.Contains(posted[0], "11884/shutdown") {
		t.Fatalf("应让管理器按自己的端口停掉子进程，实际 %v", posted)
	}
	if killed != 777 {
		t.Fatalf("应结束管理器进程本身，实际 %d", killed)
	}
}

func TestUnregisterDontCrackToleratesAlreadyDeadChild(t *testing.T) {
	// 子进程早就崩了（例如绑不上端口）时 /shutdown 会返回 500；那是“已经是停的
	// 状态”，不该被当成卸载失败，管理器本身仍必须被收掉。
	dir := t.TempDir()
	m, _ := newManager(t, platform.InitNone, dir)
	m.Get = func(url string) error {
		if strings.HasSuffix(url, "/healthz") {
			return errors.New("503")
		}
		return nil
	}
	var posted int
	m.Post = func(string) error { posted++; return nil }
	var killed int
	m.Kill = func(pid int) error { killed = pid; return nil }
	res := Result{Supervisor: "dontcrack", Unit: "ferc-simple-time-service", ControlPort: 11884, ManagerPID: 888}
	if err := m.Unregister(context.Background(), res); err != nil {
		t.Fatalf("子进程已停时卸载不该失败: %v", err)
	}
	if posted != 0 {
		t.Fatalf("子进程没在跑时不该发 /shutdown，实际 %d 次", posted)
	}
	if killed != 888 {
		t.Fatalf("管理器本身仍应被结束，实际 %d", killed)
	}
}

func TestRegisterSchtasks(t *testing.T) {
	dir := t.TempDir()
	m, run := newManager(t, platform.InitWindowsSchtasks, dir)
	m.DontCrackBin = `C:\ferc\bin\dontcrack-windows.exe`
	res, err := m.Register(context.Background(), opts())
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if res.Supervisor != "dontcrack" {
		t.Fatalf("Windows 计划任务不能 respawn，真正托管方应是 DontCrack，实际 %q", res.Supervisor)
	}
	if !hasPrefix(run.calls, "schtasks /Create") {
		t.Fatalf("应注册计划任务，实际 %v", run.calls)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("计划任务 + DontCrack 双保险必须告警说明")
	}
}

func TestUnregisterSystemd(t *testing.T) {
	dir := t.TempDir()
	m, run := newManager(t, platform.InitSystemd, dir)
	res := Result{Supervisor: "systemd", Unit: "ferc-x"}
	if err := m.Unregister(context.Background(), res); err != nil {
		t.Fatalf("反注册失败: %v", err)
	}
	if !hasCall(run.calls, "systemctl disable --now ferc-x") {
		t.Fatalf("缺少 disable，实际 %v", run.calls)
	}
}

func TestRegisterRequiresBinPath(t *testing.T) {
	dir := t.TempDir()
	m, _ := newManager(t, platform.InitSystemd, dir)
	if _, err := m.Register(context.Background(), Options{Unit: "x"}); err == nil {
		t.Fatal("缺少可执行文件路径必须报错")
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"simple":    "simple",
		"a b":       "'a b'",
		"it's":      `'it'\''s'`,
		"":          "''",
		"-args=a b": "'-args=a b'",
		"/bin/x":    "/bin/x",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func hasCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

func hasPrefix(calls []string, want string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, want) {
			return true
		}
	}
	return false
}
