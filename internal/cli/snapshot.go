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
	"sort"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/platform"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/snapshot"
	"github.com/FasterEdge/FasterEdgeRunningCore/internal/state"
)

// snapshotReport 是 `ferc snapshot list --json` 的输出。
type snapshotReport struct {
	Snapshots []snapshot.Summary `json:"snapshots"`
	Base      string             `json:"base"`
}

// snapshotCreateReport 是 `ferc snapshot create --json` 的输出。
type snapshotCreateReport struct {
	ID         string   `json:"id"`
	Components int      `json:"components"`
	Missing    int      `json:"missing"`
	Bytes      int64    `json:"bytes"`
	Warnings   []string `json:"warnings,omitempty"`
}

// cmdSnapshot 实现 `ferc snapshot create|list|restore|delete`。
//
// 它回答的是“改错了怎么退回去”。装载事务回滚只在一次 install 内部有效，
// 而现实中更多的事故发生在装载成功之后：改错端口、删了配置、装了个不兼容的
// 新版本。快照让运维者能主动打点，并在事后把整台端退回到那个点。
func cmdSnapshot(ctx context.Context, args []string, env Env) int {
	if len(args) == 0 {
		fmt.Fprintln(env.stderr(), "用法: ferc snapshot create|list|restore|delete [选项]")
		return 2
	}
	action, rest := args[0], args[1:]
	switch action {
	case "create":
		return cmdSnapshotCreate(ctx, rest, env)
	case "list":
		return cmdSnapshotList(ctx, rest, env)
	case "restore":
		return cmdSnapshotRestore(ctx, rest, env)
	case "delete":
		return cmdSnapshotDelete(ctx, rest, env)
	default:
		fmt.Fprintf(env.stderr(), "未知 snapshot 子命令 %q（可用: create、list、restore、delete）\n", action)
		return 2
	}
}

func snapshotBase(root string) string {
	// 快照放在数据根目录下：与 bin/、logs/、ferc-state.json 同级，
	// 运维者一眼能看出“这台端的快照在哪”，备份时也不会漏掉它。
	return root + "/snapshots"
}

func cmdSnapshotCreate(ctx context.Context, args []string, env Env) int {
	fs := flag.NewFlagSet("snapshot create", flag.ContinueOnError)
	fs.SetOutput(env.stderr())
	var c common
	bindCommon(fs, &c, env)
	var note string
	fs.StringVar(&note, "note", "", "快照备注，例如“升级前”")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	st, err := state.Load(state.Path(c.root))
	if err != nil {
		fmt.Fprintf(env.stderr(), "%v\n", err)
		return 1
	}
	if len(st.Components) == 0 {
		fmt.Fprintln(env.stderr(), "没有已装载的组件，无需打快照")
		return 1
	}

	if !c.json {
		fmt.Fprintf(env.stdout(), "打快照（数据根目录 %s）\n", c.root)
	}
	res, err := snapshot.Create(st, state.Path(c.root), Version, snapshot.CreateOptions{
		Base: snapshotBase(c.root),
		Note: note,
		Log: func(f string, a ...any) {
			if !c.json {
				fmt.Fprintf(env.stdout(), f+"\n", a...)
			}
		},
	})
	if err != nil {
		fmt.Fprintf(env.stderr(), "打快照失败: %v\n", err)
		return 1
	}
	if c.json {
		return writeJSON(env.stdout(), snapshotCreateReport{
			ID: res.ID, Components: res.Components, Missing: res.Missing, Bytes: res.Bytes,
		})
	}
	fmt.Fprintf(env.stdout(), "\n快照 %s 已保存（%d 个组件，%s）\n",
		res.ID, res.Components, humanBytes(res.Bytes))
	if note != "" {
		fmt.Fprintf(env.stdout(), "备注: %s\n", note)
	}
	if res.Missing > 0 {
		fmt.Fprintf(env.stderr(), "注意: %d 个组件的二进制缺失，这份快照不完整\n", res.Missing)
	}
	fmt.Fprintf(env.stdout(), "回归: ferc snapshot restore %s\n", res.ID)
	return 0
}

