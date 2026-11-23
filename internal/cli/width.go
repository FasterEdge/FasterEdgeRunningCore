package cli

import (
	"strings"
	"unicode"
)

// padRight 按终端显示宽度左对齐填充。
//
// 中文字符在等宽终端里占两列，用 %-22s 这类格式化会把表格拉歪。
// 这里按 East Asian Width 的宽字符规则计算列宽，使表格在任何语言混排时都对得齐。
func padRight(s string, width int) string {
	w := displayWidth(s)
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

// runeWidth 返回单个字符在等宽终端中占用的列数。
func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 32, r >= 0x7f && r < 0xa0:
		return 0
	case unicode.Is(unicode.Mn, r), unicode.Is(unicode.Me, r), unicode.Is(unicode.Cf, r):
		return 0
	}
	if isWide(r) {
		return 2
	}
	return 1
}

// isWide 判断是否属于东亚宽字符（CJK、全角标点、韩文、部分表情）。
func isWide(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // 谚文字母
		r >= 0x2E80 && r <= 0x303E,   // CJK 部首、标点
		r >= 0x3041 && r <= 0x33FF,   // 平假名、片假名、注音、CJK 兼容
		r >= 0x3400 && r <= 0x4DBF,   // CJK 扩展 A
		r >= 0x4E00 && r <= 0x9FFF,   // CJK 统一表意
		r >= 0xA000 && r <= 0xA4CF,   // 彝文
		r >= 0xAC00 && r <= 0xD7A3,   // 韩文音节
		r >= 0xF900 && r <= 0xFAFF,   // CJK 兼容表意
		r >= 0xFE30 && r <= 0xFE6F,   // CJK 兼容形式、小写变体
		r >= 0xFF00 && r <= 0xFF60,   // 全角 ASCII
		r >= 0xFFE0 && r <= 0xFFE6,   // 全角符号
		r >= 0x1F300 && r <= 0x1F64F, // 表情
		r >= 0x1F900 && r <= 0x1F9FF,
		r >= 0x20000 && r <= 0x3FFFD: // CJK 扩展 B 及以后
		return true
	}
	return false
}
