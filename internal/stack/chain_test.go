package stack

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"claude-abroad/internal/agent"
	"claude-abroad/internal/fakedns"
	"claude-abroad/internal/tun"
	"claude-abroad/internal/wire"
)

// 进程内全链路（M2-5b）：客户端 gVisor 栈 → tun.Pipe → Stack → Proxy → wire.Dialer（net.Pipe）
// → agent.DataPlane → 假 socks5（充当 sing-box）→ 本机回环上的后端。夹具照搬 internal/devsocks/chain_test.go。

const chainPW = "0123456789abcdef0123456789abcdef"

type sbFake struct {
	l      net.Listener
	routes map[string]string
	mu     sync.Mutex
	atyps  []byte
	names  []string
	users  []string
}

func newSBFake(t *testing.T, routes map[string]string) *sbFake {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	f := &sbFake{l: l, routes: routes}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go f.handle(c)
		}
	}()
	return f
}

func (f *sbFake) handle(c net.Conn) {
	defer c.Close()
	var b [2]byte
	io.ReadFull(c, b[:])
	io.CopyN(io.Discard, c, int64(b[1]))
	c.Write([]byte{5, 2})
	io.ReadFull(c, b[:])
	user := make([]byte, b[1])
	io.ReadFull(c, user)
	var n [1]byte
	io.ReadFull(c, n[:])
	pass := make([]byte, n[0])
	io.ReadFull(c, pass)
	if string(pass) != chainPW {
		c.Write([]byte{1, 1})
		return
	}
	c.Write([]byte{1, 0})
	var req [3]byte
	io.ReadFull(c, req[:])
	var at [1]byte
	io.ReadFull(c, at[:])
	name := ""
	switch at[0] {
	case 1:
		var a [4]byte
		io.ReadFull(c, a[:])
		name = netip.AddrFrom4(a).String()
	case 3:
		io.ReadFull(c, n[:])
		nb := make([]byte, n[0])
		io.ReadFull(c, nb)
		name = string(nb)
	}
	io.CopyN(io.Discard, c, 2)
	f.mu.Lock()
	f.atyps, f.names, f.users = append(f.atyps, at[0]), append(f.names, name), append(f.users, string(user))
	f.mu.Unlock()
	if req[1] == 3 { // UDP：回显
		u, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		defer u.Close()
		c.Write(binary.BigEndian.AppendUint16([]byte{5, 0, 0, 1, 127, 0, 0, 1}, uint16(u.LocalAddr().(*net.UDPAddr).Port)))
		go func() { io.Copy(io.Discard, c); u.Close() }()
		buf := make([]byte, 65536)
		for {
			m, from, err := u.ReadFromUDP(buf)
			if err != nil {
				return
			}
			src, data, err := agent.ParseUDPPacket(buf[:m])
			if err != nil {
				continue
			}
			u.WriteToUDP(append(agent.AppendUDPHeader(nil, "", src.Addr(), int(src.Port())), data...), from)
		}
	}
	be, err := net.Dial("tcp", f.routes[name])
	if err != nil {
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer be.Close()
	c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go cp(be, c)
	go cp(c, be)
	<-done
	<-done
}

type mainOnly struct{}

func (mainOnly) Lookup(tag string) (bool, bool) { return tag == "main", true }

type pipeLn struct {
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func (l *pipeLn) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *pipeLn) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *pipeLn) Addr() net.Addr { return &net.UnixAddr{Name: "pipe", Net: "unix"} }
func (l *pipeLn) Dial(ctx context.Context) (net.Conn, error) {
	a, b := net.Pipe()
	select {
	case l.ch <- b:
		return a, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type chainEnv struct {
	*proxyEnv
	sb       *sbFake
	closeAll func() // 关掉栈、客户端、DataPlane（幂等；Cleanup 也会调）
}

func newChainEnv(t *testing.T, routes map[string]string) *chainEnv {
	t.Helper()
	sb := newSBFake(t, routes)
	dp := &agent.DataPlane{Exits: mainOnly{},
		Timing: agent.DataTiming{DialTimeout: 5 * time.Second, UDPIdle: 5 * time.Second},
		Socks: func(e string) agent.SocksDialer {
			return agent.SocksDialer{Addr: sb.l.Addr().String(), User: e, Password: chainPW}
		}}
	pl := &pipeLn{ch: make(chan net.Conn), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	dpDone := make(chan struct{})
	go func() { dp.Serve(ctx, pl); close(dpDone) }()
	d := &wire.Dialer{Dial: pl.Dial, Config: wire.DefaultSessionConfig()}
	c := &clock{t: time.Now()}
	pool := fakedns.DefaultPool()
	srv := &fakedns.Server{Pool: pool, Bypass: fakedns.NewBypass(), Reverse: fakedns.NewReverse()}
	p := &Proxy{Pool: pool, DNS: srv, Gate: openGate(), Opener: d}
	p.SetEnabled(true)
	a, b := tun.Pipe(1500)
	s, err := New(a, p, Options{})
	if err != nil {
		t.Fatal(err)
	}
	cl := newClientRaw(t, b)
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			s.Close()
			cl.close()
			cancel()
			d.Close()
			pl.Close()
			<-dpDone
		})
	}
	t.Cleanup(closeAll)
	return &chainEnv{proxyEnv: &proxyEnv{s: s, cl: cl, p: p, clock: c}, sb: sb, closeAll: closeAll}
}

