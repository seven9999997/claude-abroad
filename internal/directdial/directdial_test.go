package directdial

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"claude-abroad/internal/fdpass"

	"github.com/miekg/dns"
)

var bg = context.Background()

var dnsAddr = netip.MustParseAddr("192.0.2.53") // TEST-NET：只在假拨号器里出现

// fakeWire 是假的“绑物理网卡”拨号器：
//   - 发往 dnsAddr:53 的连接接到进程内的 DNS 应答函数；
//   - 其余目标实际连到本机回环上的监听（TCP、UDP 各一个），并记下请求的目标。
type fakeWire struct {
	mu       sync.Mutex
	dials    []string // "tcp 1.2.3.4:443"，不含 DNS 查询
	queries  []string // DNS 收到的问题名
	answer   func(q *dns.Msg) []*dns.Msg
	fail     map[netip.Addr]bool // 拨这些地址失败
	block    bool                // 拨号一直挂到 ctx 结束
	tcpLn    net.Listener
	udpLn    net.PacketConn
	dnsTCPOK bool // TCP 查询也接
}

func newFakeNet(t *testing.T) *fakeWire {
	t.Helper()
	tl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ul, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeWire{tcpLn: tl, udpLn: ul, fail: map[netip.Addr]bool{}, dnsTCPOK: true}
	t.Cleanup(func() { tl.Close(); ul.Close() })
	go func() {
		for {
			c, err := tl.Accept()
			if err != nil {
				return
			}
			go func() { // 回显
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, from, err := ul.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = ul.WriteTo(buf[:n], from)
		}
	}()
	return f
}

func (f *fakeWire) dialCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.dials) }

func (f *fakeWire) setFail(a string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[netip.MustParseAddr(a)] = true
}

func (f *fakeWire) setBlock(b bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.block = b
}

func (f *fakeWire) setAnswer(fn func(q *dns.Msg) []*dns.Msg) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = fn
}
func (f *fakeWire) queryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

func (f *fakeWire) DialDirect(ctx context.Context, network string, dst netip.AddrPort) (net.Conn, error) {
	if dst.Addr() == dnsAddr && dst.Port() == 53 {
		if network == "tcp" && !f.dnsTCPOK {
			return nil, errors.New("TCP 拒绝")
		}
		a, b := net.Pipe()
		go f.serveDNS(network, b)
		return a, nil
	}
	f.mu.Lock()
	f.dials = append(f.dials, network+" "+dst.String())
	fail, block := f.fail[dst.Addr()], f.block
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if fail {
		return nil, errors.New("连不上（测试）")
	}
	var d net.Dialer
	if network == "udp" {
		return d.DialContext(ctx, "udp", f.udpLn.LocalAddr().String())
	}
	return d.DialContext(ctx, "tcp", f.tcpLn.Addr().String())
}

func (f *fakeWire) serveDNS(network string, c net.Conn) {
	defer c.Close()
	var raw []byte
	if network == "tcp" {
		var l [2]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return
		}
		raw = make([]byte, binary.BigEndian.Uint16(l[:]))
		if _, err := io.ReadFull(c, raw); err != nil {
			return
		}
	} else {
		buf := make([]byte, 4096)
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		raw = buf[:n]
	}
	q := new(dns.Msg)
	if q.Unpack(raw) != nil {
		return
	}
	f.mu.Lock()
	f.queries = append(f.queries, network+" "+q.Question[0].Name)
	ans := f.answer
	f.mu.Unlock()
	if ans == nil {
		_, _ = io.Copy(io.Discard, c) // 不应答，也不关：让查询方等到超时
		return
	}
	if out := ans(q); out == nil {
		_, _ = io.Copy(io.Discard, c)
		return
	}
	for _, m := range ans(q) {
		out, _ := m.Pack()
		if network == "tcp" {
			out = append(binary.BigEndian.AppendUint16(nil, uint16(len(out))), out...)
		}
		if _, err := c.Write(out); err != nil {
			return
		}
	}
}

