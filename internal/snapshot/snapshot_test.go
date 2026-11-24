// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

package snapshot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/platform"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/state"
)

// mkState 造一份带两个组件的状态，并把二进制真的写到磁盘上。
func mkState(t *testing.T, root string) (*state.State, string) {
	t.Helper()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	st := state.New(runtime.GOOS, platform.NormalizeArch(runtime.GOARCH), "test-host", root)
	for _, n := range []string{"alpha", "beta"} {
		p := filepath.Join(binDir, n+exeExt())
		if err := os.WriteFile(p, []byte("binary-of-"+n), 0o755); err != nil {
			t.Fatal(err)
		}
		st.Put(state.Record{
			Name: n, Version: "v1", Kind: "service",
			Path: p, Supervisor: "systemd", Unit: "ferc-" + n,
		})
	}
	sp := state.Path(root)
	if err := st.Save(sp); err != nil {
		t.Fatal(err)
	}
	return st, sp
}

func exeExt() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

func fixedNow() time.Time {
	return time.Date(2026, 11, 24, 10, 15, 30, 0, time.UTC)
}

func TestCreateThenList(t *testing.T) {
	root := t.TempDir()
	st, sp := mkState(t, root)
	base := filepath.Join(root, "snapshots")

	res, err := Create(st, sp, "v1.0.20261127", CreateOptions{
		Base: base, Note: "升级前", Now: fixedNow,
	})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	if res.Components != 2 || res.Missing != 0 {
		t.Fatalf("应收录 2 个组件: %+v", res)
	}
	if res.ID == "" || res.Dir != filepath.Join(base, res.ID) {
		t.Fatalf("ID/目录不对: %+v", res)
	}
	// 二进制必须真的存下来了，而且可执行。
	for _, n := range []string{"alpha", "beta"} {
		p := filepath.Join(res.Dir, BinDirName, n+exeExt())
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("快照里应有 %s: %v", n, err)
		}
		if string(b) != "binary-of-"+n {
			t.Fatalf("%s 内容不对: %q", n, b)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && fi.Mode().Perm()&0o100 == 0 {
			t.Fatalf("快照里的二进制应可执行: %o", fi.Mode().Perm())
		}
	}

	list, err := List(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("应列出 1 份快照: %+v", list)
	}
	s := list[0]
	if s.ID != res.ID || s.Note != "升级前" || s.Components != 2 || s.Missing != 0 {
		t.Fatalf("摘要不对: %+v", s)
	}
	if s.Hostname != "test-host" || s.GOOS != runtime.GOOS {
		t.Fatalf("摘要应带上来源机器信息: %+v", s)
	}
}

func TestCreateRecordsMissingBinary(t *testing.T) {
	root := t.TempDir()
	st, sp := mkState(t, root)
	// 把 beta 的二进制删掉：快照仍应收录它，但标记为不完整。
	if err := os.Remove(filepath.Join(root, "bin", "beta"+exeExt())); err != nil {
		t.Fatal(err)
	}

	res, err := Create(st, sp, "v", CreateOptions{Base: filepath.Join(root, "snapshots"), Now: fixedNow})
	if err != nil {
		t.Fatalf("二进制缺失不该让 Create 失败: %v", err)
	}
	if res.Missing != 1 || res.Components != 2 {
		t.Fatalf("应记录 1 个缺失: %+v", res)
	}
	meta, _, err := Load(filepath.Join(root, "snapshots"), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range meta.Components {
		if c.Name == "beta" && c.Bin != "" {
			t.Fatalf("缺失的二进制不该记 Bin: %+v", c)
		}
		if c.Name == "beta" && c.SHA256 != "" {
			t.Fatalf("缺失的二进制不该有 SHA256: %+v", c)
		}
	}
}

func TestRestoreRollsBackModifiedBinary(t *testing.T) {
	root := t.TempDir()
	st, sp := mkState(t, root)
	base := filepath.Join(root, "snapshots")
	if _, err := Create(st, sp, "v", CreateOptions{Base: base, Now: fixedNow}); err != nil {
		t.Fatal(err)
	}

	// 模拟“改坏了”：二进制被换掉、状态文件里多装了一个组件。
	if err := os.WriteFile(filepath.Join(root, "bin", "alpha"+exeExt()), []byte("BROKEN"), 0o755); err != nil {
		t.Fatal(err)
	}
	st.Put(state.Record{Name: "gamma", Version: "v9", Kind: "tool", Path: filepath.Join(root, "bin", "gamma")})
	if err := st.Save(sp); err != nil {
		t.Fatal(err)
	}

	res, err := Restore(t.Context(), RestoreOptions{Base: base, Root: root})
	if err != nil {
		t.Fatalf("Restore 失败: %v", err)
	}
	if res.Restored != 2 || res.Failed != 0 {
		t.Fatalf("应还原 2 个组件: %+v", res)
	}
	for _, it := range res.Items {
		if !it.SHA256OK {
			t.Errorf("%s 应通过 SHA256 核对", it.Name)
		}
	}
	b, err := os.ReadFile(filepath.Join(root, "bin", "alpha"+exeExt()))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "binary-of-alpha" {
		t.Fatalf("二进制应回到快照内容: %q", b)
	}
	// 状态文件必须整体替换：gamma 不该留下。
	after, err := state.Load(sp)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Components) != 2 {
		t.Fatalf("回归后应只有 2 个组件，实际 %d: %+v", len(after.Components), after.Components)
	}
	if _, ok := after.Get("gamma"); ok {
		t.Fatal("回归后 gamma 应消失（状态文件必须整体替换）")
	}
	if after.Root != root {
		t.Fatalf("回归后 Root 应为当前根目录: %s", after.Root)
	}
}

