// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

package artifact

// 合成器自检。
//
// 这一组测试**不依赖任何被测实现**（ParseModule / ParseELF / InspectArchive
// 都还没写），只用标准库 debug/elf 与 archive/tar 验证测试件本身是对的。
//
// 它存在的理由：如果合成器坏了，artifact_test.go 里的红测试会失败，但失败
// 原因变成"测试件构造错了"而不是"缺实现"——那会让 P1 的实现方向被带偏。
// 先证明测试件可信，红测试才具备基线价值。

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"debug/elf"
	"os"
	"strings"
	"testing"
)

// 合成 ELF 必须能被标准库读回，且 e_machine 与 PT_INTERP 符合设定。
func TestSyntheticELFIsWellFormed(t *testing.T) {
	cases := []struct {
		name    string
		machine uint16
		interp  string
	}{
		{"arm64 无 interp", machineARM64, ""},
		{"amd64 有 interp", machineAMD64, "/lib/ld-linux.so.2"},
		{"arm 有 interp", machineARM, "/lib/ld-linux.so.3"},
		{"riscv64 无 interp", machineRISCV64, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := buildMinimalELF(t, tc.machine, nil, false, syntheticELFOptions{
				machine: tc.machine, withInterp: tc.interp != "", interp: tc.interp,
			})
			f, err := elf.NewFile(bytes.NewReader(b))
			if err != nil {
				t.Fatalf("合成的 ELF 不合法: %v", err)
			}
			if uint16(f.Machine) != tc.machine {
				t.Fatalf("e_machine = %d，期望 %d", f.Machine, tc.machine)
			}
			if f.Class != elf.ELFCLASS64 {
				t.Fatalf("Class = %v，期望 64 位", f.Class)
			}
			got := interpOf(t, f)
			if got != tc.interp {
				t.Fatalf("PT_INTERP = %q，期望 %q", got, tc.interp)
			}
			// 静态判定必须与 PT_INTERP 的有无严格对应——这是被测逻辑的
			// 唯一依据，测试件必须把这一点做实。
			if (got == "") != (tc.interp == "") {
				t.Fatal("PT_INTERP 的有无与预期不符")
			}
		})
	}
}

// 合成的 .modinfo 段必须真的是 null 分隔的 key=value，且能被原样读回。
//
// 这一条最关键：如果分隔符写错（比如用 \n 而不是 \0），P1 的实现照着
// 错的格式做，在真实 .ko 上必然失败。
func TestSyntheticModinfoIsNullSeparated(t *testing.T) {
	path := writeSyntheticKO(t, syntheticKOOptions{
		machine:  machineARM,
		vermagic: "6.6.0 SMP mod_unload aarch64",
		depends:  "can,dev",
		name:     "fec",
	})
	b, err := readFileHelper(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := elf.NewFile(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	sec := f.Section(".modinfo")
	if sec == nil {
		t.Fatal("合成文件里应有 .modinfo 段")
	}
	data, err := sec.Data()
	if err != nil {
		t.Fatal(err)
	}
	// 必须含 null 分隔符。
	if !bytes.Contains(data, []byte{0}) {
		t.Fatalf(".modinfo 必须用 null 分隔，实际内容: %q", data)
	}
	// 解析出的键值对必须齐全。
	kv := parseNullKV(data)
	for _, want := range []string{"vermagic=6.6.0 SMP mod_unload aarch64", "name=fec", "depends=can,dev"} {
		found := false
		for _, got := range kv {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Fatalf(".modinfo 缺少 %q，实际解析出 %v", want, kv)
		}
	}
	// 最后一个字节必须是 null（真实内核就是这么放的）。
	if data[len(data)-1] != 0 {
		t.Fatalf(".modinfo 末尾应为 null，实际 %d", data[len(data)-1])
	}
}

// 同一份输入两次合成必须字节一致——测试要可重复。
func TestSyntheticELFIsDeterministic(t *testing.T) {
	a := buildMinimalELF(t, machineARM64, []byte("vermagic=6.6.0\x00"), false)
	b := buildMinimalELF(t, machineARM64, []byte("vermagic=6.6.0\x00"), false)
	if !bytes.Equal(a, b) {
		t.Fatal("同一输入两次合成的字节不一致，测试将不可重复")
	}
}

// 合成归档必须能被标准库读回，成员名与内容都对。
func TestSyntheticArchiveIsWellFormed(t *testing.T) {
	dir := t.TempDir()
	p := writeSyntheticArchive(t, dir, "ok.tar.gz", []archiveMember{
		{name: "bin/main", machine: machineARM64, mode: 0o755},
		{name: "lib/libfoo.so", machine: machineARM64, mode: 0o755},
	})
	b, err := readFileHelper(p)
	if err != nil {
		t.Fatal(err)
	}
	// gzip 解压。
	zr, err := gzipReader(b)
	if err != nil {
		t.Fatalf("合成归档不是合法 gzip: %v", err)
	}
	tr := tar.NewReader(bytes.NewReader(zr))
	var names []string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
		body := make([]byte, h.Size)
		if _, err := tr.Read(body); err != nil && err.Error() != "EOF" {
			t.Fatalf("读 %s 失败: %v", h.Name, err)
		}
		if _, err := elf.NewFile(bytes.NewReader(body)); err != nil {
			t.Fatalf("归档成员 %s 应是合法 ELF: %v", h.Name, err)
		}
	}
	if len(names) != 2 || names[0] != "bin/main" || names[1] != "lib/libfoo.so" {
		t.Fatalf("归档成员不对: %v", names)
	}
}

// ── 本地辅助（只服务自检，不参与被测逻辑） ──────────────────

func interpOf(t *testing.T, f *elf.File) string {
	t.Helper()
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		buf := make([]byte, p.Filesz)
		n, err := p.ReadAt(buf, 0)
		if err != nil && n == 0 {
			t.Fatalf("读 PT_INTERP 失败: %v", err)
		}
		return strings.TrimRight(string(buf[:n]), "\x00")
	}
	return ""
}

func parseNullKV(data []byte) []string {
	var out []string
	for _, part := range bytes.Split(data, []byte{0}) {
		if len(part) == 0 {
			continue
		}
		if !bytes.Contains(part, []byte("=")) {
			continue
		}
		out = append(out, string(part))
	}
	return out
}

func gzipReader(b []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var out bytes.Buffer
	if _, err := out.ReadFrom(zr); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func readFileHelper(p string) ([]byte, error) { return os.ReadFile(p) }
