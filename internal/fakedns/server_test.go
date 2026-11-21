package fakedns

import (
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"net"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// fakeUp 是假的 Upstream：记录调用次数；A 查询回 realA，其余回空 NOERROR。
type fakeUp struct {
	calls atomic.Int32
	err   error
	delay time.Duration
	realA []string
	ttl   uint32
	big   int // 非 0 时塞这么多条 TXT（触发 TC）
}

func (f *fakeUp) Exchange(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	m := new(dns.Msg)
	m.SetReply(q)
	m.Id = 0xdead // 服务端应改回请求的 ID
	qs := q.Question[0]
	if qs.Qtype == dns.TypeA {
		for _, a := range f.realA {
			m.Answer = append(m.Answer, &dns.A{Hdr: dns.RR_Header{Name: qs.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: f.ttl}, A: net.ParseIP(a)})
		}
	}
	for i := range f.big {
		m.Answer = append(m.Answer, &dns.TXT{Hdr: dns.RR_Header{Name: qs.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60},
			Txt: []string{strings.Repeat("x", 200) + strconv.Itoa(i)}})
	}
	return m, nil
}

func newServer(up Upstream) *Server {
	return &Server{Pool: DefaultPool(), Upstream: up, Bypass: NewBypass(), Reverse: NewReverse(), Now: func() time.Time { return t0 }}
}

func query(name string, qtype uint16) *dns.Msg {
	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(name), qtype)
	q.Id = 0x1234
	return q
}

// a. 每个分支
func TestHandleTable(t *testing.T) {
	multi := query("a.com", dns.TypeA)
	multi.Question = append(multi.Question, multi.Question[0])
	chaos := query("a.com", dns.TypeA)
	chaos.Question[0].Qclass = dns.ClassCHAOS
	notify := query("a.com", dns.TypeA)
	notify.Opcode = dns.OpcodeNotify
	for _, c := range []struct {
		name     string
		q        *dns.Msg
		rcode    int
		answers  int
		upCalls  int32
		fakeAddr bool
	}{
		{"A 分配假地址", query("claude.ai", dns.TypeA), dns.RcodeSuccess, 1, 0, true},
		{"AAAA 回空", query("claude.ai", dns.TypeAAAA), dns.RcodeSuccess, 0, 0, false},
		{"HTTPS 回空", query("claude.ai", dns.TypeHTTPS), dns.RcodeSuccess, 0, 0, false},
		{"SVCB 回空", query("_dns.claude.ai", dns.TypeSVCB), dns.RcodeSuccess, 0, 0, false},
		{"名单内 AAAA 也回空", query("time.apple.com", dns.TypeAAAA), dns.RcodeSuccess, 0, 0, false},
		{"名单内 HTTPS 也回空", query("printer.local", dns.TypeHTTPS), dns.RcodeSuccess, 0, 0, false},
		{"fake 段 PTR NXDOMAIN", query("5.1.18.198.in-addr.arpa", dns.TypePTR), dns.RcodeNameError, 0, 0, false},
		{"fake 段 198.19 PTR", query("1.255.19.198.in-addr.arpa", dns.TypePTR), dns.RcodeNameError, 0, 0, false},
		{"其他 PTR 交 Upstream", query("8.8.8.8.in-addr.arpa", dns.TypePTR), dns.RcodeSuccess, 0, 1, false},
		{"名单内 A 交 Upstream", query("time.apple.com", dns.TypeA), dns.RcodeSuccess, 1, 1, false},
		{"TXT 交 Upstream", query("claude.ai", dns.TypeTXT), dns.RcodeSuccess, 0, 1, false},
		{"MX 交 Upstream", query("example.com", dns.TypeMX), dns.RcodeSuccess, 0, 1, false},
		{"不合法的名字交 Upstream 不分配", query("bad name.com", dns.TypeA), dns.RcodeSuccess, 1, 1, false},
		{"多问题 FORMERR", multi, dns.RcodeFormatError, 0, 0, false},
		{"非 IN NOTIMP", chaos, dns.RcodeNotImplemented, 0, 0, false},
		{"非 QUERY NOTIMP", notify, dns.RcodeNotImplemented, 0, 0, false},
		{"ANY NOTIMP 不问 Upstream", query("api.anthropic.com", dns.TypeANY), dns.RcodeNotImplemented, 0, 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			up := &fakeUp{realA: []string{"17.253.4.125"}, ttl: 60}
			s := newServer(up)
			m := s.Handle(context.Background(), c.q)
			if m.Rcode != c.rcode || len(m.Answer) != c.answers || up.calls.Load() != c.upCalls {
				t.Fatalf("rcode %s 应答 %d 条 Upstream %d 次；期望 %s %d %d", dns.RcodeToString[m.Rcode], len(m.Answer), up.calls.Load(),
					dns.RcodeToString[c.rcode], c.answers, c.upCalls)
			}
			if m.Id != c.q.Id || !m.RecursionAvailable || !m.Response {
				t.Fatalf("应回显 ID、RA=1：%+v", m.MsgHdr)
			}
			if c.rcode != dns.RcodeFormatError && (len(m.Question) != len(c.q.Question) || m.Question[0] != c.q.Question[0]) {
				t.Fatalf("应回显问题段：%v", m.Question)
			}
			if c.fakeAddr {
				a := m.Answer[0].(*dns.A)
				ip, _ := netip.AddrFromSlice(a.A)
				if !s.Pool.Contains(ip.Unmap()) || a.Hdr.Ttl != FakeTTL {
					t.Fatalf("应是 TTL 1 的假地址：%v", a)
				}
			}
			if strings.HasPrefix(c.name, "不合法") && s.Pool.Stats().Mappings != 0 {
				t.Fatal("不合法的名字不应分配")
			}
		})
	}
}

