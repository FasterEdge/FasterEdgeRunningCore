// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

// Package artifact 解析产物文件，回答"这个文件到底是什么、能不能装到目标节点上"。
//
// P0 阶段这里只有会失败的红测试。课题对应项 F1。
//
// 要解决的问题：装载流程此前从不打开产物内容，只按文件名和 SHA256 判断。
// 于是"文件名写着 arm64、内容其实是 amd64"或者"内核模块的 vermagic 与目标
// 内核不匹配"这类问题，只能等到装完跑不起来才发现。解析器把这两类判断
// 前移到落盘之前。
//
// 三条硬规则（实现时不许放松）：
//
//  1. **vermagic 精确字符串相等**，不做版本模糊比较。vermagic 含内核版本、
//     SMP/PREEMPT 等编译开关甚至 GCC 版本，任一项不同 insmod 都会报
//     invalid module format。模糊比较等于制造假阳性。
//  2. **Static 只看有没有 PT_INTERP 段**，不看文件名、不看大小、不看后缀。
//  3. **三类错误可区分，不得合并**：缺 .modinfo 段 / 段内无 vermagic /
//     非预期 e_machine。它们对应完全不同的处置方式。
package artifact

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// F1-1：ParseModule 必须能从合成的 .ko 里取出全部字段。
func TestParseModuleHappyPath(t *testing.T) {
	path := writeSyntheticKO(t, syntheticKOOptions{
		machine:  machineARM,
		vermagic: "6.6.0 SMP mod_unload modversions aarch64",
		depends:  "can,dev",
		name:     "fec",
	})

	m, err := ParseModule(path)
	if err != nil {
		t.Fatalf("ParseModule 失败: %v", err)
	}
	if m.Vermagic != "6.6.0 SMP mod_unload modversions aarch64" {
		t.Fatalf("Vermagic 不对: %q", m.Vermagic)
	}
	// Release 取首个空格前的部分。
	if m.Release != "6.6.0" {
		t.Fatalf("Release 应为 vermagic 首个空格前的部分，得到 %q", m.Release)
	}
	if m.Name != "fec" {
		t.Fatalf("Name 不对: %q", m.Name)
	}
	if len(m.Depends) != 2 || m.Depends[0] != "can" || m.Depends[1] != "dev" {
		t.Fatalf("Depends 解析不对: %+v", m.Depends)
	}
	if m.Arch == "" {
		t.Fatal("Arch 不该为空")
	}
}

