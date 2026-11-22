package usernet

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestParseFirstHops(t *testing.T) {
	hops, err := ParseFirstHops([]string{"Node.Example.com.:443,8000-8010", "8.8.4.4:443", "node.example.com:9443", "1.1.1.1:443"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hops) != 3 || hops[0].Host != "node.example.com" || len(hops[0].Ports) != 3 || hops[1].Host != "8.8.4.4" {
		t.Fatalf("解析结果不对：%+v", hops)
	}
	for _, bad := range []string{"node.example.com", "node.example.com:", "node.example.com:0", "node.example.com:10-5",
		"node.example.com:70000", "10.0.0.1:443", "127.0.0.1:7890", "192.168.1.2:1080", "[2001:db8::1]:443",
		"bad_host:443", "-x.com:443", "1.2.3:443", "a..b:443", "x.com:443,", "x.com:a"} {
		if _, err := ParseFirstHops([]string{bad}); err == nil {
			t.Errorf("%q 应被拒", bad)
		}
	}
	var many []string
	for i := range MaxFirstHops + 1 {
		many = append(many, fmt.Sprintf("n%d.example.com:443", i))
	}
	if _, err := ParseFirstHops(many); err == nil {
		t.Error("超过上限应被拒")
	}
}

func TestParseFirstHopsDocNetRejected(t *testing.T) {
	if _, err := ParseFirstHops([]string{"203.0.113.5:443"}); err == nil {
		t.Fatal("文档网段不是公网地址，应被拒")
	}
	hops, err := ParseFirstHops([]string{"Node.Example.com.:443,8000-8010", "8.8.4.4:443", "node.example.com:9443"})
	if err != nil || len(hops) != 2 || len(hops[0].Ports) != 3 || !hops[0].HasPort(8005) || hops[0].HasPort(8011) || !hops[0].HasPort(9443) {
		t.Fatalf("同一主机的端口应合并：%+v %v", hops, err)
	}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func ap(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

func TestOutbound(t *testing.T) {
	hops, err := ParseFirstHops([]string{"8.8.4.4:443", "node.example.com:443,8000-8010"})
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	o := NewOutbound([]netip.AddrPort{ap("127.0.0.1:7890")}, hops, c.now)
	allow := func(net, dst string) bool { return o.BeforeDial(net, ap(dst)) == nil }
	for _, x := range []struct {
		net, dst string
		want     bool
	}{
		{"tcp", "127.0.0.1:7890", true},  // 白名单
		{"udp", "127.0.0.1:7890", true},  // 白名单（udp 同样）
		{"tcp", "127.0.0.1:7891", false}, // 白名单之外的端口
		{"tcp", "8.8.4.4:443", true},     // 字面量第一跳
		{"udp", "8.8.4.4:443", true},
		{"tcp", "8.8.4.4:53", false},    // 端口不符
		{"tcp", "1.1.1.1:443", false},   // DoH 的 IP 不对虚拟机放行
		{"tcp", "223.5.5.5:443", false}, // 同上
		{"tcp", "9.9.9.9:443", false},   // 没答过
		{"icmp", "8.8.4.4:443", false},  // 只认 tcp、udp
		{"tcp", "[::ffff:8.8.4.4]:443", true},
	} {
		if got := allow(x.net, x.dst); got != x.want {
			t.Errorf("%s %s：%v，应为 %v", x.net, x.dst, got, x.want)
		}
	}
	o.Learn("other.example.com", []netip.Addr{netip.MustParseAddr("9.9.9.9")}) // 没登记：忽略
	if allow("tcp", "9.9.9.9:443") {
		t.Error("没登记的域名答出的地址不应放行")
	}
	o.Learn("node.example.com", []netip.Addr{netip.MustParseAddr("9.9.9.9")})
	if !allow("tcp", "9.9.9.9:443") || !allow("udp", "9.9.9.9:8005") || allow("tcp", "9.9.9.9:8011") || allow("tcp", "9.9.9.9:53") {
		t.Error("答过的地址只在该域名的端口上放行")
	}
	c.add(AnsweredKeep + time.Second)
	if allow("tcp", "9.9.9.9:443") {
		t.Error("最后一次应答 1 小时后应过期")
	}
	// 上限：超过 64 个淘汰最旧的。
	for i := range MaxAnsweredPer + 1 {
		c.add(time.Second)
		o.Learn("node.example.com", []netip.Addr{netip.AddrFrom4([4]byte{9, 9, 1, byte(i)})})
	}
	if allow("tcp", "9.9.1.0:443") || !allow("tcp", "9.9.1.1:443") || !allow("tcp", fmt.Sprintf("9.9.1.%d:443", MaxAnsweredPer)) {
		t.Error("超过上限时应只淘汰最旧的")
	}
	// 再次应答会刷新时间。
	o.Learn("node.example.com", []netip.Addr{netip.MustParseAddr("9.9.1.1")})
	c.add(AnsweredKeep - time.Second)
	if !allow("tcp", "9.9.1.1:443") || allow("tcp", "9.9.1.2:443") {
		t.Error("重新应答应刷新保留时间")
	}
}

func TestOutboundTightWithoutHops(t *testing.T) {
	o := NewOutbound(nil, nil, nil)
	if o.BeforeDial("tcp", ap("8.8.8.8:443")) == nil {
		t.Fatal("收紧模式没有第一跳时什么公网都不放行")
	}
}

func TestDialerHopDeniedCountedSeparately(t *testing.T) {
	o := NewOutbound(nil, nil, nil)
	d := &Dialer{Rules: openRules{}, BeforeDial: o.BeforeDial,
		Interface: func(context.Context) (string, error) { return "nope0", nil }}
	if _, err := d.DialContext(context.Background(), "tcp", "8.8.8.8:443"); !errors.Is(err, ErrNotFirstHop) {
		t.Fatalf("应被第一跳检查拒绝：%v", err)
	}
	if c := d.Counters(); c.HopDenied != 1 || c.PolicyDenied != 0 || c.DialOK != 0 {
		t.Fatalf("应计 hop_denied 而不是 policy_denied：%+v", c)
	}
}

func TestDialResolver(t *testing.T) {
	dst, n := listenCount(t)
	d := &Dialer{Rules: openRules{}, Interface: func(context.Context) (string, error) { return "lo0", nil }}
	if _, err := d.DialResolver(context.Background(), "tcp", dst.String(), []netip.AddrPort{ap("1.1.1.1:443")}); !errors.Is(err, ErrDenied) {
		t.Fatalf("不在允许列表里的地址应被拒：%v", err)
	}
	if _, err := d.DialResolver(context.Background(), "udp", dst.String(), []netip.AddrPort{dst}); !errors.Is(err, ErrDenied) {
		t.Fatalf("只允许 tcp：%v", err)
	}
	c, err := d.DialResolver(context.Background(), "tcp", dst.String(), []netip.AddrPort{dst})
	if err != nil {
		t.Skipf("本机不能把 socket 绑到 lo0（%v），跳过", err)
	}
	c.Close()
	settle()
	if n.Load() != 1 {
		t.Fatal("应连上")
	}
	if cs := d.Counters(); cs.ResolverDialOK != 1 || cs.DialOK != 0 || cs.DialOKLoopback != 0 {
		t.Fatalf("解析器的拨号只计 resolver_dial_ok：%+v", cs)
	}
	// 网卡识别失败：拒绝，计 bind_failed。
	d2 := &Dialer{Interface: func(context.Context) (string, error) { return "", errors.New("x") }}
	if _, err := d2.DialResolver(context.Background(), "tcp", dst.String(), []netip.AddrPort{dst}); err == nil || d2.Counters().BindFailed != 1 {
		t.Fatalf("识别不到网卡应拒绝：%v %+v", err, d2.Counters())
	}
}

func query(name string, qtype uint16) *dns.Msg {
	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(name), qtype)
	q.Id = 0x4242
	return q
}

func TestGatewayDNS(t *testing.T) {
	hops, _ := ParseFirstHops([]string{"node.example.com:443"})
	o := NewOutbound(nil, hops, nil)
	var calls atomic.Int32
	answer := []netip.Addr{netip.MustParseAddr("9.9.9.9"), netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("127.0.0.1")}
	var fail atomic.Bool
	g := &GatewayDNS{Out: o, Resolve: func(_ context.Context, name string) ([]netip.Addr, time.Duration, error) {
		calls.Add(1)
		if name != "node.example.com" {
			t.Errorf("只应解析登记的域名：%s", name)
		}
		if fail.Load() {
			return nil, 0, errors.New("boom")
		}
		return answer, 90 * time.Second, nil
	}}
	ctx := context.Background()
	m, _ := g.Exchange(ctx, query("other.example.com", dns.TypeA))
	if m.Rcode != dns.RcodeRefused || m.Id != 0x4242 || calls.Load() != 0 {
		t.Fatalf("未登记的域名应 REFUSED 且不解析：%v", m)
	}
	m, _ = g.Exchange(ctx, query("NODE.example.com", dns.TypeA))
	if m.Rcode != dns.RcodeSuccess || len(m.Answer) != 1 || m.Answer[0].(*dns.A).A.String() != "9.9.9.9" || m.Answer[0].Header().Ttl != 90 {
		t.Fatalf("登记域名的 A 应只留公网地址：%v", m)
	}
	if o.BeforeDial("tcp", ap("9.9.9.9:443")) != nil {
		t.Fatal("答出的地址应进放行集合")
	}
	if o.BeforeDial("tcp", ap("10.0.0.1:443")) == nil {
		t.Fatal("被滤掉的私有地址不应进放行集合")
	}
	m, _ = g.Exchange(ctx, query("node.example.com", dns.TypeAAAA))
	if m.Rcode != dns.RcodeSuccess || len(m.Answer) != 0 {
		t.Fatalf("AAAA 应回空的 NOERROR：%v", m)
	}
	multi := query("node.example.com", dns.TypeA)
	multi.Question = append(multi.Question, multi.Question[0])
	if m, _ = g.Exchange(ctx, multi); m.Rcode != dns.RcodeRefused {
		t.Fatal("多问题应 REFUSED")
	}
	chaos := query("node.example.com", dns.TypeA)
	chaos.Question[0].Qclass = dns.ClassCHAOS
	if m, _ = g.Exchange(ctx, chaos); m.Rcode != dns.RcodeRefused {
		t.Fatal("非 IN 类应 REFUSED")
	}
	answer = []netip.Addr{netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("100.64.0.1")}
	if m, _ = g.Exchange(ctx, query("node.example.com", dns.TypeA)); m.Rcode != dns.RcodeServerFailure {
		t.Fatal("全被滤掉应 SERVFAIL")
	}
	fail.Store(true)
	if m, _ = g.Exchange(ctx, query("node.example.com", dns.TypeA)); m.Rcode != dns.RcodeServerFailure {
		t.Fatal("解析失败应 SERVFAIL")
	}
	fail.Store(false)
	answer = nil
	for i := range 40 {
		answer = append(answer, netip.AddrFrom4([4]byte{9, 9, 9, byte(i)}))
	}
	if m, _ = g.Exchange(ctx, query("node.example.com", dns.TypeA)); len(m.Answer) != MaxGatewayAnswers {
		t.Fatalf("最多 %d 条：%d", MaxGatewayAnswers, len(m.Answer))
	}
	if c := g.Counters(); c.Answered != 2 || c.Refused != 3 || c.Failed != 2 {
		t.Fatalf("计数：%+v", c)
	}
}

// dohServer 是假的 DoH 服务（httptest TLS，证书带 IP 127.0.0.1）。
type dohServer struct {
	srv   *httptest.Server
	hits  atomic.Int32
	delay time.Duration
	fail  atomic.Bool
	ttl   uint32
	big   bool
}

func newDoHServer(t *testing.T, ttl uint32) *dohServer {
	s := &dohServer{ttl: ttl}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		time.Sleep(s.delay)
		if s.fail.Load() || r.Method != http.MethodPost || r.URL.Path != "/dns-query" || r.Header.Get("Content-Type") != "application/dns-message" {
			http.Error(w, "no", http.StatusBadGateway)
			return
		}
		body, _ := io.ReadAll(r.Body)
		q := new(dns.Msg)
		if q.Unpack(body) != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		m := new(dns.Msg)
		m.SetReply(q)
		m.Answer = append(m.Answer, &dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: s.ttl}, A: net.IPv4(9, 9, 9, 9)})
		b, _ := m.Pack()
		if s.big {
			b = append(b, make([]byte, DoHMaxAnswer)...)
		}
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(b)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *dohServer) addr() netip.AddrPort {
	return netip.MustParseAddrPort(s.srv.Listener.Addr().String())
}

