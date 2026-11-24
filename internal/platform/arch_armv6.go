// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

// GOARM=6 时 Go 置上 arm.6；GOARM=7（以及未显式设置时的默认值）同时置上
// arm.6 与 arm.7，所以这里必须排除 arm.7，否则会与 arch_armv7.go 重复定义。
//go:build arm && arm.6 && !arm.7

package platform

func armArchLabel() string { return "armv6" }