// b. 稳定
func TestSameNameSameAddr(t *testing.T) {
	s := newServer(nil)
	get := func(n string) netip.Addr {
		m := s.Handle(context.Background(), query(n, dns.TypeA))
		ip, _ := netip.AddrFromSlice(m.Answer[0].(*dns.A).A)
		return ip.Unmap()
	}
	a := get("API.Anthropic.com")
	if b := get("api.anthropic.com."); a != b {
		t.Fatalf("大小写、末尾点不同应同一地址：%s %s", a, b)
	}
	if n, ok := s.Pool.Lookup(a); !ok || n != "api.anthropic.com" {
		t.Fatalf("Lookup 应查回域名：%q", n)
	}
}

// c. 池满
func TestPoolFullServfail(t *testing.T) {
	s := newServer(nil)
	s.Pool = smallPool(t, 1)
	s.Handle(context.Background(), query("a.com", dns.TypeA))
	m := s.Handle(context.Background(), query("b.com", dns.TypeA))
	if m.Rcode != dns.RcodeServerFailure || s.Stats().PoolFull != 1 || s.Stats().ServFail != 1 {
		t.Fatalf("池满应 SERVFAIL 并计数：%s %+v", dns.RcodeToString[m.Rcode], s.Stats())
	}
	if n, _ := s.Pool.Lookup(netip.MustParseAddr("198.18.1.0")); n != "a.com" || s.Pool.Stats().Mappings != 1 {
		t.Fatal("池内映射不应变")
	}
}

// d. Upstream 失败
func TestUpstreamFailures(t *testing.T) {
	for name, up := range map[string]Upstream{
		"出错":  &fakeUp{err: errors.New("boom")},
		"超时":  &fakeUp{delay: time.Hour},
		"nil": nil,
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(up)
			ctx := context.Background()
			if name == "超时" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond) // UpstreamTimeout 是 5 秒，这里用调用方的更短期限
				defer cancel()
			}
			m := s.Handle(ctx, query("example.com", dns.TypeTXT))
			if m.Rcode != dns.RcodeServerFailure || s.Stats().UpstreamFail != 1 {
				t.Fatalf("应 SERVFAIL 并计数：%s %+v", dns.RcodeToString[m.Rcode], s.Stats())
			}
		})
	}
}

