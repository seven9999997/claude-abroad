package rules

import "net/netip"

// builtinPinned 是内置的置顶域名（design 5.2），只能追加，不能删除。
var builtinPinned = []pattern{
	{Base: "claude.ai"}, {Wild: true, Base: "claude.ai"},
	{Base: "claude.com"}, {Wild: true, Base: "claude.com"},
	{Base: "anthropic.com"}, {Wild: true, Base: "anthropic.com"},
}

// anthropicNets 是 Anthropic 的地址段（design 5.2、R14）。只用来让连接更严格，不作为放行依据。
var anthropicNets = []netip.Prefix{
	netip.MustParsePrefix("160.79.104.0/21"),
	netip.MustParsePrefix("2607:6bc0::/48"),
}

// BuiltinPinned 返回内置置顶域名的副本（"claude.ai"、"*.claude.ai" 这种写法）。
func BuiltinPinned() []string {
	out := make([]string, len(builtinPinned))
	for i, p := range builtinPinned {
		out[i] = p.String()
	}
	return out
}

// BuiltinPinnedBases 返回内置置顶名单里的基础域名（去重、按名单顺序），给 fakedns 的受保护名单用（m4-plan v2.7 第 0 步：
// 两份名单不能各自维护）。
func BuiltinPinnedBases() []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range builtinPinned {
		if !seen[p.Base] {
			seen[p.Base] = true
			out = append(out, p.Base)
		}
	}
	return out
}

// IsPinned 判断域名是否命中置顶规则：内置名单永远生效，extra 是用户追加的（已校验过的模式）。
// 名字不合法时返回 false 和 ok=false，调用方要按“看不清去向”处理。
func IsPinned(name string, extra []string) (pinned, ok bool) {
	n, err := NormalizeName(name)
	if err != nil {
		return false, false
	}
	return pinnedNorm(n, extra), true
}

func pinnedNorm(n string, extra []string) bool {
	for _, p := range builtinPinned {
		if p.match(n) {
			return true
		}
	}
	for _, e := range extra {
		if p, err := parsePattern(e); err == nil && p.match(n) {
			return true
		}
	}
	return false
}

// InAnthropicNet 判断地址是否落在 Anthropic 的地址段内（IPv4 映射的 IPv6 先归一）。
func InAnthropicNet(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range anthropicNets {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
