package stack

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	gstack "gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"

	"claude-abroad/internal/tun"
)

var clientIP = netip.MustParseAddr("198.18.0.1") // 相当于 utun 的地址（本机应用的源地址）

// ioWait 是正向等待（等包到、等计数）的上限。-race 下 GC 的并发标记阶段会拖几秒墙钟时间，
// 期间 goroutine 唤醒很慢（不开 -race 时没有这个现象），所以留足余量；负向检查一律先等计数到位。
const ioWait = 10 * time.Second

// client 是挂在 Pipe 另一端的 gVisor 栈，模拟本机应用。
type client struct {
	s    *gstack.Stack
	link *channel.Endpoint
	dev  tun.Device
	stop context.CancelFunc
	wg   sync.WaitGroup
}

func newClient(t *testing.T, dev tun.Device) *client {
	t.Helper()
	c := newClientRaw(t, dev)
	t.Cleanup(c.close)
	return c
}

func (c *client) close() {
	c.stop()
	c.dev.Close()
	c.wg.Wait()
	c.s.Close()
	c.s.Wait()
}

func full(ap netip.AddrPort) tcpip.FullAddress {
	return tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(ap.Addr().As4()), Port: ap.Port()}
}

func (c *client) dialTCP(dst netip.AddrPort, timeout time.Duration) (*gonet.TCPConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return gonet.DialContextTCP(ctx, c.s, full(dst), ipv4.ProtocolNumber)
}

func (c *client) udp(t *testing.T) *gonet.UDPConn {
	t.Helper()
	laddr := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(clientIP.As4())}
	u, err := gonet.DialUDP(c.s, &laddr, nil, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { u.Close() })
	return u
}

// fakeHandler 是测试用的 Handler。
type fakeHandler struct {
	acceptTCP func(Flow) bool
	acceptUDP func(Flow) bool
	tcp       func(context.Context, Flow, net.Conn)
	udp       func(context.Context, Flow, net.PacketConn)

	mu       sync.Mutex
	tcpFlows []Flow
	udpFlows []Flow
	tcpCalls atomic.Int32
	udpCalls atomic.Int32
}

func (h *fakeHandler) AcceptTCP(f Flow) bool {
	if h.acceptTCP != nil {
		return h.acceptTCP(f)
	}
	return true
}

func (h *fakeHandler) AcceptUDP(f Flow) bool {
	if h.acceptUDP != nil {
		return h.acceptUDP(f)
	}
	return true
}

func (h *fakeHandler) TCP(ctx context.Context, f Flow, c net.Conn) {
	h.tcpCalls.Add(1)
	h.mu.Lock()
	h.tcpFlows = append(h.tcpFlows, f)
	h.mu.Unlock()
	if h.tcp != nil {
		h.tcp(ctx, f, c)
		return
	}
	io.Copy(c, c) // 回显，读到 EOF 后返回
}

func (h *fakeHandler) UDP(ctx context.Context, f Flow, c net.PacketConn) {
	h.udpCalls.Add(1)
	h.mu.Lock()
	h.udpFlows = append(h.udpFlows, f)
	h.mu.Unlock()
	if h.udp != nil {
		h.udp(ctx, f, c)
		return
	}
	buf := make([]byte, 2000)
	for { // 回显，直到空闲超时或关闭
		n, from, err := c.ReadFrom(buf)
		if err != nil {
			return
		}
		c.WriteTo(buf[:n], from)
	}
}

