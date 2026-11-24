// ─────────────────────────────────────────────────────────────
// FasterEdge 开源项目
// Github: https://github.com/FasterEdge
// Gitee:  https://gitee.com/FasterEdge
// ─────────────────────────────────────────────────────────────

package snapshot

import (
	"encoding/json"
	"runtime"

	"github.com/FasterEdge/FasterEdgeRunningCore/internal/platform"
)

// archPair 是一对 GOOS/GOARCH。
type archPair struct {
	GOOS string
	Arch string
}

// currentArch 返回本机的 GOOS 与归一化架构。
func currentArch() archPair {
	return archPair{GOOS: runtime.GOOS, Arch: platform.NormalizeArch(runtime.GOARCH)}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func jsonMarshalIndent(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
