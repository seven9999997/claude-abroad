package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"claude-abroad/internal/wire"
)

// ---------- 假的 socks5 服务（充当 sing-box） ----------

type fakeSocks struct {
	t     *testing.T
	l     net.Listener
	user  string
	pass  string
	mode  string        // ""、"reject"、"hang"、"udp-unsupported"、"bad-relay"
	delay time.Duration // 回 CONNECT 应答前等多久
	// UDP 回包的花样
	udpFrag     bool // 先回一个 FRAG=1 的包
	udpWrongSrc bool // 先回一个来源不对的包

	conns  atomic.Int32
	closed chan struct{} // "hang" 模式下客户端关连接时关闭
	inHang chan struct{} // "hang" 模式下收到 CONNECT、开始挂起时关闭
	hangMu sync.Once
	mu     sync.Mutex
	atyp   byte
	name   string
}

func newFakeSocks(t *testing.T) *fakeSocks {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSocks{t: t, l: l, user: "main", pass: testPW, closed: make(chan struct{}), inHang: make(chan struct{})}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			f.conns.Add(1)
			go f.handle(c)
		}
	}()
	return f
}

func (f *fakeSocks) handle(c net.Conn) {
	defer c.Close()
	var b [2]byte
	if _, err := io.ReadFull(c, b[:]); err != nil {
		return
	}
	methods := make([]byte, b[1])
	io.ReadFull(c, methods)
	c.Write([]byte{5, 2})
	io.ReadFull(c, b[:])
	user := make([]byte, b[1])
	io.ReadFull(c, user)
	var pl [1]byte
	io.ReadFull(c, pl[:])
	pass := make([]byte, pl[0])
	io.ReadFull(c, pass)
	if string(user) != f.user || string(pass) != f.pass {
		c.Write([]byte{1, 1})
		return
	}
	c.Write([]byte{1, 0})
	var req [3]byte
	if _, err := io.ReadFull(c, req[:]); err != nil {
		return
	}
	var t [1]byte
	io.ReadFull(c, t[:])
	name := ""
	switch t[0] {
	case 1:
		io.CopyN(io.Discard, c, 4)
	case 4:
		io.CopyN(io.Discard, c, 16)
	case 3:
		var n [1]byte
		io.ReadFull(c, n[:])
		nb := make([]byte, n[0])
		io.ReadFull(c, nb)
		name = string(nb)
	}
	io.CopyN(io.Discard, c, 2)
	f.mu.Lock()
	f.atyp, f.name = t[0], name
	f.mu.Unlock()

	ok := []byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}
	switch {
	case f.mode == "hang":
		f.hangMu.Do(func() { close(f.inHang) })
		io.Copy(io.Discard, c) // 等客户端放弃
		close(f.closed)
		return
	case f.mode == "reject":
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	case req[1] == 3:
		f.udp(c)
		return
	}
	time.Sleep(f.delay)
	c.Write(ok)
	if f.mode == "server-first" { // 对端先结束：立即半关闭，但一直读
		c.Write([]byte("bye"))
		c.(*net.TCPConn).CloseWrite()
		io.Copy(io.Discard, c)
		return
	}
	// CONNECT：自己当回显服务，读到 EOF 后半关闭。
	io.Copy(c, c)
	c.(*net.TCPConn).CloseWrite()
	io.Copy(io.Discard, c)
}

