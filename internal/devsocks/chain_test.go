package devsocks_test

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync"
	"testing"
	"time"

	"claude-abroad/internal/agent"
	"claude-abroad/internal/devsocks"
	"claude-abroad/internal/wire"
)

// 外部测试包：agent 引用了 devsocks（分流出口的 socks 服务），包内测试再引用 agent 会成环。
// SOCKS5 应答码（和 devsocks 里的未导出常量一致）。
const (
	repNotAllowed = 0x02
	repRefused    = 0x05
)

// 进程内全链路（M1 开发完成的自动化验收）：
// 本地 SOCKS5（devsocks）→ wire/yamux（net.Pipe）→ agent.DataPlane → 假 socks5（充当 sing-box）→ 后端。

const pw = "0123456789abcdef0123456789abcdef"

// fakeSingBox：校验用户名口令；CONNECT 按目标名路由到后端（记录 ATYP 和名字）；UDP ASSOCIATE 原样回显。
type fakeSingBox struct {
	l      net.Listener
	routes map[string]string // 目标名 → 后端地址
	reject bool
	mu     sync.Mutex
	atyps  []byte
	names  []string
}

func newFakeSingBox(t *testing.T, routes map[string]string) *fakeSingBox {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	f := &fakeSingBox{l: l, routes: routes}
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

func (f *fakeSingBox) handle(c net.Conn) {
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
	if string(user) != "main" || string(pass) != pw {
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
	f.atyps, f.names = append(f.atyps, at[0]), append(f.names, name)
	f.mu.Unlock()
	if f.reject {
		c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
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
	pipe(c, be)
}

type exits struct{}

func (exits) Lookup(tag string) (bool, bool) { return tag == "main", true }

// pipeListener：Dial 一次产生一对 net.Pipe。
type pipeListener struct {
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once
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
	}
}

type chain struct {
	sb    *fakeSingBox
	dp    *agent.DataPlane
	socks string // 本地 SOCKS5 地址
}

func newChain(t *testing.T, exit string, routes map[string]string, reject bool) *chain {
	sb := newFakeSingBox(t, routes)
	sb.reject = reject
	dp := &agent.DataPlane{Exits: exits{},
		Timing: agent.DataTiming{DialTimeout: time.Second, UDPIdle: 2 * time.Second},
		Socks: func(e string) agent.SocksDialer {
			return agent.SocksDialer{Addr: sb.l.Addr().String(), User: e, Password: pw}
		}}
	pl := &pipeListener{ch: make(chan net.Conn), closed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	dpDone := make(chan struct{})
	go func() { dp.Serve(ctx, pl); close(dpDone) }()
	d := &wire.Dialer{Dial: pl.Dial, Config: wire.DefaultSessionConfig()}
	sl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &devsocks.Server{Open: d.Open, Exit: exit}
	srvDone := make(chan struct{})
	go func() { srv.Serve(ctx, sl); close(srvDone) }()
	t.Cleanup(func() {
		cancel()
		d.Close()
		<-srvDone
		<-dpDone
	})
	return &chain{sb: sb, dp: dp, socks: sl.Addr().String()}
}

// socksConnect 用最小的 SOCKS5 客户端发 CONNECT，返回连接和应答码。
func socksConnect(t *testing.T, addr string, cmd byte, host string, port int) (net.Conn, byte) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte{5, 1, 0})
	var b [2]byte
	io.ReadFull(c, b[:])
	req := []byte{5, cmd, 0}
	if a, err := netip.ParseAddr(host); err == nil {
		a4 := a.As4()
		req = append(append(req, 1), a4[:]...)
	} else {
		req = append(append(req, 3, byte(len(host))), host...)
	}
	c.Write(binary.BigEndian.AppendUint16(req, uint16(port)))
	rep := make([]byte, 10)
	if _, err := io.ReadFull(c, rep); err != nil {
		t.Fatalf("读 SOCKS 应答：%v", err)
	}
	c.SetDeadline(time.Time{})
	return c, rep[1]
}

func TestChainHTTPDomainNotResolved(t *testing.T) {
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer web.Close()
	ch := newChain(t, "main", map[string]string{"generate.test": web.Listener.Addr().String()}, false)
	pu, _ := url.Parse("socks5://" + ch.socks)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}, Timeout: 5 * time.Second}
	resp, err := client.Get("http://generate.test/generate_204")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Fatalf("应拿到 204：%d", resp.StatusCode)
	}
	ch.sb.mu.Lock()
	defer ch.sb.mu.Unlock()
	if len(ch.sb.atyps) != 1 || ch.sb.atyps[0] != 3 || ch.sb.names[0] != "generate.test" {
		t.Fatalf("目标应以域名（ATYP=3）原样到达 sing-box：%v %v", ch.sb.atyps, ch.sb.names)
	}
}