// reply 构造 A 应答。
func reply(q *dns.Msg, ips ...string) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(q)
	for _, ip := range ips {
		rr, _ := dns.NewRR(q.Question[0].Name + " 60 IN A " + ip)
		m.Answer = append(m.Answer, rr)
	}
	return m
}

type rig struct {
	net  *fakeWire
	srv  *Server
	cli  *Client
	path string
}

// newRig 起一个服务端和客户端；opts 在服务启动前修改服务端配置（服务运行中不再改，避免数据竞争）。
func newRig(t *testing.T, opts ...func(*Server)) *rig {
	t.Helper()
	fn := newFakeNet(t)
	dir, err := os.MkdirTemp("", "dd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "d.sock")
	me := uint32(os.Getuid())
	srv := &Server{
		Dialer:     fn,
		DNSServers: func() []netip.Addr { return []netip.Addr{dnsAddr} },
		AllowUID:   func(u uint32) bool { return u == me },
	}
	for _, o := range opts {
		o(srv)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return &rig{net: fn, srv: srv, cli: &Client{Path: path, ServerUID: me}, path: path}
}

func (r *rig) dial(t *testing.T, network, host, ip string, port uint16) (net.Conn, error) {
	t.Helper()
	var a netip.Addr
	if ip != "" {
		a = netip.MustParseAddr(ip)
	}
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	return r.cli.Dial(ctx, network, host, a, port)
}

// rawRequest 发一个原始请求（绕过客户端的编码），返回应答码。
func (r *rig) rawRequest(t *testing.T, b []byte) string {
	t.Helper()
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: r.path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if len(b) > fdpass.MaxMsg {
		// 超过上限：手写长度前缀
		_, _ = c.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(b))), b...))
	} else if err := fdpass.Send(c, b, nil); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	msg, f, err := fdpass.Recv(c, 0)
	if f != nil {
		f.Close()
	}
	if err != nil {
		return "（无应答）"
	}
	var rep Reply
	_ = json.Unmarshal(msg, &rep)
	return rep.Code
}

