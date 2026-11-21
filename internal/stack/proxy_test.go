package stack

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"

	"claude-abroad/internal/fakedns"
	"claude-abroad/internal/wire"
)

type gate struct{ open atomic.Bool }

func (g *gate) Open() bool { return g.open.Load() }

func openGate() *gate { g := &gate{}; g.open.Store(true); return g }

// fakeOpener 记录帧头；按 mode 决定行为。
type fakeOpener struct {
	mu      sync.Mutex
	headers []wire.Header
	calls   atomic.Int32
	err     error                     // 非 nil 时 Open 返回它
	errFor  func(h wire.Header) error // 非 nil 时按帧头决定是否失败（M3 的负缓存用例）
	mode    string                    // "echo"（默认）、"hold"（不读不写直到关闭）、"eof"（立即关闭）
	ends    []net.Conn
}

func (o *fakeOpener) Open(ctx context.Context, h wire.Header) (net.Conn, error) {
	o.calls.Add(1)
	o.mu.Lock()
	o.headers = append(o.headers, h)
	o.mu.Unlock()
	if o.err != nil {
		return nil, o.err
	}
	if o.errFor != nil {
		if err := o.errFor(h); err != nil {
			return nil, err
		}
	}
	a, b := net.Pipe()
	o.mu.Lock()
	o.ends = append(o.ends, b)
	o.mu.Unlock()
	switch o.mode {
	case "eof":
		b.Close()
	case "hold":
		go func() { io.Copy(io.Discard, b); b.Close() }()
	default:
		if h.Net == wire.UDP {
			go func() {
				dc := wire.NewDatagramConn(b)
				buf := make([]byte, wire.MaxDatagram)
				for {
					n, err := dc.ReadDatagram(buf)
					if err != nil {
						b.Close()
						return
					}
					dc.WriteDatagram(buf[:n])
				}
			}()
		} else {
			go func() { io.Copy(b, b); b.Close() }()
		}
	}
	return a, nil
}

func (o *fakeOpener) all() []wire.Header {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]wire.Header(nil), o.headers...)
}

func (o *fakeOpener) last() wire.Header {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.headers[len(o.headers)-1]
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type proxyEnv struct {
	s     *Stack
	cl    *client
	p     *Proxy
	op    *fakeOpener
	gate  *gate
	clock *clock
}

func newProxyEnv(t *testing.T, pool *fakedns.Pool, o Options) *proxyEnv {
	t.Helper()
	return newProxyEnvWith(t, pool, o, nil)
}

// newProxyEnvWith 同 newProxyEnv，configure 在栈启动前修改 Proxy（M3 的字段）。
func newProxyEnvWith(t *testing.T, pool *fakedns.Pool, o Options, configure func(*Proxy)) *proxyEnv {
	t.Helper()
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	if pool == nil {
		pool = fakedns.DefaultPool()
	}
	pool.Now = c.now
	srv := &fakedns.Server{Pool: pool, Bypass: fakedns.NewBypass(), Reverse: fakedns.NewReverse(), Now: c.now}
	op := &fakeOpener{}
	g := openGate()
	p := &Proxy{Pool: pool, DNS: srv, Gate: g, Opener: op, Now: c.now, DNSIdle: 300 * time.Millisecond, MaxDNSTCP: 2}
	p.SetEnabled(true) // 测试台默认是启用状态（没设状态时网络栈按停用处理）
	if configure != nil {
		configure(p)
	}
	s, cl := setup(t, p, o)
	return &proxyEnv{s: s, cl: cl, p: p, op: op, gate: g, clock: c}
}

// lookupA 经客户端栈向 198.18.0.2:53 查 A，返回假地址。
func (e *proxyEnv) lookupA(t *testing.T, name string) netip.Addr {
	t.Helper()
	u := e.cl.udp(t)
	m := e.query(t, u, name, dns.TypeA)
	if len(m.Answer) != 1 {
		t.Fatalf("应拿到一条 A：%v", m)
	}
	a, _ := netip.AddrFromSlice(m.Answer[0].(*dns.A).A)
	return a.Unmap()
}

func (e *proxyEnv) query(t *testing.T, u *gonet.UDPConn, name string, qt uint16) *dns.Msg {
	t.Helper()
	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(name), qt)
	b, _ := q.Pack()
	u.WriteTo(b, &net.UDPAddr{IP: net.IPv4(198, 18, 0, 2), Port: 53})
	u.SetReadDeadline(time.Now().Add(ioWait))
	buf := make([]byte, 4096)
	n, _, err := u.ReadFrom(buf)
	if err != nil {
		t.Fatalf("DNS 查询没有应答：%v", err)
	}
	m := new(dns.Msg)
	if err := m.Unpack(buf[:n]); err != nil || m.Id != q.Id {
		t.Fatalf("DNS 应答不对：%v", err)
	}
	return m
}

