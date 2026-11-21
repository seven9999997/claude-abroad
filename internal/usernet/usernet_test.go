package usernet

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestPolicyTable(t *testing.T) {
	p := &Policy{
		Allow:    []netip.AddrPort{netip.MustParseAddrPort("192.168.1.20:7890"), netip.MustParseAddrPort("127.0.0.1:7891")},
		OwnAddrs: func() []netip.Addr { return []netip.Addr{netip.MustParseAddr("8.8.4.4")} }, // 假装本机有个公网地址
	}
	cases := []struct {
		dst  string
		want bool
		why  string
	}{
		{"1.1.1.1:443", true, "公网"},
		{"127.0.0.1:443", false, "回环"},
		{"10.1.2.3:443", false, "私有 10/8"},
		{"172.20.0.1:443", false, "私有 172.16/12"},
		{"192.168.1.20:443", false, "私有 192.168/16（白名单只放行 7890）"},
		{"169.254.169.254:80", false, "链路本地 / 元数据"},
		{"100.64.0.1:443", false, "CGNAT"},
		{"198.18.0.5:443", false, "fake-IP 网段"},
		{"224.0.0.251:5353", false, "组播"},
		{"255.255.255.255:67", false, "广播"},
		{"0.0.0.0:80", false, "0/8"},
		{"8.8.4.4:53", false, "本机网卡自己的地址"},
		{"[2606:4700::1111]:443", false, "IPv6 一律拒绝"},
		{"[::1]:443", false, "IPv6 回环"},
		{"[::ffff:1.1.1.1]:443", true, "IPv4 映射地址按 IPv4 处理"},
		{"192.168.1.20:7890", true, "精确白名单"},
		{"127.0.0.1:7891", true, "回环白名单"},
		{"127.0.0.1:7892", false, "白名单同地址换端口仍拒绝"},
		{"1.1.1.1:0", false, "端口 0"},
	}
	for _, c := range cases {
		for _, network := range []string{"tcp", "udp"} {
			if got := p.Allows(network, netip.MustParseAddrPort(c.dst)); got != c.want {
				t.Errorf("%s %s（%s）：得到 %v，应为 %v", network, c.dst, c.why, got, c.want)
			}
		}
	}
	if p.Allows("icmp", netip.MustParseAddrPort("1.1.1.1:0")) {
		t.Error("tcp、udp 以外一律拒绝")
	}
}

// openRules 允许一切（用来在回环上测绑定路径），白名单可配。
type openRules struct{ white map[netip.AddrPort]bool }

func (o openRules) Allows(string, netip.AddrPort) bool { return true }
func (o openRules) Whitelisted(d netip.AddrPort) bool  { return o.white[d] }

// listenCount 在 127.0.0.1 上监听，统计收到的连接数。
func listenCount(t *testing.T) (netip.AddrPort, *atomic.Int32) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	var n atomic.Int32
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			c.Close()
		}
	}()
	return netip.MustParseAddrPort(l.Addr().String()), &n
}

func settle() { time.Sleep(30 * time.Millisecond) }

func TestDialerBindsAndFailsClosed(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("IP_BOUND_IF 只在 macOS 上")
	}
	dst, n := listenCount(t)
	var queries atomic.Int32
	name := "lo0"
	d := &Dialer{Rules: openRules{}, Interface: func(context.Context) (string, error) { queries.Add(1); return name, nil }}

	c, err := d.DialContext(context.Background(), "tcp", dst.String())
	if err != nil {
		t.Fatalf("绑定 lo0 拨回环应成功：%v", err)
	}
	c.Close()
	if ct := d.Counters(); ct.DialOK != 1 || ct.DialOKLoopback != 0 {
		t.Fatalf("绑网卡拨号只计入 dial_ok：%+v", ct)
	}
	settle()
	if n.Load() != 1 || queries.Load() != 1 {
		t.Fatalf("连接 %d，查询 %d", n.Load(), queries.Load())
	}

	// 网卡不存在：绑定失败，没有连接，调用 OnBindFailed；拨号器不缓存，每次拨号都经 Interface 取网卡。
	var invalidated atomic.Int32
	d.OnBindFailed = func() { invalidated.Add(1) }
	settle() // 先等前面成功的拨号被计数
	before := n.Load()
	name = "nope0"
	if _, err := d.DialContext(context.Background(), "tcp", dst.String()); err == nil {
		t.Fatal("绑定不存在的网卡应失败")
	}
	settle()
	if n.Load() != before {
		t.Fatal("绑定失败时不应有连接")
	}
	if invalidated.Load() != 1 || d.Counters().BindFailed != 1 {
		t.Fatalf("绑定失败应调用 OnBindFailed 并计 bind_failed：%d %+v", invalidated.Load(), d.Counters())
	}
	q := queries.Load()
	name = "lo0"
	if _, err := d.DialContext(context.Background(), "tcp", dst.String()); err != nil {
		t.Fatalf("网卡名变回来后应成功：%v", err)
	}
	if queries.Load() != q+1 || d.CurrentInterface() != "lo0" {
		t.Fatal("下一次拨号应重新经 Interface 取网卡")
	}
}

