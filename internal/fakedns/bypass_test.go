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