func setup(t *testing.T, h Handler, o Options) (*Stack, *client) {
	t.Helper()
	a, b := tun.Pipe(1500)
	s, err := New(a, h, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, newClient(t, b)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(ioWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等待超时：%s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// c. TCP：Flow、双向数据、半关闭
func TestTCPFlowDataAndHalfClose(t *testing.T) {
	h := &fakeHandler{tcp: func(_ context.Context, _ Flow, c net.Conn) {
		data, _ := io.ReadAll(c) // 读到对方半关闭
		c.Write(append([]byte("got:"), data...))
	}}
	_, cl := setup(t, h, Options{})
	dst := netip.MustParseAddrPort("203.0.113.5:443")
	c, err := cl.dialTCP(dst, ioWait)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("hello"))
	c.CloseWrite()
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "got:hello" {
		t.Fatalf("半关闭后应仍能收到回写：%q %v", got, err)
	}
	h.mu.Lock()
	f := h.tcpFlows[0]
	h.mu.Unlock()
	local := c.LocalAddr().(*net.TCPAddr).AddrPort()
	if f.Dst != dst || f.Src.Addr() != clientIP || f.Src.Port() != local.Port() {
		t.Fatalf("Flow 不对：%+v，客户端本地 %s", f, local)
	}
	// 回显：大块数据双向
	h.tcp = nil
	c2, err := cl.dialTCP(netip.MustParseAddrPort("203.0.113.6:80"), ioWait)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	payload := bytes.Repeat([]byte("abcdefgh"), 20000)
	go c2.Write(payload)
	back := make([]byte, len(payload))
	if _, err := io.ReadFull(c2, back); err != nil || !bytes.Equal(back, payload) {
		t.Fatalf("双向数据不对：%v", err)
	}
}

// d. AcceptTCP 为 false：客户端收到 RST，TCP 不被调用
func TestTCPRejectSendsRST(t *testing.T) {
	h := &fakeHandler{acceptTCP: func(Flow) bool { return false }}
	s, cl := setup(t, h, Options{})
	_, err := cl.dialTCP(netip.MustParseAddrPort("203.0.113.5:443"), ioWait)
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("应 connection refused（RST）：%v", err)
	}
	if h.tcpCalls.Load() != 0 || s.Stats().TCPRejected != 1 || s.Stats().TCPActive != 0 {
		t.Fatalf("TCP 不应被调用，名额应归还：%d %+v", h.tcpCalls.Load(), s.Stats())
	}
}

// 必须改 1：TCPInFlight=4 时顺序建立 25 条（夹着被拒的）全部符合预期。
func TestTCPInFlightCompleteEveryPath(t *testing.T) {
	h := &fakeHandler{acceptTCP: func(f Flow) bool { return f.Dst.Port()%5 != 0 }}
	s, cl := setup(t, h, Options{TCPInFlight: 4})
	for i := 1; i <= 25; i++ {
		dst := netip.AddrPortFrom(netip.MustParseAddr("203.0.113.5"), uint16(1000+i))
		c, err := cl.dialTCP(dst, ioWait)
		if dst.Port()%5 == 0 {
			if err == nil {
				t.Fatalf("第 %d 条应被拒", i)
			}
			continue
		}
		if err != nil {
			t.Fatalf("第 %d 条应建立（inFlight 表项没清会导致后面全部失败）：%v", i, err)
		}
		c.Write([]byte("x"))
		buf := make([]byte, 1)
		if _, err := io.ReadFull(c, buf); err != nil {
			t.Fatalf("第 %d 条回显失败：%v", i, err)
		}
		c.Close()
	}
	if d := s.Stats().TCPInFlightDrop; d != 0 {
		t.Fatalf("不应有 SYN 因为 inFlight 超限被丢：%d", d)
	}
}

// 必须改 1：并发 2×MaxTCP 条拨号，tcp_active 峰值不超过 MaxTCP。
func TestTCPConcurrentLimit(t *testing.T) {
	const max = 8
	var peak atomic.Int64
	var s *Stack
	h := &fakeHandler{tcp: func(_ context.Context, _ Flow, c net.Conn) {
		if a := s.Stats().TCPActive; a > peak.Load() {
			peak.Store(a)
		}
		time.Sleep(100 * time.Millisecond)
	}}
	s, cl := setup(t, h, Options{MaxTCP: max})
	var wg sync.WaitGroup
	var ok, refused atomic.Int32
	for i := range 2 * max {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := cl.dialTCP(netip.AddrPortFrom(netip.MustParseAddr("203.0.113.9"), uint16(2000+i)), ioWait)
			if err != nil {
				refused.Add(1)
				return
			}
			ok.Add(1)
			io.Copy(io.Discard, c)
			c.Close()
		}()
	}
	wg.Wait()
	if peak.Load() > max || ok.Load() < 1 || ok.Load()+refused.Load() != 2*max {
		t.Fatalf("峰值 %d 应不超过 %d；成功 %d、被拒 %d", peak.Load(), max, ok.Load(), refused.Load())
	}
	waitFor(t, "名额全部归还", func() bool { return s.Stats().TCPActive == 0 })
}

