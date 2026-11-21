package wfprules

import (
	"bytes"
	"flag"
	"go/parser"
	"go/token"
	"math/rand/v2"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "重写 testdata 里的期望摘要")

const (
	sidSvc   = "S-1-5-80-1111111111-2222222222-3333333333-4444444444-555555555"
	sidLocal = "S-1-5-21-100-200-300-1001"
	tunLUID  = 1689399632855040
	enLUID   = 1689399632855041 // 物理网卡
	otherTun = 1689399632855042 // 别人的虚拟网卡
)

func pfx(ss ...string) []netip.Prefix {
	var out []netip.Prefix
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

var (
	fullCfg  = Config{TunLUID: tunLUID, AllowSIDs: []string{sidSvc}, LAN: LANPrefixes(pfx("192.168.1.0/24"))}
	noTunCfg = Config{AllowSIDs: []string{sidSvc}, LAN: LANPrefixes(pfx("192.168.1.0/24"))}
	noLANCfg = Config{TunLUID: tunLUID, AllowSIDs: []string{sidSvc}}
	lanNone  = Config{TunLUID: tunLUID, AllowSIDs: []string{sidSvc}, LAN: LANPrefixes(nil)}
)

func mustRender(t testing.TB, c Config) []Filter {
	t.Helper()
	fs, err := Render(c)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

// a. 金样：完整配置、没有虚拟网卡（开机）、局域网关闭、局域网开着但没有直连网段。
func TestRenderGolden(t *testing.T) {
	for name, c := range map[string]Config{"full": fullCfg, "notun": noTunCfg, "nolan": noLANCfg, "lan-none": lanNone} {
		got := Summary(mustRender(t, c))
		path := filepath.Join("testdata", name+".txt")
		if *update {
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s：摘要与 %s 不一致：\n%s", name, path, got)
		}
	}
}

// 确定性：相同输入（顺序、重复不同）逐字节相同；摘要与过滤器顺序无关。
func TestDeterministic(t *testing.T) {
	a := Config{TunLUID: tunLUID, AllowSIDs: []string{sidSvc, sidLocal}, LAN: pfx("192.168.1.0/24", "10.0.0.0/8")}
	b := Config{TunLUID: tunLUID, AllowSIDs: []string{sidLocal, sidSvc, sidLocal}, LAN: pfx("10.0.0.0/8", "192.168.1.7/24", "192.168.1.0/24")}
	fa, fb := mustRender(t, a), mustRender(t, b)
	if !bytes.Equal(Summary(fa), Summary(fb)) {
		t.Fatal("同样的输入摘要不同")
	}
	rev := make([]Filter, len(fa))
	for i := range fa {
		rev[len(fa)-1-i] = fa[i]
	}
	if !bytes.Equal(Summary(fa), Summary(rev)) {
		t.Fatal("摘要随过滤器顺序变化")
	}
	if ProviderKey == SublayerKey || ProviderKey.String() != derive("provider").String() {
		t.Fatal("键不稳定")
	}
}

// 每个过滤器都带我们的提供者和子层，键各不相同，并且由名字唯一决定。
func TestEveryFilterOwned(t *testing.T) {
	fs := mustRender(t, Config{TunLUID: tunLUID, AllowSIDs: []string{sidSvc, sidLocal}, LAN: LANPrefixes(pfx("192.168.1.0/24", "10.1.0.0/16"))})
	seen := map[GUID]bool{}
	for _, f := range fs {
		if f.Provider != ProviderKey || f.Sublayer != SublayerKey {
			t.Errorf("%s 不属于我们的提供者或子层", f.Name)
		}
		if seen[f.Key] {
			t.Errorf("%s 的键重复", f.Name)
		}
		seen[f.Key] = true
		if f.Key != derive("filter/"+f.Name) {
			t.Errorf("%s 的键不是由名字算出的", f.Name)
		}
		if err := Validate(f); err != nil {
			t.Errorf("%s：%v", f.Name, err)
		}
	}
}

// 不合法的输入：拒绝，结果为空。
func TestRenderRejects(t *testing.T) {
	cases := map[string]Config{
		"没有放行账户":           {TunLUID: tunLUID},
		"SYSTEM":           {AllowSIDs: []string{"S-1-5-18"}},
		"LOCAL SERVICE":    {AllowSIDs: []string{"S-1-5-19"}},
		"NETWORK SERVICE":  {AllowSIDs: []string{"S-1-5-20"}},
		"Everyone":         {AllowSIDs: []string{"S-1-1-0"}},
		"Authenticated":    {AllowSIDs: []string{"S-1-5-11"}},
		"Administrators":   {AllowSIDs: []string{"S-1-5-32-544"}},
		"内置 Administrator": {AllowSIDs: []string{"S-1-5-21-100-200-300-500"}},
		"Guest":            {AllowSIDs: []string{"S-1-5-21-100-200-300-501"}},
		"小写 sid":           {AllowSIDs: []string{"s-1-5-21-100-200-300-1001"}},
		"带空格":              {AllowSIDs: []string{sidLocal + " "}},
		"段数不对":             {AllowSIDs: []string{"S-1-5-21-100-200-1001"}},
		"80 段数不对":          {AllowSIDs: []string{"S-1-5-80-1-2-3-4"}},
		"空串":               {AllowSIDs: []string{""}},
		"公网局域网段":           {AllowSIDs: []string{sidSvc}, LAN: pfx("160.79.104.0/23")},
		"跨越私有和公网":          {AllowSIDs: []string{sidSvc}, LAN: pfx("10.0.0.0/7")},
		"全部地址":             {AllowSIDs: []string{sidSvc}, LAN: pfx("0.0.0.0/0")},
		"IPv6 局域网段":        {AllowSIDs: []string{sidSvc}, LAN: pfx("fd00::/8")},
		"无效网段":             {AllowSIDs: []string{sidSvc}, LAN: []netip.Prefix{{}}},
	}
	for name, c := range cases {
		fs, err := Render(c)
		if err == nil || fs != nil {
			t.Errorf("%s：应拒绝（得到 %d 条，%v）", name, len(fs), err)
		}
	}
}

func TestLANPrefixes(t *testing.T) {
	got := LANPrefixes(pfx("192.168.1.7/24", "160.79.104.0/23", "10.0.0.0/7", "100.64.1.0/24", "192.168.1.0/24"))
	want := pfx("100.64.1.0/24", "169.254.0.0/16", "192.168.1.0/24", "224.0.0.0/4")
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%v", got)
		}
	}
}

