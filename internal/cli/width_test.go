package cli

import (
	"strings"
	"testing"
)

func TestRuneWidth(t *testing.T) {
	cases := map[rune]int{
		'a':  1,
		'中':  2,
		'　':  2, // 全角空格
		'é':  1,
		'\n': 0,
		0:    0,
		'😀':  2,
	}
	for r, want := range cases {
		if got := runeWidth(r); got != want {
			t.Errorf("runeWidth(%q) = %d，期望 %d", r, got, want)
		}
	}
}

func TestDisplayWidth(t *testing.T) {
	if got := displayWidth("abc"); got != 3 {
		t.Errorf("displayWidth(abc) = %d", got)
	}
	if got := displayWidth("中文"); got != 4 {
		t.Errorf("displayWidth(中文) = %d", got)
	}
	if got := displayWidth("a中b"); got != 4 {
		t.Errorf("displayWidth(a中b) = %d", got)
	}
}

func TestPadRight(t *testing.T) {
	got := padRight("中文", 8)
	if displayWidth(got) != 8 {
		t.Fatalf("填充后宽度 = %d，期望 8（%q）", displayWidth(got), got)
	}
	if !strings.HasPrefix(got, "中文") {
		t.Fatalf("应保留原内容: %q", got)
	}
	if got := padRight("toolongvalue", 4); got != "toolongvalue" {
		t.Fatalf("超宽不该截断: %q", got)
	}
}
