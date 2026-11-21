// Package stack 把 tun 设备接到 gVisor 的用户态网络栈（设计 4.2）：连接在本机这一侧结束，
// 每条 TCP 连接、每个 UDP 五元组交给 Handler 处理。
//
// 行为：
//   - 接受任意目的地址（promiscuous + spoofing），路由表只有 0.0.0.0/0 → 这块网卡，不配任何通往宿主的路由；
//   - 只注册 IPv4、TCP、UDP。IPv6、ICMP 和其他协议在入口丢弃并计数，不回任何包
//     （gVisor 的 IPv4 层会自己回 ICMP 不可达、应答 echo，所以必须在入口丢）；
//   - TCP、UDP 各有流数上限，达到上限时 TCP 回 RST、UDP 丢弃，不排队。
//
// 本包不碰宿主网络（不 Dial、不 Listen，有源码检查测试守着）。
package stack

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	gstack "gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"claude-abroad/internal/tun"
)

// Flow 是一条连接或一个 UDP 五元组：Src 是发起方（本机应用），Dst 是它要连的目标。
type Flow struct{ Src, Dst netip.AddrPort }

// Handler 处理网络栈交上来的流。
//
//   - AcceptTCP 在握手之前调用（各自的 goroutine 里）；返回 false 时对 SYN 回 RST，不建立连接。
//     它可能在 Close 之后被调用（gVisor 为迟到的 SYN 起的 goroutine），必须没有副作用。
//   - TCP 在握手完成后、在新 goroutine 里调用；返回时连接被关闭、名额归还。
//   - AcceptUDP 在新五元组的第一个包到达时调用，**在“设备 → 栈”的搬运 goroutine 上同步执行**：
//     不能阻塞、不能做 I/O，否则整台机器的入包都会停下。返回 false 时丢包，不建流。
//   - UDP 在新 goroutine 里调用；**返回即关闭端点、归还名额**（例如 DNS 应答后立即返回，别占着名额）。
//     conn 的对端固定是 Flow.Src：WriteTo 忽略传入的地址，ReadFrom 返回 Flow.Src。
//     空闲超过 Options.UDPIdle 时 conn 被关闭，读写返回错误。
//
// TCP、UDP 必须在 ctx 取消或连接关闭后返回（Close 会等它们）。
type Handler interface {
	AcceptTCP(id Flow) bool
	TCP(ctx context.Context, id Flow, conn net.Conn)
	AcceptUDP(id Flow) bool
	UDP(ctx context.Context, id Flow, conn net.PacketConn)
}

// Options 是网络栈的参数；零值字段用默认值。这些默认值在 M2-9 做 R6 实测时复核。
type Options struct {
	MaxTCP      int           // 同时存在的 TCP 连接上限，默认 4096
	MaxUDP      int           // 同时存在的 UDP 流上限，默认 1024
	UDPIdle     time.Duration // UDP 流的空闲超时，默认 60 秒
	TCPInFlight int           // 握手中的 TCP 连接上限（gVisor forwarder 的 maxInFlight），默认 512
}

func (o Options) withDefaults() Options {
	if o.MaxTCP <= 0 {
		o.MaxTCP = 4096
	}
	if o.MaxUDP <= 0 {
		o.MaxUDP = 1024
	}
	if o.UDPIdle <= 0 {
		o.UDPIdle = 60 * time.Second
	}
	if o.TCPInFlight <= 0 {
		o.TCPInFlight = 512
	}
	return o
}

// Stats 是网络栈的计数。
type Stats struct {
	TCPActive       int64  `json:"tcp_active"`
	UDPActive       int64  `json:"udp_active"`
	TCPRejected     int64  `json:"tcp_rejected"` // 上限、AcceptTCP=false、建端点失败
	UDPRejected     int64  `json:"udp_rejected"` // 上限、AcceptUDP=false、建端点失败
	DroppedIPv6     int64  `json:"dropped_ipv6"`
	DroppedICMP     int64  `json:"dropped_icmp"`
	DroppedOther    int64  `json:"dropped_other"`     // 其他协议、太短、版本号或 IHL 不对
	TCPInFlightDrop uint64 `json:"tcp_inflight_drop"` // 握手中超限被静默丢的 SYN（gVisor 计数）
}

