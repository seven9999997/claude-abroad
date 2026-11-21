package fakedns

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"claude-abroad/internal/wire"
)

// —— 证书 ——

type testPKI struct {
	pool *x509.CertPool
	good tls.Certificate // IP SAN 1.1.1.1
	bad  tls.Certificate // IP SAN 2.2.2.2
}

func newPKI(t *testing.T) *testPKI {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	leaf := func(ip string, serial int64) tls.Certificate {
		k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "doh"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			IPAddresses: []net.IP{net.ParseIP(ip)}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			KeyUsage: x509.KeyUsageDigitalSignature}
		der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &k.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &testPKI{pool: pool, good: leaf("1.1.1.1", 2), bad: leaf("2.2.2.2", 3)}
}

// —— 假 DoH 服务器（经 net.Pipe 接到假 Opener）——

// trackedConn 记录客户端一侧的连接是否被关闭。
type trackedConn struct {
	net.Conn
	closed atomic.Bool
}

func (c *trackedConn) Close() error { c.closed.Store(true); return c.Conn.Close() }

type dohServer struct {
	mode    atomic.Value // string
	ln      net.Listener // 本机回环上的 TCP（有缓冲；net.Pipe 在 TLS 告警时会两边互相等）
	opens   atomic.Int32
	mu      sync.Mutex
	headers []wire.Header
	conns   []*trackedConn
	reqs    atomic.Int32
}

func newDoHServer(t *testing.T, cert tls.Certificate) *dohServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &dohServer{ln: ln}
	s.mode.Store("ok")
	srv := &http.Server{Handler: http.HandlerFunc(s.handle)}
	tl := tls.NewListener(s.ln, &tls.Config{Certificates: []tls.Certificate{cert}})
	done := make(chan struct{})
	go func() { srv.Serve(tl); close(done) }()
	t.Cleanup(func() { srv.Close(); s.ln.Close(); <-done })
	return s
}

func (s *dohServer) Open(ctx context.Context, h wire.Header) (net.Conn, error) {
	s.opens.Add(1)
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", s.ln.Addr().String())
	if err != nil {
		return nil, err
	}
	tc := &trackedConn{Conn: c}
	s.mu.Lock()
	s.headers = append(s.headers, h)
	s.conns = append(s.conns, tc)
	s.mu.Unlock()
	return tc, nil
}

func (s *dohServer) allClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		if !c.closed.Load() {
			return false
		}
	}
	return len(s.conns) > 0
}