// b. 服务端的拒绝：每条都没有发出任何解析查询和拨号。
func TestServerRejects(t *testing.T) {
	r := newRig(t)
	r.net.setAnswer(func(q *dns.Msg) []*dns.Msg { return []*dns.Msg{reply(q, "93.184.216.34")} })
	raw := map[string]string{
		"超过 1 KiB":   `{"v":1,"net":"tcp","host":"` + strings.Repeat("a", 1100) + `","port":443}`,
		"未知字段":       `{"v":1,"net":"tcp","host":"a.example","port":443,"x":1}`,
		"版本不对":       `{"v":2,"net":"tcp","host":"a.example","port":443}`,
		"网络类型不对":     `{"v":1,"net":"icmp","host":"a.example","port":443}`,
		"域名和 IP 都给":  `{"v":1,"net":"tcp","host":"a.example","ip":"1.2.3.4","port":443}`,
		"都不给":        `{"v":1,"net":"tcp","port":443}`,
		"端口为 0":      `{"v":1,"net":"tcp","host":"a.example","port":0}`,
		"IP 写在 host": `{"v":1,"net":"tcp","host":"1.2.3.4","port":443}`,
		"多余内容":       `{"v":1,"net":"tcp","host":"a.example","port":443}{}`,
		"不是 JSON":    `hello`,
		"域名不合法":      `{"v":1,"net":"tcp","host":"bad_-.x-","port":443}`,
	}
	for name, b := range raw {
		want := CodeBadRequest
		if got := r.rawRequest(t, []byte(b)); got != want && !(name == "超过 1 KiB" && got == "（无应答）") {
			t.Errorf("%s：应答 %q，期望 %q", name, got, want)
		}
	}
	cases := []struct {
		name, host, ip string
		want           error
	}{
		{"置顶域名", "claude.ai", "", ErrPinned},
		{"置顶子域大小写末尾点", "API.Anthropic.COM.", "", ErrPinned},
		{"置顶 claude.com 子域", "x.y.claude.com", "", ErrPinned},
		{"Anthropic 地址段", "", "160.79.104.10", ErrPinned},
		{"Anthropic 段边界内", "", "160.79.111.255", ErrPinned},
		{"回环", "", "127.0.0.1", ErrForbiddenDst},
		{"0/8", "", "0.1.2.3", ErrForbiddenDst},
		{"链路本地", "", "169.254.1.1", ErrForbiddenDst},
		{"组播", "", "224.0.0.251", ErrForbiddenDst},
		{"广播", "", "255.255.255.255", ErrForbiddenDst},
		{"fake 段", "", "198.18.1.7", ErrForbiddenDst},
		{"IPv6", "", "2606:4700::1", ErrForbiddenDst},
		{"IPv4 映射的 Anthropic 地址", "", "::ffff:160.79.104.10", ErrPinned},
	}
	for _, c := range cases {
		_, err := r.dial(t, "tcp", c.host, c.ip, 443)
		if !errors.Is(err, c.want) {
			t.Errorf("%s：%v，期望 %v", c.name, err, c.want)
		}
	}
	if r.net.dialCount() != 0 || r.net.queryCount() != 0 {
		t.Fatalf("拒绝时不应有拨号或解析：拨号 %d，查询 %d", r.net.dialCount(), r.net.queryCount())
	}
	// 配置的本机 DNS 是回环或 fake 段：域名请求回 resolve_failed，不发查询。
	for _, bad := range []string{"127.0.0.1", "198.18.0.2", ""} {
		servers := []netip.Addr{}
		if bad != "" {
			servers = append(servers, netip.MustParseAddr(bad))
		}
		r2 := newRig(t, func(s *Server) { s.DNSServers = func() []netip.Addr { return servers } })
		if _, err := r2.dial(t, "tcp", "a.example", "", 443); !errors.Is(err, ErrResolveFailed) {
			t.Errorf("本机 DNS 是 %q：%v", bad, err)
		}
		if r2.net.dialCount() != 0 || r2.net.queryCount() != 0 {
			t.Fatalf("本机 DNS 不可用时不应有拨号或解析：拨号 %d，查询 %d", r2.net.dialCount(), r2.net.queryCount())
		}
	}
}

func TestPeerUIDRejected(t *testing.T) {
	r := newRig(t, func(s *Server) { s.AllowUID = func(uint32) bool { return false } })
	if _, err := r.dial(t, "tcp", "", "93.184.216.34", 443); err == nil {
		t.Fatal("对端 uid 不符应失败")
	}
	if r.net.dialCount() != 0 || r.srv.Stats()["uid_rejected"] != 1 {
		t.Fatalf("对端 uid 不符时不应拨号：%d %v", r.net.dialCount(), r.srv.Stats())
	}
}