func TestDialerInterfaceQueryFails(t *testing.T) {
	dst, n := listenCount(t)
	d := &Dialer{Rules: openRules{}, Interface: func(context.Context) (string, error) { return "", errors.New("no route") }}
	if _, err := d.DialContext(context.Background(), "tcp", dst.String()); err == nil {
		t.Fatal("网卡查询失败应拒绝")
	}
	settle()
	if n.Load() != 0 || d.Counters().BindFailed != 1 {
		t.Fatalf("网卡查询失败时不应有连接（不退回不绑定的拨号），计 bind_failed：%+v", d.Counters())
	}
	d.Interface = func(context.Context) (string, error) { return "", nil }
	if _, err := d.DialContext(context.Background(), "tcp", dst.String()); err == nil {
		t.Fatal("网卡名为空应拒绝")
	}
	d.Interface = nil
	if _, err := d.DialContext(context.Background(), "tcp", dst.String()); err == nil {
		t.Fatal("没有配置网卡查询应拒绝")
	}
	settle()
	if n.Load() != 0 {
		t.Fatal("任何情况下都不应有不绑网卡的连接")
	}
}

func TestDialerHookAndWhitelistLoopback(t *testing.T) {
	dst, n := listenCount(t)
	var seen netip.AddrPort
	var ifQueries atomic.Int32
	d := &Dialer{
		Rules:     openRules{white: map[netip.AddrPort]bool{dst: true}},
		Interface: func(context.Context) (string, error) { ifQueries.Add(1); return "nope0", nil },
		BeforeDial: func(_ string, a netip.AddrPort) error {
			seen = a
			return errors.New("veto")
		},
	}
	if _, err := d.DialContext(context.Background(), "tcp", dst.String()); err == nil || seen != dst {
		t.Fatalf("钩子应在拨号前拿到目标并能否决：%v %v", err, seen)
	}
	settle()
	if n.Load() != 0 {
		t.Fatal("钩子否决时不应有连接")
	}
	// 回环白名单目标不绑网卡：网卡查询返回不存在的网卡也能连上，且根本没查网卡。
	d.BeforeDial = nil
	c, err := d.DialContext(context.Background(), "tcp", dst.String())
	if err != nil {
		t.Fatalf("回环白名单目标应不绑网卡直接连：%v", err)
	}
	c.Close()
	if ifQueries.Load() != 0 {
		t.Fatal("回环白名单目标不应查询网卡")
	}
}

func TestDialerRejects(t *testing.T) {
	p := &Policy{OwnAddrs: func() []netip.Addr { return nil }}
	var queried atomic.Bool
	d := &Dialer{Rules: p, Interface: func(context.Context) (string, error) { queried.Store(true); return "lo0", nil }}
	for _, c := range []struct{ network, addr string }{
		{"tcp", "example.com:443"},    // 域名
		{"tcp", "[2606:4700::1]:443"}, // IPv6
		{"udp", "10.0.0.1:53"},        // 私有（udp 同一套逻辑）
		{"tcp", "127.0.0.1:80"},       // 回环（不在白名单）
		{"ip4:icmp", "1.1.1.1:0"},     // 其他网络类型
	} {
		if _, err := d.DialContext(context.Background(), c.network, c.addr); err == nil {
			t.Errorf("%s %s 应被拒", c.network, c.addr)
		}
	}
	if queried.Load() {
		t.Error("被策略拒绝的目标不应走到网卡查询")
	}
}