func ap(a netip.Addr, port uint16) netip.AddrPort { return netip.AddrPortFrom(a, port) }

// a. 判定的每个分支
func TestDecideTable(t *testing.T) {
	pool := fakedns.DefaultPool()
	known, _ := pool.Assign("claude.ai", time.Now())
	g := openGate()
	p := &Proxy{Pool: pool, Gate: g}
	p.SetEnabled(true)
	src := netip.MustParseAddrPort("198.18.0.1:5000")
	cases := []struct {
		name string
		dst  string
		gate bool
		want verdict
		stat func(ProxyStats) int64
	}{
		{"DNS", "198.18.0.2:53", true, vDNS, nil},
		{"DNS 不看闸门", "198.18.0.2:53", false, vDNS, nil},
		{"198.18.0.2 其他端口", "198.18.0.2:80", true, vReject, func(s ProxyStats) int64 { return s.BadDst }},
		{"保留 /24", "198.18.0.9:443", true, vReject, func(s ProxyStats) int64 { return s.BadDst }},
		{"没有映射的假地址", "198.18.5.5:443", true, vReject, func(s ProxyStats) int64 { return s.NoMapping }},
		{"有映射的假地址", known.String() + ":443", true, vFlow, nil},
		{"有映射但闸门关", known.String() + ":443", false, vReject, func(s ProxyStats) int64 { return s.GateClosed }},
		{"回环", "127.0.0.1:80", true, vReject, func(s ProxyStats) int64 { return s.BadDst }},
		{"0/8", "0.1.2.3:80", true, vReject, func(s ProxyStats) int64 { return s.BadDst }},
		{"链路本地", "169.254.169.254:80", true, vReject, func(s ProxyStats) int64 { return s.BadDst }},
		{"组播", "224.0.0.251:5353", true, vReject, func(s ProxyStats) int64 { return s.BadDst }},
		{"广播", "255.255.255.255:67", true, vReject, func(s ProxyStats) int64 { return s.BadDst }},
		{"真实 IP 闸门开", "8.8.8.8:443", true, vFlow, nil},
		{"真实 IP 闸门关", "8.8.8.8:443", false, vReject, func(s ProxyStats) int64 { return s.GateClosed }},
	}
	for _, c := range cases {
		g.open.Store(c.gate)
		before := p.Stats()
		got := p.precheck(Flow{Src: src, Dst: netip.MustParseAddrPort(c.dst)})
		if got != c.want {
			t.Errorf("%s：%v，应为 %v", c.name, got, c.want)
		}
		if c.stat != nil && c.stat(p.Stats())-c.stat(before) != 1 {
			t.Errorf("%s：对应计数应加一", c.name)
		}
	}
	if (&Proxy{Pool: pool}).precheck(Flow{Src: src, Dst: netip.MustParseAddrPort("8.8.8.8:443")}) != vReject {
		t.Error("没有闸门（nil）按关闭处理")
	}
}

// a. 被拒时：Opener 不被调用、TCP 收到 RST、UDP 无回包
func TestRejectedNeverOpens(t *testing.T) {
	e := newProxyEnv(t, nil, Options{})
	for _, dst := range []string{"198.18.5.5:443", "169.254.169.254:80", "198.18.0.2:80"} {
		if _, err := e.cl.dialTCP(netip.MustParseAddrPort(dst), ioWait); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("%s 应 connection refused：%v", dst, err)
		}
	}
	u := e.cl.udp(t)
	u.WriteTo([]byte("x"), &net.UDPAddr{IP: net.IPv4(198, 18, 5, 5), Port: 443})
	waitFor(t, "UDP 被拒计数", func() bool { return e.s.Stats().UDPRejected >= 1 })
	u.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := u.ReadFrom(make([]byte, 10)); err == nil {
		t.Fatal("被拒的 UDP 不应有回包")
	}
	if e.op.calls.Load() != 0 {
		t.Fatalf("被拒时 Opener 不应被调用：%d", e.op.calls.Load())
	}
}

