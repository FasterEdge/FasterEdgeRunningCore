// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

// 兜底：非 arm 构建，以及 GOARM 未置上 arm.6 / arm.7 的 arm 构建（如 GOARM=5）。
//go:build !arm || (!arm.6 && !arm.7)

package platform

import (
	"runtime"
	"testing"
)

func armSelfLabelTest(t *testing.T) {
	t.Helper()
	if runtime.GOARCH == "arm" {
		// GOARM=5 不在发布矩阵里，此时仍回退 armv7 以保持历史兼容。
		if got := NormalizeArch("arm"); got != "armv7" {
			t.Fatalf("NormalizeArch(\"arm\") = %q，无 arm.6/arm.7 时须回退 armv7", got)
		}
		return
	}
	if got, want := NormalizeArch(runtime.GOARCH), runtime.GOARCH; got != want {
		t.Fatalf("NormalizeArch(%q) = %q，期望 %q", runtime.GOARCH, got, want)
	}
}
