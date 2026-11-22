package api

import (
	"fmt"
	"math/rand"
	"net/netip"
	"strings"
	"testing"

	"claude-abroad/internal/rules"
)

func TestClassifyAndAllowed(t *testing.T) {
	for _, c := range []struct {
		peer, owner, console int
		want                 Caller
	}{
		{0, 0, 501, Trusted},     // 真实模式：root
		{501, 0, 501, Console},   // 真实模式：控制台用户
		{502, 0, 501, Other},     // 真实模式：别的用户
		{501, 0, -1, Other},      // 没人登录时，非 root 一律其他
		{501, 501, 501, Trusted}, // 开发模式：属主
		{502, 501, 502, Console}, // 开发模式：别的控制台用户
		{-1, 501, 501, Other},    // 取不到对端
		{0, 501, 501, Trusted},   // 开发模式下 root 也信任
	} {
		if got := Classify(c.peer, c.owner, c.console); got != c.want {
			t.Errorf("Classify(%d,%d,%d) = %s，应为 %s", c.peer, c.owner, c.console, got, c.want)
		}
	}
	for _, c := range []struct {
		c    Caller
		l    Level
		want bool
	}{
		{Trusted, Sensitive, true}, {Trusted, Read, true},
		{Console, Read, true}, {Console, Normal, true}, {Console, PerCall, true}, {Console, Sensitive, false},
		{Other, Read, false}, {Other, Normal, false},
	} {
		if Allowed(c.c, c.l) != c.want {
			t.Errorf("Allowed(%s,%s) 应为 %v", c.c, c.l, c.want)
		}
	}
	if AllowedSensitive(Console) || !AllowedSensitive(Trusted) || AllowedSensitive(Other) {
		t.Fatal("按本次调用算出敏感时，只有完全信任的调用方能做")
	}
	if (Disabled{}).Enabled() || (Disabled{}).Name() != "disabled" {
		t.Fatal("签名校验推迟期间是“未启用”实现")
	}
}

func TestCheckRoutes(t *testing.T) {
	ok := []Route{{ID: "a", Method: "GET", Path: "/v1/status", Level: Read}}
	if err := CheckRoutes(ok); err != nil {
		t.Fatal(err)
	}
	for name, rs := range map[string][]Route{
		"ID 重复":  {{ID: "a", Method: "GET", Path: "/v1/a", Level: Read}, {ID: "a", Method: "GET", Path: "/v1/b", Level: Read}},
		"路由重复":   {{ID: "a", Method: "GET", Path: "/v1/a", Level: Read}, {ID: "b", Method: "GET", Path: "/v1/a", Level: Read}},
		"没有 v1":  {{ID: "a", Method: "GET", Path: "/status", Level: Read}},
		"读取凭据":   {{ID: "a", Method: "GET", Path: "/v1/main-proxy/credentials", Level: Sensitive}},
		"调试终端":   {{ID: "a", Method: "POST", Path: "/v1/debug/shell", Level: Sensitive}},
		"改进程清单":  {{ID: "a", Method: "PUT", Path: "/v1/claude-procs", Level: Sensitive}},
		"级别无效":   {{ID: "a", Method: "GET", Path: "/v1/a"}},
		"不认识的方法": {{ID: "a", Method: "PATCH", Path: "/v1/a", Level: Read}},
	} {
		if err := CheckRoutes(rs); err == nil {
			t.Errorf("%s：应被拒", name)
		}
	}
}

func parse(t *testing.T, y string) *rules.Config {
	t.Helper()
	c, err := rules.Parse([]byte(y))
	if err != nil {
		t.Fatalf("%v\n%s", err, y)
	}
	return c
}

