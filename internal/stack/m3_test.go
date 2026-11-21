package stack

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"claude-abroad/internal/rules"
	"claude-abroad/internal/wire"
)

func headersFor(e *m3Env, exit string) []wire.Header {
	var out []wire.Header
	for _, h := range e.op.all() {
		if h.Exit == exit {
			out = append(out, h)
		}
	}
	return out
}

// b. 三个应用同时运行：A 走出口 a，B 走出口 b，C 直连；各 50 条连接不串。
func TestThreeAppsConcurrently(t *testing.T) {
	e := newM3Env(t, m3Rules)
	dst := ap(e.lookupA(t, "www.example.org"), 443)
	var wg sync.WaitGroup
	errs := make(chan error, 150)
	for i := 0; i < 50; i++ {
		for _, base := range []uint16{portA, portB, portC} {
			wg.Add(1)
			go func(port uint16) {
				defer wg.Done()
				c, err := e.cl.dialFrom(port, dst, ioWait)
				if err != nil {
					errs <- err
					return
				}
				defer c.Close()
				if err := echoOnce(c, []byte(fmt.Sprintf("GET / HTTP/1.1\r\nHost: www.example.org\r\nX: %d\r\n\r\n", port))); err != nil {
					errs <- fmt.Errorf("端口 %d：%w", port, err)
				}
			}(base + uint16(i))
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	a, b := headersFor(e, "wg-a"), headersFor(e, "clash-b")
	if len(a) != 50 || len(b) != 50 || len(e.op.all()) != 100 {
		t.Fatalf("出口 a %d 条、出口 b %d 条、开流共 %d 条", len(a), len(b), len(e.op.all()))
	}
	for _, h := range a {
		if h.App != "com.app.a" || h.Host != "www.example.org" {
			t.Fatalf("出口 a 的帧头不对：%+v", h)
		}
	}
	for _, h := range b {
		if h.App != "com.app.b" {
			t.Fatalf("出口 b 的帧头不对：%+v", h)
		}
	}
	d := e.direct.list()
	if len(d) != 50 {
		t.Fatalf("直连应有 50 条：%d", len(d))
	}
	for _, c := range d {
		if c != "tcp www.example.org:443" {
			t.Fatalf("直连请求不对：%s", c)
		}
	}
}

// c. 设成直连的应用：这些去向都经 main 到达，直连拨号器的调用数为 0（最后一项除外）。
func TestDirectAppTightened(t *testing.T) {
	e := newM3Env(t, m3Rules)
	pinnedFake := ap(e.lookupA(t, "claude.ai"), 443)
	plainFake := ap(e.lookupA(t, "plain.example.org"), 443)
	realIP := netip.MustParseAddrPort("203.0.113.9:443")
	sniPinned := tlsHello(t, &tls.Config{ServerName: "claude.ai"})
	ech := tlsHello(t, &tls.Config{ServerName: "secret.example", MinVersion: tls.VersionTLS13, EncryptedClientHelloConfigList: echConfigList(t)})
	noSNI := tlsHello(t, &tls.Config{InsecureSkipVerify: true})
	cases := []struct {
		name   string
		port   uint16
		dst    netip.AddrPort
		data   []byte // nil：客户端不先发数据
		reason string
	}{
		{"置顶域名的假地址", portC + 1, pinnedFake, []byte("x"), "pinned"},
		{"真实 IP 加置顶 SNI", portC + 2, realIP, sniPinned, "pinned"},
		{"真实 IP 加 ECH", portC + 3, realIP, ech, "opaque_tls"},
		{"真实 IP 加无 SNI", portC + 4, realIP, noSNI, "opaque_tls"},
		{"Anthropic 地址段", portC + 5, netip.MustParseAddrPort("160.79.104.10:8080"), []byte("own-protocol"), "anthropic_net"},
		{"443 且客户端不先发", portC + 6, realIP, nil, "port443_timeout"},
		{"Claude 进程访问普通域名", portClaude + 1, plainFake, []byte("x"), "claude_process"},
	}
	for _, c := range cases {
		before := len(e.op.all())
		r0 := e.p.Stats().Reasons[c.reason]
		conn, err := e.cl.dialFrom(c.port, c.dst, ioWait)
		if err != nil {
			t.Fatalf("%s：%v", c.name, err)
		}
		if c.data != nil {
			if err := echoOnce(conn, c.data); err != nil {
				t.Fatalf("%s：经 main 应能收发：%v", c.name, err)
			}
		} else {
			waitFor(t, c.name+"：首包超时后开流", func() bool { return len(e.op.all()) > before })
			if err := echoOnce(conn, []byte("late")); err != nil {
				t.Fatalf("%s：%v", c.name, err)
			}
		}
		conn.Close()
		hs := e.op.all()
		if len(hs) != before+1 || hs[len(hs)-1].Exit != "main" {
			t.Fatalf("%s：应经 main 开一条流：%+v", c.name, hs[before:])
		}
		if e.p.Stats().Reasons[c.reason] != r0+1 {
			t.Fatalf("%s：Reason %s 的计数应加一：%v", c.name, c.reason, e.p.Stats().Reasons)
		}
	}
	if n := len(e.direct.list()); n != 0 {
		t.Fatalf("这些去向都不应尝试直连：%v", e.direct.list())
	}
	// 普通域名的假地址，但直连拨号器回 pinned：直连恰好 1 次，随后经 main 到达，没有第二次直连。
	e.direct.mu.Lock()
	e.direct.pinned["plain.example.org"] = true
	e.direct.mu.Unlock()
	before := len(e.op.all())
	conn, err := e.cl.dialFrom(portC+7, plainFake, ioWait)
	if err != nil {
		t.Fatal(err)
	}
	if err := echoOnce(conn, []byte("hello")); err != nil {
		t.Fatalf("pinned 后应经 main 到达：%v", err)
	}
	conn.Close()
	if d := e.direct.list(); len(d) != 1 || d[0] != "tcp plain.example.org:443" {
		t.Fatalf("直连应恰好 1 次：%v", d)
	}
	if hs := e.op.all(); len(hs) != before+1 || hs[before].Exit != "main" {
		t.Fatalf("pinned 后应经 main：%+v", hs[before:])
	}
	if e.p.Stats().Reasons["pinned_resolved"] != 1 || e.p.Stats().DirectFail["pinned"] != 1 {
		t.Fatalf("计数：%+v", e.p.Stats())
	}
}

// d. 设成直连的应用确实直连；首包里读到的字节原样送到对端。
func TestDirectAppDirect(t *testing.T) {
	e := newM3Env(t, m3Rules)
	plainFake := ap(e.lookupA(t, "plain.example.org"), 443)
	localFake := ap(e.lookupA(t, "git.corp.example"), 22)
	realIP := netip.MustParseAddrPort("203.0.113.9:443")
	sni := tlsHello(t, &tls.Config{ServerName: "www.example.com"})
	cases := []struct {
		name string
		port uint16
		dst  netip.AddrPort
		data []byte
		want string
	}{
		{"普通域名的假地址", portC + 1, plainFake, []byte("x"), "tcp plain.example.org:443"},
		{"真实 IP 加普通 SNI", portC + 2, realIP, sni, "tcp 203.0.113.9:443"},
		{"真实 IP 加自有协议", portC + 3, netip.MustParseAddrPort("203.0.113.9:5222"), []byte("\x00\x00\x00\x10wechat-like"), "tcp 203.0.113.9:5222"},
		{"本地域名（规则是走代理）", portA + 1, localFake, []byte("SSH-2.0-test\r\n"), "tcp git.corp.example:22"},
	}
	for _, c := range cases {
		before := len(e.direct.list())
		conn, err := e.cl.dialFrom(c.port, c.dst, ioWait)
		if err != nil {
			t.Fatal(err)
		}
		if err := echoOnce(conn, c.data); err != nil { // 回显完整：首包字节不丢不重
			t.Fatalf("%s：%v", c.name, err)
		}
		conn.Close()
		d := e.direct.list()
		if len(d) != before+1 || d[before] != c.want {
			t.Fatalf("%s：直连请求 %v，期望 %s", c.name, d[before:], c.want)
		}
	}
	if n := len(e.op.all()); n != 0 {
		t.Fatalf("这些都不应开代理流：%d", n)
	}
	if st := e.p.Stats(); st.DirectOK != 4 || st.Reasons["local_domain"] != 1 || st.Reasons["rule"] != 3 || st.Sniff["tls_name"] != 1 || st.Sniff["not_tls"] != 1 {
		t.Fatalf("计数：%+v", st)
	}
}

// e. 帧头：真实 IP 加任意 SNI，出口看到的目的是那个 IP。
func TestHeaderIgnoresSNI(t *testing.T) {
	e := newM3Env(t, m3Rules)
	conn, err := e.cl.dialFrom(portA+1, netip.MustParseAddrPort("203.0.113.77:443"), ioWait)
	if err != nil {
		t.Fatal(err)
	}
	// 走代理的应用不读首包；即使发了带名字的 ClientHello，帧头也只填 IP。
	if err := echoOnce(conn, tlsHello(t, &tls.Config{ServerName: "evil.example"})); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if h := e.op.last(); h.IP != "203.0.113.77" || h.Host != "" || h.Exit != "wg-a" {
		t.Fatalf("帧头不对：%+v", h)
	}
	// 直连应用被收紧到 main 的那条（置顶 SNI）也一样。
	conn, err = e.cl.dialFrom(portC+1, netip.MustParseAddrPort("203.0.113.78:443"), ioWait)
	if err != nil {
		t.Fatal(err)
	}
	if err := echoOnce(conn, tlsHello(t, &tls.Config{ServerName: "api.anthropic.com"})); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if h := e.op.last(); h.IP != "203.0.113.78" || h.Host != "" || h.Exit != "main" {
		t.Fatalf("帧头不应带 SNI：%+v", h)
	}
}

// f. 闸门关闭：不可能直连时 SYN 就拒；可能直连时走代理的被接受后重置，直连照常；期间开流 0 次。
func TestGateClosedModes(t *testing.T) {
	e := newM3Env(t, "rules: [{app: com.app.a, action: \"proxy:wg-a\"}]\n")
	dst := ap(e.lookupA(t, "www.example.org"), 443)
	e.gate.open.Store(false)
	if _, err := e.cl.dialFrom(portA+1, dst, ioWait); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("没有直连可能时应在 SYN 被拒：%v", err)
	}
	e.setRules(t, m3Rules, e.p.Pool)
	// 可能直连：先接受（握手完成），判定为拦截后 RST。客户端可能在 connect 返回前就收到 RST，
	// 所以 reset 出现在 connect 或之后的读都算；与 SYN 时就拒（refused）区分开。
	conn, err := e.cl.dialFrom(portA+2, dst, ioWait)
	if err == nil {
		_ = conn.SetDeadline(time.Now().Add(ioWait))
		_, _ = conn.Write([]byte("x"))
		buf := make([]byte, 1)
		_, err = conn.Read(buf)
		conn.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "reset") {
		t.Fatalf("走代理的应在接受后被重置（不是 SYN 时被拒）：%v", err)
	}
	c2, err := e.cl.dialFrom(portC+1, dst, ioWait)
	if err != nil {
		t.Fatal(err)
	}
	if err := echoOnce(c2, []byte("direct")); err != nil {
		t.Fatalf("闸门关闭时直连照常：%v", err)
	}
	c2.Close()
	if n := len(e.op.all()); n != 0 {
		t.Fatalf("闸门关闭期间不应开流：%d", n)
	}
	if e.p.Stats().Reasons["gate_closed"] < 1 || e.p.Stats().GateClosed < 1 {
		t.Fatalf("计数：%+v", e.p.Stats())
	}
}

// i. 没有应用规则、offline_allow 和 local_domains 都为空时不调用识别器；加一条规则后调用。
func TestIdentOnlyWhenNeeded(t *testing.T) {
	e := newM3Env(t, "")
	dst := ap(e.lookupA(t, "www.example.org"), 443)
	for i := 0; i < 5; i++ {
		c, err := e.cl.dialFrom(portC+uint16(i), dst, ioWait)
		if err != nil {
			t.Fatal(err)
		}
		_ = echoOnce(c, []byte("x"))
		c.Close()
	}
	if n := e.ident.calls.Load(); n != 0 {
		t.Fatalf("配置用不到应用时不应识别：%d", n)
	}
	e.setRules(t, "rules: [{app: com.app.a, action: block}]\n", e.p.Pool)
	c, err := e.cl.dialFrom(portC+10, dst, ioWait)
	if err != nil {
		t.Fatal(err)
	}
	_ = echoOnce(c, []byte("x"))
	c.Close()
	if e.ident.calls.Load() == 0 {
		t.Fatal("有规则时应识别")
	}
}

// k. 计数里搜不到测试用的域名、地址、应用 ID。
func TestStatsHaveNoSecrets(t *testing.T) {
	e := newM3Env(t, m3Rules)
	for _, d := range []struct {
		port uint16
		dst  netip.AddrPort
	}{{portA + 1, ap(e.lookupA(t, "secret-a.example.org"), 443)}, {portC + 1, ap(e.lookupA(t, "secret-c.example.org"), 443)},
		{portC + 2, ap(e.lookupA(t, "claude.ai"), 443)}, {portC + 3, netip.MustParseAddrPort("203.0.113.55:5222")}} {
		c, err := e.cl.dialFrom(d.port, d.dst, ioWait)
		if err == nil {
			_ = echoOnce(c, []byte("x"))
			c.Close()
		}
	}
	b, _ := json.Marshal(e.p.Stats())
	for _, s := range []string{"secret", "claude.ai", "203.0.113", "com.app", "example"} {
		if strings.Contains(string(b), s) {
			t.Fatalf("计数里出现了 %q：%s", s, b)
		}
	}
	if !strings.Contains(string(b), `"reasons"`) || e.p.Stats().Reasons[string(rules.RRule)] == 0 {
		t.Fatalf("应有按 Reason 的计数：%s", b)
	}
}