// F1-2：Release 只取首个空格前——vermagic 后面还有 SMP 等一长串编译开关。
func TestParseModuleReleaseStopsAtFirstSpace(t *testing.T) {
	path := writeSyntheticKO(t, syntheticKOOptions{
		machine:  machineARM,
		vermagic: "5.15.0 SMP preempt mod_unload ARMv7 p2v8 ",
	})
	m, err := ParseModule(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Release != "5.15.0" {
		t.Fatalf("Release 应在首个空格处截断，得到 %q", m.Release)
	}
	if m.Vermagic != "5.15.0 SMP preempt mod_unload ARMv7 p2v8 " {
		// 尾部空格是 .modinfo 里的真实内容，原样保留才能精确比对。
		t.Fatalf("Vermagic 应原样保留（含尾部空格），得到 %q", m.Vermagic)
	}
}

// F1-3：多空格 vermagic 也不能把 Release 算错。
func TestParseModuleReleaseWithMultipleSpaces(t *testing.T) {
	path := writeSyntheticKO(t, syntheticKOOptions{
		machine:  machineARM,
		vermagic: "6.6.0   SMP   mod_unload",
	})
	m, err := ParseModule(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Release != "6.6.0" {
		t.Fatalf("多空格时 Release 仍应为 6.6.0，得到 %q", m.Release)
	}
}

// F1-4：缺 .modinfo 段必须报 ErrNoModinfo，且不能被合并成别的错误。
func TestParseModuleMissingModinfo(t *testing.T) {
	path := writeSyntheticKO(t, syntheticKOOptions{machine: machineARM, omitModinfo: true})
	_, err := ParseModule(path)
	if !errors.Is(err, ErrNoModinfo) {
		t.Fatalf("应报 ErrNoModinfo，得到 %v", err)
	}
}

// F1-5：有 .modinfo 但无 vermagic 键必须报 ErrNoVermagic。
// 这与"缺段"是两回事：段在，只是内容不对。
func TestParseModuleMissingVermagic(t *testing.T) {
	path := writeSyntheticKO(t, syntheticKOOptions{
		machine:  machineARM,
		vermagic: "",
		extra:    map[string]string{"name": "fec"},
	})
	_, err := ParseModule(path)
	if !errors.Is(err, ErrNoVermagic) {
		t.Fatalf("应报 ErrNoVermagic，得到 %v", err)
	}
}

// F1-6：非预期 e_machine 必须报 ErrUnexpectedMachine，且在缺 .modinfo 之前判定。
//
// 顺序很重要：一个 amd64 的 .ko 打到 arm 节点上，最该说的是"架构不对"，
// 而不是"缺 .modinfo"——后者会把人引向错误的方向。
func TestParseModuleUnexpectedMachine(t *testing.T) {
	path := writeSyntheticKO(t, syntheticKOOptions{
		machine:  machineAMD64,
		vermagic: "6.6.0 SMP mod_unload x86_64",
	})
	_, err := ParseModule(path, WithExpectArch("arm64"))
	if !errors.Is(err, ErrUnexpectedMachine) {
		t.Fatalf("应报 ErrUnexpectedMachine，得到 %v", err)
	}
}

// F1-6b：同样内容的 .ko，在期望架构匹配时必须通过——证明上面的拒绝
// 不是"一律拒绝"，而是真的在比架构。
func TestParseModuleAcceptsMatchingMachine(t *testing.T) {
	path := writeSyntheticKO(t, syntheticKOOptions{
		machine:  machineAMD64,
		vermagic: "6.6.0 SMP mod_unload x86_64",
	})
	m, err := ParseModule(path, WithExpectArch("amd64"))
	if err != nil {
		t.Fatalf("架构匹配时应通过: %v", err)
	}
	if m.Arch != "amd64" {
		t.Fatalf("Arch 应为 amd64，得到 %q", m.Arch)
	}
}

// F1-7：vermagic 必须精确相等——模糊比较是课题明令禁止的。
func TestVermagicExactEquality(t *testing.T) {
	a := writeSyntheticKO(t, syntheticKOOptions{machine: machineARM, vermagic: "6.6.0 SMP mod_unload aarch64"})
	b := writeSyntheticKO(t, syntheticKOOptions{machine: machineARM, vermagic: "6.6.1 SMP mod_unload aarch64"})
	ma, err := ParseModule(a)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := ParseModule(b)
	if err != nil {
		t.Fatal(err)
	}
	// 版本号只差一位，字符串比较必须判不等。
	if ma.Vermagic == mb.Vermagic {
		t.Fatal("6.6.0 与 6.6.1 的 vermagic 必须判为不同")
	}
	if !VermagicMatches(ma.Vermagic, "6.6.0 SMP mod_unload aarch64") {
		t.Fatal("完全相同的 vermagic 必须判为匹配")
	}
	if VermagicMatches(ma.Vermagic, "6.6.0 SMP mod_unload aarch64 ") {
		t.Fatal("尾部多一个空格也必须判为不匹配（精确相等）")
	}
}

// F1-8：ParseELF 必须按 PT_INTERP 段判定静态/动态，不看文件名。
func TestParseELFStaticByMissingInterp(t *testing.T) {
	path := writeSyntheticELF(t, syntheticELFOptions{machine: machineARM64, withInterp: false})
	info, err := ParseELF(path)
	if err != nil {
		t.Fatalf("ParseELF 失败: %v", err)
	}
	if !info.Static {
		t.Fatal("没有 PT_INTERP 段必须判为静态链接")
	}
	if info.Interp != "" {
		t.Fatalf("静态链接不该有 Interp，得到 %q", info.Interp)
	}
	if info.Arch != "arm64" {
		t.Fatalf("Arch 应为 arm64，得到 %q", info.Arch)
	}
}

// F1-8b：有 PT_INTERP 必须判为动态，并取出解释器路径。
func TestParseELFDynamicWithInterp(t *testing.T) {
	path := writeSyntheticELF(t, syntheticELFOptions{
		machine: machineAMD64, withInterp: true, interp: "/lib/ld-linux-aarch64.so.1",
	})
	info, err := ParseELF(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Static {
		t.Fatal("有 PT_INTERP 段必须判为动态链接")
	}
	if info.Interp != "/lib/ld-linux-aarch64.so.1" {
		t.Fatalf("Interp 不对: %q", info.Interp)
	}
}

// F1-9：归档包要递归检查内部 ELF，架构不一致必须列出冲突文件。
//
// 端侧现场常见：一个 tar.gz 里同时装了多个架构的二进制（发布脚本图省事），
// 或者 main 与 libfoo.so 架构不一致。这类包装在目标节点上表现成
// "主程序能起、一加载库就 exec format error"。
func TestInspectArchiveListsArchConflicts(t *testing.T) {
	dir := t.TempDir()
	arc := writeSyntheticArchive(t, dir, "mixed.tar.gz", []archiveMember{
		{name: "bin/main", machine: machineARM64, mode: 0o755},
		{name: "lib/libfoo.so", machine: machineAMD64, mode: 0o755},
	})

	res, err := InspectArchive(arc, WithExpectArch("arm64"))
	if err != nil {
		t.Fatalf("InspectArchive 失败: %v", err)
	}
	if len(res.Conflicts) == 0 {
		t.Fatal("归档内架构不一致必须列出冲突文件")
	}
	found := false
	for _, c := range res.Conflicts {
		if containsName(c, "libfoo.so") {
			found = true
		}
	}
	if !found {
		t.Fatalf("冲突列表里应有 libfoo.so，实际: %+v", res.Conflicts)
	}
}

// F1-9b：归档内架构一致时不该报冲突（防止实现一律报冲突）。
func TestInspectArchiveNoConflictWhenConsistent(t *testing.T) {
	dir := t.TempDir()
	arc := writeSyntheticArchive(t, dir, "same.tar.gz", []archiveMember{
		{name: "bin/main", machine: machineARM64, mode: 0o755},
		{name: "lib/libfoo.so", machine: machineARM64, mode: 0o755},
	})
	res, err := InspectArchive(arc, WithExpectArch("arm64"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 0 {
		t.Fatalf("架构一致不该报冲突: %+v", res.Conflicts)
	}
}

// F1-10：真 .ko 测试件走 EDGECAP_KO_DIR；未设置则跳过并打印原因。
//
// 合成的 ELF 覆盖不了内核真实 .ko 的全部怪癖（符号表、重定位段、多个
// .modinfo 变体），因此保留一条通往真实样本的路，但不把它设为硬依赖——
// CI 上没有内核模块样本，硬依赖会让流水线红。
func TestParseModuleWithRealKOSamples(t *testing.T) {
	dir := os.Getenv("EDGECAP_KO_DIR")
	if dir == "" {
		t.Skip("未设置 EDGECAP_KO_DIR，跳过真实 .ko 样本测试（合成 ELF 用例已覆盖解析逻辑）")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读 EDGECAP_KO_DIR 失败: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !hasKOSuffix(e.Name()) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		m, err := ParseModule(p)
		if err != nil {
			// 真实样本解析失败要报出来，不能静默跳过——那会冒充通过。
			t.Errorf("%s 解析失败: %v", e.Name(), err)
			continue
		}
		if m.Vermagic == "" {
			t.Errorf("%s 解析出了空 vermagic", e.Name())
		}
		if m.Release == "" {
			t.Errorf("%s 解析出了空 Release", e.Name())
		}
		t.Logf("%s: arch=%s release=%s name=%s depends=%v",
			e.Name(), m.Arch, m.Release, m.Name, m.Depends)
	}
}

func containsName(s, sub string) bool {
	return len(sub) == 0 || indexOf(s, sub) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