// g. 流数上限：达到上限后新连接 RST，已有的不受影响，释放后可以再建
func TestTCPLimitAndRelease(t *testing.T) {
	h := &fakeHandler{tcp: func(ctx context.Context, _ Flow, c net.Conn) {
		buf := make([]byte, 10)
		for {
			n, err := c.Read(buf)
			if err != nil {
				return
			}
			c.Write(buf[:n])
		}
	}}
	s, cl := setup(t, h, Options{MaxTCP: 2})
	c1, err1 := cl.dialTCP(netip.MustParseAddrPort("203.0.113.1:1"), ioWait)
	c2, err2 := cl.dialTCP(netip.MustParseAddrPort("203.0.113.1:2"), ioWait)
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	waitFor(t, "两条都建立", func() bool { return s.Stats().TCPActive == 2 })
	if _, err := cl.dialTCP(netip.MustParseAddrPort("203.0.113.1:3"), ioWait); err == nil {
		t.Fatal("达到上限后新连接应被拒")
	}
	c1.Write([]byte("a"))
	b := make([]byte, 1)
	if _, err := io.ReadFull(c1, b); err != nil {
		t.Fatal("已有连接不应受影响")
	}
	c2.Close()
	waitFor(t, "释放名额", func() bool { return s.Stats().TCPActive == 1 })
	c3, err := cl.dialTCP(netip.MustParseAddrPort("203.0.113.1:4"), ioWait)
	if err != nil {
		t.Fatalf("释放后应能再建：%v", err)
	}
	c3.Close()
	c1.Close()
}

// e. UDP：往返、回包源地址、同一五元组复用、空闲回收、AcceptUDP=false
func TestUDP(t *testing.T) {
	h := &fakeHandler{}
	s, cl := setup(t, h, Options{UDPIdle: 2 * time.Second})
	u := cl.udp(t)
	dst := netip.MustParseAddrPort("203.0.113.53:53")
	to := &net.UDPAddr{IP: dst.Addr().AsSlice(), Port: int(dst.Port())}
	buf := make([]byte, 100)
	for i := range 3 {
		msg := []byte(fmt.Sprintf("ping%d", i))
		u.WriteTo(msg, to)
		u.SetReadDeadline(time.Now().Add(ioWait))
		n, from, err := u.ReadFrom(buf)
		if err != nil || string(buf[:n]) != string(msg) {
			t.Fatalf("UDP 往返：%q %v", buf[:n], err)
		}
		if from.(*net.UDPAddr).AddrPort() != dst {
			t.Fatalf("回包源地址应是原目的地址：%s", from)
		}
	}
	if h.udpCalls.Load() != 1 {
		t.Fatalf("同一五元组应复用同一条流：%d", h.udpCalls.Load())
	}
	h.mu.Lock()
	f := h.udpFlows[0]
	h.mu.Unlock()
	if f.Dst != dst || f.Src.Addr() != clientIP {
		t.Fatalf("Flow 不对：%+v", f)
	}
	waitFor(t, "空闲后回收", func() bool { return s.Stats().UDPActive == 0 })
	u.WriteTo([]byte("again"), to)
	u.SetReadDeadline(time.Now().Add(ioWait))
	if _, _, err := u.ReadFrom(buf); err != nil || h.udpCalls.Load() != 2 {
		t.Fatalf("回收后再发应建新流：%v %d", err, h.udpCalls.Load())
	}
}