func (f *fakeSocks) udp(c net.Conn) {
	if f.mode == "udp-unsupported" {
		c.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	relay, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return
	}
	defer relay.Close()
	port := relay.LocalAddr().(*net.UDPAddr).Port
	addr := []byte{0, 0, 0, 0} // BND=0.0.0.0：客户端应换成 127.0.0.1
	if f.mode == "bad-relay" {
		addr = []byte{203, 0, 113, 1}
	}
	rep := append([]byte{5, 0, 0, 1}, addr...)
	c.Write(binary.BigEndian.AppendUint16(rep, uint16(port)))
	go func() { io.Copy(io.Discard, c); relay.Close() }() // 控制连接断开即结束
	buf := make([]byte, 70000)
	for {
		n, from, err := relay.ReadFromUDP(buf)
		if err != nil {
			return
		}
		src, data, err := ParseUDPPacket(buf[:n])
		if err != nil {
			continue
		}
		if !src.Addr().IsValid() { // 目标是域名：回包来源用一个固定地址
			src = netip.MustParseAddrPort("192.0.2.53:53")
		}
		if f.udpFrag {
			relay.WriteToUDP(append([]byte{0, 0, 1}, AppendUDPHeader(nil, "", src.Addr(), int(src.Port()))[3:]...), from)
		}
		if f.udpWrongSrc {
			relay.WriteToUDP(append(AppendUDPHeader(nil, "", netip.MustParseAddr("192.0.2.99"), 9), data...), from)
		}
		relay.WriteToUDP(append(AppendUDPHeader(nil, "", src.Addr(), int(src.Port())), data...), from)
	}
}

// ---------- 被测的数据面 ----------

type staticExits struct{ up bool }

func (s staticExits) Lookup(tag string) (bool, bool) { return tag == "main", s.up }

type dpFixture struct {
	dp      *DataPlane
	socks   *fakeSocks
	sess    *yamux.Session
	dialed  sync.Map // 数据面拨过的地址
	logs    *syncBuffer
	cancel  context.CancelFunc
	done    chan error
	dialer  *wire.Dialer
	clients []net.Conn
}

var testDataTiming = DataTiming{HeaderTimeout: time.Second, DialTimeout: 300 * time.Millisecond,
	UDPIdle: 300 * time.Millisecond, HalfCloseLinger: time.Second, MaxSessions: 4, MaxStreams: 4096}

func newDP(t *testing.T, up bool, opts ...func(*dpFixture)) *dpFixture {
	f := &dpFixture{socks: newFakeSocks(t), logs: &syncBuffer{}}
	f.dp = &DataPlane{Exits: staticExits{up}, Timing: testDataTiming,
		Logf: func(format string, a ...any) { fmt.Fprintf(f.logs, format+"\n", a...) },
		Socks: func(exit string) SocksDialer {
			return SocksDialer{Addr: f.socks.l.Addr().String(), User: exit, Password: testPW,
				Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
					f.dialed.Store(addr, true)
					var d net.Dialer
					return d.DialContext(ctx, network, addr)
				}}
		}}
	for _, o := range opts {
		o(f)
	}
	l := newPipeListener()
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel, f.done = cancel, make(chan error, 1)
	go func() { f.done <- f.dp.Serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-f.done:
		case <-time.After(5 * time.Second):
			t.Error("数据面没有在 ctx 结束后退出")
		}
	})
	cfg := wire.DefaultSessionConfig()
	f.dialer = &wire.Dialer{Dial: l.Dial, Config: cfg}
	t.Cleanup(func() { f.dialer.Close() })
	return f
}