func listen(t *testing.T, serve func(net.Conn)) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); serve(c) }()
		}
	}()
	return l.Addr().String()
}

// c. 先查 DNS 拿假地址再连；假 socks 收到的是域名，exit 是 main；双向 160 KB；两个方向的半关闭都透传
func TestChainDomainAndHalfClose(t *testing.T) {
	var got bytes.Buffer
	var gotMu sync.Mutex
	gotDone := make(chan struct{})
	halfBE := listen(t, func(c net.Conn) {
		c.Write([]byte("hello"))
		c.(*net.TCPConn).CloseWrite() // 上游先半关闭
		b, _ := io.ReadAll(c)         // 应用之后还能写过来
		gotMu.Lock()
		got.Write(b)
		gotMu.Unlock()
		close(gotDone)
	})
	echoBE := listen(t, func(c net.Conn) {
		io.Copy(c, c)
		c.(*net.TCPConn).CloseWrite()
	})
	e := newChainEnv(t, map[string]string{"half.test": halfBE, "echo.test": echoBE})

	// 上游 FIN → 应用读到 EOF；应用之后写的数据和 FIN 仍到达上游。
	a := e.lookupA(t, "Half.Test")
	c, err := e.cl.dialTCP(ap(a, 7), ioWait)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(ioWait))
	b, err := io.ReadAll(c)
	if err != nil || string(b) != "hello" {
		t.Fatalf("上游半关闭应透传为 EOF：%q %v", b, err)
	}
	c.Write([]byte("after"))
	c.CloseWrite()
	select {
	case <-gotDone:
	case <-time.After(ioWait):
		t.Fatal("应用半关闭应透传到上游")
	}
	gotMu.Lock()
	if got.String() != "after" {
		t.Fatalf("上游应收到 after：%q", got.String())
	}
	gotMu.Unlock()
	c.Close()

	// 双向 160 KB
	a2 := e.lookupA(t, "echo.test")
	c2, err := e.cl.dialTCP(ap(a2, 7), ioWait)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	c2.SetDeadline(time.Now().Add(ioWait))
	payload := bytes.Repeat([]byte("0123456789abcdef"), 10240)
	go func() { c2.Write(payload); c2.CloseWrite() }()
	back, err := io.ReadAll(c2)
	if err != nil || !bytes.Equal(back, payload) {
		t.Fatalf("双向 160 KB 不对：%d 字节 %v", len(back), err)
	}

	e.sb.mu.Lock()
	defer e.sb.mu.Unlock()
	for i, n := range e.sb.names {
		if e.sb.atyps[i] != 3 || (n != "half.test" && n != "echo.test") || e.sb.users[i] != "main" {
			t.Fatalf("假 socks 应收到域名（ATYP=3）、出口 main：%v %v %v", e.sb.atyps, e.sb.names, e.sb.users)
		}
	}
}