func TestUDPRejectAndReleaseOnReturn(t *testing.T) {
	h := &fakeHandler{acceptUDP: func(f Flow) bool { return f.Dst.Port() != 9 }}
	h.udp = func(_ context.Context, _ Flow, c net.PacketConn) { // 应答一次就返回（像 DNS）
		buf := make([]byte, 100)
		n, from, err := c.ReadFrom(buf)
		if err == nil {
			c.WriteTo(buf[:n], from)
		}
	}
	s, cl := setup(t, h, Options{MaxUDP: 1})
	u := cl.udp(t)
	buf := make([]byte, 100)
	u.WriteTo([]byte("x"), &net.UDPAddr{IP: net.IPv4(203, 0, 113, 1), Port: 9})
	waitFor(t, "计 udp_rejected", func() bool { return s.Stats().UDPRejected == 1 })
	u.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := u.ReadFrom(buf); err == nil || h.udpCalls.Load() != 0 {
		t.Fatal("AcceptUDP 为 false 时不应建流、不应有回包")
	}
	if s.Stats().UDPActive != 0 {
		t.Fatalf("名额应归还：%+v", s.Stats())
	}
	// MaxUDP=1：h.UDP 返回即释放名额，所以连续多个新五元组都能用。
	for i := range 5 {
		u.WriteTo([]byte("q"), &net.UDPAddr{IP: net.IPv4(203, 0, 113, 1), Port: 1000 + i})
		u.SetReadDeadline(time.Now().Add(ioWait))
		if _, _, err := u.ReadFrom(buf); err != nil {
			t.Fatalf("第 %d 个新五元组：h.UDP 返回后名额应立即释放：%v", i, err)
		}
		waitFor(t, "名额释放", func() bool { return s.Stats().UDPActive == 0 })
	}
}

func TestUDPLimit(t *testing.T) {
	h := &fakeHandler{}
	s, cl := setup(t, h, Options{MaxUDP: 1})
	u := cl.udp(t)
	buf := make([]byte, 100)
	a := &net.UDPAddr{IP: net.IPv4(203, 0, 113, 1), Port: 1}
	u.WriteTo([]byte("a"), a)
	u.SetReadDeadline(time.Now().Add(ioWait))
	if _, _, err := u.ReadFrom(buf); err != nil {
		t.Fatal(err)
	}
	u.WriteTo([]byte("b"), &net.UDPAddr{IP: net.IPv4(203, 0, 113, 1), Port: 2})
	waitFor(t, "第二个五元组被拒", func() bool { return s.Stats().UDPRejected >= 1 })
	u.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := u.ReadFrom(buf); err == nil {
		t.Fatal("达到上限后新五元组应被丢")
	}
	u.WriteTo([]byte("a2"), a)
	u.SetReadDeadline(time.Now().Add(ioWait))
	if n, _, err := u.ReadFrom(buf); err != nil || string(buf[:n]) != "a2" {
		t.Fatal("已有流不受影响")
	}
}

// f. 入口过滤：IPv6、ICMP、其他协议、畸形包一律丢，不回包，Handler 不被调用
func ipv4Packet(proto byte, src, dst netip.Addr, payload []byte, fragOffset uint16) []byte {
	p := make([]byte, 20+len(payload))
	p[0] = 0x45
	binary.BigEndian.PutUint16(p[2:], uint16(len(p)))
	binary.BigEndian.PutUint16(p[6:], fragOffset)
	p[8] = 64
	p[9] = proto
	s4, d4 := src.As4(), dst.As4()
	copy(p[12:], s4[:])
	copy(p[16:], d4[:])
	binary.BigEndian.PutUint16(p[10:], ^checksum.Checksum(p[:20], 0))
	copy(p[20:], payload)
	return p
}