func (s *dohServer) handle(w http.ResponseWriter, r *http.Request) {
	s.reqs.Add(1)
	body, _ := io.ReadAll(r.Body)
	q := new(dns.Msg)
	if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/dns-message" || q.Unpack(body) != nil || q.Id != 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	m := new(dns.Msg)
	m.SetReply(q)
	qs := q.Question[0]
	switch qs.Qtype {
	case dns.TypeTXT:
		m.Answer = append(m.Answer, &dns.TXT{Hdr: dns.RR_Header{Name: qs.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60}, Txt: []string{"hello"}})
	case dns.TypeA:
		ip := "17.253.4.125"
		if s.mode.Load() == "fake-a" {
			ip = "198.18.3.4"
		}
		m.Answer = append(m.Answer, &dns.A{Hdr: dns.RR_Header{Name: qs.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP(ip)})
	}
	out, _ := m.Pack()
	ct := "application/dns-message"
	switch s.mode.Load() {
	case "500":
		http.Error(w, "oops", http.StatusInternalServerError)
		return
	case "ctype":
		ct = "text/plain"
	case "big":
		out = append(out, make([]byte, DoHMaxBody)...)
	case "garbage":
		out = []byte{1, 2, 3}
	case "mismatch":
		m.Question[0].Name = "other.example."
		out, _ = m.Pack()
	case "redirect":
		http.Redirect(w, r, "https://1.1.1.1/elsewhere", http.StatusFound)
		return
	case "hang":
		<-r.Context().Done()
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Write(out)
}

type gateFlag struct{ open atomic.Bool }

func (g *gateFlag) Open() bool { return g.open.Load() }

func openGateFlag() *gateFlag { g := &gateFlag{}; g.open.Store(true); return g }

func txtQuery(name string) *dns.Msg {
	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(name), dns.TypeTXT)
	q.Id = 0x4321
	return q
}

func newDoH(t *testing.T, cert func(*testPKI) tls.Certificate) (*DoHUpstream, *dohServer, *gateFlag) {
	t.Helper()
	pki := newPKI(t)
	srv := newDoHServer(t, cert(pki))
	g := openGateFlag()
	d := &DoHUpstream{Opener: srv, Gate: g, RootCAs: pki.pool}
	t.Cleanup(d.Close)
	return d, srv, g
}

func good(p *testPKI) tls.Certificate { return p.good }

// a. TXT 拿到应答；帧头逐字段等于 main/tcp/1.1.1.1/443，Host 为空
func TestDoHExchangeAndHeader(t *testing.T) {
	d, srv, _ := newDoH(t, good)
	r, err := d.Exchange(context.Background(), txtQuery("example.com"))
	if err != nil || len(r.Answer) != 1 || r.Id != 0x4321 {
		t.Fatalf("应拿到 TXT 应答，ID 改回请求的：%v %v", r, err)
	}
	srv.mu.Lock()
	h := srv.headers[0]
	srv.mu.Unlock()
	if h != (wire.Header{V: wire.Version, Exit: "main", Net: wire.TCP, IP: "1.1.1.1", Port: 443}) {
		t.Fatalf("帧头不对：%+v", h)
	}
}

// b. 证书不被信任、IP SAN 不是 1.1.1.1
func TestDoHCertificateChecks(t *testing.T) {
	pki := newPKI(t)
	srv := newDoHServer(t, pki.good)
	d := &DoHUpstream{Opener: srv, Gate: openGateFlag(), RootCAs: x509.NewCertPool()} // 不信任测试 CA
	defer d.Close()
	if _, err := d.Exchange(context.Background(), txtQuery("example.com")); err == nil {
		t.Fatal("证书不被信任时应报错")
	}
	d2, _, _ := newDoH(t, func(p *testPKI) tls.Certificate { return p.bad })
	if _, err := d2.Exchange(context.Background(), txtQuery("example.com")); err == nil {
		t.Fatal("证书的 IP SAN 不是 1.1.1.1 时应报错")
	}
}

// c. 各种坏应答
func TestDoHBadResponses(t *testing.T) {
	for _, mode := range []string{"500", "ctype", "big", "garbage", "mismatch", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			d, srv, _ := newDoH(t, good)
			srv.mode.Store(mode)
			_, err := d.Exchange(context.Background(), txtQuery("secret-name.example"))
			if err == nil {
				t.Fatal("应报错")
			}
			if strings.Contains(err.Error(), "secret-name") {
				t.Fatalf("错误里不应带查询的域名：%v", err)
			}
			if mode == "redirect" && (srv.opens.Load() != 1 || srv.reqs.Load() != 1) {
				t.Fatalf("重定向不应跟随：开流 %d 次、请求 %d 次", srv.opens.Load(), srv.reqs.Load())
			}
		})
	}
}

// d. ctx 取消及时返回、连接关闭；并发 50 次复用连接
func TestDoHCancelAndReuse(t *testing.T) {
	d, srv, _ := newDoH(t, good)
	srv.mode.Store("hang")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := d.Exchange(ctx, txtQuery("example.com")); err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("ctx 超时应及时返回错误：%v %s", err, time.Since(start))
	}
	deadline := time.Now().Add(5 * time.Second)
	for !srv.allClosed() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !srv.allClosed() {
		t.Fatal("超时后底层连接应被关闭")
	}

	d2, srv2, _ := newDoH(t, good)
	var wg sync.WaitGroup
	var fails atomic.Int32
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := d2.Exchange(context.Background(), txtQuery("example.com")); err != nil {
				fails.Add(1)
			}
		}()
	}
	wg.Wait()
	if fails.Load() != 0 || srv2.opens.Load() > DoHMaxConns {
		t.Fatalf("并发 50 次应复用连接：失败 %d，开流 %d", fails.Load(), srv2.opens.Load())
	}
}

// e. 闸门
func TestDoHGate(t *testing.T) {
	d, srv, g := newDoH(t, good)
	g.open.Store(false)
	if _, err := d.Exchange(context.Background(), txtQuery("example.com")); !errors.Is(err, ErrGateClosed) || srv.opens.Load() != 0 {
		t.Fatalf("闸门关闭时应报错且不开流：%v %d", err, srv.opens.Load())
	}
	g.open.Store(true)
	if _, err := d.Exchange(context.Background(), txtQuery("example.com")); err != nil {
		t.Fatal(err)
	}
	g.open.Store(false)
	d.Close()
	deadline := time.Now().Add(5 * time.Second)
	for !srv.allClosed() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !srv.allClosed() {
		t.Fatal("Close 后空闲连接应被关闭")
	}
	if (&DoHUpstream{Opener: srv}).Gate != nil {
		t.Fatal("测试前提")
	}
	if _, err := (&DoHUpstream{Opener: srv}).Exchange(context.Background(), txtQuery("x.com")); !errors.Is(err, ErrGateClosed) {
		t.Fatal("没有闸门（nil）按关闭处理")
	}
}

