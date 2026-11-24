// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

//go:build arm && arm.7

package platform

import (
	"runtime"
	"testing"
)

func armSelfLabelTest(t *testing.T) {
	t.Helper()
	if runtime.GOARCH != "arm" {
		t.Fatalf("arm.7 构建的 GOARCH 应为 arm，实际 %q", runtime.GOARCH)
	}
	if got := NormalizeArch("arm"); got != "armv7" {
		t.Fatalf("NormalizeArch(\"arm\") = %q，GOARM=7 构建必须给 armv7", got)
	}
	if got := NormalizeArch(runtime.GOARCH); got != "armv7" {
		t.Fatalf("NormalizeArch(%q) = %q，期望 armv7", runtime.GOARCH, got)
	}
}