func TestDialerUDPBinds(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("IP_BOUND_IF 只在 macOS 上")
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	d := &Dialer{Rules: openRules{}, Interface: func(context.Context) (string, error) { return "lo0", nil }}
	c, err := d.DialContext(context.Background(), "udp", pc.LocalAddr().String())
	if err != nil {
		t.Fatalf("UDP 绑定 lo0 应成功：%v", err)
	}
	c.Write([]byte("x"))
	c.Close()
	pc.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 4)
	if n, _, err := pc.ReadFrom(buf); err != nil || n != 1 {
		t.Fatalf("UDP 没收到：%v", err)
	}
	d2 := &Dialer{Rules: openRules{}, Interface: func(context.Context) (string, error) { return "nope0", nil }}
	if _, err := d2.DialContext(context.Background(), "udp", pc.LocalAddr().String()); err == nil {
		t.Fatal("UDP 绑定不存在的网卡也应失败")
	}
}

func TestRefuseDNS(t *testing.T) {
	for _, qt := range []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeHTTPS} {
		q := new(dns.Msg)
		q.SetQuestion("claude.ai.", qt)
		q.Id = 0x4242
		r, err := RefuseDNS(context.Background(), q)
		if err != nil || r.Rcode != dns.RcodeRefused || r.Id != 0x4242 || len(r.Answer) != 0 ||
			len(r.Question) != 1 || r.Question[0] != q.Question[0] || !r.Response {
			t.Fatalf("%s：应回 REFUSED 且保留 ID 和问题段：%v", dns.TypeToString[qt], r)
		}
	}
}

func TestNewConfiguration(t *testing.T) {
	p := &Policy{OwnAddrs: func() []netip.Addr { return nil }}
	d := &Dialer{Interface: func(context.Context) (string, error) { return "en0", nil }}
	ref := NewPolicyRef(p)
	c, err := NewConfiguration(d, ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.DialContext == nil || c.DNSExchange == nil || c.Outbound == nil {
		t.Fatal("DialContext、DNSExchange、Outbound 都必须非空")
	}
	if len(c.NAT) != 0 || len(c.Forwards) != 0 || len(c.DNS) != 0 || c.Ec2MetadataAccess {
		t.Fatalf("NAT、Forwards、DNS 应为空，元数据访问应关闭：%+v", c)
	}
	if c.Subnet != "192.168.127.0/24" || c.GatewayIP != "192.168.127.1" {
		t.Fatalf("网段：%s %s", c.Subnet, c.GatewayIP)
	}
	if c.Outbound("tcp", netip.MustParseAddrPort("10.0.0.1:80")) || !c.Outbound("tcp", netip.MustParseAddrPort("1.1.1.1:443")) {
		t.Fatal("Outbound 应是出站策略")
	}
	if d.Counters().PolicyDenied != 1 {
		t.Fatalf("Outbound 拒绝应计入 policy_denied：%+v", d.Counters())
	}
	// 换策略：Outbound 和拨号器都立即看到新白名单。
	ref.Store(&Policy{Allow: []netip.AddrPort{netip.MustParseAddrPort("10.0.0.1:80")}, OwnAddrs: p.OwnAddrs})
	if !c.Outbound("tcp", netip.MustParseAddrPort("10.0.0.1:80")) || !d.Rules.Allows("tcp", netip.MustParseAddrPort("10.0.0.1:80")) {
		t.Fatal("替换策略后应立即生效")
	}
	if _, err := NewConfiguration(nil, ref, nil); err == nil {
		t.Fatal("缺拨号器应报错")
	}
}

func TestPolicyOwnAddrsErrorDenies(t *testing.T) {
	calls := 0
	p := &Policy{InterfaceAddrs: func() ([]net.Addr, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("boom")
		}
		return nil, nil
	}}
	if p.Allows("tcp", netip.MustParseAddrPort("1.1.1.1:443")) {
		t.Fatal("查不到本机地址时应拒绝")
	}
	if !p.Allows("tcp", netip.MustParseAddrPort("1.1.1.1:443")) || calls != 2 {
		t.Fatalf("失败不应缓存，下次应重查并放行：%d 次", calls)
	}
}