// c. UDP 往返，回包源地址等于原目的地址；真实 IP 目标时帧头带 ip
func TestChainUDPAndRealIP(t *testing.T) {
	echoBE := listen(t, func(c net.Conn) { io.Copy(c, c) })
	e := newChainEnv(t, map[string]string{"203.0.113.77": echoBE})
	a := e.lookupA(t, "udp.test")
	u := e.cl.udp(t)
	dst := &net.UDPAddr{IP: a.AsSlice(), Port: 4500}
	u.WriteTo([]byte("ping"), dst)
	u.SetReadDeadline(time.Now().Add(ioWait))
	buf := make([]byte, 100)
	n, from, err := u.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "ping" {
		t.Fatalf("UDP 往返：%q %v", buf[:n], err)
	}
	if from.(*net.UDPAddr).AddrPort() != ap(a, 4500) {
		t.Fatalf("回包源地址应是原目的地址：%s", from)
	}

	c, err := e.cl.dialTCP(netip.MustParseAddrPort("203.0.113.77:7"), ioWait)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(ioWait))
	c.Write([]byte("x"))
	if _, err := io.ReadFull(c, buf[:1]); err != nil {
		t.Fatalf("真实 IP 目标：%v", err)
	}
	c.Close()
	e.sb.mu.Lock()
	defer e.sb.mu.Unlock()
	found := false
	for i, n := range e.sb.names {
		if n == "udp.test" && e.sb.atyps[i] != 3 {
			t.Fatal("UDP 的目标应以域名交出去")
		}
		if n == "203.0.113.77" && e.sb.atyps[i] == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("真实 IP 目标应以 IP（ATYP=1）交出去：%v %v", e.sb.atyps, e.sb.names)
	}
}

// h. 全链路上有 TCP、UDP、DNS 流时 Close 在期限内返回，goroutine 回到起点附近
func TestChainCloseWithFlows(t *testing.T) {
	before := runtime.NumGoroutine()
	holdBE := make(chan struct{})
	func() {
		be := listen(t, func(c net.Conn) { <-holdBE })
		defer close(holdBE)
		e := newChainEnv(t, map[string]string{"hold.test": be})
		a := e.lookupA(t, "hold.test")
		c, err := e.cl.dialTCP(ap(a, 7), ioWait)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		u := e.cl.udp(t)
		u.WriteTo([]byte("x"), &net.UDPAddr{IP: a.AsSlice(), Port: 9})
		d, err := e.cl.dialTCP(netip.MustParseAddrPort("198.18.0.2:53"), ioWait)
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		dq := new(dns.Msg)
		dq.SetQuestion("q.test.", dns.TypeA)
		(&dns.Conn{Conn: d}).WriteMsg(dq)
		waitFor(t, "三种流都在", func() bool {
			st := e.s.Stats()
			return st.TCPActive == 2 && st.UDPActive >= 1 && e.p.Stats().TCPProxied == 1
		})
		done := make(chan struct{})
		go func() { e.s.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(ioWait):
			t.Fatal("有流时 Close 应在期限内返回")
		}
		c.Close()
		d.Close()
		u.Close()
		e.closeAll()
	}()
	deadline := time.Now().Add(ioWait)
	for runtime.NumGoroutine() > before+8 { // 留几个给后端监听器（在 Cleanup 里关）
		if time.Now().After(deadline) {
			t.Fatalf("goroutine 没有回落：%d → %d", before, runtime.NumGoroutine())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 应用先 FIN、上游读到 EOF 之后才应答（最常见的半关闭用法）。
func TestChainAppFinFirst(t *testing.T) {
	be := listen(t, func(c net.Conn) {
		req, _ := io.ReadAll(c) // 等应用的 FIN
		c.Write(append([]byte("resp:"), req...))
	})
	e := newChainEnv(t, map[string]string{"fin.test": be})
	a := e.lookupA(t, "fin.test")
	c, err := e.cl.dialTCP(ap(a, 7), ioWait)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(ioWait))
	c.Write([]byte("req"))
	c.CloseWrite()
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "resp:req" {
		t.Fatalf("应用半关闭后应收到上游的应答：%q %v", got, err)
	}
}