func TestIngressFilter(t *testing.T) {
	h := &fakeHandler{}
	a, b := tun.Pipe(1500)
	s, err := New(a, h, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	defer b.Close()
	dst := netip.MustParseAddr("203.0.113.5")
	ipv6 := make([]byte, 60)
	ipv6[0] = 0x60
	ipv6[6] = 6 // next header TCP
	echo := []byte{8, 0, 0, 0, 0, 1, 0, 1}
	binary.BigEndian.PutUint16(echo[2:], ^checksum.Checksum(echo, 0))
	badIHL := ipv4Packet(6, clientIP, dst, make([]byte, 20), 0)
	badIHL[0] = 0x44
	for _, pkt := range [][]byte{
		ipv6,
		ipv4Packet(1, clientIP, dst, echo, 0), // ICMP echo
		ipv4Packet(47, clientIP, dst, make([]byte, 8), 0),  // GRE
		ipv4Packet(132, clientIP, dst, make([]byte, 8), 0), // SCTP
		{0x45, 0, 0}, // 太短
		append([]byte{0x75}, make([]byte, 30)...), // 版本号 7
		badIHL, // IHL < 5
	} {
		if _, err := b.Write(pkt); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "全部计数", func() bool {
		st := s.Stats()
		return st.DroppedIPv6 == 1 && st.DroppedICMP == 1 && st.DroppedOther == 5
	})
	got := make(chan struct{}, 1)
	go func() {
		if _, err := b.Read(make([]byte, 2000)); err == nil {
			got <- struct{}{}
		}
	}()
	select {
	case <-got:
		t.Fatal("被丢的包不应有任何回包（包括 ICMP 不可达、echo 应答）")
	case <-time.After(300 * time.Millisecond):
	}
	if h.tcpCalls.Load()+h.udpCalls.Load() != 0 {
		t.Fatal("Handler 不应被调用")
	}
}

func TestClassifyFragments(t *testing.T) {
	s := &Stack{}
	dst := netip.MustParseAddr("203.0.113.5")
	// TCP、UDP 的分片（MF 置位、偏移非零）照常交给栈；ICMP 分片照样丢。
	if !s.classify(ipv4Packet(6, clientIP, dst, make([]byte, 8), 0x2000)) || !s.classify(ipv4Packet(17, clientIP, dst, make([]byte, 8), 0x0010)) {
		t.Fatal("TCP、UDP 分片应交给栈")
	}
	if s.classify(ipv4Packet(1, clientIP, dst, make([]byte, 8), 0x0010)) || s.droppedICMP.Load() != 1 {
		t.Fatal("ICMP 分片应丢")
	}
}

// h. Close：所有流断开，goroutine 回到起点附近，重复 Close 不 panic
func TestCloseStopsEverything(t *testing.T) {
	before := runtime.NumGoroutine()
	h := &fakeHandler{}
	a, b := tun.Pipe(1500)
	s, err := New(a, h, Options{})
	if err != nil {
		t.Fatal(err)
	}
	cl := newClientRaw(t, b)
	var conns []net.Conn
	for i := range 5 {
		c, err := cl.dialTCP(netip.AddrPortFrom(netip.MustParseAddr("203.0.113.5"), uint16(100+i)), ioWait)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	u, _ := gonet.DialUDP(cl.s, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(clientIP.As4())}, nil, ipv4.ProtocolNumber)
	u.WriteTo([]byte("x"), &net.UDPAddr{IP: net.IPv4(203, 0, 113, 1), Port: 53})
	waitFor(t, "流都建好", func() bool { return s.Stats().TCPActive == 5 && s.Stats().UDPActive == 1 })
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(ioWait):
		t.Fatal("Close 应在流仍打开时也能返回")
	}
	s.Close() // 幂等
	if _, err := a.Write([]byte{0x45}); !errors.Is(err, tun.ErrClosed) {
		t.Fatal("Close 应关掉设备")
	}
	for _, c := range conns {
		c.Close()
	}
	u.Close()
	cl.close()
	deadline := time.Now().Add(ioWait)
	for runtime.NumGoroutine() > before+3 {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			n := runtime.Stack(buf, true)
			t.Fatalf("goroutine 泄漏：%d → %d\n%s", before, runtime.NumGoroutine(), buf[:n])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// newClientRaw 与 newClient 相同但不注册 Cleanup（由调用方关闭）。
func newClientRaw(t *testing.T, dev tun.Device) *client {
	t.Helper()
	s := gstack.New(gstack.Options{
		NetworkProtocols:   []gstack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []gstack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	link := channel.New(512, uint32(dev.MTU()), "")
	s.CreateNIC(1, link)
	s.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFrom4(clientIP.As4()).WithPrefix()}, gstack.AddressProperties{})
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})
	ctx, cancel := context.WithCancel(context.Background())
	c := &client{s: s, link: link, dev: dev, stop: cancel}
	c.wg.Add(2)
	go func() {
		defer c.wg.Done()
		buf := make([]byte, 2000)
		for {
			n, err := dev.Read(buf)
			if err != nil {
				return
			}
			pkt := gstack.NewPacketBuffer(gstack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), buf[:n]...))})
			link.InjectInbound(header.IPv4ProtocolNumber, pkt)
			pkt.DecRef()
		}
	}()
	go func() {
		defer c.wg.Done()
		for {
			pkt := link.ReadContext(ctx)
			if pkt == nil {
				return
			}
			v := pkt.ToView()
			pkt.DecRef()
			dev.Write(v.AsSlice())
			v.Release()
		}
	}()
	return c
}

