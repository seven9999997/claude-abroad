// Package tunsim 是开发工具 abroad-dev tunsim 的实现（不进发布包）：在用户态模拟“本机应用 → utun → 网络栈”，
// 不需要 root、不建真实 utun、不碰宿主网络。网络栈接在内存管道（tun.Pipe）上，另一端是一个 gVisor 客户端栈，
// 扮演本机应用发起 DNS 查询、TCP、UDP；网络栈经 data.sock（只连 unix socket）把流交给虚拟机（M2-5c）。
package tunsim

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	gstack "gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"

	"claude-abroad/internal/tun"
)

// DefaultSrc 是模拟应用的源地址（相当于 utun 的地址）。
var DefaultSrc = netip.MustParseAddr("198.18.0.1")

// Client 是挂在 tun 设备上的 gVisor 客户端栈，扮演本机应用。
type Client struct {
	s      *gstack.Stack
	link   *channel.Endpoint
	dev    tun.Device
	src    netip.Addr
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
}

// NewClient 在 dev 上建一个客户端栈，源地址 src（无效时用 DefaultSrc）。
func NewClient(dev tun.Device, src netip.Addr) (*Client, error) {
	if !src.IsValid() {
		src = DefaultSrc
	}
	s := gstack.New(gstack.Options{
		NetworkProtocols:   []gstack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []gstack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	link := channel.New(512, uint32(dev.MTU()), "")
	if err := s.CreateNIC(1, link); err != nil {
		s.Close()
		return nil, errors.New("创建客户端网卡：" + err.String())
	}
	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFrom4(src.As4()).WithPrefix()}, gstack.AddressProperties{}); err != nil {
		s.Close()
		return nil, errors.New("设置客户端地址：" + err.String())
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{s: s, link: link, dev: dev, src: src, cancel: cancel}
	c.wg.Add(2)
	go func() {
		defer c.wg.Done()
		buf := make([]byte, dev.MTU()+64)
		for {
			n, err := dev.Read(buf)
			if err != nil {
				if errors.Is(err, tun.ErrShortBuffer) || errors.Is(err, tun.ErrTooBig) {
					continue
				}
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
			_, _ = dev.Write(v.AsSlice())
			v.Release()
		}
	}()
	return c, nil
}

func (c *Client) full(ap netip.AddrPort) tcpip.FullAddress {
	return tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(ap.Addr().As4()), Port: ap.Port()}
}

// DialTCP 发起一条 TCP 连接。
func (c *Client) DialTCP(ctx context.Context, dst netip.AddrPort) (*gonet.TCPConn, error) {
	return gonet.DialContextTCP(ctx, c.s, c.full(dst), ipv4.ProtocolNumber)
}

// DialTCPFrom 从指定的源端口发起一条 TCP 连接（测试按源端口段区分应用用）。
func (c *Client) DialTCPFrom(ctx context.Context, srcPort uint16, dst netip.AddrPort) (*gonet.TCPConn, error) {
	local := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(c.src.As4()), Port: srcPort}
	return gonet.DialTCPWithBind(ctx, c.s, local, c.full(dst), ipv4.ProtocolNumber)
}

// DialUDP 返回一个未连接的 UDP 端点（WriteTo/ReadFrom，便于核对回包的源地址）。
func (c *Client) DialUDP() (*gonet.UDPConn, error) {
	laddr := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(c.src.As4())}
	return gonet.DialUDP(c.s, &laddr, nil, ipv4.ProtocolNumber)
}

// Query 向 dnsAddr 发一个 UDP 查询，返回应答（不检查 Rcode）。
func (c *Client) Query(ctx context.Context, name string, qtype uint16, dnsAddr netip.AddrPort) (*dns.Msg, error) {
	u, err := c.DialUDP()
	if err != nil {
		return nil, err
	}
	defer u.Close()
	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(name), qtype)
	b, err := q.Pack()
	if err != nil {
		return nil, err
	}
	if _, err := u.WriteTo(b, net.UDPAddrFromAddrPort(dnsAddr)); err != nil {
		return nil, err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	_ = u.SetReadDeadline(deadline)
	buf := make([]byte, 65535)
	for {
		n, _, err := u.ReadFrom(buf)
		if err != nil {
			return nil, errors.New("DNS 查询没有应答")
		}
		m := new(dns.Msg)
		if m.Unpack(buf[:n]) != nil || m.Id != q.Id {
			continue
		}
		return m, nil
	}
}

// LookupA 向 dnsAddr 发一个 UDP 的 A 查询，返回第一条 A 记录。
func (c *Client) LookupA(ctx context.Context, name string, dnsAddr netip.AddrPort) (netip.Addr, error) {
	m, err := c.Query(ctx, name, dns.TypeA, dnsAddr)
	if err != nil {
		return netip.Addr{}, err
	}
	if m.Rcode != dns.RcodeSuccess {
		return netip.Addr{}, errors.New("DNS 应答 " + dns.RcodeToString[m.Rcode])
	}
	for _, rr := range m.Answer {
		if a, ok := rr.(*dns.A); ok {
			if ip, ok := netip.AddrFromSlice(a.A); ok {
				return ip.Unmap(), nil
			}
		}
	}
	return netip.Addr{}, errors.New("DNS 应答里没有 A 记录")
}

// Close 停止客户端栈并关闭设备。幂等。
func (c *Client) Close() error {
	c.once.Do(func() {
		c.cancel()
		_ = c.dev.Close()
		c.wg.Wait()
		c.s.Close()
		c.s.Wait()
	})
	return nil
}