// b. 闸门：关闭时新流被拒而 DNS 照常；打开后可建；关闸门 + DropAll 断开已有的流
func TestGateAndDropAll(t *testing.T) {
	e := newProxyEnv(t, nil, Options{})
	e.gate.open.Store(false)
	fake := e.lookupA(t, "claude.ai") // DNS 不看闸门
	if _, err := e.cl.dialTCP(ap(fake, 443), ioWait); err == nil {
		t.Fatal("闸门关闭时新 TCP 应被拒")
	}
	if e.op.calls.Load() != 0 {
		t.Fatal("闸门关闭时不应开流")
	}
	e.gate.open.Store(true)
	e.op.mode = "hold"
	c, err := e.cl.dialTCP(ap(fake, 443), ioWait)
	if err != nil {
		t.Fatalf("闸门打开后应能建：%v", err)
	}
	defer c.Close()
	u := e.cl.udp(t)
	u.WriteTo([]byte("x"), &net.UDPAddr{IP: net.IPv4(8, 8, 8, 8), Port: 443})
	waitFor(t, "TCP、UDP 都在走代理", func() bool { return e.p.Stats().TCPProxied == 1 && e.p.Stats().UDPProxied == 1 })
	e.gate.open.Store(false)
	e.p.DropAll()
	c.SetReadDeadline(time.Now().Add(ioWait))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("DropAll 后已有 TCP 应被断开")
	}
	waitFor(t, "流全部结束", func() bool { return e.s.Stats().TCPActive == 0 && e.s.Stats().UDPActive == 0 })
	if e.p.Stats().Dropped < 2 {
		t.Fatalf("应计 dropped：%+v", e.p.Stats())
	}
	if a := e.lookupA(t, "api.anthropic.com"); !a.IsValid() {
		t.Fatal("DropAll 后 DNS 应不受影响")
	}
}