func cmdSnapshotList(ctx context.Context, args []string, env Env) int {
	fs := flag.NewFlagSet("snapshot list", flag.ContinueOnError)
	fs.SetOutput(env.stderr())
	var c common
	bindCommon(fs, &c, env)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	list, err := snapshot.List(snapshotBase(c.root))
	if err != nil {
		fmt.Fprintf(env.stderr(), "%v\n", err)
		return 1
	}
	if c.json {
		if list == nil {
			list = []snapshot.Summary{}
		}
		return writeJSON(env.stdout(), snapshotReport{Snapshots: list, Base: snapshotBase(c.root)})
	}
	if len(list) == 0 {
		fmt.Fprintf(env.stdout(), "没有快照（快照目录 %s）\n", snapshotBase(c.root))
		return 0
	}
	fmt.Fprintf(env.stdout(), "快照（数据根目录 %s）\n\n", c.root)
	for _, s := range list {
		fmt.Fprintf(env.stdout(), "%s  %s  %d 个组件  %s",
			s.ID, s.CreatedAt.Local().Format("2006-01-02 15:04:05"), s.Components, humanBytes(s.Bytes))
		if s.Missing > 0 {
			fmt.Fprintf(env.stdout(), "  （%d 个二进制缺失）", s.Missing)
		}
		if s.Hostname != "" {
			fmt.Fprintf(env.stdout(), "  @%s %s/%s", s.Hostname, s.GOOS, s.Arch)
		}
		if s.Note != "" {
			fmt.Fprintf(env.stdout(), "  “%s”", s.Note)
		}
		fmt.Fprintln(env.stdout())
	}
	fmt.Fprintf(env.stdout(), "\n回归: ferc snapshot restore <id>（不带 id 用最新的一份）\n")
	return 0
}

func cmdSnapshotRestore(ctx context.Context, args []string, env Env) int {
	fs := flag.NewFlagSet("snapshot restore", flag.ContinueOnError)
	fs.SetOutput(env.stderr())
	var c common
	bindCommon(fs, &c, env)
	var id string
	fs.StringVar(&id, "id", "", "快照 ID；为空表示最新的一份")
	want, err := parseFlagsAnywhere(fs, args)
	if err != nil {
		return 2
	}
	// 也接受位置参数写法：ferc snapshot restore 20261124-101530-ab12cd
	if id == "" && len(want) > 0 {
		id = want[0]
	}

	info, err := env.detect(ctx)
	if err != nil {
		fmt.Fprintf(env.stderr(), "探测失败: %v\n", err)
		return 1
	}

	if !c.json {
		if id == "" {
			fmt.Fprintf(env.stdout(), "回归到最新快照（数据根目录 %s）\n", c.root)
		} else {
			fmt.Fprintf(env.stdout(), "回归到快照 %s（数据根目录 %s）\n", id, c.root)
		}
	}
	res, err := snapshot.Restore(ctx, snapshot.RestoreOptions{
		Base: snapshotBase(c.root),
		ID:   id,
		Root: c.root,
		// 用探测到的平台而不是 runtime.*：ferc 常在工作站上管理别的架构的节点。
		Hostname: info.Hostname,
		GOOS:     info.GOOS,
		Arch:     info.Arch,
		Log: func(f string, a ...any) {
			if !c.json {
				fmt.Fprintf(env.stdout(), f+"\n", a...)
			}
		},
	})
	if err != nil {
		fmt.Fprintf(env.stderr(), "回归失败: %v\n", err)
		if c.json && res != nil {
			writeJSON(env.stdout(), res)
		}
		return 1
	}

	// 二进制与状态文件都归位了，但托管关系还没重建：重启后没有任何进程记得
	// 组件该怎么起来。这里直接复用 restore 的同一套逻辑，不重复实现。
	if !c.dryRun && res.Restored > 0 {
		if code := replaySupervision(ctx, &c, env, info); code != 0 && !c.json {
			fmt.Fprintf(env.stderr(), "警告: 托管关系重放未完全成功（%d），可手工执行 ferc restore\n", code)
		}
	}

	if c.json {
		return writeJSON(env.stdout(), res)
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(env.stderr(), "警告: %s\n", w)
	}
	fmt.Fprintf(env.stdout(), "\n已回归到快照 %s（%s）：还原 %d 个组件",
		res.ID, res.CreatedAt, res.Restored)
	if res.Note != "" {
		fmt.Fprintf(env.stdout(), "，备注“%s”", res.Note)
	}
	fmt.Fprintln(env.stdout())
	return 0
}

