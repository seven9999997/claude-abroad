package api

import (
	"testing"

	"claude-abroad/internal/rules"
)

// 补齐三验收 f：不分配假地址的名单——追加是放松，删除是收紧。
func TestRulesLevelBypassExtra(t *testing.T) {
	none, _ := rules.Parse([]byte("default: proxy:main\n"))
	one, _ := rules.Parse([]byte("default: proxy:main\nbypass_extra: [nas.home.example]\n"))
	if lv, why := RulesLevel(none, one); lv != Sensitive || len(why) == 0 {
		t.Fatalf("追加应是放松：%v %v", lv, why)
	}
	if lv, _ := RulesLevel(one, none); lv != Normal {
		t.Fatalf("删除应是收紧：%v", lv)
	}
}