// i. 源码断言：不碰宿主网络，不用 wireguard
func TestSourceNoHostNetwork(t *testing.T) {
	for _, dir := range []string{".", "../tun"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, p := range files {
			if strings.HasSuffix(p, "_test.go") {
				continue
			}
			src, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			for _, bad := range []string{"net.Dial", "net.Listen"} { // wireguard 只查 import（包注释里会解释为什么不用它）
				if strings.Contains(string(src), bad) {
					t.Errorf("%s 里出现了 %s", p, bad)
				}
			}
			f, err := parser.ParseFile(token.NewFileSet(), p, src, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range f.Imports {
				path, _ := strconv.Unquote(imp.Path.Value)
				if strings.Contains(path, "wireguard") {
					t.Errorf("%s import 了 %s", p, path)
				}
			}
		}
	}
}

// 第 0 步：Done、Err
func TestDoneOnPeerCloseAndOnClose(t *testing.T) {
	a, b := tun.Pipe(1500)
	s, err := New(a, &fakeHandler{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
		t.Fatal("正常运行时 Done 不应关闭")
	case <-time.After(50 * time.Millisecond):
	}
	b.Close() // 对端关闭：搬运 goroutine 意外退出
	select {
	case <-s.Done():
	case <-time.After(ioWait):
		t.Fatal("设备关闭导致搬运退出时 Done 应关闭")
	}
	if s.Err() == nil {
		t.Fatal("意外退出时 Err 应给出原因")
	}
	s.Close()

	a2, _ := tun.Pipe(1500)
	s2, _ := New(a2, &fakeHandler{}, Options{})
	s2.Close()
	select {
	case <-s2.Done():
	case <-time.After(ioWait):
		t.Fatal("Close 后 Done 也应关闭")
	}
	if s2.Err() != nil {
		t.Fatalf("正常 Close 不应报原因：%v", s2.Err())
	}
}

// 第 0 步：udpFlow 在 Close 之后 touch 不再重装定时器
func TestUDPFlowTouchAfterClose(t *testing.T) {
	var closes atomic.Int32
	u := &udpFlow{idle: 20 * time.Millisecond}
	u.timer = time.AfterFunc(time.Hour, func() { closes.Add(1) })
	u.mu.Lock()
	u.closed = true
	u.timer.Stop()
	u.mu.Unlock()
	u.touch()
	time.Sleep(60 * time.Millisecond)
	if closes.Load() != 0 || u.timer.Stop() {
		t.Fatal("Close 之后 touch 不应重装定时器")
	}
}