// e. 源码断言：没有任何退回本机解析器的路径，也没有自己拨号（DoH 只经 StreamOpener）、不跳过证书校验、
// 不读环境变量里的代理；只 import 标准库、miekg/dns，以及本项目的 wire、guestabi（M2-5d）。
func TestNoHostResolver(t *testing.T) {
	files, _ := os.ReadDir(".")
	for _, f := range files {
		p := f.Name()
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			continue
		}
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"net.Resolver", "net.Lookup", "DefaultResolver", "resolv.conf", "net.Dial", ".DialContext(",
			"dns.Client", "dns.Exchange(", "InsecureSkipVerify", "ProxyFromEnvironment",
			"http.Default", "http.Get(", "http.Post("} { // 默认 HTTP 客户端读环境变量里的代理、走本机解析
			if strings.Contains(string(src), bad) {
				t.Errorf("%s 里出现了 %s", p, bad)
			}
		}
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, p, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			third := strings.Contains(strings.SplitN(path, "/", 2)[0], ".")
			// rules：受保护名单从内置置顶名单推导（m4-plan v2.7 第 0 步；rules 不做网络访问，见它自己的 nodial 检查）。
			// platform：W-1 把落盘的不跟随符号链接打开挪到了那里（只用标准库、不做网络访问）。
			ownOK := path == "claude-abroad/internal/wire" || path == "claude-abroad/internal/guestabi" || path == "claude-abroad/internal/rules" ||
				path == "claude-abroad/internal/platform"
			if (third && path != "github.com/miekg/dns") || (strings.HasPrefix(path, "claude-abroad/") && !ownOK) {
				t.Errorf("%s import 了 %s", p, path)
			}
		}
	}
}

// f. Bypass 与 Reverse
func TestBypass(t *testing.T) {
	b := NewBypass()
	for n, want := range map[string]bool{
		"time.apple.com": true, "captive.apple.com": true, "printer.local": true, "nas.home.lan": true,
		"lan": false, "local": false, "apple.com": false, "x.time.apple.com": false, "example.com": false,
	} {
		if b.Match(n) != want {
			t.Errorf("Match(%s) 应为 %v", n, want)
		}
	}
	if err := b.Add("Intranet.Corp."); err != nil || !b.Match("intranet.corp") {
		t.Fatalf("精确规则：%v", err)
	}
	if err := b.Add("*.corp"); err != nil || !b.Match("a.b.corp") {
		t.Fatalf("后缀规则：%v", err)
	}
	b.Remove("*.corp")
	b.Remove("intranet.corp")
	if b.Match("a.b.corp") || b.Match("intranet.corp") {
		t.Fatal("删除后不应再匹配")
	}
	for _, bad := range []string{"claude.ai", "api.anthropic.com", "Claude.COM.", "*.claude.ai", "*.anthropic.com", "*.com", "*.ai", "x.claude.com"} {
		if err := b.Add(bad); !errors.Is(err, ErrProtected) {
			t.Errorf("Add(%s) 应被拒：%v", bad, err)
		}
	}
	if err := b.Add("bad name"); err == nil {
		t.Error("不合法的规则应被拒")
	}
	// 绕过 Add 直接塞进去：查询时仍分配假地址。
	b.mu.Lock()
	b.exact["claude.ai"], b.suffix["anthropic.com"], b.suffix["com"] = true, true, true
	b.mu.Unlock()
	for _, n := range []string{"claude.ai", "api.anthropic.com", "claude.com"} {
		if b.Match(n) {
			t.Errorf("受保护的 %s 永远不应匹配", n)
		}
	}
	s := newServer(&fakeUp{realA: []string{"1.2.3.4"}, ttl: 60})
	s.Bypass = b
	m := s.Handle(context.Background(), query("claude.ai", dns.TypeA))
	ip, _ := netip.AddrFromSlice(m.Answer[0].(*dns.A).A)
	if !s.Pool.Contains(ip.Unmap()) {
		t.Fatal("claude.ai 即使被塞进名单也应分配假地址")
	}
}

