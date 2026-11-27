package exitcheck

import (
	"regexp"
	"strings"
	"testing"
)

var asciiCode = regexp.MustCompile(`[a-z]+_[a-z]+|\b(ok|timeout|mainland)\b`)

// 每个原因都有中文，文字里没有原因码。
func TestCauseTextCoversAll(t *testing.T) {
	for _, c := range Causes() {
		s := CauseText(c)
		if s == "" || s == "原因不明" {
			t.Errorf("原因 %s 没有中文", c)
		}
		if asciiCode.MatchString(s) || strings.Contains(s, string(c)) {
			t.Errorf("原因 %s 的文字里有原因码：%q", c, s)
		}
	}
	if got := CauseText(Cause("x_unknown")); got != "原因不明" || strings.Contains(got, "x_unknown") {
		t.Fatalf("不认识的原因不能回显原值：%q", got)
	}
}

// 每个检查项都有中文名。
func TestItemLabelCoversAll(t *testing.T) {
	for _, n := range ItemNames() {
		s := ItemLabel(n)
		if s == "" || s == "其他检查项" || s == n {
			t.Errorf("检查项 %s 的中文名不对：%q", n, s)
		}
	}
	if got := ItemLabel("zzz"); strings.Contains(got, "zzz") {
		t.Fatalf("不认识的项不能回显：%q", got)
	}
}