func TestRestoreDetectsCorruptedSnapshot(t *testing.T) {
	root := t.TempDir()
	st, sp := mkState(t, root)
	base := filepath.Join(root, "snapshots")
	res, err := Create(st, sp, "v", CreateOptions{Base: base, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	// 篡改快照里的二进制：SHA256 必须对不上。
	target := filepath.Join(res.Dir, BinDirName, "alpha"+exeExt())
	if err := os.WriteFile(target, []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := Restore(t.Context(), RestoreOptions{Base: base, Root: root})
	if err == nil {
		t.Fatal("校验和不符必须判失败")
	}
	if out == nil || out.Failed == 0 {
		t.Fatalf("应报出失败的组件: %+v", out)
	}
	// 关键：失败时状态文件不能被改。
	cur, err := state.Load(sp)
	if err != nil {
		t.Fatal(err)
	}
	if len(cur.Components) != 2 {
		t.Fatalf("失败时状态文件应保持原样: %+v", cur.Components)
	}
	// 二进制也不该被写成坏内容。
	b, _ := os.ReadFile(filepath.Join(root, "bin", "alpha"+exeExt()))
	if string(b) == "tampered" {
		t.Fatal("坏内容不该被写进 bin/")
	}
}

func TestRestoreRefusesCrossArch(t *testing.T) {
	root := t.TempDir()
	st, sp := mkState(t, root)
	base := filepath.Join(root, "snapshots")
	res, err := Create(st, sp, "v", CreateOptions{Base: base, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	// 把快照的架构改成别的：跨平台回归毫无意义，必须拒绝。
	meta, _, err := Load(base, res.ID)
	if err != nil {
		t.Fatal(err)
	}
	other := "riscv64"
	if meta.Arch == "riscv64" {
		other = "amd64"
	}
	meta.Arch = other
	if err := writeJSON(filepath.Join(res.Dir, MetaName), meta); err != nil {
		t.Fatal(err)
	}

	if _, err := Restore(t.Context(), RestoreOptions{Base: base, Root: root}); err == nil {
		t.Fatal("跨架构回归必须被拒绝")
	}
}

func TestRestoreRewritesRoot(t *testing.T) {
	root := t.TempDir()
	st, sp := mkState(t, root)
	base := filepath.Join(t.TempDir(), "snapshots")
	res, err := Create(st, sp, "v", CreateOptions{Base: base, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	// 换一台机器的数据根目录来还原。
	newRoot := t.TempDir()
	out, err := Restore(t.Context(), RestoreOptions{Base: base, ID: res.ID, Root: newRoot})
	if err != nil {
		t.Fatalf("换根目录还原应成功: %v", err)
	}
	if len(out.Warnings) == 0 {
		t.Fatal("改写根目录必须给出告警")
	}
	after, err := state.Load(state.Path(newRoot))
	if err != nil {
		t.Fatal(err)
	}
	if after.Root != newRoot {
		t.Fatalf("Root 应被改写成当前的: %s", after.Root)
	}
	// 组件路径指向的是快照里的绝对路径（原机器的），还原到新根目录后这些
	// 路径已经不存在；这件事必须让人知道，否则 status 会显示一堆幽灵路径。
	for _, r := range after.Components {
		if _, err := os.Stat(r.Path); err == nil {
			t.Logf("组件 %s 的路径 %s 在新根目录下居然存在（可能是原路径恰好在）", r.Name, r.Path)
		}
	}
}

func TestListEmptyAndMissingDir(t *testing.T) {
	list, err := List(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("目录不存在应返回空列表而非错误: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("应为空: %+v", list)
	}
}

func TestDelete(t *testing.T) {
	root := t.TempDir()
	st, sp := mkState(t, root)
	base := filepath.Join(root, "snapshots")
	if _, err := Create(st, sp, "v", CreateOptions{Base: base, Now: fixedNow}); err != nil {
		t.Fatal(err)
	}
	list, _ := List(base)
	if len(list) != 1 {
		t.Fatal("应有 1 份")
	}
	if err := Delete(base, list[0].ID); err != nil {
		t.Fatal(err)
	}
	list, _ = List(base)
	if len(list) != 0 {
		t.Fatalf("删除后应为空: %+v", list)
	}
	// 删不存在的一定要报错，不能静默成功。
	if err := Delete(base, "no-such-id"); err == nil {
		t.Fatal("删不存在的快照应报错")
	}
	if err := Delete(base, ""); err == nil {
		t.Fatal("空 ID 应报错")
	}
}

func TestLoadRejectsNewerSchema(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "20260101-000000-abcdef")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Meta{SchemaVersion: SchemaVersion + 1, ID: "x"})
	if err := os.WriteFile(filepath.Join(dir, MetaName), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadMeta(filepath.Join(dir, MetaName)); err == nil {
		t.Fatal("更高版本的快照格式必须报错")
	}
	// List 应跳过这个坏目录而不是整体失败。
	list, err := List(base)
	if err != nil {
		t.Fatalf("坏目录不该让 List 失败: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("坏目录应被跳过: %+v", list)
	}
}

func TestListSkipsGarbageDirs(t *testing.T) {
	base := t.TempDir()
	// 半截目录：创建中途断电会留下这种。
	if err := os.MkdirAll(filepath.Join(base, "20260101-000000-deadbe"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 普通文件也不该让 List 出错。
	if err := os.WriteFile(filepath.Join(base, "README.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := List(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("垃圾条目应被跳过: %+v", list)
	}
}

func TestNewIDUniqueWithinSameSecond(t *testing.T) {
	t0 := fixedNow()
	base := t.TempDir()
	ids := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := uniqueID(base, t0)
		if ids[id] {
			t.Fatalf("同一时刻生成了重复 ID: %s", id)
		}
		ids[id] = true
		// uniqueID 靠“目录已存在”判断占用，因此测试要把目录建出来。
		if err := os.MkdirAll(filepath.Join(base, id), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// 同一秒内连打两份快照，第二份不能覆盖第一份。
func TestCreateTwiceInSameSecondKeepsBoth(t *testing.T) {
	root := t.TempDir()
	st, sp := mkState(t, root)
	base := filepath.Join(root, "snapshots")
	opt := CreateOptions{Base: base, Now: fixedNow}
	a, err := Create(st, sp, "v", opt)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Create(st, sp, "v", opt)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatalf("同一秒内的两份快照用了同一个 ID，第二份会覆盖第一份: %s", a.ID)
	}
	list, err := List(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("两份快照都应保留，实际 %d 份", len(list))
	}
}

// Restore 必须用调用方给的平台信息做校验，而不是 runtime.GOOS：
// ferc 经常在 Windows 工作站上管理一台 Linux 边缘节点。
func TestRestoreUsesInjectedPlatform(t *testing.T) {
	root := t.TempDir()
	st, sp := mkState(t, root)
	base := filepath.Join(root, "snapshots")
	res0, err := Create(st, sp, "v", CreateOptions{Base: base, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	meta0, _, err := Load(base, res0.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 注入一个与快照不同的目标平台必须被拒绝（快照的 GOOS/Arch 取自本机，
	// 所以这里取一个必然不同的 arch 来构造冲突）。
	other := "riscv64"
	if meta0.Arch == "riscv64" {
		other = "amd64"
	}
	if _, err := Restore(t.Context(), RestoreOptions{
		Base: base, Root: root, GOOS: meta0.GOOS, Arch: other,
	}); err == nil {
		t.Fatal("目标平台与快照不一致必须被拒绝")
	}

	// 注入与快照一致的平台：即便本机是别的系统也该放行。
	res, err := Restore(t.Context(), RestoreOptions{
		Base: base, Root: root, GOOS: runtime.GOOS,
		Arch: platform.NormalizeArch(runtime.GOARCH),
	})
	if err != nil {
		t.Fatalf("注入一致平台应放行: %v", err)
	}
	if res.Restored != 2 {
		t.Fatalf("应还原 2 个组件: %+v", res)
	}
}

func TestRestoreWarnsOnDifferentHost(t *testing.T) {
	root := t.TempDir()
	st, sp := mkState(t, root)
	base := filepath.Join(root, "snapshots")
	if _, err := Create(st, sp, "v", CreateOptions{Base: base, Now: fixedNow}); err != nil {
		t.Fatal(err)
	}
	res, err := Restore(t.Context(), RestoreOptions{
		Base: base, Root: root, Hostname: "edge-02",
		GOOS: runtime.GOOS, Arch: platform.NormalizeArch(runtime.GOARCH),
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range res.Warnings {
		if strings.Contains(w, "test-host") && strings.Contains(w, "edge-02") {
			found = true
		}
	}
	if !found {
		t.Fatalf("跨主机回归应给出端口冲突告警: %+v", res.Warnings)
	}
}
