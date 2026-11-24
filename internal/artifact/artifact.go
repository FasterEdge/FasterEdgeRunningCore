// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

// Package artifact 解析产物文件，回答"这个文件到底是什么、能不能装到目标节点上"。
//
// 要解决的问题：装载流程此前只看文件名与 SHA256，从不打开产物内容。于是
// "文件名写着 arm64、内容其实是 amd64"或者"内核模块的 vermagic 与目标内核
// 不匹配"这类问题，只能等到装完跑不起来才发现——而在边缘现场，发现即意味着
// 一次上门。
//
// 三条硬规则（改动时不许放松）：
//
//  1. **vermagic 精确字符串相等**，不做版本模糊比较。vermagic 含内核版本、
//     SMP/PREEMPT 等编译开关甚至 GCC 版本，任一项不同 insmod 都会报
//     invalid module format。模糊比较等于制造假阳性。
//  2. **Static 只看有没有 PT_INTERP 段**，不看文件名、不看大小、不看后缀。
//  3. **三类错误可区分，不得合并**：缺 .modinfo 段 / 段内无 vermagic /
//     非预期 e_machine。它们对应完全不同的处置方式，合成一个
//     "parse failed" 会把人引向错误的方向。
package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ModuleInfo 是内核模块的解析结果。
type ModuleInfo struct {
	// Arch 是由 ELF e_machine 推出的架构名（归一后，与 AIO model.Arch 同一套名字）。
	Arch string
	// Vermagic 是 .modinfo 里 vermagic= 的原始值。精确比对用。
	Vermagic string
	// Release 是 vermagic 的首个空格前的部分，即内核版本，例如 6.6.0。
	Release string
	// Flags 是 .modinfo 里除 vermagic 外的键值对，例如 name、depends、license。
	Flags map[string]string
	// Depends 是模块依赖列表，来自 depends= 字段（逗号分隔）。
	Depends []string
	// Name 是模块名，来自 name= 字段；缺该字段时为空。
	Name string
}

// ELFInfo 是普通 ELF 可执行文件/共享库的解析结果。
type ELFInfo struct {
	// Arch 是由 ELF e_machine 推出的架构名。
	Arch string
	// Static 为真表示没有 PT_INTERP 段，即静态链接。
	Static bool
	// Interp 是 PT_INTERP 段指向的解释器路径（动态链接时非空）。
	Interp string
}

// ArchiveResult 是归档包的检查结果。
type ArchiveResult struct {
	// Members 是归档内的 ELF 文件清单（按归档内路径）。
	Members []ArchiveMember
	// Conflicts 是架构与期望不一致的成员路径列表。
	Conflicts []string
}

// ArchiveMember 是归档内的一个 ELF 成员。
type ArchiveMember struct {
	// Path 是归档内路径，例如 bin/main。
	Path string
	// Arch 是该成员的架构。
	Arch string
	// Static 为该成员是否静态链接。
	Static bool
}

// 三类必须可区分的错误。调用方据此分流处置，不允许合成一个 "parse failed"。
var (
	// ErrNoModinfo 表示 ELF 里没有 .modinfo 段。
	ErrNoModinfo = errors.New("artifact: ELF 缺少 .modinfo 段")
	// ErrNoVermagic 表示有 .modinfo 段但段内没有 vermagic 键。
	ErrNoVermagic = errors.New("artifact: .modinfo 段内没有 vermagic")
	// ErrUnexpectedMachine 表示 e_machine 不是预期的架构。
	ErrUnexpectedMachine = errors.New("artifact: 非预期的 ELF e_machine")
)

// ── 选项 ────────────────────────────────────────────────────

type options struct {
	expectArch string
}

// Option 修改解析行为。
type Option func(*options)

// WithExpectArch 指定期望架构。
//
// 传空串表示不校验架构（只解析）。校验放在最前面：一个 amd64 的 .ko 打到 arm
// 节点上，最该说的是"架构不对"，而不是"缺 .modinfo"——后者会把人引向错误的方向。
func WithExpectArch(arch string) Option {
	return func(o *options) { o.expectArch = arch }
}

// ── 解析入口 ────────────────────────────────────────────────

