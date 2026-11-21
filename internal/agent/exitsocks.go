package agent

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"claude-abroad/internal/devsocks"
	"claude-abroad/internal/wire"
)

// ExitSocks 是分流出口命名空间里的 socks 服务（`abroad-agent socks`，m4-plan M4-7）。它整个进程运行在出口的
// 命名空间里：拨号和解析都从命名空间发出，默认路由是隧道，断网规则只放行隧道和服务器端点。
//
// 域名只经隧道解析（v2 必须改 3）：用出口目录里的 dns 文件（WireGuard 的 DNS=、OpenVPN 推送的 dhcp-option DNS），
// 没有时用固定的公共解析器（经隧道访问）。不退回虚拟机根命名空间的解析路径，解析失败就是失败。
type ExitSocks struct {
	Listen   netip.AddrPort
	DNSFile  string
	Fallback []netip.Addr
	Dial     func(ctx context.Context, network, addr string) (net.Conn, error) // nil 用 net.Dialer
	Logf     func(format string, a ...any)
}

// DefaultExitDNS 是出口没有给 DNS 时用的公共解析器（经隧道访问）。
var DefaultExitDNS = []netip.Addr{netip.MustParseAddr("1.1.1.1")}

func (e *ExitSocks) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if e.Dial != nil {
		return e.Dial(ctx, network, addr)
	}
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// dnsServers 每次都重读 dns 文件（OpenVPN 推送的 DNS 可能在 socks 起来之后才写好）。
// fakeIPNet 是 fake-IP 网段（宿主网络栈分配给域名的假地址，不能当解析器）。
var fakeIPNet = netip.MustParsePrefix("198.18.0.0/15")

// ValidExitDNS 判断推送或配置里的 DNS 地址能不能用（design 7.3 v0.14）：只收 IPv4；拒绝回环、组播、未指定、广播、
// 链路本地、fake 段和出口 veth 的网段；接受私有地址（服务商常推隧道里的内网解析器）。
func ValidExitDNS(a netip.Addr) bool {
	a = a.Unmap()
	return a.Is4() && !a.IsLoopback() && !a.IsMulticast() && !a.IsUnspecified() && !a.IsLinkLocalUnicast() &&
		a != netip.AddrFrom4([4]byte{255, 255, 255, 255}) && !fakeIPNet.Contains(a) && !ExitNet.Contains(a)
}

// dnsServers 每次重读出口的 DNS 文件：校验过的最多 2 个；一个都没有时用固定的公共解析器（仍只经隧道）。
func (e *ExitSocks) dnsServers() []netip.Addr {
	var out []netip.Addr
	if e.DNSFile != "" {
		if f, err := os.Open(e.DNSFile); err == nil {
			sc := bufio.NewScanner(io.LimitReader(f, 4096))
			for sc.Scan() {
				// 2.6 推送 dns 时，OpenVPN 同时给新旧两种环境变量（实测 2.6.20），同一个地址会写两遍：去重。
				if a, err := netip.ParseAddr(strings.TrimSpace(sc.Text())); err == nil && ValidExitDNS(a) && len(out) < 2 && !slices.Contains(out, a) {
					out = append(out, a)
				}
			}
			f.Close()
		}
	}
	if len(out) == 0 {
		out = e.Fallback
		if len(out) == 0 {
			out = DefaultExitDNS
		}
	}
	return out
}

func (e *ExitSocks) resolver() *net.Resolver {
	servers := e.dnsServers()
	i := 0
	return &net.Resolver{PreferGo: true, StrictErrors: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		s := servers[i%len(servers)]
		i++
		return e.dial(ctx, "udp", netip.AddrPortFrom(s, 53).String())
	}}
}