// 验收 c：每一类字段的“放松”和“收紧”两个方向。
func TestRulesLevelTable(t *testing.T) {
	base := `rules:
  - {app: com.example.a, action: "proxy:main"}
  - {app: com.example.b, action: direct}
offline_allow: ["com.example.b"]
local_domains: ["l1.test"]
pinned_extra: ["p1.test"]
block_udp_443: true
`
	cases := []struct {
		name string
		next string
		want Level
	}{
		{"不变", base, Normal},
		{"a 改成直连（放松）", strings.Replace(base, `com.example.a, action: "proxy:main"`, `com.example.a, action: direct`, 1), Sensitive},
		{"b 从直连改成主代理（收紧）", strings.Replace(base, `com.example.b, action: direct`, `com.example.b, action: "proxy:main"`, 1), Normal},
		{"新增直连规则（放松）", base + "", Normal},
		{"local_domains 加（放松）", strings.Replace(base, `["l1.test"]`, `["l1.test", "l2.test"]`, 1), Sensitive},
		{"local_domains 删（收紧）", strings.Replace(base, `local_domains: ["l1.test"]`, `local_domains: []`, 1), Normal},
		{"pinned_extra 删（放松）", strings.Replace(base, `pinned_extra: ["p1.test"]`, `pinned_extra: []`, 1), Sensitive},
		{"pinned_extra 加（收紧）", strings.Replace(base, `["p1.test"]`, `["p1.test", "p2.test"]`, 1), Normal},
		{"offline_allow 加（放松）", strings.Replace(base, `offline_allow: ["com.example.b"]`, `offline_allow: ["com.example.b", "com.example.c"]`, 1), Sensitive},
		{"offline_allow 删（收紧）", strings.Replace(base, `offline_allow: ["com.example.b"]`, `offline_allow: []`, 1), Normal},
		{"关掉拦截 UDP 443（放松）", strings.Replace(base, `block_udp_443: true`, `block_udp_443: false`, 1), Sensitive},
		{"加 block 规则（收紧）", base + "", Normal},
		{"a 改走分流出口（普通）", strings.Replace(base, `com.example.a, action: "proxy:main"`, `com.example.a, action: "proxy:wg"`, 1), Normal},
		{"默认改成拦截（收紧）", "default: block\n" + base, Normal},
	}
	old := parse(t, base)
	for _, c := range cases {
		got, why := RulesLevel(old, parse(t, c.next))
		if got != c.want {
			t.Errorf("%s：%s（%v），应为 %s", c.name, got, why, c.want)
		}
	}
	// 新增一条直连规则（原来走默认的主代理）：放松。
	add := strings.Replace(base, "offline_allow", "  - {app: com.example.c, action: direct}\noffline_allow", 1)
	add = strings.Replace(add, "rules:\n", "rules:\n", 1)
	got, _ := RulesLevel(old, parse(t, strings.Replace(base, "  - {app: com.example.b, action: direct}\n", "  - {app: com.example.b, action: direct}\n  - {app: com.example.c, action: direct}\n", 1)))
	if got != Sensitive {
		t.Errorf("新增直连规则应是敏感：%s", got)
	}
	// 新增 block 规则：收紧。
	got, _ = RulesLevel(old, parse(t, strings.Replace(base, "  - {app: com.example.b, action: direct}\n", "  - {app: com.example.b, action: direct}\n  - {app: com.example.c, action: block}\n", 1)))
	if got != Normal {
		t.Errorf("新增拦截规则应是普通：%s", got)
	}
	// 解除拦截：新规则里有本地域名时是放松（拦截优先于本地域名）；没有本地域名、也不在停用名单里时是普通。
	blk := parse(t, "rules:\n  - {app: com.example.c, action: block}\nlocal_domains: [\"l1.test\"]\n")
	if got, _ := RulesLevel(blk, parse(t, "local_domains: [\"l1.test\"]\n")); got != Sensitive {
		t.Errorf("有本地域名时解除拦截应是敏感：%s", got)
	}
	if got, _ := RulesLevel(parse(t, "rules:\n  - {app: com.example.c, action: block}\n"), rules.DefaultConfig()); got != Normal {
		t.Errorf("没有本地域名、不在停用名单时解除拦截是普通：%s", got)
	}
	// Claude 应用的规则：任何改动都敏感（即使只是改成拦截）。
	cl := parse(t, "rules:\n  - {app: com.anthropic.claudefordesktop, action: block}\n")
	if got, _ := RulesLevel(rules.DefaultConfig(), cl); got != Sensitive {
		t.Errorf("改 Claude 应用的规则应是敏感：%s", got)
	}
	if got, _ := RulesLevel(nil, nil); got != Normal {
		t.Error("都是默认配置时是普通")
	}
}

