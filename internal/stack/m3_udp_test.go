package stack

import (
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"claude-abroad/internal/wire"
)

// udpEcho 发一个数据报并等回显（期望收到时等 ioWait；udpNoEcho 期望收不到，只等 300 毫秒）。
func udpEcho(t *testing.T, e *m3Env, port uint16, dst netip.AddrPort, data []byte) bool {
	t.Helper()
	return udpEchoWait(t, e, port, dst, data, ioWait)
}

func udpNoEcho(t *testing.T, e *m3Env, port uint16, dst netip.AddrPort, data []byte) bool {
	t.Helper()
	return !udpEchoWait(t, e, port, dst, data, 300*time.Millisecond)
}

func udpEchoWait(t *testing.T, e *m3Env, port uint16, dst netip.AddrPort, data []byte, wait time.Duration) bool {
	t.Helper()
	u := e.cl.udpFrom(t, port, dst)
	if _, err := u.Write(data); err != nil {
		t.Fatal(err)
	}
	_ = u.SetReadDeadline(time.Now().Add(wait))
	buf := make([]byte, 2048)
	n, err := u.Read(buf)
	return err == nil && string(buf[:n]) == string(data)
}

// g. UDP：直连和走代理各一条流的回显。
func TestUDPDirectAndProxy(t *testing.T) {
	e := newM3Env(t, m3Rules)
	dst := netip.MustParseAddrPort("203.0.113.9:5000")
	if !udpEcho(t, e, portC+1, dst, []byte("direct-dg")) {
		t.Fatal("直连的 UDP 回显失败")
	}
	if d := e.direct.list(); len(d) != 1 || d[0] != "udp 203.0.113.9:5000" {
		t.Fatalf("直连请求：%v", d)
	}
	if !udpEcho(t, e, portA+1, dst, []byte("proxy-dg")) {
		t.Fatal("走代理的 UDP 回显失败")
	}
	if h := e.op.last(); h.Exit != "wg-a" || h.Net != wire.UDP || h.IP != "203.0.113.9" {
		t.Fatalf("帧头：%+v", h)
	}
}

// g. 负缓存：出口 a 回“不支持 UDP”后 60 秒内新流改走 main；main 也不支持时拦截；60 秒后重新尝试出口 a。
func TestUDPNegativeCache(t *testing.T) {
	e := newM3Env(t, m3Rules)
	var mainUnsupported atomic.Bool
	e.op.errFor = func(h wire.Header) error {
		if h.Net == wire.UDP && (h.Exit == "wg-a" || h.Exit == "main" && mainUnsupported.Load()) {
			return &wire.ReplyError{Code: wire.CodeUDPUnsupported}
		}
		return nil
	}
	dst := netip.MustParseAddrPort("203.0.113.9:5000")
	if !udpNoEcho(t, e, portA+1, dst, []byte("1")) {
		t.Fatal("出口 a 不支持 UDP，第一条流应失败")
	}
	waitFor(t, "尝试出口 a", func() bool { return len(e.op.all()) == 1 })
	if h := e.op.last(); h.Exit != "wg-a" {
		t.Fatalf("第一条应尝试出口 a：%+v", h)
	}
	if !udpEcho(t, e, portA+2, dst, []byte("2")) {
		t.Fatal("负缓存期间新流应改走 main")
	}
	if h := e.op.last(); h.Exit != "main" {
		t.Fatalf("应改走 main：%+v", h)
	}
	if e.p.Stats().Reasons["udp_fallback"] != 1 {
		t.Fatalf("计数：%v", e.p.Stats().Reasons)
	}
	// main 也不支持：先让一条 main 的流失败一次（进负缓存），之后的新流直接拦截、不开流。
	mainUnsupported.Store(true)
	e.clock.add(61 * time.Second)                                                        // 出口 a 的负缓存过期
	_ = udpEcho(t, e, portB+1, dst, []byte("3"))                                         // 出口 b 没被负缓存：尝试 clash-b（假 Opener 支持）
	_ = udpEcho(t, e, portC+50, netip.MustParseAddrPort("203.0.113.9:443"), []byte("x")) // 无关
	n0 := len(e.op.all())
	_ = udpEcho(t, e, portOther+1, dst, []byte("4")) // default 走 main：失败并进负缓存
	if h := e.op.last(); h.Exit != "main" || len(e.op.all()) != n0+1 {
		t.Fatalf("应尝试 main 一次：%+v", h)
	}
	n1 := len(e.op.all())
	if udpEcho(t, e, portOther+2, dst, []byte("5")) || len(e.op.all()) != n1 {
		t.Fatal("main 不支持 UDP 时新流应拦截、不开流")
	}
	if e.p.Stats().Reasons["udp_unsupported"] < 1 {
		t.Fatalf("计数：%v", e.p.Stats().Reasons)
	}
	// 60 秒后重新尝试出口 a（它的负缓存已过期；main 的负缓存也在 60 秒后过期）。
	mainUnsupported.Store(false)
	e.clock.add(61 * time.Second)
	n2 := len(e.op.all())
	_ = udpEcho(t, e, portA+3, dst, []byte("6"))
	waitFor(t, "重新尝试出口 a", func() bool { return len(e.op.all()) == n2+1 })
	hs := e.op.all()
	if len(hs) != n2+1 || hs[n2].Exit != "wg-a" {
		t.Fatalf("60 秒后应重新尝试出口 a：%+v", hs[n2:])
	}
}

