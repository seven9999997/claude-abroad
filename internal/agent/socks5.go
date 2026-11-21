package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"time"
)

// 最小的 SOCKS5 客户端（RFC 1928、RFC 1929），只用来连虚拟机回环上的 sing-box：
// 用户名口令认证、CONNECT、UDP ASSOCIATE。目标是域名时用 ATYP=域名原样交给 sing-box，
// agent 自己绝不解析（设计文档 4.4、4.5）。

// SocksError 是 SOCKS5 服务端的拒绝（REP 字段）。
type SocksError struct{ Rep byte }

func (e *SocksError) Error() string { return fmt.Sprintf("socks5 服务端拒绝（REP=%d）", e.Rep) }

// ErrSocksAuth 是用户名口令认证失败。
var ErrSocksAuth = errors.New("socks5 认证失败")

// SocksRepCommandNotSupported 是 REP=7（命令不支持），UDP ASSOCIATE 不支持时会收到。
const SocksRepCommandNotSupported = 0x07

// SocksDialer 连接一个 SOCKS5 服务端。
type SocksDialer struct {
	Addr     string // 127.0.0.1:1090
	User     string
	Password string
	// Dial 拨 TCP；为空时用 net.Dialer。数据面唯一允许的拨号目标是 Addr。
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (d SocksDialer) dial(ctx context.Context) (net.Conn, error) {
	if d.Dial != nil {
		return d.Dial(ctx, "tcp", d.Addr)
	}
	var nd net.Dialer
	return nd.DialContext(ctx, "tcp", d.Addr)
}

// handshake 连上服务端、认证、发请求、读应答，返回控制连接和 BND 地址。ctx 结束时连接被关。
func (d SocksDialer) handshake(ctx context.Context, cmd byte, host string, ip netip.Addr, port int) (net.Conn, netip.AddrPort, error) {
	c, err := d.dial(ctx)
	if err != nil {
		return nil, netip.AddrPort{}, err
	}
	stop := context.AfterFunc(ctx, func() { c.Close() })
	fail := func(err error) (net.Conn, netip.AddrPort, error) {
		stop()
		c.Close()
		if ctx.Err() != nil {
			return nil, netip.AddrPort{}, ctx.Err()
		}
		return nil, netip.AddrPort{}, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	if len(d.User) == 0 || len(d.User) > 255 || len(d.Password) > 255 {
		return fail(errors.New("socks5 用户名或口令长度不合法"))
	}
	// 问候：只提供用户名口令认证。
	if _, err := c.Write([]byte{5, 1, 2}); err != nil {
		return fail(err)
	}
	var b [2]byte
	if _, err := io.ReadFull(c, b[:]); err != nil {
		return fail(err)
	}
	if b[0] != 5 || b[1] != 2 {
		return fail(ErrSocksAuth)
	}
	auth := append([]byte{1, byte(len(d.User))}, d.User...)
	auth = append(append(auth, byte(len(d.Password))), d.Password...)
	if _, err := c.Write(auth); err != nil {
		return fail(err)
	}
	if _, err := io.ReadFull(c, b[:]); err != nil {
		return fail(err)
	}
	if b[1] != 0 {
		return fail(ErrSocksAuth)
	}
	req := []byte{5, cmd, 0}
	switch {
	case host != "":
		if len(host) > 255 {
			return fail(errors.New("域名太长"))
		}
		req = append(append(req, 3, byte(len(host))), host...)
	case ip.Is4():
		a := ip.As4()
		req = append(append(req, 1), a[:]...)
	default:
		a := ip.As16()
		req = append(append(req, 4), a[:]...)
	}
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := c.Write(req); err != nil {
		return fail(err)
	}
	var rep [3]byte
	if _, err := io.ReadFull(c, rep[:]); err != nil {
		return fail(err)
	}
	if rep[0] != 5 {
		return fail(errors.New("socks5 应答版本不对"))
	}
	bnd, err := readSocksAddr(c)
	if err != nil {
		return fail(err)
	}
	if rep[1] != 0 {
		return fail(&SocksError{Rep: rep[1]})
	}
	if !stop() { // ctx 恰好在握手完成时结束：连接已被关
		c.Close()
		return nil, netip.AddrPort{}, ctx.Err()
	}
	_ = c.SetDeadline(time.Time{})
	return c, bnd, nil
}

// readSocksAddr 读 ATYP + 地址 + 端口。域名形式的 BND 地址当作无效地址返回。
func readSocksAddr(r io.Reader) (netip.AddrPort, error) {
	var t [1]byte
	if _, err := io.ReadFull(r, t[:]); err != nil {
		return netip.AddrPort{}, err
	}
	var addr netip.Addr
	switch t[0] {
	case 1:
		var a [4]byte
		if _, err := io.ReadFull(r, a[:]); err != nil {
			return netip.AddrPort{}, err
		}
		addr = netip.AddrFrom4(a)
	case 4:
		var a [16]byte
		if _, err := io.ReadFull(r, a[:]); err != nil {
			return netip.AddrPort{}, err
		}
		addr = netip.AddrFrom16(a)
	case 3:
		var n [1]byte
		if _, err := io.ReadFull(r, n[:]); err != nil {
			return netip.AddrPort{}, err
		}
		if _, err := io.CopyN(io.Discard, r, int64(n[0])); err != nil {
			return netip.AddrPort{}, err
		}
	default:
		return netip.AddrPort{}, fmt.Errorf("socks5 地址类型 %d 不认识", t[0])
	}
	var p [2]byte
	if _, err := io.ReadFull(r, p[:]); err != nil {
		return netip.AddrPort{}, err
	}
	return netip.AddrPortFrom(addr, binary.BigEndian.Uint16(p[:])), nil
}

// Connect 经 SOCKS5 连目标。host 非空时以域名发出（不在本地解析），否则用 ip。
func (d SocksDialer) Connect(ctx context.Context, host string, ip netip.Addr, port int) (net.Conn, error) {
	c, _, err := d.handshake(ctx, 1, host, ip, port)
	return c, err
}

// SocksUDP 是一个 UDP ASSOCIATE 会话：控制连接 + 连到中继地址的 UDP 套接字。
type SocksUDP struct {
	Ctrl  net.Conn
	Relay *net.UDPConn
}

// MaxSocksUDPPayload 是 UDP 数据报（含 SOCKS 头）的上限：IPv4 上 UDP 载荷最大 65507。
const MaxSocksUDPPayload = 65507

// ErrRelayNotLoopback 是中继地址不是回环地址。
var ErrRelayNotLoopback = errors.New("socks5 的 UDP 中继地址不是回环地址")

// Associate 建立 UDP ASSOCIATE。中继地址必须是回环：0.0.0.0 换成 127.0.0.1，其他非回环拒绝。
func (d SocksDialer) Associate(ctx context.Context) (*SocksUDP, error) {
	c, bnd, err := d.handshake(ctx, 3, "", netip.IPv4Unspecified(), 0)
	if err != nil {
		return nil, err
	}
	a := bnd.Addr()
	if a.IsUnspecified() {
		a = netip.AddrFrom4([4]byte{127, 0, 0, 1})
	}
	if !a.IsValid() || !a.IsLoopback() || bnd.Port() == 0 {
		c.Close()
		return nil, ErrRelayNotLoopback
	}
	u, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(a, bnd.Port())))
	if err != nil {
		c.Close()
		return nil, err
	}
	return &SocksUDP{Ctrl: c, Relay: u}, nil
}