const nicID = 1

// Stack 是一个接在 tun 设备上的 gVisor 网络栈。
type Stack struct {
	dev  tun.Device
	h    Handler
	o    Options
	gs   *gstack.Stack
	link *channel.Endpoint

	ctx    context.Context
	cancel context.CancelFunc

	pumps sync.WaitGroup // 两个搬运 goroutine
	flows sync.WaitGroup // 所有 handler

	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}

	done     chan struct{} // 任一搬运 goroutine 退出（故障或 Close）时关闭
	doneOnce sync.Once
	err      error // 故障原因（Close 时为 nil）

	tcpActive, udpActive                   atomic.Int64
	tcpRejected, udpRejected               atomic.Int64
	droppedIPv6, droppedICMP, droppedOther atomic.Int64

	closeOnce sync.Once
}

// New 在 dev 上建一个网络栈并开始搬包。
func New(dev tun.Device, h Handler, o Options) (*Stack, error) {
	if dev == nil || h == nil {
		return nil, errors.New("tun 设备和 Handler 都必须提供")
	}
	o = o.withDefaults()
	gs := gstack.New(gstack.Options{
		NetworkProtocols:   []gstack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []gstack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	link := channel.New(512, uint32(dev.MTU()), "")
	if err := gs.CreateNIC(nicID, link); err != nil {
		gs.Close()
		return nil, errors.New("创建网卡：" + err.String())
	}
	if err := gs.SetPromiscuousMode(nicID, true); err != nil {
		gs.Close()
		return nil, errors.New("设置 promiscuous：" + err.String())
	}
	if err := gs.SetSpoofing(nicID, true); err != nil {
		gs.Close()
		return nil, errors.New("设置 spoofing：" + err.String())
	}
	gs.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: nicID}})
	// TIME_WAIT 只留 1 秒（M2-9b-2）：utun 是本机内的虚拟网卡，没有迟到的报文；默认 60 秒会让 Close 里的
	// gs.Wait() 一直等到它超时（网络栈这一侧先关连接时，端点留在 TIME_WAIT），utun 重建和恢复都被拖住。
	tw := tcpip.TCPTimeWaitTimeoutOption(time.Second)
	if err := gs.SetTransportProtocolOption(tcp.ProtocolNumber, &tw); err != nil {
		gs.Close()
		return nil, errors.New("设置 TIME_WAIT：" + err.String())
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Stack{dev: dev, h: h, o: o, gs: gs, link: link, ctx: ctx, cancel: cancel, conns: map[net.Conn]struct{}{},
		done: make(chan struct{})}
	tf := tcp.NewForwarder(gs, 0, o.TCPInFlight, s.handleTCP)
	gs.SetTransportProtocolHandler(tcp.ProtocolNumber, tf.HandlePacket)
	uf := udp.NewForwarder(gs, s.handleUDP)
	gs.SetTransportProtocolHandler(udp.ProtocolNumber, uf.HandlePacket)

	s.pumps.Add(2)
	go s.inbound()
	go s.outbound()
	return s, nil
}

// Stats 返回计数快照。
func (s *Stack) Stats() Stats {
	return Stats{
		TCPActive: s.tcpActive.Load(), UDPActive: s.udpActive.Load(),
		TCPRejected: s.tcpRejected.Load(), UDPRejected: s.udpRejected.Load(),
		DroppedIPv6: s.droppedIPv6.Load(), DroppedICMP: s.droppedICMP.Load(), DroppedOther: s.droppedOther.Load(),
		TCPInFlightDrop: s.gs.Stats().TCP.ForwardMaxInFlightDrop.Value(),
	}
}