// target 把帧头里的目的地解析成 IPv4 地址（域名只经隧道里的解析器）。
func (e *ExitSocks) target(ctx context.Context, h wire.Header) (netip.AddrPort, error) {
	if h.Port <= 0 || h.Port > 65535 {
		return netip.AddrPort{}, errors.New("端口不对")
	}
	if h.Host == "" {
		a, err := netip.ParseAddr(h.IP)
		if err != nil || !a.Is4() {
			return netip.AddrPort{}, errors.New("只支持 IPv4")
		}
		return netip.AddrPortFrom(a, uint16(h.Port)), nil
	}
	if a, err := netip.ParseAddr(h.Host); err == nil {
		if !a.Is4() {
			return netip.AddrPort{}, errors.New("只支持 IPv4")
		}
		return netip.AddrPortFrom(a, uint16(h.Port)), nil
	}
	addrs, err := e.resolver().LookupNetIP(ctx, "ip4", h.Host)
	if err != nil || len(addrs) == 0 {
		return netip.AddrPort{}, fmt.Errorf("经隧道解析失败")
	}
	return netip.AddrPortFrom(addrs[0].Unmap(), uint16(h.Port)), nil
}

// open 是 devsocks 的 Opener：TCP 直接拨；UDP 拨一个已连接的 UDP 套接字，再接成按数据报分帧的流。
func (e *ExitSocks) open(ctx context.Context, h wire.Header) (net.Conn, error) {
	ap, err := e.target(ctx, h)
	if err != nil {
		return nil, err
	}
	switch h.Net {
	case wire.TCP:
		return e.dial(ctx, "tcp", ap.String())
	case wire.UDP:
		u, err := e.dial(ctx, "udp", ap.String())
		if err != nil {
			return nil, err
		}
		return udpStream(u), nil
	}
	return nil, errors.New("不认识的网络类型")
}

// udpStream 把已连接的 UDP 套接字接成按 wire 数据报分帧的流（devsocks 的 UDP ASSOCIATE 用这种流）。
func udpStream(u net.Conn) net.Conn {
	a, b := net.Pipe()
	dc := wire.NewDatagramConn(b)
	go func() { // 流 → UDP
		buf := make([]byte, wire.MaxDatagram)
		for {
			n, err := dc.ReadDatagram(buf)
			if err != nil {
				u.Close()
				return
			}
			_, _ = u.Write(buf[:n])
		}
	}()
	go func() { // UDP → 流（空闲 60 秒结束）
		buf := make([]byte, wire.MaxDatagram)
		for {
			_ = u.SetReadDeadline(time.Now().Add(60 * time.Second))
			n, err := u.Read(buf)
			if err != nil {
				b.Close()
				return
			}
			if dc.WriteDatagram(buf[:n]) != nil {
				u.Close()
				return
			}
		}
	}()
	return a
}

// Serve 在 Listen 上提供 socks5（TCP CONNECT、UDP ASSOCIATE）。只接受同一个 /30 里的客户端（根命名空间那头的 veth）。
func (e *ExitSocks) Serve(ctx context.Context) error {
	l, err := net.Listen("tcp", e.Listen.String())
	if err != nil {
		return err
	}
	peer := netip.PrefixFrom(e.Listen.Addr(), 30).Masked()
	srv := &devsocks.Server{Open: e.open, Exit: "exit", Logf: e.Logf, UDPBind: e.Listen.Addr(),
		AllowClient: func(a netip.Addr) bool { return peer.Contains(a) }}
	return srv.Serve(ctx, l)
}

// RunSocksCmd 是 `abroad-agent socks --listen <ip:端口> [--dns-file <文件>]`（在出口命名空间里由 agent 启动）。
func RunSocksCmd(ctx context.Context, args []string, w io.Writer) int {
	fs := flag.NewFlagSet("socks", flag.ContinueOnError)
	fs.SetOutput(w)
	listen := fs.String("listen", "", "监听地址（出口命名空间里 veth 的地址:端口）")
	dnsFile := fs.String("dns-file", "", "出口的 DNS 文件（每行一个 IPv4；没有时用 1.1.1.1，经隧道）")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ap, err := netip.ParseAddrPort(*listen)
	if err != nil || !ap.Addr().Is4() {
		fmt.Fprintln(w, "abroad-agent socks：--listen 要是 IPv4:端口")
		return 2
	}
	e := &ExitSocks{Listen: ap, DNSFile: *dnsFile}
	if err := e.Serve(ctx); err != nil && ctx.Err() == nil {
		fmt.Fprintf(w, "abroad-agent socks：%v\n", err)
		return 1
	}
	return 0
}

var _ = strconv.Itoa