// c. 解析
func TestResolve(t *testing.T) {
	r := newRig(t)
	var mu sync.Mutex
	mode := ""
	r.net.setAnswer(func(q *dns.Msg) []*dns.Msg {
		mu.Lock()
		defer mu.Unlock()
		switch mode {
		case "cname":
			m := reply(q)
			c1, _ := dns.NewRR(q.Question[0].Name + " 60 IN CNAME b.example.")
			c2, _ := dns.NewRR("b.example. 60 IN CNAME c.example.")
			a, _ := dns.NewRR("c.example. 60 IN A 93.184.216.34")
			m.Answer = append(m.Answer, c1, c2, a)
			return []*dns.Msg{m}
		case "badid":
			wrongID := reply(q, "203.0.113.66")
			wrongID.Id = q.Id + 1
			wrongQ := reply(q, "203.0.113.67")
			wrongQ.Question[0].Name = "other.example."
			return []*dns.Msg{wrongID, wrongQ, reply(q, "93.184.216.34")}
		case "trunc":
			m := reply(q)
			m.Truncated = true
			return []*dns.Msg{m}
		case "silent":
			return nil
		case "empty":
			return []*dns.Msg{reply(q)}
		case "many":
			var ips []string
			for i := 1; i <= 20; i++ {
				ips = append(ips, "93.184.216."+string(rune('0'+i%10))+string(rune('0'+i/10)))
			}
			return []*dns.Msg{reply(q, ips...)}
		case "anthropic":
			return []*dns.Msg{reply(q, "93.184.216.34", "160.79.104.10")}
		case "mixed":
			return []*dns.Msg{reply(q, "127.0.0.1", "198.18.3.3", "0.0.0.1", "10.1.2.3")}
		case "allbad":
			return []*dns.Msg{reply(q, "127.0.0.1", "224.0.0.1")}
		}
		return []*dns.Msg{reply(q, "93.184.216.34")}
	})
	set := func(m string) { mu.Lock(); mode = m; mu.Unlock() }
	last := func() string { r.net.mu.Lock(); defer r.net.mu.Unlock(); return r.net.dials[len(r.net.dials)-1] }

	c, err := r.dial(t, "tcp", "www.Example.com", "", 443)
	if err != nil || last() != "tcp 93.184.216.34:443" {
		t.Fatalf("正常的 A：%v", err)
	}
	c.Close()
	set("cname")
	if c, err = r.dial(t, "tcp", "a.example", "", 80); err != nil || last() != "tcp 93.184.216.34:80" {
		t.Fatalf("别名链：%v", err)
	}
	c.Close()
	set("badid")
	if c, err = r.dial(t, "tcp", "a.example", "", 81); err != nil || last() != "tcp 93.184.216.34:81" {
		t.Fatalf("事务号或问题段不符的应答应丢弃：%v %s", err, last())
	}
	c.Close()
	set("trunc")
	r.net.mu.Lock()
	r.net.queries = nil
	r.net.mu.Unlock()
	r.net.setAnswer(func(q *dns.Msg) []*dns.Msg {
		// UDP 回截断，TCP 回正常
		r.net.mu.Lock()
		isTCP := strings.HasPrefix(r.net.queries[len(r.net.queries)-1], "tcp")
		r.net.mu.Unlock()
		if !isTCP {
			m := reply(q)
			m.Truncated = true
			return []*dns.Msg{m}
		}
		return []*dns.Msg{reply(q, "93.184.216.34")}
	})
	if c, err = r.dial(t, "tcp", "a.example", "", 82); err != nil {
		t.Fatalf("截断后改用 TCP：%v", err)
	}
	c.Close()
	r.net.mu.Lock()
	qs := append([]string(nil), r.net.queries...)
	r.net.mu.Unlock()
	if len(qs) != 2 || !strings.HasPrefix(qs[0], "udp") || !strings.HasPrefix(qs[1], "tcp") {
		t.Fatalf("应先 UDP 再 TCP：%v", qs)
	}
}