// Done 在网络栈停止收发时关闭：任一搬运 goroutine 意外退出（例如 utun 被系统删除），或者 Close。
// 意外退出时 Err 给出原因；调用方（M2-7、M2-9）要当故障处理：重建 utun 和栈，期间闸门关闭。
func (s *Stack) Done() <-chan struct{} { return s.done }

// Err 返回搬运 goroutine 意外退出的原因；正常运行或正常 Close 时为 nil。
func (s *Stack) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// stopped 记下搬运停止；不是 Close 引起的就记原因。
func (s *Stack) stopped(err error) {
	s.mu.Lock()
	if !s.closed && s.err == nil && err != nil {
		s.err = err
	}
	s.mu.Unlock()
	s.doneOnce.Do(func() { close(s.done) })
}

// classify 决定入站包交不交给栈；不交的分类计数。
func (s *Stack) classify(p []byte) bool {
	if len(p) < 1 {
		s.droppedOther.Add(1)
		return false
	}
	switch p[0] >> 4 {
	case 6:
		s.droppedIPv6.Add(1)
		return false
	case 4:
	default:
		s.droppedOther.Add(1)
		return false
	}
	if len(p) < header.IPv4MinimumSize {
		s.droppedOther.Add(1)
		return false
	}
	ihl := int(p[0]&0x0f) * 4
	if ihl < header.IPv4MinimumSize || ihl > len(p) {
		s.droppedOther.Add(1)
		return false
	}
	// 分片也按协议号判断（每个分片的 IPv4 头里都有协议号）：TCP、UDP 的分片交给栈重组。
	switch tcpip.TransportProtocolNumber(p[9]) {
	case tcp.ProtocolNumber, udp.ProtocolNumber:
		return true
	case header.ICMPv4ProtocolNumber:
		s.droppedICMP.Add(1)
	default:
		s.droppedOther.Add(1)
	}
	return false
}

// inbound：设备 → 栈。
func (s *Stack) inbound() {
	defer s.pumps.Done()
	buf := make([]byte, s.dev.MTU()+64)
	for {
		n, err := s.dev.Read(buf)
		if err != nil {
			if errors.Is(err, tun.ErrShortBuffer) || errors.Is(err, tun.ErrTooBig) {
				s.droppedOther.Add(1)
				continue
			}
			s.stopped(fmt.Errorf("读 tun 设备：%w", err))
			return
		}
		if !s.classify(buf[:n]) {
			continue
		}
		pkt := gstack.NewPacketBuffer(gstack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), buf[:n]...))})
		s.link.InjectInbound(header.IPv4ProtocolNumber, pkt)
		pkt.DecRef()
	}
}

// outbound：栈 → 设备。
func (s *Stack) outbound() {
	defer s.pumps.Done()
	for {
		pkt := s.link.ReadContext(s.ctx)
		if pkt == nil {
			s.stopped(nil) // ctx 取消（Close）
			return
		}
		v := pkt.ToView()
		pkt.DecRef()
		_, err := s.dev.Write(v.AsSlice())
		v.Release()
		if errors.Is(err, tun.ErrClosed) {
			s.stopped(fmt.Errorf("写 tun 设备：%w", err))
			return
		}
	}
}

func flowOf(id gstack.TransportEndpointID) Flow {
	src := netip.AddrPortFrom(netip.AddrFrom4(id.RemoteAddress.As4()), id.RemotePort)
	dst := netip.AddrPortFrom(netip.AddrFrom4(id.LocalAddress.As4()), id.LocalPort)
	return Flow{Src: src, Dst: dst}
}

// reserve 原子地预占一个名额（达到 max 时失败）。
func reserve(n *atomic.Int64, max int) bool {
	for {
		cur := n.Load()
		if cur >= int64(max) {
			return false
		}
		if n.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// startFlow 登记一条流并在新 goroutine 里运行 fn；Stack 已关闭时返回 false。
func (s *Stack) startFlow(c net.Conn, fn func()) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	s.conns[c] = struct{}{}
	s.flows.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.flows.Done()
		defer func() {
			c.Close()
			s.mu.Lock()
			delete(s.conns, c)
			s.mu.Unlock()
		}()
		fn()
	}()
	return true
}