func TestChainTCPHalfClose(t *testing.T) {
	echo, _ := net.Listen("tcp", "127.0.0.1:0")
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() {
				b, _ := io.ReadAll(c)
				c.Write(append([]byte("echo:"), b...))
				c.Close()
			}()
		}
	}()
	ch := newChain(t, "main", map[string]string{"198.51.100.9": echo.Addr().String()}, false)
	c, rep := socksConnect(t, ch.socks, 1, "198.51.100.9", 7)
	defer c.Close()
	if rep != 0 {
		t.Fatalf("CONNECT 应成功：%d", rep)
	}
	c.Write([]byte("ping"))
	c.(*net.TCPConn).CloseWrite()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, _ := io.ReadAll(c)
	if string(got) != "echo:ping" {
		t.Fatalf("半关闭后应读到回显并 EOF：%q", got)
	}
	waitIdle(t, ch.dp)
}

func TestChainUDP(t *testing.T) {
	ch := newChain(t, "main", nil, false)
	relay := relayAddr(t, ch.socks)
	defer relay.ctrl.Close()
	u, err := net.DialUDP("udp", nil, relay.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	pkt := append(agent.AppendUDPHeader(nil, "", netip.MustParseAddr("198.51.100.7"), 53), []byte("q?")...)
	u.Write(pkt)
	u.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 2048)
	n, err := u.Read(buf)
	if err != nil {
		t.Fatalf("UDP 没收到回包：%v", err)
	}
	src, data, err := agent.ParseUDPPacket(buf[:n])
	if err != nil || string(data) != "q?" || src.String() != "198.51.100.7:53" {
		t.Fatalf("UDP 回包：%v %q %v", src, data, err)
	}
	relay.ctrl.Close()
	waitIdle(t, ch.dp)
}

type assoc struct {
	ctrl net.Conn
	addr *net.UDPAddr
}

// relayAddr 发一次 UDP ASSOCIATE 并解析 BND（127.0.0.1:端口）。
func relayAddr(t *testing.T, socks string) assoc {
	c, err := net.Dial("tcp", socks)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte{5, 1, 0})
	var b [2]byte
	io.ReadFull(c, b[:])
	c.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0})
	rep := make([]byte, 10)
	io.ReadFull(c, rep)
	c.SetDeadline(time.Time{})
	if rep[1] != 0 {
		t.Fatalf("UDP ASSOCIATE 失败：%d", rep[1])
	}
	return assoc{ctrl: c, addr: &net.UDPAddr{IP: net.IP(rep[4:8]), Port: int(binary.BigEndian.Uint16(rep[8:10]))}}
}

func TestChainErrors(t *testing.T) {
	ch := newChain(t, "nope", nil, false)
	c, rep := socksConnect(t, ch.socks, 1, "example.com", 443)
	c.Close()
	if rep != repNotAllowed {
		t.Fatalf("未知出口应回 SOCKS 0x02：%#x", rep)
	}
	ch2 := newChain(t, "main", nil, true)
	c, rep = socksConnect(t, ch2.socks, 1, "example.com", 443)
	c.Close()
	if rep != repRefused {
		t.Fatalf("出口拨号失败应回 SOCKS 0x05：%#x", rep)
	}
	waitIdle(t, ch2.dp)
}

func waitIdle(t *testing.T, dp *agent.DataPlane) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for dp.Stats().ActiveStreams != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := dp.Stats().ActiveStreams; n != 0 {
		t.Fatalf("结束后流数应归零：%d", n)
	}
}

// pipe 双向拷贝（测试里的假 sing-box 用；和 devsocks 的未导出 pipe 同样的做法）。
func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	wg.Add(2)
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}