// d. DNS
func TestDNSOverStack(t *testing.T) {
	e := newProxyEnv(t, nil, Options{})
	u := e.cl.udp(t)
	seen := map[netip.Addr]bool{}
	for _, n := range []string{"a.example.com", "b.example.com", "c.example.com"} { // 同一源端口连发
		m := e.query(t, u, n, dns.TypeA)
		a, _ := netip.AddrFromSlice(m.Answer[0].(*dns.A).A)
		seen[a] = true
	}
	if len(seen) != 3 {
		t.Fatal("三个查询都应有各自的应答")
	}
	waitFor(t, "DNS 流空闲后归还名额", func() bool { return e.s.Stats().UDPActive == 0 })

	tcpQuery := func() error {
		c, err := e.cl.dialTCP(netip.MustParseAddrPort("198.18.0.2:53"), ioWait)
		if err != nil {
			return err
		}
		defer c.Close()
		dc := &dns.Conn{Conn: c}
		q := new(dns.Msg)
		q.SetQuestion("tcp.example.com.", dns.TypeA)
		if err := dc.WriteMsg(q); err != nil {
			return err
		}
		c.SetReadDeadline(time.Now().Add(ioWait))
		m, err := dc.ReadMsg()
		if err != nil || len(m.Answer) != 1 {
			return errors.New("TCP 查询没有应答")
		}
		return nil
	}
	if err := tcpQuery(); err != nil {
		t.Fatal(err)
	}
	// MaxDNSTCP=2：第三条被关
	var held []net.Conn
	for range 2 {
		c, err := e.cl.dialTCP(netip.MustParseAddrPort("198.18.0.2:53"), ioWait)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	waitFor(t, "两条 DNS TCP 在服务", func() bool { return e.p.dnsTCP.Load() == 2 })
	c3, err := e.cl.dialTCP(netip.MustParseAddrPort("198.18.0.2:53"), ioWait)
	if err != nil {
		t.Fatal(err)
	}
	c3.SetReadDeadline(time.Now().Add(ioWait))
	if _, err := c3.Read(make([]byte, 1)); err == nil {
		t.Fatal("第 MaxDNSTCP+1 条应被关")
	}
	if e.p.Stats().DNSTCPRejected != 1 {
		t.Fatalf("应计 dns_tcp_rejected：%+v", e.p.Stats())
	}
	for _, c := range held {
		c.Close()
	}
}

// e. 映射引用：连接存续期间不被淘汰，各条结束路径之后都能被淘汰
func smallPool(t *testing.T) *fakedns.Pool {
	p, err := fakedns.NewPool(netip.MustParseAddr("198.18.1.0"), netip.MustParseAddr("198.18.1.1"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// evictable 把时钟拨过 30 分钟，再分配两个新域名，返回 addr 原来的映射是否被淘汰。
func (e *proxyEnv) evictable(addr netip.Addr, name string, tag string) bool {
	e.clock.add(31 * time.Minute)
	e.p.Pool.Assign("x1-"+tag+".example.com", e.clock.now())
	e.p.Pool.Assign("x2-"+tag+".example.com", e.clock.now())
	got, _ := e.p.Pool.Lookup(addr)
	return got != name
}

func TestMappingHeldWhileConnected(t *testing.T) {
	e := newProxyEnv(t, smallPool(t), Options{})
	e.op.mode = "hold"
	a := e.lookupA(t, "a.com")
	c, err := e.cl.dialTCP(ap(a, 443), ioWait)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "走代理", func() bool { return e.p.Stats().TCPProxied == 1 })
	if e.evictable(a, "a.com", "held") {
		t.Fatal("连接存续期间映射不应被淘汰")
	}
	if got, _ := e.p.Pool.Lookup(a); got != "a.com" {
		t.Fatal("Lookup 仍应是原域名")
	}
	c.Close()
	waitFor(t, "连接结束", func() bool { return e.s.Stats().TCPActive == 0 })
	if !e.evictable(a, "a.com", "closed") {
		t.Fatal("连接关闭后映射应可被淘汰")
	}
}

func TestMappingReleasedOnEveryPath(t *testing.T) {
	paths := map[string]func(e *proxyEnv, a netip.Addr){
		"Open 失败": func(e *proxyEnv, a netip.Addr) {
			e.op.err = &wire.ReplyError{Code: wire.CodeExitDown}
			c, err := e.cl.dialTCP(ap(a, 443), ioWait)
			if err == nil {
				c.SetReadDeadline(time.Now().Add(ioWait))
				c.Read(make([]byte, 1))
				c.Close()
			}
		},
		"DropAll": func(e *proxyEnv, a netip.Addr) {
			e.op.mode = "hold"
			c, err := e.cl.dialTCP(ap(a, 443), ioWait)
			if err != nil {
				panic(err)
			}
			defer c.Close()
			waitForNoT(func() bool { return e.p.Stats().TCPProxied == 1 })
			e.p.DropAll()
		},
		"ctx 取消（Stack.Close）": func(e *proxyEnv, a netip.Addr) {
			e.op.mode = "hold"
			c, err := e.cl.dialTCP(ap(a, 443), ioWait)
			if err != nil {
				panic(err)
			}
			defer c.Close()
			waitForNoT(func() bool { return e.p.Stats().TCPProxied == 1 })
			e.s.Close()
		},
	}
	for name, run := range paths {
		t.Run(name, func(t *testing.T) {
			e := newProxyEnv(t, smallPool(t), Options{})
			a := e.lookupA(t, "a.com")
			run(e, a)
			waitFor(t, "流结束", func() bool { return e.s.Stats().TCPActive == 0 })
			if !e.evictable(a, "a.com", "after") {
				t.Fatalf("%s 之后引用应归零（映射可被淘汰）", name)
			}
		})
	}
	// Acquire 失败：映射已没了，newRoute 返回 ok=false，不产生引用。
	pool := smallPool(t)
	p := &Proxy{Pool: pool}
	if r, ok := p.newRoute(context.Background(), Flow{Dst: netip.MustParseAddrPort("198.18.1.0:443")}, "tcp"); ok || r != nil {
		t.Fatal("没有映射时 newRoute 应失败")
	}
}

func waitForNoT(cond func() bool) {
	deadline := time.Now().Add(ioWait)
	for !cond() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
}

// f. 失败路径：应用侧连接被关闭、计数加一、只开一次、名额归零
func TestOpenFailures(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		mode string
		stat func(ProxyStats) int64
	}{
		{"unknown_exit", &wire.ReplyError{Code: wire.CodeUnknownExit}, "", func(s ProxyStats) int64 { return s.UnknownExit }},
		{"exit_down", &wire.ReplyError{Code: wire.CodeExitDown}, "", func(s ProxyStats) int64 { return s.ExitDown }},
		{"dial_failed", &wire.ReplyError{Code: wire.CodeDialFailed}, "", func(s ProxyStats) int64 { return s.DialFailed }},
		{"普通错误", errors.New("boom"), "", func(s ProxyStats) int64 { return s.OpenOther }},
		{"ok 之后零字节 EOF", nil, "eof", func(s ProxyStats) int64 { return s.ZeroEOF }},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newProxyEnv(t, nil, Options{})
			e.op.err, e.op.mode = c.err, c.mode
			conn, err := e.cl.dialTCP(netip.MustParseAddrPort("8.8.8.8:443"), ioWait)
			if err == nil {
				conn.SetReadDeadline(time.Now().Add(ioWait))
				if _, err := conn.Read(make([]byte, 1)); err == nil {
					t.Fatal("应用侧连接应被关闭")
				}
				conn.Close()
			}
			waitFor(t, "计数", func() bool { return c.stat(e.p.Stats()) == 1 })
			waitFor(t, "名额归零", func() bool { return e.s.Stats().TCPActive == 0 })
			if e.op.calls.Load() != 1 {
				t.Fatalf("不应重试：Opener 调用了 %d 次", e.op.calls.Load())
			}
		})
	}
	t.Run("udp_unsupported", func(t *testing.T) {
		e := newProxyEnv(t, nil, Options{})
		e.op.err = &wire.ReplyError{Code: wire.CodeUDPUnsupported}
		u := e.cl.udp(t)
		u.WriteTo([]byte("x"), &net.UDPAddr{IP: net.IPv4(8, 8, 8, 8), Port: 443})
		waitFor(t, "计 udp_unsupported", func() bool { return e.p.Stats().UDPUnsupported == 1 })
		u.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		if _, _, err := u.ReadFrom(make([]byte, 10)); err == nil {
			t.Fatal("不应有回包（不改走直连）")
		}
		waitFor(t, "名额归零", func() bool { return e.s.Stats().UDPActive == 0 })
		if e.op.calls.Load() != 1 {
			t.Fatalf("不应重试：%d", e.op.calls.Load())
		}
	})
}

