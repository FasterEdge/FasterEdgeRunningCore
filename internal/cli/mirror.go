// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

package cli

import (
	"context"
	"flag"
	"fmt"
	"runtime"
	"strings"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/fetch"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/mirror"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/platform"
)

// mirrorExportReport 是 `ferc mirror export --json` 的输出。
type mirrorExportReport struct {
	Base     string               `json:"base"`
	Archs    []string             `json:"archs,omitempty"`
	Repos    []mirror.ExportEntry `json:"repos,omitempty"`
	Warnings []string             `json:"warnings,omitempty"`
}

// cmdMirror 实现 `ferc mirror export|serve`。
//
// 这一对命令解决的是“有一批设备压根不通外网”：能上网的那台先 export 出
// 一份可整体搬迁的目录，再 serve 起来；下游设备 `ferc install --from <它>`
// 就能装。全程不需要下游有任何凭据。
func cmdMirror(ctx context.Context, args []string, env Env) int {
	if len(args) == 0 {
		fmt.Fprintln(env.stderr(), "用法: ferc mirror export|serve [选项]")
		return 2
	}
	action, rest := args[0], args[1:]
	switch action {
	case "export":
		return cmdMirrorExport(ctx, rest, env)
	case "serve":
		return cmdMirrorServe(ctx, rest, env)
	default:
		fmt.Fprintf(env.stderr(), "未知 mirror 子命令 %q（可用: export、serve）\n", action)
		return 2
	}
}

func cmdMirrorExport(ctx context.Context, args []string, env Env) int {
	fs := flag.NewFlagSet("mirror export", flag.ContinueOnError)
	fs.SetOutput(env.stderr())
	var c common
	bindCommon(fs, &c, env)
	var dir string
	var archs multiValue
	fs.StringVar(&dir, "dir", "", "导出目录（必填）")
	fs.Var(&archs, "arch", "只导出指定架构的产物，可重复")
	want, err := parseFlagsAnywhere(fs, args)
	if err != nil {
		return 2
	}
	if dir == "" {
		fmt.Fprintln(env.stderr(), "请用 --dir <目录> 指定导出目标")
		return 2
	}

	repos, err := mirrorRepos(c, want)
	if err != nil {
		fmt.Fprintf(env.stderr(), "%v\n", err)
		return 1
	}

	info, err := env.detect(ctx)
	if err != nil {
		fmt.Fprintf(env.stderr(), "探测失败: %v\n", err)
		return 1
	}
	src := mirrorExportSource(env, &c, info)

	fmt.Fprintf(env.stdout(), "导出到 %s（源: %s）\n", dir, sourceName(c))
	res, err := mirror.Export(ctx, src, mirror.ExportOptions{
		Base:  dir,
		Repos: repos,
		Archs: archs,
		Log: func(f string, a ...any) {
			fmt.Fprintf(env.stdout(), f+"\n", a...)
		},
	})
	if err != nil {
		fmt.Fprintf(env.stderr(), "导出失败: %v\n", err)
		return 1
	}
	if c.json {
		writeJSON(env.stdout(), mirrorExportReport{
			Base:  dir,
			Archs: archs,
			Repos: res.Repos,
		})
		return 0
	}
	for _, e := range res.Repos {
		fmt.Fprintf(env.stdout(), "  %s %s（%d 个产物）\n", e.Repo, e.Tag, len(e.Assets))
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(env.stderr(), "  告警: %s\n", w)
	}
	fmt.Fprintf(env.stdout(), "完成：%d 个仓库，共 %d 个产物\n", len(res.Repos), totalAssets(res))
	fmt.Fprintf(env.stdout(), "\n下游用法：\n")
	fmt.Fprintf(env.stdout(), "  本机/局域网:  ferc install <组件> --from %s\n", dir)
	fmt.Fprintf(env.stdout(), "  跨网段:       在上游执行 `ferc mirror serve --dir %s`，\n", dir)
	fmt.Fprintf(env.stdout(), "                下游用 `ferc install <组件> --from http://<上游>:8090`\n")
	return 0
}

func cmdMirrorServe(ctx context.Context, args []string, env Env) int {
	fs := flag.NewFlagSet("mirror serve", flag.ContinueOnError)
	fs.SetOutput(env.stderr())
	var c common
	bindCommon(fs, &c, env)
	var dir, addr string
	fs.StringVar(&dir, "dir", "", "镜像目录（必填）")
	fs.StringVar(&addr, "addr", "0.0.0.0:8090", "监听地址")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if dir == "" {
		fmt.Fprintln(env.stderr(), "请用 --dir <目录> 指定要分享的镜像目录")
		return 2
	}
	// Ctrl-C / kill 时 ctx 取消，Serve 会优雅退出；ferc 自己仍是非常驻工具。
	err := mirror.Serve(ctx, mirror.ServeOptions{
		Base: dir,
		Addr: addr,
		Log: func(f string, a ...any) {
			fmt.Fprintf(env.stdout(), f+"\n", a...)
		},
	})
	if err != nil {
		fmt.Fprintf(env.stderr(), "%v\n", err)
		return 1
	}
	return 0
}

// mirrorRepos 决定这次要导出哪些仓库。
//
// 位置参数给的是**组件名**（用户在 ferc list 里看到的那个），必须经清单翻成
// owner/name 才能拿去问 GitHub：组件名是小写连字符（any2utf8），仓库名是
// 组织下的驼峰（FasterEdge/Any2UTF8），两者不是一回事。直接拿组件名当仓库名
// 会得到一个空镜像，而且失败被记成告警、不报错，现场很难看出问题。
func mirrorRepos(c common, want []string) ([]string, error) {
	cat, err := c.loadCatalog()
	if err != nil {
		return nil, err
	}
	if len(want) > 0 {
		out := make([]string, 0, len(want))
		for _, name := range want {
			comp, ok := cat.Get(name)
			if !ok {
				return nil, fmt.Errorf("未知组件 %q（用 ferc list 看有哪些）", name)
			}
			repo := comp.Repo(runtime.GOOS)
			if repo == "" {
				return nil, fmt.Errorf("组件 %q 没有 %s 平台的产物仓库", name, runtime.GOOS)
			}
			out = append(out, repo)
		}
		return out, nil
	}
	out := make([]string, 0, len(cat.Components))
	for _, m := range cat.Components {
		out = append(out, m.Repo(runtime.GOOS))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("清单里没有任何组件")
	}
	return out, nil
}

// mirrorExportSource 决定导出时从哪儿取包。
//
// `--from` 是“二级镜像再转发”（比如 U 盘 → 上游节点 → 现场）：上游节点
// 自己没有外网时，也要能把 U 盘上的目录搬上局域网，所以这里同样支持。
func mirrorExportSource(env Env, c *common, info platform.Info) fetch.Source {
	if c.mirrorBase != "" {
		if c.mirrorChain {
			return mirror.NewChain(
				[]string{"mirror", "github"},
				[]fetch.Source{mirror.New(c.mirrorBase), fetch.New(c.token)}, nil)
		}
		return mirror.New(c.mirrorBase)
	}
	return fetch.New(c.token)
}

func sourceName(c common) string {
	if c.mirrorBase == "" {
		return "GitHub"
	}
	return c.mirrorBase
}

func totalAssets(res *mirror.ExportResult) int {
	n := 0
	for _, e := range res.Repos {
		n += len(e.Assets)
	}
	return n
}

// multiValue 让 --arch 可以重复写，也可以逗号分隔。
type multiValue []string

func (m *multiValue) String() string { return strings.Join(*m, ",") }

func (m *multiValue) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			*m = append(*m, part)
		}
	}
	return nil
}