func cmdSnapshotDelete(ctx context.Context, args []string, env Env) int {
	fs := flag.NewFlagSet("snapshot delete", flag.ContinueOnError)
	fs.SetOutput(env.stderr())
	var c common
	bindCommon(fs, &c, env)
	var all bool
	fs.BoolVar(&all, "all", false, "删除全部快照")
	want, err := parseFlagsAnywhere(fs, args)
	if err != nil {
		return 2
	}

	if all {
		list, err := snapshot.List(snapshotBase(c.root))
		if err != nil {
			fmt.Fprintf(env.stderr(), "%v\n", err)
			return 1
		}
		if len(list) == 0 {
			fmt.Fprintln(env.stdout(), "没有快照可删")
			return 0
		}
		ids := make([]string, 0, len(list))
		for _, s := range list {
			ids = append(ids, s.ID)
		}
		// 逆序删：先删新的。列表本身已按新→旧排，正序即可。
		sort.Strings(ids)
		for _, id := range ids {
			if err := snapshot.Delete(snapshotBase(c.root), id); err != nil {
				fmt.Fprintf(env.stderr(), "删除 %s 失败: %v\n", id, err)
				return 1
			}
		}
		fmt.Fprintf(env.stdout(), "已删除 %d 份快照\n", len(ids))
		return 0
	}

	if len(want) == 0 {
		fmt.Fprintln(env.stderr(), "请指定要删除的快照 ID，或用 --all 删全部")
		return 2
	}
	for _, id := range want {
		if err := snapshot.Delete(snapshotBase(c.root), id); err != nil {
			fmt.Fprintf(env.stderr(), "%v\n", err)
			return 1
		}
		fmt.Fprintf(env.stdout(), "已删除快照 %s\n", id)
	}
	return 0
}

// replaySupervision 在回归之后把托管关系重建起来。
//
// 直接复用 restore 的逐条处理函数：两处都需要“按状态文件把服务注册回去”，
// 分开实现必然漂移。dry-run 时不调它——回归本身已经是 dry-run 了。
func replaySupervision(ctx context.Context, c *common, env Env, info platform.Info) int {
	st, err := state.Load(state.Path(c.root))
	if err != nil || len(st.Components) == 0 {
		return 0
	}
	mgr := env.supervisor(info, c.root)
	if mgr == nil {
		return 1
	}
	if bin := dontCrackBinFromState(st, info.GOOS, c.root); bin != "" {
		mgr.DontCrackBin = bin
	}
	recs := append([]state.Record(nil), st.Components...)
	sort.SliceStable(recs, func(i, j int) bool { return recs[i].Name < recs[j].Name })

	var (
		updates []state.Record
		offset  int
		failed  int
	)
	for _, rec := range recs {
		item, upd, next := restoreRecord(ctx, c, mgr, rec, offset)
		offset = next
		if item.Outcome == outcomeFailed {
			failed++
		}
		if upd != nil {
			updates = append(updates, *upd)
		}
	}
	if len(updates) > 0 && !c.dryRun {
		for _, u := range updates {
			st.Put(u)
		}
		if err := st.Save(state.Path(c.root)); err != nil {
			fmt.Fprintf(env.stderr(), "写回状态文件失败: %v\n", err)
			return 1
		}
	}
	if failed > 0 {
		return failed
	}
	return 0
}

// humanBytes 把字节数格式化成人类可读的形式。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
