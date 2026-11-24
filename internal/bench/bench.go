// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

// Package bench 把装载实验跑成固定列的 CSV，供论文直接取数。
//
// 要解决的问题（论文一句话）：边缘装载的成功率与耗时必须有可复现的量化数据，
// 而"装一次看看"不构成实验。bench 把「形态 × 网络」的矩阵跑成 CSV，
// 列固定因此多次实验可以直接 diff。
//
// 这一包只负责**度量**，不负责真装载：每个组合跑什么由调用方给的 Runner 决定，
// 这里只计时、记成败、记失败阶段。这样测试可以用假 Runner 跑完整流程，
// 生产再把真的装载逻辑接进去。
package bench

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Header 是 CSV 固定表头。
//
// 列固定是为了让多次实验的 CSV 能直接 diff——列名一变，历史数据全部作废。
const Header = "form,network,ok,stage,elapsed_ms,error_code"

// Form 是被装载组件采用的形态。取值与 AIO model.Kind、ferc catalog 对齐。
type Form string

const (
	FormBinary       Form = "binary"
	FormContainer    Form = "container"
	FormKernelModule Form = "kernel-module"
	FormArchive      Form = "archive"
)

// Network 是取包途径。
type Network string

const (
	NetworkOnline Network = "online" // 直连 GitHub / 上游镜像站
	NetworkMirror Network = "mirror" // 内网 HTTP 镜像（ferc mirror serve）
	NetworkUSB    Network = "usb"    // U 盘离线目录
)

// Stage 是装载流程的阶段，用于把失败定位到具体步骤。
type Stage string

const (
	StageResolve  Stage = "resolve"  // 取 release 清单
	StageDownload Stage = "download" // 取产物字节
	StageVerify   Stage = "verify"   // SHA256 校验
	StageExtract  Stage = "extract"  // 解包
	StagePrecheck Stage = "precheck" // 产物预检（架构/内核）
	StageInstall  Stage = "install"  // 落盘
	StageRegister Stage = "register" // 注册托管
)

// Row 是一行实验结果。
type Row struct {
	Form      Form
	Network   Network
	OK        bool
	Stage     Stage
	ElapsedMs int64
	ErrorCode string
}

// String 渲染成 CSV 行。
func (r Row) String() string {
	ok := "false"
	if r.OK {
		ok = "true"
	}
	return strings.Join([]string{
		string(r.Form), string(r.Network), ok, string(r.Stage),
		strconv.FormatInt(r.ElapsedMs, 10), r.ErrorCode,
	}, ",")
}

// Runner 跑一个「形态 × 网络」组合，返回失败阶段与错误编码。
//
// 成功时 stage 为空、code 为空。失败时必须给出 stage——只说"失败了"无法
// 回答"败在哪一步"，而阶段分布正是实验要的东西。
type Runner func(ctx context.Context, form Form, network Network) (Stage, string, error)

// Matrix 返回要跑的「形态 × 网络」组合。
//
// 形态与网络都取可枚举的全集：漏一个组合，论文里的矩阵就有空格，而空格会被
// 追问"为什么这个没测"。
func Matrix() []Row {
	var out []Row
	for _, f := range []Form{FormBinary, FormContainer, FormKernelModule, FormArchive} {
		for _, n := range []Network{NetworkOnline, NetworkMirror, NetworkUSB} {
			out = append(out, Row{Form: f, Network: n})
		}
	}
	return out
}

// Run 按顺序跑矩阵，返回所有行。
func Run(ctx context.Context, r Runner) ([]Row, error) {
	if r == nil {
		return nil, errors.New("bench: 缺少 Runner")
	}
	combos := Matrix()
	out := make([]Row, 0, len(combos))
	for _, c := range combos {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		start := time.Now()
		stage, code, err := r(ctx, c.Form, c.Network)
		elapsed := time.Since(start).Milliseconds()
		row := Row{Form: c.Form, Network: c.Network, Stage: stage, ElapsedMs: elapsed}
		if err != nil {
			row.ErrorCode = code
			if row.ErrorCode == "" {
				row.ErrorCode = "unknown"
			}
			if row.Stage == "" {
				row.Stage = StageResolve
			}
		} else {
			row.OK = true
		}
		out = append(out, row)
	}
	return out, nil
}

// Write 把结果写成 CSV（含固定表头）。
func Write(path string, rows []Row) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write(strings.Split(Header, ",")); err != nil {
		return err
	}
	for _, r := range rows {
		if err := w.Write(strings.Split(r.String(), ",")); err != nil {
			return err
		}
	}
	return w.Error()
}

// Summary 是矩阵结果的汇总，供人直接看。
type Summary struct {
	Total   int
	OK      int
	Failed  int
	ByStage map[Stage]int
	AvgMsOK int64
}

// Summarize 汇总结果。
func Summarize(rows []Row) Summary {
	s := Summary{Total: len(rows), ByStage: map[Stage]int{}}
	var okMs int64
	for _, r := range rows {
		if r.OK {
			s.OK++
			okMs += r.ElapsedMs
			continue
		}
		s.Failed++
		if r.Stage != "" {
			s.ByStage[r.Stage]++
		}
	}
	if s.OK > 0 {
		s.AvgMsOK = okMs / int64(s.OK)
	}
	return s
}

// Format 渲染成人类可读的一行。
func (s Summary) Format() string {
	return fmt.Sprintf("共 %d 组合：成功 %d、失败 %d、成功平均 %d ms",
		s.Total, s.OK, s.Failed, s.AvgMsOK)
}
