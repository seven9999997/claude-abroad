package rules

import (
	"fmt"
	"math/rand"
	"net/netip"
	"testing"
)

// 随机生成的取值池：刻意让每种危险情况都高频出现。
var (
	propApps = []AppID{
		"com.google.Chrome", "com.tencent.xinWeChat", "path:/opt/homebrew/bin/curl",
		"com.example.Telemetry", "com.example.Other", "system:mDNSResponder", Unknown,
		"com.anthropic.claudefordesktop", "path:/Users/u/.local/bin/claude",
	}
	propRuleApps = []AppID{ // 能写进规则的
		"com.google.Chrome", "com.tencent.xinWeChat", "path:/opt/homebrew/bin/curl",
		"com.example.Telemetry", "com.example.Other", "system:mDNSResponder",
	}
	propNames = []string{
		"", "", "example.org", "git.corp.example", "claude.ai", "api.anthropic.com",
		"x.claude.com", "notclaude.ai", "pinned.extra.test", "bad_-.x-",
	}
	propAddrs = []string{
		"203.0.113.9", "198.18.1.7", "160.79.104.10", "160.79.111.255", "160.79.112.0", "::ffff:160.79.104.1", "10.1.2.3",
	}
	propPorts  = []uint16{443, 80, 53, 5222}
	propExits  = []string{"main", "wg-a", "clash-b"}
	propSniffs = []SniffKind{SniffNone, NotTLS, TLSName, TLSNoName, TLSECH, QUICName, QUICNoName, HTTPHost, Timeout}
)

func pick[T any](r *rand.Rand, s []T) T { return s[r.Intn(len(s))] }

func randAction(r *rand.Rand) string {
	switch r.Intn(4) {
	case 0:
		return "direct"
	case 1:
		return "block"
	default:
		return "proxy:" + pick(r, propExits)
	}
}

func randConfig(r *rand.Rand) *Config {
	y := fmt.Sprintf("default: %s\n", pick(r, []string{"proxy:main", "block", "proxy:main"}))
	y += "rules:\n"
	for _, a := range propRuleApps {
		if r.Intn(3) > 0 {
			act := randAction(r)
			if act == "direct" && r.Intn(4) > 0 || a == "com.tencent.xinWeChat" && r.Intn(2) == 0 {
				act = "direct" // 让直连规则占多数，危险情况才会真正走到阶段 2
			}
			y += fmt.Sprintf("  - {app: %q, action: %q}\n", a, act)
		}
	}
	y += "offline_allow: ["
	for _, a := range propRuleApps {
		if r.Intn(2) == 0 {
			y += fmt.Sprintf("%q,", a)
		}
	}
	y += "]\n"
	if r.Intn(2) == 0 {
		y += "local_domains: [\"*.corp.example\"]\n"
	}
	if r.Intn(2) == 0 {
		y += "pinned_extra: [\"pinned.extra.test\"]\n"
	}
	if r.Intn(4) == 0 {
		y += "block_udp_443: true\n"
	}
	c, err := Parse([]byte(y))
	if err != nil {
		panic(fmt.Sprintf("随机配置不合法：%v\n%s", err, y))
	}
	return c
}

func randFn(r *rand.Rand) func(string) bool {
	m := map[string]bool{}
	for _, x := range propExits {
		m[x] = r.Intn(3) > 0
	}
	return func(x string) bool { return m[x] }
}

func randCase(r *rand.Rand) (Who, Dest, Env) {
	w := Who{App: pick(r, propApps), Claude: r.Intn(8) == 0}
	d := Dest{
		Net:   pick(r, []string{"tcp", "udp"}),
		Addr:  netip.AddrPortFrom(netip.MustParseAddr(pick(r, propAddrs)), pick(r, propPorts)),
		Sniff: pick(r, propSniffs),
	}
	if r.Intn(2) == 0 {
		d.FakeDomain = pick(r, propNames)
	}
	if d.Sniff == TLSName || d.Sniff == QUICName || d.Sniff == HTTPHost || d.Sniff == TLSECH {
		d.SniffName = pick(r, propNames)
	}
	if r.Intn(3) == 0 {
		d.Reverse = pick(r, propNames)
	}
	d.Local = r.Intn(4) == 0
	d.Refused = r.Intn(8) == 0
	e := Env{
		State:    pick(r, []State{Enabled, Enabled, Disabled}),
		GateOpen: r.Intn(4) > 0,
		ExitUp:   randFn(r),
		UDPOK:    randFn(r),
	}
	return w, d, e
}

// candidateDirect 只算阶段 1，判断“如果没有阶段 2，会不会直连”，用来确认危险情况真的被考验到了。
func candidateDirect(c *Config, w Who, d Dest, e Env) bool {
	rule, has := c.rule(w.App)
	dis := e.State != Enabled
	switch {
	case has && rule.Kind == Block:
		return false
	case d.Local && !dis:
		return true
	case dis:
		return c.offline(w.App)
	case has:
		return rule.Kind == Direct
	}
	return false
}

func pinnedAny(c *Config, d Dest) bool {
	for _, n := range []string{d.FakeDomain, d.SniffName, d.Reverse} {
		if n != "" {
			if p, _ := IsPinned(n, c.PinnedExtra); p {
				return true
			}
		}
	}
	return false
}