func cn(v6 bool, lo bool, luid uint64, sid, proto string, lport, rport uint16, dst string) Conn {
	return Conn{V6: v6, Loopback: lo, IfLUID: luid, SID: sid, Proto: proto, LocalPort: lport, RemotePort: rport, Remote: netip.MustParseAddr(dst)}
}

// d. 求值表：逐条对应 internal/pf 的 TestEvalTable（接口名换成 LUID，用户换成 SID；Windows 没有虚拟机网桥那两条）。
func TestEvalTable(t *testing.T) {
	full := mustRender(t, fullCfg)
	nolan := mustRender(t, noLANCfg)
	boot := mustRender(t, noTunCfg)
	evil := mustRender(t, Config{TunLUID: tunLUID, AllowSIDs: []string{sidSvc}, LAN: LANPrefixes(pfx("160.79.104.0/23"))})
	const alice = "S-1-5-21-100-200-300-1002"
	cases := []struct {
		name string
		fs   []Filter
		c    Conn
		want bool
	}{
		{"IPv6 经物理网卡", full, cn(true, false, enLUID, sidSvc, "tcp", 1000, 443, "2606:4700::1"), false},
		{"::1 回环放行", full, cn(true, true, 1, alice, "tcp", 1000, 443, "::1"), true},
		{"IPv6 在我们的虚拟网卡上仍被拦", full, cn(true, false, tunLUID, sidSvc, "tcp", 1000, 443, "2606:4700::1"), false},
		{"IPv6 udp 在虚拟网卡上", full, cn(true, false, tunLUID, alice, "udp", 1000, 443, "2606:4700::1"), false},
		{"回环放行", full, cn(false, true, 1, alice, "tcp", 1000, 8080, "127.0.0.1"), true},
		{"我们的虚拟网卡放行", full, cn(false, false, tunLUID, alice, "tcp", 1000, 443, "1.1.1.1"), true},
		{"我们的虚拟网卡上 icmp 也放行", full, cn(false, false, tunLUID, alice, "icmp", 0, 0, "1.1.1.1"), true},
		{"DHCP 广播", full, cn(false, false, enLUID, "S-1-5-21-100-200-300-1003", "udp", 68, 67, "255.255.255.255"), true},
		{"DHCP 单播到私有", full, cn(false, false, enLUID, alice, "udp", 68, 67, "192.168.1.1"), true},
		{"DHCP 到公网被拦", full, cn(false, false, enLUID, alice, "udp", 68, 67, "8.8.8.8"), false},
		{"放行账户 tcp 到公网", full, cn(false, false, enLUID, sidSvc, "tcp", 50000, 443, "203.0.113.5"), true},
		{"放行账户 udp 到公网", full, cn(false, false, enLUID, sidSvc, "udp", 50000, 443, "203.0.113.5"), true},
		{"放行账户 icmp 到公网被拦", full, cn(false, false, enLUID, sidSvc, "icmp", 0, 0, "203.0.113.5"), false},
		{"到局域网放行", full, cn(false, false, enLUID, alice, "tcp", 1000, 631, "192.168.1.20"), true},
		{"到组播放行", full, cn(false, false, enLUID, alice, "udp", 5353, 5353, "224.0.0.251"), true},
		{"局域网关闭时同一个连接被拦", nolan, cn(false, false, enLUID, alice, "tcp", 1000, 631, "192.168.1.20"), false},
		{"恶意 DHCP 发的公网直连网段不放行", evil, cn(false, false, enLUID, alice, "tcp", 1000, 443, "160.79.104.10"), false},
		{"物理网卡 普通账户 tcp 到公网", full, cn(false, false, enLUID, alice, "tcp", 1000, 443, "1.1.1.1"), false},
		{"物理网卡 普通账户 udp 到公网", full, cn(false, false, enLUID, alice, "udp", 1000, 443, "1.1.1.1"), false},
		{"物理网卡 普通账户 icmp 到公网", full, cn(false, false, enLUID, alice, "icmp", 0, 0, "1.1.1.1"), false},
		{"别人的虚拟网卡 tcp", full, cn(false, false, otherTun, alice, "tcp", 1000, 443, "1.1.1.1"), false},
		{"别人的虚拟网卡 udp", full, cn(false, false, otherTun, alice, "udp", 1000, 443, "1.1.1.1"), false},
		{"别人的虚拟网卡 icmp", full, cn(false, false, otherTun, alice, "icmp", 0, 0, "1.1.1.1"), false},
		{"SYSTEM 在物理网卡到公网", full, cn(false, false, enLUID, "S-1-5-18", "tcp", 1000, 443, "1.1.1.1"), false},
		{"开机配置 虚拟网卡到公网被拦", boot, cn(false, false, tunLUID, alice, "tcp", 1000, 443, "1.1.1.1"), false},
		{"开机配置 放行账户照常", boot, cn(false, false, enLUID, sidSvc, "tcp", 1000, 443, "1.1.1.1"), true},
		// 远程执行的前提（design-windows 0.2）：开发机从局域网经 SSH 连进来，那台电脑的回包在“允许局域网”时放行。
		{"SSH 回包到局域网", full, cn(false, false, enLUID, "S-1-5-18", "tcp", 22, 50123, "192.168.1.30"), true},
	}
	for _, c := range cases {
		v, err := Eval(c.fs, c.c)
		if err != nil || v.Permit != c.want {
			t.Errorf("%s：%v（%v，%s），应为 %v", c.name, v.Permit, err, v.Matched, c.want)
		}
	}
}

