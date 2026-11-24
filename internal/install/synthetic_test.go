// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

package install

// 测试件：构造最小 ELF，供产物预检测试使用。
//
// 与 internal/artifact 的 synthetic_test.go 是同一套思路（那里有完整自检），
// 但那边是 _test.go 不能导出。这里只实现预检真正会用到的两个特征：
// e_machine 与 .modinfo section。其余一概从简——测试件越薄，失败原因越容易
// 定位到被测逻辑。

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"testing"
)

// e_machine 常量。用数字而不是 elf.EM_*，让"这台机器是什么"在测试里一目了然。
const (
	machine386     = 3
	machineARM     = 40
	machineX86_64  = 62
	machineAARCH64 = 183
	machineRISCV64 = 243
)

// buildSyntheticELFForTest 造一个 64 位小端 ELF，可选带 .modinfo section。
//
// 三遍式布局：先定段内容，再算字符串表与偏移，最后 emit。一遍式的问题在于
// section header 的 name offset 指向 .shstrtab，而字符串表的位置又依赖前面
// 所有段的大小——先建表后填偏移必然错位，debug/elf 会报 bad section name index。
func buildSyntheticELFForTest(t *testing.T, machine uint16, modinfo []byte) []byte {
	t.Helper()

	const ehdrSize = 64
	const phdrSize = 56
	const shdrSize = 64
	const ptLoad = 1
	const shtNull = 0
	const shtProgbits = 1
	const shtStrtab = 3
	const shfAlloc = 2

	type seg struct {
		typ     uint32
		flags   uint32
		data    []byte
		secName string
	}
	var segs []seg
	if len(modinfo) > 0 {
		segs = append(segs, seg{typ: ptLoad, flags: 4, data: modinfo, secName: ".modinfo"})
	} else {
		// 没有 .modinfo 时给一个可装载段，让文件至少是"可执行文件"的形态。
		segs = append(segs, seg{typ: ptLoad, flags: 5, data: []byte{0x90}})
	}
	for i := range segs {
		if segs[i].typ == ptLoad && segs[i].flags == 5 {
			continue // 代码段不补齐也无妨
		}
		segs[i].data = pad8(segs[i].data)
	}

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
	shdrs := []shdr{{typ: shtNull}}

	segBase := uint64(ehdrSize + len(segs)*phdrSize)
	type laid struct {
		seg seg
		off uint64
	}
	layout := make([]laid, 0, len(segs))
	cur := segBase
	for _, s := range segs {
		if s.secName != "" {
			shdrs = append(shdrs, shdr{
				nameOff: addName(s.secName), typ: shtProgbits, flags: shfAlloc,
				addr: 0x400000 + cur, off: cur, size: uint64(len(s.data)), addralign: 1,
			})
		}
		layout = append(layout, laid{seg: s, off: cur})
		cur += uint64(len(s.data))
	}

	shstrOff := cur
	shstrndx := len(shdrs)
	shdrs = append(shdrs, shdr{
		nameOff: addName(".shstrtab"), typ: shtStrtab,
		addr: 0x400000 + shstrOff, off: shstrOff, size: uint64(len(shstr)), addralign: 1,
	})
	eShoff := shstrOff + uint64(len(shstr))

	var buf bytes.Buffer
	w := func(v any) { binary.Write(&buf, binary.LittleEndian, v) }
	buf.Write([]byte{0x7f, 'E', 'L', 'F'})
	buf.WriteByte(2)
	buf.WriteByte(1)
	buf.WriteByte(1)
	buf.WriteByte(0)
	buf.Write(make([]byte, 8))
	w(uint16(2))
	w(machine)
	w(uint32(1))
	w(uint64(0))
	w(uint64(ehdrSize))
	w(eShoff)
	w(uint32(0))
	w(uint16(ehdrSize))
	w(uint16(phdrSize))
	w(uint16(len(segs)))
	w(uint16(shdrSize))
	w(uint16(len(shdrs)))
	w(uint16(shstrndx))

	for _, lo := range layout {
		w(lo.seg.typ)
		w(lo.seg.flags)
		w(lo.off)
		w(uint64(0x400000) + lo.off)
		w(uint64(0))
		w(uint64(len(lo.seg.data)))
		w(uint64(len(lo.seg.data)))
		w(uint64(8))
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
	// 自检：测试件坏了会让红测试的失败原因变成"测试件错了"而不是"缺实现"。
	f, err := elf.NewFile(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("合成的 ELF 不合法: %v", err)
	}
	if uint16(f.Machine) != machine {
		t.Fatalf("e_machine = %d，期望 %d", f.Machine, machine)
	}
	if len(modinfo) > 0 && f.Section(".modinfo") == nil {
		t.Fatal("合成的 ELF 应含 .modinfo section")
	}
	return out
}

func pad8(b []byte) []byte {
	if len(b) == 0 || len(b)%8 == 0 {
		return b
	}
	out := make([]byte, ((len(b)+7)/8)*8)
	copy(out, b)
	return out
}
