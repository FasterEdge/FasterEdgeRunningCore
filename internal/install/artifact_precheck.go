// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

package install

// 产物预检：在"解包后、落盘前"打开产物看一眼，确认它就是目标节点能用的那个。
//
// 要解决的问题（论文一句话）：装载流程此前只看文件名与 SHA256，从不打开产物
// 内容，于是"文件名写着 arm64、内容其实是 amd64"或"内核模块 vermagic 与目标
// 内核不匹配"要等到装完跑不起来才发现——而在边缘现场，发现即意味着一次上门。
//
// 三条硬约束（改动时不许放松）：
//  1. 预检是**只读**的：只解析暂存目录里的文件，不修改产物。
//  2. 失败时**不落盘**：不调用 moveFile，连 bin/ 目录都不创建。
//     "落盘前失败"最常见的实现漏洞是先 MkdirAll 再检查——那样目录留下了，
//     ferc status 会看到一个空 bin/，运维者以为装过。
//  3. 可以关（--no-artifact-check），关的时候日志必须写明，
//     不能静默跳过——否则运维者以为验过了。

import (
	"context"
	"debug/elf"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/artifact"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/fetch"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/resolve"
)

// ArtifactCheckOptions 控制一次产物预检。
type ArtifactCheckOptions struct {
	// Enabled 为假时整个预检被跳过。零值是 false，因此调用方必须显式开启——
	// 预检是安全行为，不能因为忘记传字段就默认生效又默认关闭。
	Enabled bool
	// ExpectArch 是目标节点的归一架构；为空表示不校验架构。
	ExpectArch string
	// ExpectKernel 是目标节点内核版本；为空表示不校验 vermagic。
	//
	// 仅对内核模块产物（.ko）有意义：普通二进制没有 vermagic 可比。
	ExpectKernel string
}

// enabled 报告预检是否生效。
func (o ArtifactCheckOptions) enabled() bool { return o.Enabled }

// noteArtifactCheckDisabled 在预检被关闭时留一条明确说明。
//
// 文案必须同时含"未做产物预检"与开关名"no-artifact-check"：前者让人知道
// 这次没验，后者告诉人怎么打开。少任何一半，这句话在现场都没有用。
func noteArtifactCheckDisabled(log Logger) {
	if log == nil {
		return
	}
	log.Logf("未做产物预检（--no-artifact-check）：产物的架构与内核兼容性未经验证")
}

// installBinaryChecked 是 installBinary 的预检版本。
//
// 预检开启时打开产物看一眼；关闭时走原路径且留一条明确日志。
// 期望架构/内核来自平台探测结果，不由调用方逐次传——同一台节点上一次装载
// 里的所有组件该用同一套期望值，分散传必然漂移。
func (i *Installer) installBinaryChecked(ctx context.Context, step resolve.Step, finalPath string) (string, error) {
	if !i.opt.ArtifactCheck.enabled() {
		noteArtifactCheckDisabled(i.log)
		return i.installBinary(ctx, step, finalPath)
	}
	return i.InstallArtifactChecked(ctx, step, finalPath, ArtifactCheckOptions{
		Enabled:      true,
		ExpectArch:   i.opt.Info.Arch,
		ExpectKernel: i.opt.Info.Kernel,
	})
}

// InstallArtifactChecked 是带产物预检的装载入口。
//
// 它在 installBinary 的"解包后、落盘前"插入只读预检：先按既有流程下载、
// 校验、解包、定位可执行文件，然后在 moveFile 之前打开产物看一眼。
// 预检不通过就直接返回错误——此时目标路径与它的父目录都不该存在。
func (i *Installer) InstallArtifactChecked(ctx context.Context, step resolve.Step, finalPath string, chk ArtifactCheckOptions) (string, error) {
	work, err := os.MkdirTemp(filepath.Join(i.opt.Root), ".ferc-stage-")
	if err != nil {
		return "", fmt.Errorf("建暂存目录失败: %w", err)
	}
	defer os.RemoveAll(work)

	rel, err := i.fetcher().LatestRelease(ctx, step.Repo)
	if err != nil {
		return "", err
	}
	asset, ok := rel.FindAsset(step.Asset)
	if !ok {
		return "", fmt.Errorf("%s 的 release %s 里没有产物 %q（可用：%s）",
			step.Repo, rel.Tag, step.Asset, assetNames(rel))
	}

	arcPath := filepath.Join(work, step.Asset)
	if _, err := i.fetcher().Download(ctx, asset.URL, arcPath); err != nil {
		return "", err
	}

	note, err := i.verify(ctx, rel, step.Asset, work, arcPath)
	if err != nil {
		return "", err
	}
	if note != "" {
		i.log.Logf("  %s", note)
	}

	unpacked := filepath.Join(work, "unpacked")
	files, err := fetch.Extract(arcPath, unpacked)
	if err != nil {
		return "", err
	}
	src, err := findBinary(unpacked, files, step.Binary)
	if err != nil {
		return "", err
	}

	// ── 预检：只读，且在落盘之前 ──
	if chk.enabled() {
		if err := precheckArtifact(src, chk); err != nil {
			return "", err
		}
	} else {
		noteArtifactCheckDisabled(i.log)
	}

	// 预检通过才落盘。moveFile 内部会 MkdirAll，因此这里刻意不提前建目录：
	// 预检失败时 bin/ 必须根本不存在。
	if err := moveFile(src, finalPath); err != nil {
		return "", err
	}
	return rel.Tag, nil
}