// raw 开一条原始流（不经 Dialer.Open 的握手），用来测回应前的行为。
func (f *dpFixture) raw(t *testing.T) net.Conn {
	if f.sess == nil {
		l := f.dialer.Dial
		c, err := l(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		s, err := wire.Client(c, wire.DefaultSessionConfig())
		if err != nil {
			t.Fatal(err)
		}
		f.sess = s
		t.Cleanup(func() { s.Close() })
	}
	st, err := f.sess.Open()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (f *dpFixture) open(t *testing.T, h wire.Header) (net.Conn, error) {
	h.V = wire.Version
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return f.dialer.Open(ctx, h)
}

func expectCode(t *testing.T, err error, code wire.Code) {
	t.Helper()
	var re *wire.ReplyError
	if !errors.As(err, &re) || re.Code != code {
		t.Fatalf("应回 %s：%v", code, err)
	}
}

// pipeListener：Dial 一次产生一对 net.Pipe。
type pipeListener struct {
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{ch: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *pipeListener) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *pipeListener) Addr() net.Addr { return &net.UnixAddr{Name: "pipe", Net: "unix"} }
func (l *pipeListener) Dial(ctx context.Context) (net.Conn, error) {
	a, b := net.Pipe()
	select {
	case l.ch <- b:
		return a, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

// ---------- TCP ----------

func TestDataTCPRoundTripAndHalfClose(t *testing.T) {
	f := newDP(t, true)
	c, err := f.open(t, wire.Header{Exit: "main", Net: "tcp", IP: "198.51.100.7", Port: 443})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("hello"))
	c.Close() // yamux：半关闭，回显方读到 EOF 后也半关闭
	got, _ := io.ReadAll(c)
	if string(got) != "hello" {
		t.Fatalf("回显：%q", got)
	}
	f.socks.mu.Lock()
	atyp := f.socks.atyp
	f.socks.mu.Unlock()
	if atyp != 1 {
		t.Errorf("IP 目标应以 ATYP=1 发出：%d", atyp)
	}
}

func TestDataDomainNotResolved(t *testing.T) {
	f := newDP(t, true)
	c, err := f.open(t, wire.Header{Exit: "main", Net: "tcp", Host: "claude.ai", Port: 443, App: "com.anthropic." + sentinel})
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("x"))
	c.Close()
	io.ReadAll(c)
	f.socks.mu.Lock()
	atyp, name := f.socks.atyp, f.socks.name
	f.socks.mu.Unlock()
	if atyp != 3 || name != "claude.ai" {
		t.Fatalf("域名应原样以 ATYP=3 交给 sing-box：%d %q", atyp, name)
	}
	n := 0
	f.dialed.Range(func(k, _ any) bool {
		n++
		if k.(string) != f.socks.l.Addr().String() {
			t.Errorf("数据面只能拨 socks 地址，实际拨了 %s", k)
		}
		return true
	})
	if n != 1 {
		t.Fatalf("应只拨过 socks 地址：%d 个", n)
	}
	if strings.Contains(f.logs.String(), sentinel) || strings.Contains(f.logs.String(), "claude.ai") {
		t.Fatal("日志里不应有目标和应用")
	}
}

func TestDataUnknownExitAndExitDown(t *testing.T) {
	f := newDP(t, true)
	_, err := f.open(t, wire.Header{Exit: "wg-us", Net: "tcp", IP: "198.51.100.7", Port: 443})
	expectCode(t, err, wire.CodeUnknownExit)
	g := newDP(t, false)
	_, err = g.open(t, wire.Header{Exit: "main", Net: "tcp", IP: "198.51.100.7", Port: 443})
	expectCode(t, err, wire.CodeExitDown)
	if f.socks.conns.Load()+g.socks.conns.Load() != 0 {
		t.Fatal("出口不认识或不可用时不应连 sing-box")
	}
	if s := g.dp.Stats(); s.Rejects["exit_down"] != 1 {
		t.Fatalf("拒绝计数：%+v", s)
	}
}

func TestDataDialFailures(t *testing.T) {
	for _, mode := range []string{"reject", "badpass", "hang"} {
		t.Run(mode, func(t *testing.T) {
			f := newDP(t, true, func(f *dpFixture) {
				if mode == "badpass" {
					f.socks.pass = "different"
				} else {
					f.socks.mode = mode
				}
			})
			start := time.Now()
			_, err := f.open(t, wire.Header{Exit: "main", Net: "tcp", IP: "198.51.100.7", Port: 443})
			expectCode(t, err, wire.CodeDialFailed)
			if time.Since(start) > 2*time.Second {
				t.Fatalf("应在拨号超时后尽快返回：%s", time.Since(start))
			}
		})
	}
}

func TestDataAbandonBeforeReply(t *testing.T) {
	f := newDP(t, true, func(f *dpFixture) { f.socks.mode = "hang"; f.dp.Timing.DialTimeout = 5 * time.Second })
	st := f.raw(t)
	wire.WriteHeader(st, wire.Header{V: 1, Exit: "main", Net: "tcp", IP: "198.51.100.7", Port: 443})
	// 等数据面真的连上 sing-box 再放弃（固定睡 50 毫秒在全量并行时不够：还没拨就放弃，就没有连接可关）。
	select {
	case <-f.socks.inHang:
	case <-time.After(5 * time.Second):
		t.Fatal("数据面没有连上 sing-box")
	}
	st.Close() // 回应前放弃（半关闭 → 数据面读到 EOF）
	select {
	case <-f.socks.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("放弃后数据面应立即关掉到 sing-box 的连接")
	}
}

func TestDataEarlyDataNotLost(t *testing.T) {
	f := newDP(t, true, func(f *dpFixture) { f.socks.delay = 100 * time.Millisecond })
	st := f.raw(t)
	var b bytes.Buffer
	wire.WriteHeader(&b, wire.Header{V: 1, Exit: "main", Net: "tcp", IP: "198.51.100.7", Port: 443})
	b.WriteString("early-bytes")
	st.Write(b.Bytes()) // 帧头和数据一起发，回应还没回来
	rep, err := wire.ReadReply(st)
	if err != nil || !rep.OK {
		t.Fatalf("回应：%+v %v", rep, err)
	}
	st.Close()
	got, _ := io.ReadAll(st)
	if string(got) != "early-bytes" {
		t.Fatalf("回应前到达的数据丢了：%q", got)
	}
}

func TestDataBadHeader(t *testing.T) {
	f := newDP(t, true)
	st := f.raw(t)
	st.Write([]byte{0, 5, '{', 'x', '}', 0, 0})
	rep, err := wire.ReadReply(st)
	if err != nil || rep.OK || rep.Code != wire.CodeBadRequest {
		t.Fatalf("非法帧头应回 bad_request：%+v %v", rep, err)
	}
}

func TestDataConcurrentStreams(t *testing.T) {
	f := newDP(t, true)
	var wg sync.WaitGroup
	var bad atomic.Int32
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := f.open(t, wire.Header{Exit: "main", Net: "tcp", IP: "198.51.100.7", Port: 443})
			if err != nil {
				bad.Add(1)
				return
			}
			msg := fmt.Sprintf("msg-%d", i)
			c.Write([]byte(msg))
			c.Close()
			got, _ := io.ReadAll(c)
			if string(got) != msg {
				bad.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d 条流失败", bad.Load())
	}
}

func TestDataShutdownClosesOutbound(t *testing.T) {
	f := newDP(t, true)
	c, err := f.open(t, wire.Header{Exit: "main", Net: "tcp", IP: "198.51.100.7", Port: 443})
	if err != nil {
		t.Fatal(err)
	}
	f.cancel()
	select {
	case <-f.done:
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 结束后数据面应退出")
	}
	f.done <- nil // 让 Cleanup 不再等
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("数据面退出后流应已关闭")
	}
	if s := f.dp.Stats(); s.ActiveStreams != 0 || s.Sessions != 0 {
		t.Fatalf("退出后不应有活动流和会话：%+v", s)
	}
}

// ---------- UDP ----------

func udpOpen(t *testing.T, f *dpFixture, h wire.Header) *wire.DatagramConn {
	t.Helper()
	c, err := f.open(t, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return wire.NewDatagramConn(c)
}

func readDG(t *testing.T, dc *wire.DatagramConn) []byte {
	t.Helper()
	buf := make([]byte, wire.MaxDatagram)
	done := make(chan []byte, 1)
	go func() {
		n, err := dc.ReadDatagram(buf)
		if err != nil {
			done <- nil
			return
		}
		done <- bytes.Clone(buf[:n]) // 0 字节时也返回非 nil
	}()
	select {
	case b := <-done:
		return b
	case <-time.After(2 * time.Second):
		t.Fatal("没有收到数据报")
		return nil
	}
}

func TestDataUDPRoundTrip(t *testing.T) {
	f := newDP(t, true)
	dc := udpOpen(t, f, wire.Header{Exit: "main", Net: "udp", IP: "198.51.100.7", Port: 53})
	for _, msg := range [][]byte{[]byte("ping"), {}} { // 含 0 字节数据报
		if err := dc.WriteDatagram(msg); err != nil {
			t.Fatal(err)
		}
		if got := readDG(t, dc); got == nil || !bytes.Equal(got, msg) {
			t.Fatalf("回显：%q", got)
		}
	}
	big := make([]byte, 65500) // 加上 SOCKS 头会超过 65507：丢弃并计数
	dc.WriteDatagram(big)
	dc.WriteDatagram([]byte("after"))
	if got := readDG(t, dc); string(got) != "after" {
		t.Fatalf("超长数据报应被丢弃：%q", got)
	}
	if f.dp.Stats().UDPDropped != 1 {
		t.Fatalf("丢弃计数：%d", f.dp.Stats().UDPDropped)
	}
}

func TestDataUDPFilters(t *testing.T) {
	f := newDP(t, true, func(f *dpFixture) { f.socks.udpFrag, f.socks.udpWrongSrc = true, true })
	dc := udpOpen(t, f, wire.Header{Exit: "main", Net: "udp", IP: "198.51.100.7", Port: 53})
	dc.WriteDatagram([]byte("one"))
	if got := readDG(t, dc); string(got) != "one" {
		t.Fatalf("FRAG≠0 和来源不对的包应被丢弃，只收到正确的回包：%q", got)
	}
	if f.dp.Stats().UDPDropped != 2 {
		t.Fatalf("应丢 2 个：%d", f.dp.Stats().UDPDropped)
	}
	// 目标是域名时不过滤来源。
	g := newDP(t, true, func(f *dpFixture) { f.socks.udpWrongSrc = true })
	dc2 := udpOpen(t, g, wire.Header{Exit: "main", Net: "udp", Host: "dns.example", Port: 53})
	dc2.WriteDatagram([]byte("two"))
	if got := readDG(t, dc2); string(got) != "two" {
		t.Fatalf("域名目标：%q", got)
	}
}

func TestDataUDPIdleAndErrors(t *testing.T) {
	f := newDP(t, true)
	c, err := f.open(t, wire.Header{Exit: "main", Net: "udp", IP: "198.51.100.7", Port: 53})
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	_, err = c.Read(make([]byte, 1))
	if err == nil || time.Since(start) > 2*time.Second || time.Since(start) < testDataTiming.UDPIdle {
		t.Fatalf("空闲后应关闭：%v，用时 %s", err, time.Since(start))
	}

	g := newDP(t, true, func(f *dpFixture) { f.socks.mode = "udp-unsupported" })
	_, err = g.open(t, wire.Header{Exit: "main", Net: "udp", IP: "198.51.100.7", Port: 53})
	expectCode(t, err, wire.CodeUDPUnsupported)

	h := newDP(t, true, func(f *dpFixture) { f.socks.mode = "bad-relay" })
	_, err = h.open(t, wire.Header{Exit: "main", Net: "udp", IP: "198.51.100.7", Port: 53})
	expectCode(t, err, wire.CodeDialFailed)
}

func TestDataIPv4MappedAndIPv6Targets(t *testing.T) {
	f := newDP(t, true)
	c, err := f.open(t, wire.Header{Exit: "main", Net: "tcp", IP: "::ffff:198.51.100.7", Port: 443})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	io.ReadAll(c)
	f.socks.mu.Lock()
	atyp := f.socks.atyp
	f.socks.mu.Unlock()
	if atyp != 1 {
		t.Fatalf("IPv4 映射地址应以 ATYP=1 发出：%d", atyp)
	}
	c, err = f.open(t, wire.Header{Exit: "main", Net: "tcp", IP: "2001:db8::7", Port: 443})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	io.ReadAll(c)
	f.socks.mu.Lock()
	atyp = f.socks.atyp
	f.socks.mu.Unlock()
	if atyp != 4 {
		t.Fatalf("IPv6 目标应以 ATYP=4 发出：%d", atyp)
	}
	// UDP：目标写成映射形式，回包来源是普通 IPv4，不能被当成来源不对丢掉。
	dc := udpOpen(t, f, wire.Header{Exit: "main", Net: "udp", IP: "::ffff:198.51.100.7", Port: 53})
	dc.WriteDatagram([]byte("m"))
	if got := readDG(t, dc); string(got) != "m" {
		t.Fatalf("映射地址目标的回包被丢了：%q", got)
	}
}

func TestDataMaxStreams(t *testing.T) {
	f := newDP(t, true, func(f *dpFixture) { f.dp.Timing.MaxStreams = 2 })
	h := wire.Header{Exit: "main", Net: "tcp", IP: "198.51.100.7", Port: 443}
	a, err := f.open(t, h)
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.open(t, h)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.open(t, h)
	expectCode(t, err, wire.CodeDialFailed)
	for _, c := range []net.Conn{a, b} {
		c.Close()
		io.ReadAll(c)
	}
	deadline := time.Now().Add(2 * time.Second)
	for f.dp.Stats().ActiveStreams != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := f.open(t, h); err != nil {
		t.Fatalf("前两条关掉后应能再开：%v", err)
	}
}

func TestDataMaxSessions(t *testing.T) {
	f := newDP(t, true, func(f *dpFixture) { f.dp.Timing.MaxSessions = 1 })
	h := wire.Header{V: 1, Exit: "main", Net: "tcp", IP: "198.51.100.7", Port: 443}
	open := func() (*yamux.Session, error) {
		c, err := f.dialer.Dial(context.Background())
		if err != nil {
			return nil, err
		}
		cfg := wire.DefaultSessionConfig()
		cfg.KeepAliveInterval = -1
		s, err := wire.Client(c, cfg)
		if err != nil {
			return nil, err
		}
		st, err := s.Open()
		if err != nil {
			s.Close()
			return nil, err
		}
		st.SetDeadline(time.Now().Add(time.Second))
		if err := wire.WriteHeader(st, h); err != nil {
			s.Close()
			return nil, err
		}
		if _, err := wire.ReadReply(st); err != nil {
			s.Close()
			return nil, err
		}
		st.Close()
		return s, nil
	}
	s1, err := open()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := open(); err == nil {
		t.Fatal("会话数到上限时第二条连接应被关掉")
	}
	s1.Close()
	deadline := time.Now().Add(2 * time.Second)
	for f.dp.Stats().Sessions != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	s3, err := open()
	if err != nil {
		t.Fatalf("第一条断开后新连接应可用：%v", err)
	}
	s3.Close()
}

func TestDataHalfCloseLinger(t *testing.T) {
	f := newDP(t, true, func(f *dpFixture) {
		f.dp.Timing.HalfCloseLinger = 200 * time.Millisecond
		f.socks.mode = "server-first"
	})
	c, err := f.open(t, wire.Header{Exit: "main", Net: "tcp", IP: "198.51.100.7", Port: 443})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	buf := make([]byte, 3)
	io.ReadFull(c, buf) // 对端发完 "bye" 就半关闭；客户端不关自己的写方向
	start := time.Now()
	deadline := start.Add(2 * time.Second)
	for f.dp.Stats().ActiveStreams != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := f.dp.Stats().ActiveStreams; n != 0 {
		t.Fatalf("linger 到期后流应被关闭：活动流 %d", n)
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Fatalf("应等 linger（200ms）再关，实际 %s", d)
	}
}