// 验收 c 的性质测试：随机的新旧两份规则，只要存在一个应用和去向，使规则引擎的判定从“不是直连”变成“直连”，
// 分级必须是敏感。断言这种情况实际覆盖到的组数下限（不能因为生成器太弱而空过）。
func TestRulesLevelProperty(t *testing.T) {
	r := rand.New(rand.NewSource(20261122))
	apps := []string{"com.example.a", "com.example.b", "com.example.c", "path:/usr/bin/curl"}
	actions := []string{"direct", "block", `"proxy:main"`, `"proxy:wg"`}
	doms := []string{"x.test", "y.test", "z.test", "p.test"}
	pick := func(xs []string) []string {
		var out []string
		for _, x := range xs {
			if r.Intn(3) == 0 {
				out = append(out, x)
			}
		}
		return out
	}
	quote := func(xs []string) string {
		var q []string
		for _, x := range xs {
			q = append(q, fmt.Sprintf("%q", x))
		}
		return "[" + strings.Join(q, ", ") + "]"
	}
	gen := func() string {
		var b strings.Builder
		if r.Intn(4) == 0 {
			b.WriteString("default: block\n")
		}
		b.WriteString("rules:\n")
		for _, a := range apps {
			if r.Intn(2) == 0 {
				fmt.Fprintf(&b, "  - {app: %q, action: %s}\n", a, actions[r.Intn(len(actions))])
			}
		}
		fmt.Fprintf(&b, "offline_allow: %s\nlocal_domains: %s\npinned_extra: %s\nblock_udp_443: %v\n", quote(pick(apps)), quote(pick(doms)), quote(pick(doms)), r.Intn(2) == 0)
		return b.String()
	}
	type probe struct {
		w rules.Who
		d func(c *rules.Config) rules.Dest
		e rules.Env
	}
	var probes []probe
	for _, a := range append(apps, "com.example.other") {
		for _, dom := range append(doms, "other.test") {
			for _, n := range []string{"tcp", "udp"} {
				for _, st := range []rules.State{rules.Enabled, rules.Disabled} {
					dom, n := dom, n
					probes = append(probes, probe{
						w: rules.Who{App: rules.AppID(a)},
						d: func(c *rules.Config) rules.Dest {
							return rules.Dest{Net: n, Addr: netip.MustParseAddrPort("198.18.0.9:443"), FakeDomain: dom, Sniff: rules.TLSName, SniffName: dom, Local: c.IsLocal(dom)}
						},
						e: rules.Env{State: st, GateOpen: true, ExitUp: func(string) bool { return true }, UDPOK: func(string) bool { return true }},
					})
				}
			}
		}
	}
	const N = 8000
	valid, loosened := 0, 0
	for i := 0; i < N; i++ {
		ov, nv := gen(), gen()
		old, err1 := rules.Parse([]byte(ov))
		next, err2 := rules.Parse([]byte(nv))
		if err1 != nil || err2 != nil {
			continue
		}
		valid++
		loose := false
		for _, p := range probes {
			o := old.Decide(p.w, p.d(old), p.e)
			n := next.Decide(p.w, p.d(next), p.e)
			if n.Action.Kind == rules.Direct && o.Action.Kind != rules.Direct {
				loose = true
				break
			}
		}
		lv, why := RulesLevel(old, next)
		if loose {
			loosened++
			if lv != Sensitive {
				t.Fatalf("第 %d 组：存在连接从不是直连变成直连，分级却是 %s（%v）\n旧：\n%s新：\n%s", i, lv, why, ov, nv)
			}
		}
	}
	t.Logf("有效 %d 组，其中存在“变成直连”的 %d 组", valid, loosened)
	if valid < 2000 || loosened < 500 {
		t.Fatalf("覆盖不够：有效 %d 组（至少 2000）、变成直连 %d 组（至少 500）", valid, loosened)
	}
}