func TestResolveEdges(t *testing.T) {
	r := newRig(t, func(s *Server) { s.Timeout = 2 * time.Second })
	type step struct {
		ans  func(q *dns.Msg) []*dns.Msg
		want error
		dial int // 期望新增的拨号次数
	}
	steps := map[string]step{
		"超时（没有应答）":    {func(q *dns.Msg) []*dns.Msg { return nil }, ErrTimeout, 0},
		"空应答":         {func(q *dns.Msg) []*dns.Msg { return []*dns.Msg{reply(q)} }, ErrResolveFailed, 0},
		"Anthropic 段": {func(q *dns.Msg) []*dns.Msg { return []*dns.Msg{reply(q, "93.184.216.34", "160.79.104.10")} }, ErrPinned, 0},
		"全被剔掉":        {func(q *dns.Msg) []*dns.Msg { return []*dns.Msg{reply(q, "127.0.0.1", "224.0.0.1", "198.18.0.9")} }, ErrForbiddenDst, 0},
	}
	for name, s := range steps {
		r.net.setAnswer(s.ans)
		before := r.net.dialCount()
		_, err := r.dial(t, "tcp", "a.example", "", 443)
		if !errors.Is(err, s.want) || r.net.dialCount()-before != s.dial {
			t.Errorf("%s：%v（期望 %v），拨号 %d 次", name, err, s.want, r.net.dialCount()-before)
		}
	}
	// 混有被禁止的地址：剔掉，剩下的照常（私有网段放行：公司内网）。
	r.net.setAnswer(func(q *dns.Msg) []*dns.Msg { return []*dns.Msg{reply(q, "127.0.0.1", "198.18.3.3", "10.1.2.3")} })
	c, err := r.dial(t, "tcp", "git.corp.example", "", 22)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	r.net.mu.Lock()
	lastDial := r.net.dials[len(r.net.dials)-1]
	r.net.mu.Unlock()
	if lastDial != "tcp 10.1.2.3:22" {
		t.Fatalf("应只拨剩下的私有地址：%s", lastDial)
	}
	// 超过 16 个地址时只取 16 个。
	var ips []string
	for i := 1; i <= 20; i++ {
		ips = append(ips, netip.AddrFrom4([4]byte{93, 184, 216, byte(i)}).String())
	}
	q := new(dns.Msg)
	q.SetQuestion("a.example.", dns.TypeA)
	got, _, err := answerA(q, reply(q, ips...))
	if err != nil || len(got) != 16 {
		t.Fatalf("应只取 16 个：%d %v", len(got), err)
	}
	// 别名链超过 8 级。
	m := reply(q)
	name := "a.example."
	for i := 0; i < 10; i++ {
		next := "c" + string(rune('a'+i)) + ".example."
		rr, _ := dns.NewRR(name + " 60 IN CNAME " + next)
		m.Answer = append(m.Answer, rr)
		name = next
	}
	rr, _ := dns.NewRR(name + " 60 IN A 93.184.216.34")
	m.Answer = append(m.Answer, rr)
	if _, _, err := answerA(q, m); err == nil {
		t.Fatal("别名链超过 8 级应失败")
	}
}

// 置顶域名永远不会出现在发出的查询里（对假 DNS 收到的全部查询断言）。
func TestPinnedNeverQueried(t *testing.T) {
	r := newRig(t)
	r.net.setAnswer(func(q *dns.Msg) []*dns.Msg { return []*dns.Msg{reply(q, "93.184.216.34")} })
	for _, h := range []string{"claude.ai", "CLAUDE.AI.", "a.claude.com", "console.anthropic.com", "ok.example", "anthropic.com"} {
		c, err := r.dial(t, "tcp", h, "", 443)
		if err == nil {
			c.Close()
		}
	}
	r.net.mu.Lock()
	defer r.net.mu.Unlock()
	for _, q := range r.net.queries {
		l := strings.ToLower(q)
		if strings.Contains(l, "claude") || strings.Contains(l, "anthropic") {
			t.Fatalf("置顶域名出现在了查询里：%s", q)
		}
	}
	if len(r.net.queries) != 1 {
		t.Fatalf("只有 ok.example 应被查询：%v", r.net.queries)
	}
}