// g. “拦截 UDP 443”对直连和走代理都生效。
func TestBlockUDP443(t *testing.T) {
	e := newM3Env(t, m3Rules+"block_udp_443: true\n")
	dst := netip.MustParseAddrPort("203.0.113.9:443")
	if !udpNoEcho(t, e, portC+1, dst, []byte("q1")) || !udpNoEcho(t, e, portA+1, dst, []byte("q2")) {
		t.Fatal("UDP 443 应拦截")
	}
	if len(e.direct.list()) != 0 || len(e.op.all()) != 0 {
		t.Fatalf("不应直连或开流：%v %d", e.direct.list(), len(e.op.all()))
	}
	if e.p.Stats().Reasons["block_udp443"] != 2 {
		t.Fatalf("计数：%v", e.p.Stats().Reasons)
	}
	if !udpEcho(t, e, portC+2, netip.MustParseAddrPort("203.0.113.9:444"), []byte("ok")) {
		t.Fatal("别的端口不受影响")
	}
}

// g. 判定期间后续数据报的缓存上限：识别慢的时候连发 20 个，只缓存 8 个，其余丢弃。
func TestUDPPendingLimit(t *testing.T) {
	e := newM3Env(t, m3Rules)
	e.ident.delay = 300 * time.Millisecond
	u := e.cl.udpFrom(t, portC+1, netip.MustParseAddrPort("203.0.113.9:5000"))
	for i := 0; i < 21; i++ {
		if _, err := u.Write([]byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	got := 0
	buf := make([]byte, 16)
	for {
		wait := 800 * time.Millisecond
		if got == 0 { // 第一个回显要等识别（故意慢 300 毫秒）和判定做完
			wait = ioWait
		}
		_ = u.SetReadDeadline(time.Now().Add(wait))
		if _, err := u.Read(buf); err != nil {
			break
		}
		got++
	}
	if got != 1+udpPendingMax {
		t.Fatalf("应回显第一个加缓存的 %d 个，实际 %d；直连 %v；计数 %+v；栈 %+v", udpPendingMax, got, e.direct.list(), e.p.Stats(), e.s.Stats())
	}
	if d := e.p.Stats().UDPPendingDropped; d != 20-udpPendingMax {
		t.Fatalf("丢弃数应为 %d：%d", 20-udpPendingMax, d)
	}
}

// g. QUIC 首包带置顶 SNI 的直连应用走 main。
func TestQUICPinnedSNI(t *testing.T) {
	e := newM3Env(t, m3Rules)
	dst := netip.MustParseAddrPort("203.0.113.9:443")
	if !udpEcho(t, e, portC+1, dst, quicInitial(t, "claude.ai")) {
		t.Fatal("应经 main 回显")
	}
	if len(e.direct.list()) != 0 {
		t.Fatalf("不应直连：%v", e.direct.list())
	}
	if h := e.op.last(); h.Exit != "main" || h.Net != wire.UDP {
		t.Fatalf("帧头：%+v", h)
	}
	if e.p.Stats().Sniff["quic_name"] != 1 || e.p.Stats().Reasons["pinned"] != 1 {
		t.Fatalf("计数：%+v", e.p.Stats())
	}
	// 普通 SNI 的 QUIC：直连。
	if !udpEcho(t, e, portC+2, dst, quicInitial(t, "www.example.com")) || len(e.direct.list()) != 1 {
		t.Fatalf("普通 SNI 的 QUIC 应直连：%v", e.direct.list())
	}
}