func TestProperties(t *testing.T) {
	const N = 40000
	r := rand.New(rand.NewSource(20261118))
	hits := map[string]int{}
	for i := 0; i < N; i++ {
		c := randConfig(r)
		w, d, e := randCase(r)
		got := c.Decide(w, d, e)
		cand := candidateDirect(c, w, d, e)
		fail := func(f string, a ...any) {
			t.Fatalf("第 %d 组：%s\nwho=%+v dest=%+v state=%v gate=%v\nconfig=%+v\n结果=%+v", i, fmt.Sprintf(f, a...), w, d, e.State, e.GateOpen, c, got)
		}
		direct := got.Action.Kind == Direct
		noFake := d.FakeDomain == ""

		// 七种情况都不会直连。
		dangers := map[string]bool{
			"claude":    w.Claude,
			"pinned":    pinnedAny(c, d),
			"refused":   d.Refused,
			"anthropic": InAnthropicNet(d.Addr.Addr()),
			"opaque":    noFake && (d.Sniff == TLSNoName || d.Sniff == TLSECH || d.Sniff == QUICNoName),
			"timeout":   noFake && d.Sniff == Timeout && d.Addr.Port() == 443,
			"nosniff":   noFake && d.Sniff == SniffNone,
		}
		for k, v := range dangers {
			if v {
				hits[k]++
				if cand {
					hits[k+"+cand"]++
				}
				if direct {
					fail("%s 的情况得到了直连", k)
				}
			}
		}
		// 停用时不在 offline_allow 的应用不会直连；Local 只在启用时带来直连。
		if e.State == Disabled && direct && !c.offline(w.App) {
			fail("停用时不在 offline_allow 的应用得到了直连")
		}
		if e.State == Disabled && d.Local {
			hits["disabled+local"]++
		}
		// 闸门关闭时不会走代理。
		if (!e.GateOpen || e.State == Disabled) && got.Action.Kind == Proxy {
			fail("闸门关闭时得到了走代理")
		}
		// block 规则命中时一定拦截。
		if a, ok := c.rule(w.App); ok && a.Kind == Block {
			hits["block"]++
			if got.Action != ActBlock {
				fail("block 规则没有拦截")
			}
		}
		// 走代理时出口要么是 main，要么可用；UDP 时支持 UDP。
		if got.Action.Kind == Proxy {
			x := got.Action.Exit
			if x != ExitMain && !e.ExitUp(x) {
				fail("走了不可用的出口")
			}
			if d.Net == "udp" && !e.UDPOK(x) {
				fail("UDP 走了不支持 UDP 的出口")
			}
		}
		// 规则指向的分流出口可用且支持 UDP（UDP 时）、闸门开、启用、不命中阶段 4 时：一定走这个出口，不拦、不回退。
		if a, ok := c.rule(w.App); ok && a.Kind == Proxy && a.Exit != ExitMain && e.State == Enabled && e.GateOpen &&
			!d.Local && e.ExitUp(a.Exit) && (d.Net == "tcp" || e.UDPOK(a.Exit)) &&
			!(c.BlockUDP443 && d.Net == "udp" && d.Addr.Port() == 443) {
			hits["exit-usable"]++
			if d.Net == "udp" && !e.UDPOK(ExitMain) {
				hits["exit-usable+main-noudp"]++
			}
			if got.Action != a {
				fail("出口 %s 可用却没有走它", a.Exit)
			}
		}
		// 纯函数。
		if again := c.Decide(w, d, e); again != got {
			fail("两次判定结果不同：%+v", again)
		}
		// NeedSniff 与 Decide 一致：没映射、没首包时，凡是某个首包结果能直连，NeedSniff 必须为真。
		if noFake {
			d0 := d
			d0.Sniff, d0.SniffName = SniffNone, ""
			need := c.NeedSniff(w, d0, e)
			for _, s := range propSniffs[1:] {
				for _, n := range []string{"", "example.org", "claude.ai"} {
					d1 := d0
					d1.Sniff, d1.SniffName = s, n
					if c.Decide(w, d1, e).Action.Kind == Direct {
						hits["sniff-direct"]++
						if !need {
							fail("首包 %v/%q 能直连，但 NeedSniff 为假", s, n)
						}
					}
				}
			}
		}
		if direct {
			hits["direct"]++
		}
	}
	// 覆盖下限：每种危险情况都要真正被考验到（阶段 1 本来会直连的组数）。
	min := map[string]int{
		"claude+cand": 300, "pinned+cand": 300, "refused+cand": 300, "anthropic+cand": 300,
		"opaque+cand": 300, "timeout+cand": 100, "nosniff+cand": 200,
		"disabled+local": 300, "exit-usable": 1000, "exit-usable+main-noudp": 100, "block": 1000, "direct": 1000, "sniff-direct": 1000,
	}
	for k, m := range min {
		if hits[k] < m {
			t.Errorf("%s 只覆盖到 %d 组，下限 %d", k, hits[k], m)
		}
	}
	t.Logf("组数 %d，覆盖：%v", N, hits)
}