// 网卡查询的合并、缓存、失败缓存已挪到 physif.Watcher（M2-2），见 internal/physif 的测试。

func TestDialerTimeoutAndCounters(t *testing.T) {
	p := &Policy{OwnAddrs: func() []netip.Addr { return nil }}
	d := &Dialer{Rules: p, Interface: func(context.Context) (string, error) { return "lo0", nil }}
	d.DialContext(context.Background(), "tcp", "10.0.0.1:80")
	d.DialContext(context.Background(), "udp", "192.168.127.1:53")
	if c := d.Counters(); c.PolicyDenied != 2 {
		t.Fatalf("被拒计数：%+v", c)
	}
	// 超时：拨号器的 Timeout 包住拨号（这里用 BeforeDial 之后的 ctx 观察）。
	var dl time.Time
	d2 := &Dialer{Rules: openRules{}, Timeout: 50 * time.Millisecond,
		Interface: func(ctx context.Context) (string, error) { dl, _ = ctx.Deadline(); return "", errors.New("x") }}
	start := time.Now()
	d2.DialContext(context.Background(), "tcp", "1.1.1.1:443")
	if dl.IsZero() || dl.Sub(start) > time.Second {
		t.Fatalf("拨号应带上超时：%v", dl)
	}
}

// IP_BOUND_IF 真的生效：绑定一块非回环网卡去拨 127.0.0.1，应失败且监听端零连接。
func TestBoundIfReallyBinds(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("IP_BOUND_IF 只在 macOS 上")
	}
	var ifname string
	ifs, _ := net.Interfaces()
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 || ifi.Flags&net.FlagPointToPoint != 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
				ifname = ifi.Name
			}
		}
		if ifname != "" {
			break
		}
	}
	if ifname == "" {
		t.Skip("没有已启用且有 IPv4 地址的非回环网卡")
	}
	dst, n := listenCount(t)
	d := &Dialer{Rules: openRules{}, Timeout: 2 * time.Second, Interface: func(context.Context) (string, error) { return ifname, nil }}
	c, err := d.DialContext(context.Background(), "tcp", dst.String())
	if c != nil {
		c.Close()
	}
	settle()
	t.Logf("绑定 %s 拨 %s：err=%v，监听端连接数 %d", ifname, dst, err, n.Load())
	if err == nil || n.Load() != 0 {
		t.Fatalf("绑定 %s 后不应能连到回环地址（IP_BOUND_IF 没生效？）", ifname)
	}
}

func TestParseAllow(t *testing.T) {
	got, err := ParseAllow([]string{"127.0.0.1:7890", "192.168.1.2:1080", "10.1.1.1:8080", "100.64.0.9:443", "172.16.0.1:1", "127.0.0.1:7890"})
	if err != nil || len(got) != 5 {
		t.Fatalf("正例（去重后 5 项）：%v %v", got, err)
	}
	for name, s := range map[string]string{
		"缺端口": "127.0.0.1", "端口 0": "127.0.0.1:0", "端口越界": "127.0.0.1:70000", "IPv6": "[::1]:80",
		"其他回环": "127.0.0.2:80", "0.0.0.0": "0.0.0.0:80", "组播": "224.0.0.1:80", "广播": "255.255.255.255:80",
		"链路本地": "169.254.1.1:80", "虚拟机子网": "192.168.127.5:80", "公网": "1.1.1.1:443", "域名": "example.com:80",
	} {
		if _, err := ParseAllow([]string{s}); err == nil {
			t.Errorf("%s（%s）应被拒", name, s)
		}
	}
	var many []string
	for i := 1; i <= MaxAllow+1; i++ {
		many = append(many, "10.0.0.1:"+strconv.Itoa(i))
	}
	if _, err := ParseAllow(many); err == nil {
		t.Error("超过上限应被拒")
	}
}