// 不认识的描述是错误；“没有过滤器”的状态按 WFP 默认放行，且没有命中的过滤器。
func TestEvalUnknownAndEmpty(t *testing.T) {
	c := cn(false, false, enLUID, sidLocal, "tcp", 1000, 443, "1.1.1.1")
	v, err := Eval(nil, c)
	if err != nil || !v.Permit || v.Matched != "" {
		t.Fatalf("没有过滤器时应放行：%+v %v", v, err)
	}
	if fs, err := ParseSummary(nil); err != nil || len(fs) != 0 {
		t.Fatalf("空摘要：%v %v", fs, err)
	}
	if len(Summary(nil)) != 0 {
		t.Fatal("空集合的摘要应为空")
	}
	base := newFilter("x", LayerConnectV4, 1, Permit, Cond{FieldRemoteAddr, "10.0.0.0/8"})
	bad := map[string]Filter{
		"未知层":     func() Filter { f := base; f.Layer = "FWPM_LAYER_OUTBOUND_TRANSPORT_V4"; return f }(),
		"未知动作":    func() Filter { f := base; f.Action = "callout"; return f }(),
		"权重越界":    func() Filter { f := base; f.Weight = 16; return f }(),
		"未知字段":    func() Filter { f := base; f.Conds = []Cond{{"app_id", "x"}}; return f }(),
		"字段重复":    func() Filter { f := base; f.Conds = []Cond{{FieldProtocol, "tcp"}, {FieldProtocol, "udp"}}; return f }(),
		"协议不认识":   func() Filter { f := base; f.Conds = []Cond{{FieldProtocol, "icmp"}}; return f }(),
		"网段不规范":   func() Filter { f := base; f.Conds = []Cond{{FieldRemoteAddr, "10.1.2.3/8"}}; return f }(),
		"IPv6 网段": func() Filter { f := base; f.Conds = []Cond{{FieldRemoteAddr, "fd00::/8"}}; return f }(),
		"v6 层上的网段": func() Filter {
			f := base
			f.Layer = LayerConnectV6
			return f
		}(),
		"端口带前导零":     func() Filter { f := base; f.Conds = []Cond{{FieldRemotePort, "067"}}; return f }(),
		"LUID 为 0":   func() Filter { f := base; f.Conds = []Cond{{FieldInterface, "0"}}; return f }(),
		"loopback 值": func() Filter { f := base; f.Conds = []Cond{{FieldLoopback, "0"}}; return f }(),
		"SID 太宽":     func() Filter { f := base; f.Conds = []Cond{{FieldUser, "S-1-1-0"}}; return f }(),
	}
	for name, f := range bad {
		if _, err := Eval([]Filter{f}, c); err == nil {
			t.Errorf("%s：应报错", name)
		}
		if _, err := ParseSummary(Summary([]Filter{f})); err == nil {
			t.Errorf("%s：解析摘要应报错", name)
		}
	}
	for name, text := range map[string]string{
		"空行":     "\n\n",
		"字段太少":   "X permit\n",
		"GUID 坏": strings.Replace(string(Summary(mustRender(t, fullCfg))), "-", "_", 1),
		"条件没有等号": strings.Replace(string(Summary(mustRender(t, fullCfg))), " loopback=1", " loopback", 1),
		"缺名字":    strings.Replace(string(Summary(mustRender(t, fullCfg))), " name=", " nom=", 1),
	} {
		if _, err := ParseSummary([]byte(text)); err == nil {
			t.Errorf("%s：应报错", name)
		}
	}
	if _, err := Eval(mustRender(t, fullCfg), Conn{V6: true, Remote: netip.MustParseAddr("1.1.1.1")}); err == nil {
		t.Error("地址族不一致应报错")
	}
}

