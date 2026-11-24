// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

package fetch

import "context"

// Source 是“组件产物的来源”这一能力的抽象。
//
// 之所以要这层抽象：边缘现场的网络条件差异极大——有的节点能直连 GitHub，
// 有的只能连局域网里的邻居，有的完全离线只能读 U 盘。把“取 release 清单”
// 与“取产物字节”抽成两个方法之后，GitHub、本地镜像目录、HTTP 镜像三种来源
// 可以用同一份装载逻辑驱动，装载代码不需要知道包从哪来。
type Source interface {
	// LatestRelease 返回某仓库的最新 release（含产物列表）。
	LatestRelease(ctx context.Context, repo string) (*Release, error)
	// Download 把 url（或本地路径）指向的产物写到 dst，返回字节数。
	Download(ctx context.Context, url, dst string) (int64, error)
}

var _ Source = (*Client)(nil)