func dohFor(t *testing.T, servers ...*dohServer) (*DoH, *[]string) {
	pool := x509.NewCertPool()
	var eps []netip.AddrPort
	for _, s := range servers {
		pool.AddCert(s.srv.Certificate())
		eps = append(eps, s.addr())
	}
	var mu sync.Mutex
	dialed := &[]string{}
	r := &DoH{Endpoints: eps, TLS: &tls.Config{RootCAs: pool},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			mu.Lock()
			*dialed = append(*dialed, addr)
			mu.Unlock()
			ok := false
			for _, e := range eps {
				ok = ok || e.String() == addr
			}
			if !ok {
				return nil, fmt.Errorf("不允许的拨号目标 %s", addr)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		}}
	return r, dialed
}

func TestDoHLookup(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1") // 读了环境变量就会连不上
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	slow, fast := newDoHServer(t, 5), newDoHServer(t, 5)
	slow.delay = 2 * time.Second
	r, dialed := dohFor(t, slow, fast)
	start := time.Now()
	addrs, ttl, err := r.Lookup(context.Background(), "node.example.com")
	if err != nil || len(addrs) != 1 || addrs[0].String() != "9.9.9.9" {
		t.Fatalf("应拿到地址：%v %v", addrs, err)
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Error("应取先成功的，不等慢的那个")
	}
	if ttl != DoHMinTTL {
		t.Errorf("TTL 5 秒应夹到 %s：%s", DoHMinTTL, ttl)
	}
	if r.httpClient().Transport.(*http.Transport).Proxy != nil {
		t.Error("Transport.Proxy 必须为 nil")
	}
	for _, a := range *dialed {
		if a != slow.addr().String() && a != fast.addr().String() {
			t.Errorf("拨号目标只能是 DoH 的地址：%s", a)
		}
	}
}