// handleTCP 在 gVisor forwarder 为每个 SYN 起的 goroutine 里运行。每条路径都调用 Complete：
// gVisor 的 inFlight 表项只有 Complete 才删除，漏掉会让 forwarder 最终丢掉所有新 SYN。
func (s *Stack) handleTCP(r *tcp.ForwarderRequest) {
	id := flowOf(r.ID())
	if s.ctx.Err() != nil || !reserve(&s.tcpActive, s.o.MaxTCP) {
		s.tcpRejected.Add(1)
		r.Complete(true)
		return
	}
	release := func() { s.tcpActive.Add(-1) }
	if !s.h.AcceptTCP(id) {
		release()
		s.tcpRejected.Add(1)
		r.Complete(true)
		return
	}
	var wq waiter.Queue
	ep, err := r.CreateEndpoint(&wq)
	if err != nil {
		release()
		s.tcpRejected.Add(1)
		r.Complete(true)
		return
	}
	r.Complete(false)
	conn := &tcpConn{TCPConn: gonet.NewTCPConn(&wq, ep), ep: ep}
	if !s.startFlow(conn, func() { defer release(); s.h.TCP(s.ctx, id, conn) }) {
		conn.Close()
		release()
	}
}

// handleUDP 在“设备 → 栈”的搬运 goroutine 上同步运行：不阻塞。
func (s *Stack) handleUDP(r *udp.ForwarderRequest) {
	id := flowOf(r.ID())
	if s.ctx.Err() != nil || !reserve(&s.udpActive, s.o.MaxUDP) {
		s.udpRejected.Add(1)
		return
	}
	release := func() { s.udpActive.Add(-1) }
	if !s.h.AcceptUDP(id) {
		release()
		s.udpRejected.Add(1)
		return
	}
	var wq waiter.Queue
	ep, err := r.CreateEndpoint(&wq)
	if err != nil { // 例如组播、广播目的地址：按丢包处理
		release()
		s.udpRejected.Add(1)
		return
	}
	uc := newUDPFlow(gonet.NewUDPConn(&wq, ep), id, s.o.UDPIdle)
	if !s.startFlow(uc, func() { defer release(); s.h.UDP(s.ctx, id, uc) }) {
		uc.Close()
		release()
	}
}

// Close 停止网络栈：取消 ctx → 关设备 → 等两个搬运 goroutine → 关所有连接 → 等所有 handler →
// gVisor 的 Close、Wait。幂等。
func (s *Stack) Close() error {
	s.closeOnce.Do(func() {
		s.cancel()
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		_ = s.dev.Close()
		s.pumps.Wait()
		s.mu.Lock()
		for c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
		s.flows.Wait()
		s.link.Close()
		// 设备已经关了，连接的 FIN 送不出去，端点会停在 FIN_WAIT/TIME_WAIT 里重传到超时，gs.Wait() 被拖住
		// （M2-9b-2 实测：utun 重建和恢复偶尔要等很久）。直接 Abort，不走优雅关闭。
		for _, ep := range s.gs.RegisteredEndpoints() {
			ep.Abort()
		}
		for _, ep := range s.gs.CleanupEndpoints() {
			ep.Abort()
		}
		s.gs.Close()
		s.gs.Wait()
	})
	return nil
}

// tcpConn 是栈交给处理器的 TCP 连接：在 gonet.TCPConn 之外多一个 Reset。
type tcpConn struct {
	*gonet.TCPConn
	ep tcpip.Endpoint
}

// Reset 以 RST 结束连接（linger 0 再关闭），给“接受之后才判定为拦截”的连接用（m3-plan M3-5）。
func (c *tcpConn) Reset() {
	c.ep.SocketOptions().SetLinger(tcpip.LingerOption{Enabled: true, Timeout: 0})
	_ = c.TCPConn.Close()
}