// d. 拨号
func TestDial(t *testing.T) {
	r := newRig(t)
	// 成功：拿到的连接能收发（假拨号器连到回显服务）。
	c, err := r.dial(t, "tcp", "", "93.184.216.34", 443)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.(*net.TCPConn); !ok {
		t.Fatalf("应是 TCP 连接：%T", c)
	}
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = c.Write([]byte("hello"))
	buf := make([]byte, 5)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("收发：%q %v", buf, err)
	}
	c.Close()
	// UDP：已连接的 UDP 套接字。
	u, err := r.dial(t, "udp", "", "93.184.216.34", 53)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := u.(*net.UDPConn); !ok {
		t.Fatalf("应是 UDP 连接：%T", u)
	}
	_ = u.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = u.Write([]byte("dg"))
	if n, err := u.Read(buf); err != nil || string(buf[:n]) != "dg" {
		t.Fatalf("UDP 收发：%v", err)
	}
	u.Close()
	// 第一个地址连不上时换下一个，最多 3 个。
	r.net.setAnswer(func(q *dns.Msg) []*dns.Msg {
		return []*dns.Msg{reply(q, "203.0.113.1", "203.0.113.2", "203.0.113.3", "203.0.113.4")}
	})
	for _, a := range []string{"203.0.113.1", "203.0.113.2"} {
		r.net.setFail(a)
	}
	before := r.net.dialCount()
	c, err = r.dial(t, "tcp", "multi.example", "", 443)
	if err != nil || r.net.dialCount()-before != 3 {
		t.Fatalf("第三个地址应成功：%v，拨号 %d 次", err, r.net.dialCount()-before)
	}
	c.Close()
	r.net.setFail("203.0.113.3")
	before = r.net.dialCount()
	if _, err := r.dial(t, "tcp", "multi.example", "", 443); !errors.Is(err, ErrDialFailed) || r.net.dialCount()-before != 3 {
		t.Fatalf("最多试 3 个：%v，拨号 %d 次", err, r.net.dialCount()-before)
	}
	// 整体超时。
	r = newRig(t, func(s *Server) { s.Timeout = 200 * time.Millisecond })
	r.net.setBlock(true)
	start := time.Now()
	if _, err := r.dial(t, "tcp", "", "93.184.216.34", 443); !errors.Is(err, ErrTimeout) || time.Since(start) > 2*time.Second {
		t.Fatalf("整体超时：%v %v", err, time.Since(start))
	}
}

func TestBusyDoesNotQueue(t *testing.T) {
	r := newRig(t, func(s *Server) { s.MaxInflight = 2; s.Timeout = 3 * time.Second })
	r.net.setBlock(true)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = r.dial(t, "tcp", "", "93.184.216.34", 443) }()
	}
	deadline := time.Now().Add(2 * time.Second)
	for r.srv.inflight.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	if _, err := r.dial(t, "tcp", "", "93.184.216.34", 443); !errors.Is(err, ErrBusy) || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("并发超过上限应立即回 busy：%v %v", err, time.Since(start))
	}
	wg.Wait()
}

// e. 客户端
func TestClientChecksServerUID(t *testing.T) {
	dir, _ := os.MkdirTemp("", "dd")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "fake.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var got atomic.Int64
	go func() {
		c, err := ln.AcceptUnix()
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		b, _ := io.ReadAll(c)
		got.Add(int64(len(b)))
	}()
	cli := &Client{Path: path, ServerUID: uint32(os.Getuid()) + 1}
	if _, err := cli.Dial(bg, "tcp", "a.example", netip.Addr{}, 443); !errors.Is(err, ErrWrongServer) {
		t.Fatalf("服务端 uid 不符应报错：%v", err)
	}
	time.Sleep(1200 * time.Millisecond)
	if got.Load() != 0 {
		t.Fatalf("服务端 uid 不符时不应发出请求：收到 %d 字节", got.Load())
	}
}

func TestClientErrorsAndCancel(t *testing.T) {
	r := newRig(t)
	_, err := r.dial(t, "tcp", "claude.ai", "", 443)
	var ce *CodeError
	if !errors.As(err, &ce) || !errors.Is(err, ErrPinned) || errors.Is(err, ErrForbiddenDst) {
		t.Fatalf("pinned 应能被单独识别：%v", err)
	}
	// 各应答码可区分。
	seen := map[string]bool{}
	for _, e := range []*CodeError{ErrBadRequest, ErrPinned, ErrForbiddenDst, ErrResolveFailed, ErrDialFailed, ErrBusy, ErrTimeout} {
		if seen[e.Code] {
			t.Fatal("应答码重复")
		}
		seen[e.Code] = true
		for _, o := range []*CodeError{ErrBadRequest, ErrPinned, ErrForbiddenDst, ErrResolveFailed, ErrDialFailed, ErrBusy, ErrTimeout} {
			if errors.Is(e, o) != (e == o) {
				t.Fatalf("%s 与 %s 的比较不对", e.Code, o.Code)
			}
		}
	}
	// ctx 取消：及时返回。
	r = newRig(t, func(s *Server) { s.Timeout = 5 * time.Second })
	r.net.setBlock(true)
	ctx, cancel := context.WithCancel(bg)
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err = r.cli.Dial(ctx, "tcp", "", netip.MustParseAddr("93.184.216.34"), 443)
	if !errors.Is(err, context.Canceled) || time.Since(start) > time.Second {
		t.Fatalf("ctx 取消应及时返回：%v %v", err, time.Since(start))
	}
}