// 摘要能原样解析回来，求值结果与原过滤器一致。
func TestSummaryRoundTrip(t *testing.T) {
	for _, c := range []Config{fullCfg, noTunCfg, noLANCfg, lanNone} {
		fs := mustRender(t, c)
		back, err := ParseSummary(Summary(fs))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(Summary(back), Summary(fs)) {
			t.Fatal("往返后摘要不同")
		}
	}
}

// 别的子层：任何子层的拦截都是最终结果（建模假设）。
func TestOtherSublayerBlockIsFinal(t *testing.T) {
	fs := mustRender(t, fullCfg)
	other := newFilter("other/block-443", LayerConnectV4, 0, Block, Cond{FieldRemotePort, "443"})
	other.Sublayer, other.Provider = derive("other-sub"), derive("other-prov")
	v, err := Eval(append(fs, other), cn(false, false, tunLUID, sidLocal, "tcp", 1000, 443, "1.1.1.1"))
	if err != nil || v.Permit {
		t.Fatalf("别的子层拦截应是最终结果：%+v %v", v, err)
	}
	permit := other
	permit.Action, permit.Name = Permit, "other/permit-all"
	permit.Conds = nil
	v, _ = Eval(append(fs, permit), cn(false, false, enLUID, sidLocal, "tcp", 1000, 443, "1.1.1.1"))
	if v.Permit {
		t.Fatal("别的子层放行不能盖过我们的拦截")
	}
}

