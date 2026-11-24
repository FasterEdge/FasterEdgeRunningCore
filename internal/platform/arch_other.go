// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

// 兜底：非 arm 构建，以及 GOARM 既没置上 arm.6 也没置上 arm.7 的 arm 构建
// （例如 GOARM=5）。arm 走不到 armv6/armv7 的文件时回退到 armv7，
// 与历史行为保持一致。
//go:build !arm || (!arm.6 && !arm.7)

package platform

func armArchLabel() string { return "armv7" }