// 帧头：fake 段用域名、真实 IP 用 IP；exit 是 main；没有识别器时 App 是 unknown
func TestHeaders(t *testing.T) {
	e := newProxyEnv(t, nil, Options{})
	a := e.lookupA(t, "Claude.AI")
	c, err := e.cl.dialTCP(ap(a, 443), ioWait)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	waitFor(t, "开流", func() bool { return e.op.calls.Load() == 1 })
	if h := e.op.last(); h.Host != "claude.ai" || h.IP != "" || h.Exit != "main" || h.Net != wire.TCP || h.Port != 443 || h.App != "unknown" || h.V != wire.Version {
		t.Fatalf("fake 段帧头不对：%+v", h)
	}
	c, err = e.cl.dialTCP(netip.MustParseAddrPort("8.8.4.4:853"), ioWait)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	waitFor(t, "开流", func() bool { return e.op.calls.Load() == 2 })
	if h := e.op.last(); h.Host != "" || h.IP != "8.8.4.4" || h.Port != 853 {
		t.Fatalf("真实 IP 帧头不对：%+v", h)
	}
}

// g. 源码断言：流处理的各个文件不打日志、不自己解析域名，import 只在白名单里。
func TestProxySource(t *testing.T) {
	allowed := map[string]bool{"claude-abroad/internal/fakedns": true, "claude-abroad/internal/wire": true,
		"claude-abroad/internal/guestabi": true, "github.com/miekg/dns": true, "claude-abroad/internal/rules": true,
		"claude-abroad/internal/sniff": true, "claude-abroad/internal/directdial": true}
	for _, name := range []string{"proxy.go", "router.go", "forward.go", "stats.go", "udpneg.go"} {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"net.Dial", "net.Listen", "net.Resolver", "net.Lookup", "log.", "fmt.Print", "slog."} {
			if strings.Contains(string(src), bad) {
				t.Errorf("%s 里出现了 %s", name, bad)
			}
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.Contains(strings.SplitN(p, "/", 2)[0], ".") || strings.HasPrefix(p, "claude-abroad/") {
				if !allowed[p] {
					t.Errorf("%s import 了 %s", name, p)
				}
			}
		}
	}
	for _, p := range []string{"stack.go", "udpflow.go"} {
		b, _ := os.ReadFile(p)
		for _, bad := range []string{"net.Resolver", "net.Lookup"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s 里出现了 %s", p, bad)
			}
		}
	}
}