func TestStatsHaveNoTargets(t *testing.T) {
	r := newRig(t)
	_, _ = r.dial(t, "tcp", "claude.ai", "", 443)
	c, err := r.dial(t, "tcp", "", "93.184.216.34", 443)
	if err == nil {
		c.Close()
	}
	st := r.srv.Stats()
	if st["pinned"] != 1 || st["ok"] != 1 {
		t.Fatalf("%v", st)
	}
	b, _ := json.Marshal(st)
	if bytes.Contains(b, []byte("claude")) || bytes.Contains(b, []byte("93.184")) {
		t.Fatalf("计数里不应有目标：%s", b)
	}
}

// 回归（M3-4 测试发现）：服务端发出 fd 后若立即放掉自己的副本，内核回收在途 fd 时偶尔会冲掉这个套接字，
// 客户端随后读到 EOF（实测 300 次里失败 6–66 次）。现在服务端持有副本直到客户端确认收下。
func TestManyDialsEcho(t *testing.T) {
	r := newRig(t)
	fails := 0
	for i := 0; i < 300; i++ {
		c, err := r.dial(t, "tcp", "", "93.184.216.34", 443)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		_, _ = c.Write([]byte("abc"))
		b := make([]byte, 3)
		if _, err := io.ReadFull(c, b); err != nil || string(b) != "abc" {
			fails++
		}
		c.Close()
	}
	if fails > 0 {
		t.Fatalf("300 次里有 %d 次经直连拿到的连接读不到回显", fails)
	}
}

// M3-5 第 0 步（architect 审 M3-4 的建议改 1）：别名链上的名字也判置顶。
func TestCNAMEToPinned(t *testing.T) {
	r := newRig(t)
	r.net.setAnswer(func(q *dns.Msg) []*dns.Msg {
		m := reply(q)
		c1, _ := dns.NewRR(q.Question[0].Name + " 60 IN CNAME edge.example.")
		c2, _ := dns.NewRR("edge.example. 60 IN CNAME x.Claude.AI.")
		a, _ := dns.NewRR("x.Claude.AI. 60 IN A 93.184.216.34") // 普通公网地址，不在 Anthropic 段
		m.Answer = append(m.Answer, c1, c2, a)
		return []*dns.Msg{m}
	})
	if _, err := r.dial(t, "tcp", "innocent.example", "", 443); !errors.Is(err, ErrPinned) {
		t.Fatalf("别名指向置顶域名应回 pinned：%v", err)
	}
	if r.net.dialCount() != 0 {
		t.Fatalf("不应拨号：%d", r.net.dialCount())
	}
	// 别名链上的名字不合法：按置顶处理（说不清）。
	r.net.setAnswer(func(q *dns.Msg) []*dns.Msg {
		m := reply(q)
		c1 := &dns.CNAME{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 60}, Target: "bad_-.x-."}
		a := &dns.A{Hdr: dns.RR_Header{Name: "bad_-.x-.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.ParseIP("93.184.216.34")}
		m.Answer = append(m.Answer, c1, a)
		return []*dns.Msg{m}
	})
	if _, err := r.dial(t, "tcp", "odd.example", "", 443); !errors.Is(err, ErrPinned) || r.net.dialCount() != 0 {
		t.Fatalf("别名链上名字不合法时应拒绝：%v，拨号 %d", err, r.net.dialCount())
	}
}