func TestLoopbackNATOnlyWithLoopbackAllow(t *testing.T) {
	own := func() []netip.Addr { return nil }
	c, _ := NewConfiguration(&Dialer{}, NewPolicyRef(&Policy{OwnAddrs: own, Allow: []netip.AddrPort{netip.MustParseAddrPort("192.168.1.2:1080")}}), nil)
	if len(c.NAT) != 0 || len(c.GatewayVirtualIPs) != 0 {
		t.Fatalf("没有回环项时不应有 NAT：%v %v", c.NAT, c.GatewayVirtualIPs)
	}
	p := &Policy{OwnAddrs: own, Allow: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:7890")}}
	c, _ = NewConfiguration(&Dialer{}, NewPolicyRef(p), nil)
	if len(c.NAT) != 1 || c.NAT["192.168.127.254"] != "127.0.0.1" || len(c.GatewayVirtualIPs) != 1 || c.GatewayVirtualIPs[0] != "192.168.127.254" {
		t.Fatalf("有回环项时应恰好一条映射：%v %v", c.NAT, c.GatewayVirtualIPs)
	}
	if !p.Allows("tcp", netip.MustParseAddrPort("127.0.0.1:7890")) || p.Allows("tcp", netip.MustParseAddrPort("127.0.0.1:7891")) {
		t.Fatal("回环白名单只放行精确端口")
	}
}

func TestDialerWhitelistBinding(t *testing.T) {
	dst, n := listenCount(t)
	lan := netip.MustParseAddrPort("192.168.1.2:1080")
	var queried atomic.Int32
	p := &Policy{OwnAddrs: func() []netip.Addr { return nil }, Allow: []netip.AddrPort{dst, lan}}
	d := &Dialer{Rules: p, Timeout: 300 * time.Millisecond, Interface: func(context.Context) (string, error) { queried.Add(1); return "", errors.New("no iface") }}
	c, err := d.DialContext(context.Background(), "tcp", dst.String())
	if err != nil {
		t.Fatalf("回环白名单应不绑网卡直接连：%v", err)
	}
	c.Close()
	settle()
	if queried.Load() != 0 || n.Load() != 1 {
		t.Fatalf("回环白名单不应查网卡：查询 %d，连接 %d", queried.Load(), n.Load())
	}
	if c := d.Counters(); c.DialOK != 1 || c.DialOKLoopback != 1 {
		t.Fatalf("回环拨号应同时计入 dial_ok 和 dial_ok_loopback：%+v", c)
	}
	if _, err := d.DialContext(context.Background(), "tcp", lan.String()); err == nil || queried.Load() != 1 {
		t.Fatalf("局域网白名单仍要绑网卡（网卡查询失败即拒绝）：%v，查询 %d", err, queried.Load())
	}
}

// DialDirect（直连拨号模块用）：一律绑网卡、失败即拒绝；不经出站策略；计 direct_dial_ok。
func TestDialDirect(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("IP_BOUND_IF 只在 macOS 上")
	}
	dst, n := listenCount(t)
	ap := netip.MustParseAddrPort(dst.String())
	name := "lo0"
	d := &Dialer{Interface: func(context.Context) (string, error) { return name, nil }} // 没有 Rules：不经策略
	c, err := d.DialDirect(context.Background(), "tcp", ap)
	if err != nil {
		t.Fatalf("绑 lo0 拨回环应成功：%v", err)
	}
	c.Close()
	if ct := d.Counters(); ct.DirectDialOK != 1 || ct.DialOK != 0 {
		t.Fatalf("只计 direct_dial_ok：%+v", ct)
	}
	settle()
	before := n.Load()
	name = "nope0"
	if _, err := d.DialDirect(context.Background(), "tcp", ap); err == nil {
		t.Fatal("绑定失败应拒绝")
	}
	d.Interface = func(context.Context) (string, error) { return "", errors.New("no route") }
	if _, err := d.DialDirect(context.Background(), "tcp", ap); err == nil {
		t.Fatal("网卡查询失败应拒绝")
	}
	for _, bad := range []string{"[::1]:80", "127.0.0.1:0"} {
		if _, err := d.DialDirect(context.Background(), "tcp", netip.MustParseAddrPort(bad)); err == nil {
			t.Fatalf("%s 应拒绝", bad)
		}
	}
	if _, err := d.DialDirect(context.Background(), "icmp", ap); err == nil {
		t.Fatal("不支持的网络类型应拒绝")
	}
	settle()
	if n.Load() != before {
		t.Fatal("失败时不应有连接（不退回不绑定的拨号）")
	}
}
