// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

package artifact

// 测试件合成器：程序化构造最小但结构合法的 ELF / tar.gz。
//
// 课题要求"测试件程序合成ELF"，且禁止提交真实驱动/镜像/大二进制。这里用
// debug/elf 的写侧能力手工拼出最小 ELF——它不执行、不需要链接器、不依赖
// 任何外部工具链，因此能在 CI 上跑。
//
// 构造的 ELF 刻意保持最小：只有解析器真正会看的段（.modinfo / PT_INTERP）
// 是完整的，其余部分能省则省。这样测试失败时原因只可能来自被测逻辑，
// 不会掺进"测试件本身构造错了"。

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// e_machine 常量。用数字而不是 elf.EM_* 常量，是为了让"这台机器是什么"
// 在测试里一目了然，也避免不同 Go 版本的常量差异。
const (
	machine386     = 3
	machineARM     = 40
	machineX86_64  = 62
	machineAARCH64 = 183
	machineRISCV64 = 243
)

// machineARM64 / machineAMD64 是测试里用的别名，指向具体数值。
const (
	machineARM64 = machineAARCH64
	machineAMD64 = machineX86_64
)

// archNameOf 把 e_machine 数值映射成归一架构名。
//
// 这里放一份映射是为了让测试件与实现解耦：实现方有自己的映射表，
// 测试件只需要知道"我造的这台机器应该被叫成什么"，两边对不上正是
// 测试要抓的问题。
func archNameOf(machine uint16) string {
	switch machine {
	case machine386:
		return "386"
	case machineARM:
		return "arm"
	case machineX86_64:
		return "amd64"
	case machineAARCH64:
		return "arm64"
	case machineRISCV64:
		return "riscv64"
	}
	return ""
}

type syntheticKOOptions struct {
	machine     uint16
	vermagic    string
	depends     string
	name        string
	extra       map[string]string
	omitModinfo bool
	// vermagicNoPad 为真时不给 vermagic 值补 null 终止符之外的填充，
	// 用于构造"段内无 vermagic"的负例。
	vermagicNoPad bool
}

// writeSyntheticKO 造一个带 .modinfo 段的最小 ELF，返回路径。
//
// .modinfo 的真实格式是 null 分隔的 key=value 序列，以 null 结尾。
// 这里严格按那个格式写，包括"最后一个条目后面还有一个 null"这个细节——
// 真实内核就是这么放的，解析器若按别的假设实现会在真 .ko 上出错。
func writeSyntheticKO(t *testing.T, opt syntheticKOOptions) string {
	t.Helper()

	var modinfo bytes.Buffer
	if opt.vermagic != "" {
		modinfo.WriteString("vermagic=" + opt.vermagic)
		modinfo.WriteByte(0)
	}
	if opt.name != "" {
		modinfo.WriteString("name=" + opt.name)
		modinfo.WriteByte(0)
	}
	if opt.depends != "" {
		modinfo.WriteString("depends=" + opt.depends)
		modinfo.WriteByte(0)
	}
	// extra 的 key 排序后写入，保证同一份输入每次合成的字节一致
	// （测试要可重复，不能因为 map 遍历顺序不同而产出不同文件）。
	for _, k := range sortedKeys(opt.extra) {
		modinfo.WriteString(k + "=" + opt.extra[k])
		modinfo.WriteByte(0)
	}
	if modinfo.Len() > 0 {
		// 真实 .modinfo 以 null 结尾；最后一个条目写完后补一个。
		modinfo.WriteByte(0)
	}

	path := filepath.Join(t.TempDir(), "synthetic.ko")
	if err := os.WriteFile(path, buildMinimalELF(t, opt.machine, modinfo.Bytes(), opt.omitModinfo), 0o644); err != nil {
		t.Fatalf("写合成 .ko 失败: %v", err)
	}
	return path
}

type syntheticELFOptions struct {
	machine    uint16
	withInterp bool
	interp     string
}

func writeSyntheticELF(t *testing.T, opt syntheticELFOptions) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "synthetic.elf")
	if err := os.WriteFile(path, buildMinimalELF(t, opt.machine, nil, false, opt), 0o755); err != nil {
		t.Fatalf("写合成 ELF 失败: %v", err)
	}
	return path
}