// Close 关掉控制连接和 UDP 套接字。
func (u *SocksUDP) Close() error {
	u.Relay.Close()
	return u.Ctrl.Close()
}

// AppendUDPHeader 把 SOCKS5 UDP 头（RSV、FRAG=0、目标地址）加到 b 后面。
func AppendUDPHeader(b []byte, host string, ip netip.Addr, port int) []byte {
	b = append(b, 0, 0, 0)
	switch {
	case host != "":
		b = append(append(b, 3, byte(len(host))), host...)
	case ip.Is4():
		a := ip.As4()
		b = append(append(b, 1), a[:]...)
	default:
		a := ip.As16()
		b = append(append(b, 4), a[:]...)
	}
	return binary.BigEndian.AppendUint16(b, uint16(port))
}

// ErrUDPFragment 是 FRAG 不为 0 的数据报（不支持分片，丢弃）。
var ErrUDPFragment = errors.New("socks5 UDP 分片")

// ParseUDPPacket 解开 SOCKS5 UDP 头，返回来源（域名来源时 Addr 无效）和数据。
func ParseUDPPacket(p []byte) (netip.AddrPort, []byte, error) {
	if len(p) < 4 || p[0] != 0 || p[1] != 0 {
		return netip.AddrPort{}, nil, errors.New("socks5 UDP 头不对")
	}
	if p[2] != 0 {
		return netip.AddrPort{}, nil, ErrUDPFragment
	}
	r := &sliceReader{b: p[3:]}
	src, err := readSocksAddr(r)
	if err != nil {
		return netip.AddrPort{}, nil, err
	}
	return src, r.b, nil
}

type sliceReader struct{ b []byte }

func (r *sliceReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}