func randAddr(r *rand.Rand, v6 bool) netip.Addr {
	if v6 {
		var b [16]byte
		for i := range b {
			b[i] = byte(r.IntN(256))
		}
		return netip.AddrFrom16(b)
	}
	// 一半落在私有、组播、广播这些特殊范围附近，一半随机。
	specials := []string{"10.1.2.3", "192.168.1.9", "172.20.0.1", "100.64.3.3", "169.254.1.1", "224.0.0.251", "255.255.255.255", "160.79.104.10", "8.8.8.8"}
	if r.IntN(2) == 0 {
		return netip.MustParseAddr(specials[r.IntN(len(specials))])
	}
	return netip.AddrFrom4([4]byte{byte(r.IntN(256)), byte(r.IntN(256)), byte(r.IntN(256)), byte(r.IntN(256))})
}

// isPublic：不在任何私有、链路本地、组播、广播、回环范围里的 IPv4。
func isPublic(a netip.Addr) bool {
	for _, p := range append(append([]netip.Prefix{}, lanAllowed...), pfx("127.0.0.0/8", "255.255.255.255/32", "0.0.0.0/8")...) {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

func randConfig(r *rand.Rand) Config {
	c := Config{AllowSIDs: []string{sidSvc}}
	if r.IntN(3) > 0 {
		c.TunLUID = tunLUID
	}
	if r.IntN(2) == 0 {
		c.AllowSIDs = append(c.AllowSIDs, sidLocal)
	}
	var direct []netip.Prefix
	for i := r.IntN(4); i > 0; i-- {
		a := randAddr(r, false)
		direct = append(direct, netip.PrefixFrom(a, 8+r.IntN(25)).Masked())
	}
	if r.IntN(4) > 0 {
		c.LAN = LANPrefixes(direct)
	}
	return c
}

// 性质测试（照搬 pf 的四条），各至少 10000 个随机连接。
func TestEvalProperty(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	sids := []string{sidSvc, sidLocal, "S-1-5-21-100-200-300-1002", "S-1-5-18", ""}
	luids := []uint64{tunLUID, enLUID, otherTun, 1}
	protos := []string{"tcp", "udp", "icmp"}
	const n = 16000
	counts := [4]int{}
	for i := 0; i < n*4; i++ {
		cfg := randConfig(r)
		fs := mustRender(t, cfg)
		v6 := r.IntN(4) == 0
		c := Conn{V6: v6, Loopback: r.IntN(8) == 0, IfLUID: luids[r.IntN(len(luids))], SID: sids[r.IntN(len(sids))],
			Proto: protos[r.IntN(3)], LocalPort: uint16(r.IntN(70)), RemotePort: uint16(r.IntN(70)), Remote: randAddr(r, v6)}
		if r.IntN(3) == 0 {
			c.Proto, c.LocalPort, c.RemotePort = "udp", 68, 67
		}
		v, err := Eval(fs, c)
		if err != nil {
			t.Fatal(err)
		}
		allowed := c.SID == sidSvc || (c.SID == sidLocal && len(cfg.AllowSIDs) == 2)
		// 1. IPv6 除回环外一律不放行。
		if v6 && !c.Loopback {
			counts[0]++
			if v.Permit {
				t.Fatalf("IPv6 非回环被放行：%+v（%s）", c, v.Matched)
			}
		}
		if v6 || c.Loopback || (c.IfLUID == cfg.TunLUID && cfg.TunLUID != 0) || allowed {
			continue
		}
		// 2. 目的是公网、接口不是回环也不是我们的虚拟网卡、账户不是放行的：一律不放行。
		if isPublic(c.Remote) {
			counts[1]++
			if v.Permit {
				t.Fatalf("公网被放行：%+v（%s）cfg=%+v", c, v.Matched, cfg)
			}
		}
		// 3. 恶意 DHCP 发来的公网“直连网段”：用它算出的局域网段不放行公网目的。
		if isPublic(c.Remote) && cfg.LAN != nil {
			evil := LANPrefixes([]netip.Prefix{netip.PrefixFrom(c.Remote, 16+r.IntN(17)).Masked()})
			ev := mustRender(t, Config{TunLUID: cfg.TunLUID, AllowSIDs: cfg.AllowSIDs, LAN: evil})
			counts[2]++
			if vv, _ := Eval(ev, c); vv.Permit {
				t.Fatalf("恶意直连网段被放行：%+v（%s）", c, vv.Matched)
			}
		}
		// 4. DHCP 只放行到广播和私有范围。
		if c.Proto == "udp" && c.LocalPort == 68 && c.RemotePort == 67 && v.Permit {
			counts[3]++
			ok := false
			for _, d := range dhcpDst {
				if d.Contains(c.Remote) {
					ok = true
				}
			}
			for _, p := range cfg.LAN {
				if p.Contains(c.Remote) {
					ok = true
				}
			}
			if !ok {
				t.Fatalf("DHCP 到 %s 被放行（%s）", c.Remote, v.Matched)
			}
		}
	}
	// 第 4 条单独再跑足 10000 个“DHCP 到公网”的连接。
	for i := 0; i < 10000; i++ {
		cfg := randConfig(r)
		a := netip.AddrFrom4([4]byte{byte(r.IntN(256)), byte(r.IntN(256)), byte(r.IntN(256)), byte(r.IntN(256))})
		if !isPublic(a) {
			continue
		}
		v, _ := Eval(mustRender(t, cfg), Conn{IfLUID: enLUID, SID: "S-1-5-18", Proto: "udp", LocalPort: 68, RemotePort: 67, Remote: a})
		counts[3]++
		if v.Permit {
			t.Fatalf("DHCP 到公网 %s 被放行", a)
		}
	}
	for i, k := range counts {
		if k < 10000 {
			t.Errorf("性质 %d 只覆盖了 %d 个连接", i+1, k)
		}
	}
	t.Logf("覆盖：%v", counts)
}

// 源码检查：本包不调用系统接口，不执行命令，不碰网络。
func TestSourcePure(t *testing.T) {
	banned := map[string]bool{"os/exec": true, "syscall": true, "net": true, "unsafe": true, "golang.org/x/sys/windows": true, "os": true}
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly|parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		if af.Doc != nil && strings.Contains(af.Doc.Text(), "go:build") {
			t.Errorf("%s 带了构建约束", f)
		}
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			if banned[p] {
				t.Errorf("%s 导入了 %s", f, p)
			}
		}
	}
}