// f. 环境变量里的代理不起作用
func TestDoHIgnoresProxyEnv(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "socks5://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	d, srv, _ := newDoH(t, good)
	if _, err := d.Exchange(context.Background(), txtQuery("example.com")); err != nil || srv.opens.Load() != 1 {
		t.Fatalf("设了代理环境变量也应只经 Opener：%v %d", err, srv.opens.Load())
	}
}

// g. 接进 Server
func TestDoHInServer(t *testing.T) {
	d, srv, _ := newDoH(t, good)
	s := &Server{Pool: DefaultPool(), Upstream: d, Bypass: NewBypass(), Reverse: NewReverse(), Now: func() time.Time { return t0 }}
	m := s.Handle(context.Background(), query("example.com", dns.TypeTXT))
	if m.Rcode != dns.RcodeSuccess || len(m.Answer) != 1 {
		t.Fatalf("TXT 经 DoH 应有应答：%v", m)
	}
	m = s.Handle(context.Background(), query("time.apple.com", dns.TypeA))
	if m.Rcode != dns.RcodeSuccess || len(m.Answer) != 1 {
		t.Fatalf("名单内域名的 A 经 DoH 应有应答：%v", m)
	}
	if n, ok := s.Reverse.Lookup(netip.MustParseAddr("17.253.4.125"), t0); !ok || n != "time.apple.com" {
		t.Fatalf("应记进 Reverse：%q", n)
	}
	srv.mode.Store("fake-a")
	if m = s.Handle(context.Background(), query("time-ios.apple.com", dns.TypeA)); m.Rcode != dns.RcodeServerFailure {
		t.Fatal("DoH 答 fake 段地址时应 SERVFAIL")
	}
}

// switchOpener 先连“只 accept、不读不写”的黑洞，切换后连正常的假 DoH 服务器。
type switchOpener struct {
	mu       sync.Mutex
	target   string
	conns    []*trackedConn
	opens    atomic.Int32
	blackhol net.Listener
}

func (o *switchOpener) Open(ctx context.Context, h wire.Header) (net.Conn, error) {
	o.opens.Add(1)
	o.mu.Lock()
	target := o.target
	o.mu.Unlock()
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		return nil, err
	}
	tc := &trackedConn{Conn: c}
	o.mu.Lock()
	o.conns = append(o.conns, tc)
	o.mu.Unlock()
	return tc, nil
}

func (o *switchOpener) allClosed() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, c := range o.conns {
		if !c.closed.Load() {
			return false
		}
	}
	return true
}

// 握手阶段黑洞：拨号加握手要有独立的期限，否则 DoHMaxConns 个挂住的握手会让 DoH 永久不可用。
func TestDoHHandshakeBlackholeRecovers(t *testing.T) {
	pki := newPKI(t)
	hole, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer hole.Close()
	var held []net.Conn
	var heldMu sync.Mutex
	go func() {
		for {
			c, err := hole.Accept()
			if err != nil {
				return
			}
			heldMu.Lock()
			held = append(held, c) // 只 accept，不读不写
			heldMu.Unlock()
		}
	}()
	defer func() {
		heldMu.Lock()
		for _, c := range held {
			c.Close()
		}
		heldMu.Unlock()
	}()
	srv := newDoHServer(t, pki.good)
	op := &switchOpener{target: hole.Addr().String()}
	d := &DoHUpstream{Opener: op, Gate: openGateFlag(), RootCAs: pki.pool, dialTimeout: 300 * time.Millisecond}
	defer d.Close()
	for range DoHMaxConns + 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		if _, err := d.Exchange(ctx, txtQuery("example.com")); err == nil {
			t.Fatal("黑洞下应报错")
		}
		cancel()
	}
	deadline := time.Now().Add(2 * time.Second)
	for !op.allClosed() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !op.allClosed() {
		t.Fatalf("握手挂住的连接应在拨号期限后被关闭（开流 %d 次）", op.opens.Load())
	}
	op.mu.Lock()
	op.target = srv.ln.Addr().String()
	op.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := d.Exchange(ctx, txtQuery("example.com")); err != nil {
		t.Fatalf("节点恢复后应能查询（不应被挂住的握手占满名额）：%v", err)
	}
}

// 闸门关闭时直接走拨号路径也不开流（排队的拨号可能在关闸门之后才开始）。
func TestDoHDialRechecksGate(t *testing.T) {
	d, srv, g := newDoH(t, good)
	d.init()
	g.open.Store(false)
	if _, err := d.tr.DialTLSContext(context.Background(), "tcp", "1.1.1.1:443"); !errors.Is(err, ErrGateClosed) || srv.opens.Load() != 0 {
		t.Fatalf("闸门关闭时拨号路径应直接返回且不开流：%v %d", err, srv.opens.Load())
	}
}