func TestReverse(t *testing.T) {
	up := &fakeUp{realA: []string{"17.253.4.125", "17.253.4.126"}, ttl: 10}
	s := newServer(up)
	s.Handle(context.Background(), query("Time.Apple.com.", dns.TypeA))
	for _, a := range []string{"17.253.4.125", "17.253.4.126"} {
		if n, ok := s.Reverse.Lookup(netip.MustParseAddr(a), t0); !ok || n != "time.apple.com" {
			t.Fatalf("名单内的 A 应答应写进 Reverse：%s → %q", a, n)
		}
	}
	// 保留时间取 max(TTL, 30 分钟)。
	if _, ok := s.Reverse.Lookup(netip.MustParseAddr("17.253.4.125"), t0.Add(29*time.Minute)); !ok {
		t.Fatal("TTL 10 秒也应至少保留 30 分钟")
	}
	if _, ok := s.Reverse.Lookup(netip.MustParseAddr("17.253.4.125"), t0.Add(31*time.Minute)); ok {
		t.Fatal("过期后应查不到")
	}
	r := newReverse(3)
	for i := range 4 {
		r.Put(netip.AddrFrom4([4]byte{1, 1, 1, byte(i)}), fmt.Sprintf("n%d", i), time.Hour, t0)
	}
	if _, ok := r.Lookup(netip.MustParseAddr("1.1.1.0"), t0); ok || r.Len() != 3 {
		t.Fatal("超过上限应淘汰最旧的")
	}
	if n, _ := r.Lookup(netip.MustParseAddr("1.1.1.3"), t0); n != "n3" {
		t.Fatal("最新的应保留")
	}
}