// buildMinimalELF 拼一个 64 位小端 ELF。
//
// 用 little-endian + ELFCLASS64：aarch64/x86_64/riscv64 都是这个组合，
// 覆盖面最广。32 位 arm 的 e_machine 仍是 40，解析器按 e_machine 判断即可
// （ELFCLASS 不是架构判据），因此不需要为 32 位单独造一类文件。
//
// 实现是**两遍式**：先把节名收集进字符串表、再算每段的偏移，最后统一 emit。
// 一遍式的问题在于 section header 里的 name offset 指向字符串表，而字符串表
// 的位置又依赖前面所有段的大小——先建表后填偏移必然错位，debug/elf 会报
// "bad section name index"。
func buildMinimalELF(t *testing.T, machine uint16, modinfo []byte, omitModinfo bool, elfOpts ...syntheticELFOptions) []byte {
	t.Helper()

	var interp string
	for _, o := range elfOpts {
		if o.withInterp {
			interp = o.interp
			if interp == "" {
				interp = "/lib/ld-linux.so.2"
			}
		}
	}

	const ehdrSize = 64
	const phdrSize = 56
	const shdrSize = 64

	const ptLoad = 1
	const ptInterp = 3
	const shtNull = 0
	const shtProgbits = 1
	const shtStrtab = 3
	const shfAlloc = 2

	// ── 第一遍：决定有哪些段、各自内容是什么 ──
	type seg struct {
		typ   uint32
		flags uint32
		data  []byte
		// secName 非空表示这个段还要对应一个同名 section。
		secName string
	}
	var segs []seg
	// .modinfo 段：PT_LOAD + 一个叫 .modinfo 的 section。
	if len(modinfo) > 0 && !omitModinfo {
		segs = append(segs, seg{typ: ptLoad, flags: 4 /*PF_R*/, data: modinfo, secName: ".modinfo"})
	}
	// PT_INTERP 段：不建 section（真实文件里 .interp 通常有，但解析器只认段）。
	if interp != "" {
		segs = append(segs, seg{typ: ptInterp, flags: 4, data: append([]byte(interp), 0)})
	}
	// 兜底：至少一个可装载段。
	if len(segs) == 0 {
		segs = append(segs, seg{typ: ptLoad, flags: 5 /*R|X*/, data: []byte{0x90}})
	}

	// PT_INTERP 的数据不补齐：解释器路径必须是"路径 + 一个 null"，
	// 补齐会让路径尾部长出多余 null，精确比对时对不上。
	for i := range segs {
		if segs[i].typ == ptInterp {
			continue
		}
		segs[i].data = padTo(segs[i].data)
	}

	// ── 第二遍：算字符串表与各段偏移 ──
	// 字符串表第一个字节按惯例是 null。
	var shstr []byte
	shstr = append(shstr, 0)
	addName := func(name string) uint32 {
		off := uint32(len(shstr))
		shstr = append(shstr, []byte(name)...)
		shstr = append(shstr, 0)
		return off
	}

	type shdr struct {
		nameOff   uint32
		typ       uint32
		flags     uint64
		addr      uint64
		off       uint64
		size      uint64
		link      uint32
		info      uint32
		addralign uint64
		entsize   uint64
	}
	// section 0 必须是全零 SHT_NULL。
	shdrs := []shdr{{typ: shtNull}}

	// 段数据区紧跟 program header 表。
	segBase := uint64(ehdrSize + len(segs)*phdrSize)

	type laid struct {
		seg   seg
		off   uint64
		shndx int // 有对应 section 时为下标，否则 -1
	}
	layout := make([]laid, 0, len(segs))
	cur := segBase
	for _, s := range segs {
		lo := laid{seg: s, off: cur, shndx: -1}
		if s.secName != "" {
			lo.shndx = len(shdrs)
			shdrs = append(shdrs, shdr{
				nameOff:   addName(s.secName),
				typ:       shtProgbits,
				flags:     shfAlloc,
				addr:      0x400000 + cur,
				off:       cur,
				size:      uint64(len(s.data)),
				addralign: 1,
			})
		}
		layout = append(layout, lo)
		cur += uint64(len(s.data))
	}

	// .shstrtab 自身：内容紧随段数据，也要占一个 section。
	shstrOff := cur
	shstrndx := len(shdrs)
	shdrs = append(shdrs, shdr{
		nameOff:   addName(".shstrtab"),
		typ:       shtStrtab,
		addr:      0x400000 + shstrOff,
		off:       shstrOff,
		size:      uint64(len(shstr)),
		addralign: 1,
	})
	eShoff := shstrOff + uint64(len(shstr))

	// ── 第三遍：emit ──
	var buf bytes.Buffer
	w := func(v any) { binary.Write(&buf, binary.LittleEndian, v) }

	buf.Write([]byte{0x7f, 'E', 'L', 'F'})
	buf.WriteByte(2) // ELFCLASS64
	buf.WriteByte(1) // ELFDATA2LSB
	buf.WriteByte(1) // EV_CURRENT
	buf.WriteByte(0) // ELFOSABI_NONE
	buf.Write(make([]byte, 8))
	w(uint16(2))          // e_type = ET_EXEC
	w(machine)            // e_machine
	w(uint32(1))          // e_version
	w(uint64(0))          // e_entry
	w(uint64(ehdrSize))   // e_phoff
	w(eShoff)             // e_shoff
	w(uint32(0))          // e_flags
	w(uint16(ehdrSize))   // e_ehsize
	w(uint16(phdrSize))   // e_phentsize
	w(uint16(len(segs)))  // e_phnum
	w(uint16(shdrSize))   // e_shentsize
	w(uint16(len(shdrs))) // e_shnum
	w(uint16(shstrndx))   // e_shstrndx

	for _, lo := range layout {
		w(lo.seg.typ)
		w(lo.seg.flags)
		w(lo.off)                    // p_offset
		w(uint64(0x400000) + lo.off) // p_vaddr
		w(uint64(0))                 // p_paddr
		w(uint64(len(lo.seg.data)))  // p_filesz
		w(uint64(len(lo.seg.data)))  // p_memsz
		w(uint64(8))                 // p_align
	}
	for _, lo := range layout {
		buf.Write(lo.seg.data)
	}
	buf.Write(shstr)
	for _, h := range shdrs {
		w(h.nameOff)
		w(h.typ)
		w(h.flags)
		w(h.addr)
		w(h.off)
		w(h.size)
		w(h.link)
		w(h.info)
		w(h.addralign)
		w(h.entsize)
	}

	out := buf.Bytes()
	// 自检：合成出来的必须能被标准库当合法 ELF 读回来，且 .modinfo 可被
	// 按节名找到。不自检的话，"测试件构造错了"会被误判成"被测代码错了"。
	f, err := elf.NewFile(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("合成的 ELF 不合法: %v", err)
	}
	if uint16(f.Machine) != machine {
		t.Fatalf("e_machine = %d，期望 %d", f.Machine, machine)
	}
	if len(modinfo) > 0 && !omitModinfo {
		if f.Section(".modinfo") == nil {
			t.Fatal("合成的 ELF 应含 .modinfo section")
		}
	}
	if got := archNameOf(machine); got == "" {
		t.Fatalf("测试件用了未登记的 e_machine %d", machine)
	}
	return out
}

func padTo(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	if len(b)%8 == 0 {
		return b
	}
	out := make([]byte, ((len(b)+7)/8)*8)
	copy(out, b)
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// 简单插入排序，避免为测试文件引入 sort 的心智负担。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// ── 归档测试件 ──────────────────────────────────────────────

type archiveMember struct {
	name    string
	machine uint16
	mode    int64
}

func writeSyntheticArchive(t *testing.T, dir, name string, members []archiveMember) string {
	t.Helper()

	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, m := range members {
		body := buildMinimalELF(t, m.machine, nil, false)
		mode := m.mode
		if mode == 0 {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: m.name, Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, gz.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func hasKOSuffix(name string) bool {
	return len(name) > 3 && name[len(name)-3:] == ".ko"
}
