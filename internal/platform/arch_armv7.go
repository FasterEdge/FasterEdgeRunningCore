// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

// GOARM=7（也是 Go 未显式设置 GOARM 时的默认值）置上 arm.7。
//go:build arm && arm.7

package platform

func armArchLabel() string { return "armv7" }