func TestDoHTTLClampAndCoalesce(t *testing.T) {
	s := newDoHServer(t, 86400)
	s.delay = 200 * time.Millisecond
	r, _ := dohFor(t, s)
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ttl, err := r.Lookup(context.Background(), "node.example.com"); err != nil || ttl != DoHMaxTTL {
				t.Errorf("TTL 一天应夹到 %s：%s %v", DoHMaxTTL, ttl, err)
			}
		}()
	}
	wg.Wait()
	if s.hits.Load() != 1 {
		t.Fatalf("并发查询应合并成一次，实际 %d 次", s.hits.Load())
	}
	if _, _, err := r.Lookup(context.Background(), "node.example.com"); err != nil || s.hits.Load() != 1 {
		t.Fatal("TTL 内应命中缓存")
	}
}

func TestDoHOversizeAndStale(t *testing.T) {
	big := newDoHServer(t, 60)
	big.big = true
	r, _ := dohFor(t, big)
	if _, _, err := r.Lookup(context.Background(), "node.example.com"); err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("超大应答应拒绝：%v", err)
	}

	s := newDoHServer(t, 60)
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	r, _ = dohFor(t, s)
	r.Now = c.now
	if _, _, err := r.Lookup(context.Background(), "node.example.com"); err != nil {
		t.Fatal(err)
	}
	s.fail.Store(true)
	c.add(2 * time.Minute) // 过了 TTL，查询失败：回旧结果，TTL 30 秒
	addrs, ttl, err := r.Lookup(context.Background(), "node.example.com")
	if err != nil || len(addrs) != 1 || ttl != DoHStaleTTL {
		t.Fatalf("1 小时内应回旧结果：%v %s %v", addrs, ttl, err)
	}
	c.add(DoHStaleFor)
	if _, _, err := r.Lookup(context.Background(), "node.example.com"); err == nil {
		t.Fatal("超过 1 小时的旧结果不应再用")
	}
	if _, _, err := r.Lookup(context.Background(), "never.example.com"); err == nil {
		t.Fatal("没有旧结果时应报错")
	}
}

