package rules

import (
	"net/netip"
	"strings"
	"testing"
)

func TestPinnedBuiltin(t *testing.T) {
	for _, p := range BuiltinPinned() {
		base := strings.TrimPrefix(p, "*.")
		name := base
		if strings.HasPrefix(p, "*.") {
			name = "a.b." + base
		}
		if ok, _ := IsPinned(name, nil); !ok {
			t.Errorf("%s 应命中内置置顶（%s）", name, p)
		}
	}
	hit := []string{
		"CLAUDE.AI", "claude.ai.", "Api.Anthropic.Com.", "x.y.claude.com",
		"ＣＬＡＵＤＥ．ＡＩ", // 全角，经 IDNA 映射
	}
	for _, n := range hit {
		if ok, valid := IsPinned(n, nil); !ok || !valid {
			t.Errorf("%q 应命中置顶（ok=%v valid=%v）", n, ok, valid)
		}
	}
	miss := []string{"notclaude.ai", "claude.ai.example.com", "xanthropic.com", "anthropic.co", "claude.aii", "example.org"}
	for _, n := range miss {
		if ok, valid := IsPinned(n, nil); ok || !valid {
			t.Errorf("%q 不应命中置顶（ok=%v valid=%v）", n, ok, valid)
		}
	}
}

func TestPinnedPunycodeAndInvalid(t *testing.T) {
	// 国际化域名：unicode 和 punycode 两种写法归一后相同。
	a, err1 := NormalizeName("例子.中国")
	b, err2 := NormalizeName("XN--FSQU00A.xn--fiqs8s.")
	if err1 != nil || err2 != nil || a != b || a != "xn--fsqu00a.xn--fiqs8s" {
		t.Fatalf("punycode 归一：%q %v / %q %v", a, err1, b, err2)
	}
	extra := []string{"*.xn--fsqu00a.xn--fiqs8s"}
	if ok, _ := IsPinned("www.例子.中国", extra); !ok {
		t.Error("用户追加的国际化置顶域名应命中 unicode 写法")
	}
	// 不合法的名字：valid=false，调用方按看不清处理。
	for _, n := range []string{"", ".", "a..b", "-a.com", "xn--zz.com", "a b.com", strings.Repeat("a", 64) + ".com"} {
		if _, valid := IsPinned(n, nil); valid {
			t.Errorf("%q 应判为不合法", n)
		}
	}
	// 下划线允许（有些主机名带它）。
	if _, err := NormalizeName("_dmarc.example.com"); err != nil {
		t.Errorf("下划线应允许：%v", err)
	}
}

func TestPinnedExtraAndBuiltinStays(t *testing.T) {
	c := mustParse(t, "pinned_extra: [\"*.Claude-Staging.Example\", exact.test]\n")
	if ok, _ := IsPinned("a.claude-staging.example", c.PinnedExtra); !ok {
		t.Error("用户追加的通配置顶应命中子域")
	}
	if ok, _ := IsPinned("claude-staging.example", c.PinnedExtra); ok {
		t.Error("*.x 不应命中 x 本身")
	}
	if ok, _ := IsPinned("exact.test", c.PinnedExtra); !ok {
		t.Error("用户追加的精确置顶应命中")
	}
	// 内置名单删不掉：没有删除的字段，写了就是未知字段。
	if _, err := Parse([]byte("pinned_domains: []\n")); err == nil {
		t.Error("pinned_domains 字段应被拒绝（内置名单不能删）")
	}
	if ok, _ := IsPinned("claude.ai", c.PinnedExtra); !ok {
		t.Error("有追加名单时内置名单仍生效")
	}
}

func TestAnthropicNet(t *testing.T) {
	in := []string{"160.79.104.0", "160.79.104.10", "160.79.111.255", "::ffff:160.79.104.10", "2607:6bc0::10"}
	out := []string{"160.79.103.255", "160.79.112.0", "::ffff:160.79.112.0", "1.1.1.1", "198.18.1.1"}
	for _, s := range in {
		if !InAnthropicNet(netip.MustParseAddr(s)) {
			t.Errorf("%s 应在 Anthropic 段内", s)
		}
	}
	for _, s := range out {
		if InAnthropicNet(netip.MustParseAddr(s)) {
			t.Errorf("%s 不应在 Anthropic 段内", s)
		}
	}
}

func TestOverlap(t *testing.T) {
	p := func(s string) pattern {
		x, err := parsePattern(s)
		if err != nil {
			t.Fatal(s, err)
		}
		return x
	}
	yes := [][2]string{{"*.ai", "claude.ai"}, {"claude.ai", "claude.ai"}, {"*.claude.ai", "*.ai"}, {"*.a.claude.ai", "*.claude.ai"}, {"x.claude.ai", "*.claude.ai"}}
	no := [][2]string{{"*.claude.ai", "claude.ai"}, {"notclaude.ai", "claude.ai"}, {"*.corp.example", "*.claude.ai"}}
	for _, c := range yes {
		if !overlap(p(c[0]), p(c[1])) || !overlap(p(c[1]), p(c[0])) {
			t.Errorf("%v 应有重叠", c)
		}
	}
	for _, c := range no {
		if overlap(p(c[0]), p(c[1])) || overlap(p(c[1]), p(c[0])) {
			t.Errorf("%v 不应有重叠", c)
		}
	}
}

func TestAppLists(t *testing.T) {
	claude := []AppID{"com.anthropic.claudefordesktop", "com.anthropic.ClaudeForDesktop", "com.anthropic.helper",
		"path:/Users/u/.local/bin/claude", "path:/opt/homebrew/lib/node_modules/@anthropic-ai/claude-code/cli.js",
		"path:/Users/u/.claude/local/node_modules/.bin/x"}
	for _, a := range claude {
		if !IsClaudeApp(a) {
			t.Errorf("%s 应识别为 Claude", a)
		}
	}
	for _, a := range []AppID{"com.google.Chrome", "path:/usr/bin/claudette", "system:claude", Unknown} {
		if IsClaudeApp(a) {
			t.Errorf("%s 不应识别为 Claude", a)
		}
	}
	for _, a := range []AppID{"com.west2online.ClashX", "com.nssurge.surge-mac", "path:/opt/homebrew/bin/mihomo", "path:/usr/local/bin/sing-box"} {
		if !IsProxyApp(a) {
			t.Errorf("%s 应识别为代理软件", a)
		}
	}
	if IsProxyApp("com.google.Chrome") {
		t.Error("Chrome 不是代理软件")
	}
}
