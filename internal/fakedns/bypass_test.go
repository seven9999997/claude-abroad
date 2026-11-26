package fakedns

import (
	"strings"
	"testing"

	"claude-abroad/internal/rules"
)

// m4-plan v2.7 第 0 步：受保护名单和规则引擎的内置置顶名单逐项相等（每个置顶模式的基础域名都受保护，反之亦然）。
func TestProtectedEqualsBuiltinPinned(t *testing.T) {
	want := map[string]bool{}
	for _, p := range rules.BuiltinPinned() {
		want[strings.TrimPrefix(p, "*.")] = true
	}
	if len(protected) != len(want) {
		t.Fatalf("受保护名单 %v，内置置顶名单 %v", protected, rules.BuiltinPinned())
	}
	for _, p := range protected {
		if !want[p] {
			t.Fatalf("%s 不在内置置顶名单里", p)
		}
	}
}

// 补齐三验收 g、h：用户追加的名单整体替换；Claude 的域名加不进；用户追加的置顶域名匹配时排除（已在名单里、之后被加进置顶
// 的，按置顶处理）；内置的始终在。
func TestBypassUser(t *testing.T) {
	b := NewBypass()
	if err := b.SetUser([]string{"nas.home.example", "*.corp.example"}, nil); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"nas.home.example": true, "x.corp.example": true, "corp.example": false, "time.apple.com": true, "other.example": false} {
		if b.Match(name) != want {
			t.Errorf("%s：应为 %v", name, want)
		}
	}
	if err := b.SetUser([]string{"api.claude.ai"}, nil); err != ErrProtected {
		t.Fatalf("Claude 子域名应拒绝：%v", err)
	}
	if !b.Match("nas.home.example") {
		t.Fatal("拒绝时什么都不改")
	}
	// 后来被加进置顶：按置顶处理
	if err := b.SetUser([]string{"nas.home.example", "*.corp.example"}, []string{"*.corp.example"}); err != nil {
		t.Fatal(err)
	}
	if b.Match("x.corp.example") || !b.Match("nas.home.example") {
		t.Fatal("追加的置顶域名应排除，别的照常")
	}
	// 删掉：恢复分配假地址；内置的仍在
	_ = b.SetUser(nil, nil)
	if b.Match("nas.home.example") || !b.Match("captive.apple.com") {
		t.Fatal("删掉后恢复、内置始终在")
	}
}