func TestDoHStaleServedQuickly(t *testing.T) {
	s := newDoHServer(t, 60)
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	r, _ := dohFor(t, s)
	r.Now = c.now
	if _, _, err := r.Lookup(context.Background(), "node.example.com"); err != nil {
		t.Fatal(err)
	}
	s.delay = 4 * time.Second // DoH 卡住
	c.add(2 * time.Minute)
	start := time.Now()
	addrs, ttl, err := r.Lookup(context.Background(), "node.example.com")
	if err != nil || len(addrs) != 1 || ttl != DoHStaleTTL || time.Since(start) > 2*time.Second {
		t.Fatalf("DoH 卡住时应在 2 秒内回旧结果：%v %s %v，用时 %s", addrs, ttl, err, time.Since(start))
	}
}

// M5-4：第一跳域名的答案全是私有地址时被拒并计 dns_hop_private（给用户“局域网里的代理请直接填 IP 地址”的提示）；
// 有公网地址时照常、不计；分流出口端点的域名答私有地址是允许的，也不计。
func TestGatewayDNSHopPrivate(t *testing.T) {
	hops, _ := ParseFirstHops([]string{"node.example.com:443"})
	o := NewOutbound(nil, hops, nil)
	_ = o.SetExitEndpoints([]ExitEndpoint{{Host: "wg.example.com", Proto: "udp", Port: 51820}}, false)
	answer := []netip.Addr{netip.MustParseAddr("192.168.1.20")}
	g := &GatewayDNS{Out: o, Resolve: func(_ context.Context, name string) ([]netip.Addr, time.Duration, error) {
		return answer, time.Minute, nil
	}}
	ctx := context.Background()
	if m, _ := g.Exchange(ctx, query("node.example.com", dns.TypeA)); m.Rcode != dns.RcodeServerFailure {
		t.Fatalf("第一跳域名只答私有地址时应 SERVFAIL：%v", m)
	}
	if c := g.Counters(); c.HopPrivate != 1 || c.Failed != 1 {
		t.Fatalf("应计一次 dns_hop_private：%+v", c)
	}
	if m, _ := g.Exchange(ctx, query("wg.example.com", dns.TypeA)); m.Rcode != dns.RcodeSuccess {
		t.Fatalf("分流出口端点的域名答私有地址是允许的：%v", m)
	}
	answer = []netip.Addr{netip.MustParseAddr("9.9.9.9"), netip.MustParseAddr("10.0.0.1")}
	g.Exchange(ctx, query("node.example.com", dns.TypeA))
	if c := g.Counters(); c.HopPrivate != 1 {
		t.Fatalf("有公网地址时不计：%+v", c)
	}
}