// errConn 模拟应用异常断开：reset 关闭后 Read 返回错误（而不是 EOF）。
type errConn struct {
	net.Conn
	reset chan struct{}
}

func (c *errConn) Read(p []byte) (int, error) {
	<-c.reset
	return 0, errors.New("connection reset by peer")
}

// 应用异常断开（RST）时，即使上游一直不说话，两端也一起断开、映射引用归还。
func TestAppResetAbortsUpstream(t *testing.T) {
	pool := smallPool(t)
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	pool.Now = c.now
	a, _ := pool.Assign("a.com", c.now())
	op := &fakeOpener{mode: "hold"}
	p := &Proxy{Pool: pool, Gate: openGate(), Opener: op, Now: c.now}
	p.SetEnabled(true)
	app, peer := net.Pipe()
	defer peer.Close()
	ec := &errConn{Conn: app, reset: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		p.TCP(context.Background(), Flow{Src: netip.MustParseAddrPort("198.18.0.1:5000"), Dst: ap(a, 443)}, ec)
		close(done)
	}()
	waitFor(t, "走代理", func() bool { return p.Stats().TCPProxied == 1 })
	close(ec.reset)
	select {
	case <-done:
	case <-time.After(ioWait):
		t.Fatal("应用 RST 后 TCP 应返回（上游一直不说话也一样）")
	}
	e := &proxyEnv{p: p, clock: c}
	if !e.evictable(a, "a.com", "rst") {
		t.Fatal("应用 RST 之后映射引用应归零")
	}
}

// slowOpener 的 Open 一直等到 ctx 取消，然后返回 ctx 的错误。
type slowOpener struct{ started chan struct{} }

func (o *slowOpener) Open(ctx context.Context, h wire.Header) (net.Conn, error) {
	close(o.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

// 被 DropAll 打断的 Open 不计入 open_failed_other。
func TestCanceledOpenNotCountedAsFailure(t *testing.T) {
	op := &slowOpener{started: make(chan struct{})}
	p := &Proxy{Gate: openGate(), Opener: op}
	p.SetEnabled(true)
	app, peer := net.Pipe()
	defer peer.Close()
	done := make(chan struct{})
	go func() {
		p.TCP(context.Background(), Flow{Src: netip.MustParseAddrPort("198.18.0.1:5000"), Dst: netip.MustParseAddrPort("8.8.8.8:443")}, app)
		close(done)
	}()
	<-op.started
	p.DropAll()
	select {
	case <-done:
	case <-time.After(ioWait):
		t.Fatal("DropAll 应打断正在进行的 Open")
	}
	if st := p.Stats(); st.OpenOther != 0 || st.Dropped != 1 {
		t.Fatalf("被打断的 Open 不应计入 open_failed_other：%+v", st)
	}
}

// halfConn 的 CloseWrite 只记一下、不关连接（像 TCP 的半关闭：写方向关了，读方向还开着）。
type halfConn struct {
	net.Conn
	cw chan struct{}
}

func (h halfConn) CloseWrite() error { close(h.cw); return nil }

// 回归（M2-9b-2 查出）：一个方向先正常结束（上游立刻 EOF → 对应用半关闭），另一个方向还开着（应用不发也不关），
// 之后流被 DropAll 取消：Proxy.TCP 只等还没结束的方向，必须很快返回。原来固定再收两次 finished，会永远卡住，
// Stack.Close 跟着卡住。
func TestDropAfterOneDirectionFinished(t *testing.T) {
	op := &fakeOpener{mode: "eof"}
	p := &Proxy{Gate: openGate(), Opener: op}
	p.SetEnabled(true)
	app, peer := net.Pipe()
	defer peer.Close()
	hc := halfConn{Conn: app, cw: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		p.TCP(context.Background(), Flow{Src: netip.MustParseAddrPort("198.18.0.1:5000"), Dst: netip.MustParseAddrPort("8.8.8.8:443")}, hc)
		close(done)
	}()
	select {
	case <-hc.cw: // 上游 → 应用的方向结束了（对应用半关闭）
	case <-time.After(ioWait):
		t.Fatal("上游 EOF 后应对应用半关闭")
	}
	time.Sleep(50 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("应用 → 上游的方向还开着，Proxy.TCP 不应已经返回")
	default:
	}
	p.DropAll()
	select {
	case <-done:
	case <-time.After(ioWait):
		t.Fatal("一个方向已结束时 DropAll，Proxy.TCP 卡住了")
	}
}