// ParseModule 解析一个内核模块（.ko）。
func ParseModule(path string, opts ...Option) (*ModuleInfo, error) {
	var o options
	for _, fn := range opts {
		fn(&o)
	}

	f, err := elf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("artifact: 打开 %s 失败: %w", path, err)
	}
	defer f.Close()

	arch := archOfMachine(f.Machine)
	// 架构校验必须早于任何内容检查。
	if o.expectArch != "" && arch != o.expectArch {
		return nil, fmt.Errorf("%w: 产物是 %s，期望 %s", ErrUnexpectedMachine, arch, o.expectArch)
	}

	sec := f.Section(".modinfo")
	if sec == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoModinfo, path)
	}
	data, err := sec.Data()
	if err != nil {
		return nil, fmt.Errorf("artifact: 读 .modinfo 失败: %w", err)
	}

	kv := parseModinfo(data)
	vermagic, ok := kv["vermagic"]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoVermagic, path)
	}

	m := &ModuleInfo{
		Arch:     arch,
		Vermagic: vermagic,
		Release:  releaseOf(vermagic),
		Flags:    map[string]string{},
		Name:     kv["name"],
	}
	for k, v := range kv {
		if k == "vermagic" {
			continue
		}
		m.Flags[k] = v
	}
	if dep, ok := kv["depends"]; ok && dep != "" {
		for _, d := range strings.Split(dep, ",") {
			d = strings.TrimSpace(d)
			if d != "" {
				m.Depends = append(m.Depends, d)
			}
		}
	}
	return m, nil
}

// ParseELF 解析一个普通 ELF，判断架构与是否静态链接。
//
// Static 的判定只看 PT_INTERP 段的有无：动态链接的可执行文件一定带这个段，
// 静态链接的不带。刻意不看文件名、大小、后缀——那些都能骗人。
func ParseELF(path string, opts ...Option) (*ELFInfo, error) {
	var o options
	for _, fn := range opts {
		fn(&o)
	}

	f, err := elf.Open(path)
	if err != nil {
		return nil, fmt.Errorf("artifact: 打开 %s 失败: %w", path, err)
	}
	defer f.Close()

	arch := archOfMachine(f.Machine)
	if o.expectArch != "" && arch != o.expectArch {
		return nil, fmt.Errorf("%w: 产物是 %s，期望 %s", ErrUnexpectedMachine, arch, o.expectArch)
	}

	info := &ELFInfo{Arch: arch, Static: true}
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		info.Static = false
		info.Interp = readInterp(p)
		break
	}
	return info, nil
}

// InspectArchive 递归检查一个 tar.gz 归档内的所有 ELF。
//
// 端侧现场常见：一个包里同时装了多个架构的二进制（发布脚本图省事），或者
// main 与 libfoo.so 架构不一致。这类包装在目标节点上表现成"主程序能起、
// 一加载库就 exec format error"——难查得很。这里在装之前就把冲突列出来。
func InspectArchive(path string, opts ...Option) (*ArchiveResult, error) {
	var o options
	for _, fn := range opts {
		fn(&o)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("artifact: 打开归档失败: %w", err)
	}
	defer f.Close()

	res := &ArchiveResult{}

	// 先试 gzip；不是 gzip 就当普通 tar（现场两种都有）。
	var tr *tar.Reader
	if gr, err := gzip.NewReader(f); err == nil {
		tr = tar.NewReader(gr)
	} else {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		tr = tar.NewReader(f)
	}

	// 归档要能放进内存才能逐成员解析；这里设一个上限，防 tar 炸弹。
	const maxMembers = 4096
	const maxMemberSize = 512 << 20 // 512 MiB
	n := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("artifact: 读归档失败: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		n++
		if n > maxMembers {
			return nil, fmt.Errorf("artifact: 归档成员超过 %d 个，拒绝继续解析", maxMembers)
		}
		if h.Size > maxMemberSize {
			return nil, fmt.Errorf("artifact: 成员 %s 超过 %d 字节，拒绝解析", h.Name, maxMemberSize)
		}
		body, err := io.ReadAll(io.LimitReader(tr, maxMemberSize))
		if err != nil {
			return nil, fmt.Errorf("artifact: 读成员 %s 失败: %w", h.Name, err)
		}
		if !looksELF(body) {
			continue
		}
		info, err := parseELFBytes(body)
		if err != nil {
			// 单个成员解析失败不该让整包检查失败：它可能只是个恰好像 ELF 的
			// 数据文件。记进 Conflicts 会误导，因此跳过但继续检查其余成员。
			continue
		}
		res.Members = append(res.Members, ArchiveMember{
			Path: h.Name, Arch: info.Arch, Static: info.Static,
		})
		if o.expectArch != "" && info.Arch != o.expectArch {
			res.Conflicts = append(res.Conflicts, h.Name)
		}
	}
	return res, nil
}