// g. 回环上的真实 UDP、TCP 往返，TC，退出不泄漏 goroutine。
func TestServeUDPAndTCP(t *testing.T) {
	before := runtime.NumGoroutine()
	s := newServer(&fakeUp{big: 20})
	ctx, cancel := context.WithCancel(context.Background())
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.ServeUDP(ctx, pc) }()
	go func() { defer wg.Done(); s.ServeTCP(ctx, ln) }()

	udp := &dns.Client{Net: "udp", Timeout: 2 * time.Second}
	m, _, err := udp.Exchange(query("claude.ai", dns.TypeA), pc.LocalAddr().String())
	if err != nil || len(m.Answer) != 1 || m.Id != 0x1234 {
		t.Fatalf("UDP 往返：%v %v", m, err)
	}
	m, _, err = udp.Exchange(query("big.example.com", dns.TypeTXT), pc.LocalAddr().String())
	if err != nil || !m.Truncated {
		t.Fatalf("超过 512 字节的 UDP 应答应置 TC：%v", err)
	}
	tcp := &dns.Client{Net: "tcp", Timeout: 2 * time.Second}
	m, _, err = tcp.Exchange(query("big.example.com", dns.TypeTXT), ln.Addr().String())
	if err != nil || m.Truncated || len(m.Answer) != 20 {
		t.Fatalf("TCP 应收到完整应答：%v", err)
	}
	m, _, err = tcp.Exchange(query("claude.ai", dns.TypeA), ln.Addr().String())
	if err != nil || len(m.Answer) != 1 {
		t.Fatalf("TCP A 往返：%v", err)
	}
	// 留一条空闲的 TCP 连接，取消时也要关掉。
	idle, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	time.Sleep(20 * time.Millisecond)
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 取消后两个 Serve 应返回")
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before+2 {
		if time.Now().After(deadline) {
			t.Fatalf("goroutine 泄漏：%d → %d", before, runtime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// h. 并发
func TestConcurrentHandle(t *testing.T) {
	s := newServer(&fakeUp{realA: []string{"17.253.4.125"}, ttl: 60})
	var wg sync.WaitGroup
	types := []uint16{dns.TypeA, dns.TypeAAAA, dns.TypeHTTPS, dns.TypeTXT, dns.TypePTR}
	for g := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 40 {
				name := fmt.Sprintf("h%d.example.com", (g+i)%15)
				qt := types[(g+i)%len(types)]
				if qt == dns.TypePTR {
					name = "3.1.18.198.in-addr.arpa"
				}
				if i%7 == 0 {
					name = "time.apple.com"
				}
				m := s.Handle(context.Background(), query(name, qt))
				if m.Id != 0x1234 {
					t.Error("ID 不对")
				}
			}
		}()
	}
	wg.Wait()
}

// 第 0 步：上游应答里有 fake 段地址 → SERVFAIL，不进 Reverse。
func TestUpstreamFakeAnswerRejected(t *testing.T) {
	up := &fakeUp{realA: []string{"17.253.4.125", "198.18.3.4"}, ttl: 60}
	s := newServer(up)
	m := s.Handle(context.Background(), query("time.apple.com", dns.TypeA))
	if m.Rcode != dns.RcodeServerFailure || s.Stats().UpstreamFake != 1 || s.Stats().ServFail != 1 {
		t.Fatalf("应 SERVFAIL 并计 upstream_fake：%s %+v", dns.RcodeToString[m.Rcode], s.Stats())
	}
	if s.Reverse.Len() != 0 {
		t.Fatal("含 fake 段地址的应答不应写进 Reverse")
	}
	up.realA = []string{"198.19.0.1"}
	if m := s.Handle(context.Background(), query("example.com", dns.TypeTXT)); m.Rcode != dns.RcodeSuccess {
		t.Fatal("非 A 查询的应答里没有 A，不受影响")
	}
}

// 第 0 步：HandlePacket 的边界。
func TestHandlePacket(t *testing.T) {
	s := newServer(nil)
	b, _ := query("claude.ai", dns.TypeA).Pack()
	out := s.HandlePacket(context.Background(), b)
	m := new(dns.Msg)
	if out == nil || m.Unpack(out) != nil || len(m.Answer) != 1 {
		t.Fatal("合法查询应得到应答")
	}
	if s.HandlePacket(context.Background(), []byte{1, 2, 3}) != nil {
		t.Fatal("解不开的包应返回 nil")
	}
	resp := query("claude.ai", dns.TypeA)
	resp.Response = true
	rb, _ := resp.Pack()
	if s.HandlePacket(context.Background(), rb) != nil {
		t.Fatal("应答包应返回 nil")
	}
	big := newServer(&fakeUp{big: 20})
	qb, _ := query("big.example.com", dns.TypeTXT).Pack()
	m = new(dns.Msg)
	if out := big.HandlePacket(context.Background(), qb); out == nil || m.Unpack(out) != nil || !m.Truncated || len(out) > 512 {
		t.Fatal("超过 512 字节应截断并置 TC")
	}
}

// 第 0 步：TCP 连接数上限。
func TestServeTCPConnLimit(t *testing.T) {
	s := newServer(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { s.ServeTCP(ctx, ln); close(done) }()
	var conns []net.Conn
	for range MaxTCPConns {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	extra, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	extra.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := extra.Read(make([]byte, 1)); err == nil {
		t.Fatal("超过上限的连接应被关闭")
	}
	deadline := time.Now().Add(2 * time.Second)
	for s.Stats().TCPRejected != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("应计 tcp_rejected：%+v", s.Stats())
		}
		time.Sleep(5 * time.Millisecond)
	}
	// 已有的连接仍可用。
	dc := &dns.Conn{Conn: conns[0]}
	if err := dc.WriteMsg(query("claude.ai", dns.TypeA)); err != nil {
		t.Fatal(err)
	}
	conns[0].SetReadDeadline(time.Now().Add(2 * time.Second))
	if m, err := dc.ReadMsg(); err != nil || len(m.Answer) != 1 {
		t.Fatalf("已有连接应正常：%v", err)
	}
	// 释放一条后可以再建。
	conns[1].Close()
	time.Sleep(50 * time.Millisecond)
	again, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	da := &dns.Conn{Conn: again}
	da.WriteMsg(query("claude.ai", dns.TypeA))
	again.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := da.ReadMsg(); err != nil {
		t.Fatalf("释放后应能再建：%v", err)
	}
	for _, c := range conns {
		c.Close()
	}
	again.Close()
	cancel()
	<-done
}