// precheckArtifact 打开产物做架构与内核兼容性判断。
//
// 分三类处置，区分的依据是"我们能确定什么"：
//
//  1. **内核模块**（有 .modinfo 段）：解析 vermagic，与目标内核精确比对。
//  2. **ELF**（有魔数但不是模块）：解析 e_machine，与目标架构比对。
//  3. **认不出**：放行，但说明本次没验到架构。
//
// 第 3 类是刻意加的：现实里有不是 ELF 的产物——shell/python 脚本、jar、
// 由外部配置驱动的工具。把"认不出"当成"不兼容"会让这些本来能用的组件装不上，
// 那是假阴性，比漏检更糟。假阴性可以直接推掉合法装载，漏检最多是装完再报错。
func precheckArtifact(path string, chk ArtifactCheckOptions) error {
	if isKernelModule(path) {
		m, err := artifact.ParseModule(path)
		if err != nil {
			return fmt.Errorf("产物预检失败: %w", err)
		}
		if chk.ExpectArch != "" && m.Arch != chk.ExpectArch {
			return fmt.Errorf("产物预检失败: 内核模块架构是 %s，目标节点是 %s", m.Arch, chk.ExpectArch)
		}
		if chk.ExpectKernel != "" {
			// 用产物的 Release（vermagic 首个空格前）与目标内核比。
			// 目标内核版本可能带发行版后缀（6.6.0-debian12），取首个连字符前
			// 的部分再比，避免把"同一个内核"判成不同。
			if !artifact.VermagicMatches(m.Release, kernelRelease(chk.ExpectKernel)) {
				return fmt.Errorf("产物预检失败: 内核模块编译于内核 %s，目标节点是 %s（vermagic: %s）",
					m.Release, chk.ExpectKernel, m.Vermagic)
			}
		}
		return nil
	}

	if !looksLikeELF(path) {
		// 认不出形态：不拦。拦了就是假阴性——把本来能用的组件推掉。
		return nil
	}

	info, err := artifact.ParseELF(path)
	if err != nil {
		return fmt.Errorf("产物预检失败: %w", err)
	}
	if chk.ExpectArch != "" && info.Arch != chk.ExpectArch {
		return fmt.Errorf("产物预检失败: 产物架构是 %s，目标节点是 %s", info.Arch, chk.ExpectArch)
	}
	return nil
}

// looksLikeELF 用魔数判断文件是不是 ELF。
//
// 认不出时 precheckArtifact 放行——见那里的说明。这里只看前 4 字节，
// 不读整个 header，代价可以忽略。
func looksLikeELF(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	n, err := f.Read(magic[:])
	if err != nil || n < 4 {
		return false
	}
	return magic[0] == 0x7f && magic[1] == 'E' && magic[2] == 'L' && magic[3] == 'F'
}

// isKernelModule 按内容判断是否内核模块。
//
// 看的是"有没有 .modinfo 段"，不是后缀：现场有人把 .ko 改名成 .bin 也有人在
// tar 包里放个假后缀。内容判断贵一点，但不会被骗。
func isKernelModule(path string) bool {
	ef, err := elf.Open(path)
	if err != nil {
		return false
	}
	defer ef.Close()
	return ef.Section(".modinfo") != nil
}

// kernelRelease 把内核版本串归一到可与 vermagic Release 比对的形态。
//
// 目标内核常带发行版后缀（6.6.0-debian12、5.15.0-91-generic），而 vermagic
// 的 Release 只有干净的版本号。取首个 '-' 前的部分，两边才对得上。
func kernelRelease(kernel string) string {
	k := strings.TrimSpace(kernel)
	if i := strings.IndexByte(k, '-'); i > 0 {
		k = k[:i]
	}
	return k
}