// VermagicMatches 精确比较两个 vermagic。
//
// 刻意不做任何规范化：不 trim 空格、不拆版本号、不比前缀。尾部多一个空格
// 也必须判为不匹配——内核就是这么严的，我们放松一分，现场就多一次上门。
func VermagicMatches(got, want string) bool {
	return got == want
}

// ── 内部实现 ────────────────────────────────────────────────

// parseModinfo 解析 .modinfo 段。
//
// 格式是 null 分隔的 key=value 序列，通常以一个 null 结尾。这里严格按
// "null 分隔 + = 首次出现处切分"解析：value 里允许含 = ，只有第一个 = 是分隔符。
func parseModinfo(data []byte) map[string]string {
	out := map[string]string{}
	for _, part := range bytes.Split(data, []byte{0}) {
		if len(part) == 0 {
			continue
		}
		s := string(part)
		i := strings.IndexByte(s, '=')
		if i <= 0 {
			continue
		}
		out[s[:i]] = s[i+1:]
	}
	return out
}

// releaseOf 取 vermagic 首个空格前的部分。
//
// vermagic 形如 "6.6.0 SMP mod_unload modversions aarch64"，首个空格前才是
// 内核版本。不 trim：尾随空格是 .modinfo 里的真实内容，Vermagic 字段要原样
// 保留才能精确比对。
func releaseOf(vermagic string) string {
	if i := strings.IndexByte(vermagic, ' '); i >= 0 {
		return vermagic[:i]
	}
	return vermagic
}

// readInterp 取出 PT_INTERP 段指向的路径。
func readInterp(p *elf.Prog) string {
	buf := make([]byte, p.Filesz)
	n, err := p.ReadAt(buf, 0)
	if err != nil && n == 0 {
		return ""
	}
	return strings.TrimRight(string(buf[:n]), "\x00")
}

// parseELFBytes 从内存里的 ELF 字节解析（归档检查用）。
func parseELFBytes(b []byte) (*ELFInfo, error) {
	f, err := elf.NewFile(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	info := &ELFInfo{Arch: archOfMachine(f.Machine), Static: true}
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		info.Static = false
		// io.ReadAll 比手工 ReadAt 稳：PT_INTERP 的内容通常只有几十字节，
		// 没有必要为它管理偏移量。
		b, err := io.ReadAll(p.Open())
		if err == nil {
			info.Interp = strings.TrimRight(string(b), "\x00")
		}
		break
	}
	return info, nil
}

// looksELF 用魔数快速判断一段字节是不是 ELF。
//
// 先看魔数再整体解析：归档里绝大多数成员不是 ELF（配置、说明、脚本），
// 逐个 NewFile 会白烧 CPU。
func looksELF(b []byte) bool {
	return len(b) >= 4 && b[0] == 0x7f && b[1] == 'E' && b[2] == 'L' && b[3] == 'F'
}

// archOfMachine 把 ELF e_machine 映射成归一架构名。
//
// 归一标准与 AIO 的 model.Arch、ferc 的 platform.NormalizeArch 一致：
// 全链路只认一套名字，避免"这份产物是 armv7、那个节点是 arm"这种对不上的事。
func archOfMachine(m elf.Machine) string {
	switch m {
	case elf.EM_386:
		return "386"
	case elf.EM_X86_64:
		return "amd64"
	case elf.EM_ARM:
		// 32 位 ARM 的 ELF 头不带 GOARM 信息，无法区分 armv6/armv7。
		// 统一报 armv7：这是最常见的目标，且调用方若需要精确判定，
		// 应由产物文件名或额外元数据提供（ELF 本身给不出）。
		return "armv7"
	case elf.EM_AARCH64:
		return "arm64"
	case elf.EM_RISCV:
		return "riscv64"
	case elf.EM_LOONGARCH:
		return "loong64"
	case elf.EM_MIPS:
		return "mips"
	case elf.EM_PPC64:
		return "ppc64le"
	case elf.EM_S390:
		return "s390x"
	}
	// 未登记的架构：用数字报出来，而不是硬套一个相似的名字。
	// 认不出比认错安全——认错会让"装得上"的误判通过校验。
	return fmt.Sprintf("machine-%d", uint16(m))
}

var _ = filepath.Join
