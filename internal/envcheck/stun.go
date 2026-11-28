package envcheck

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"claude-abroad/internal/exitpolicy"
)

// STUN（RFC 5389）的最小实现：Binding 请求和应答里的（XOR-）MAPPED-ADDRESS。第 08 项和本机真实公网 IP 的测量用。

const stunMagic = 0x2112A442

// StunRequest 返回一个 Binding 请求和它的事务号。
func StunRequest() ([]byte, [12]byte) {
	var tx [12]byte
	_, _ = rand.Read(tx[:])
	b := make([]byte, 20)
	binary.BigEndian.PutUint16(b[0:], 0x0001)
	binary.BigEndian.PutUint16(b[2:], 0)
	binary.BigEndian.PutUint32(b[4:], stunMagic)
	copy(b[8:], tx[:])
	return b, tx
}

// ErrStun 是 STUN 应答不对。
var ErrStun = errors.New("STUN 应答不对")

// ParseStunResponse 解析 Binding 成功应答，返回回报的地址（优先 XOR-MAPPED-ADDRESS）。
func ParseStunResponse(b []byte, tx [12]byte) (netip.AddrPort, error) {
	if len(b) < 20 || binary.BigEndian.Uint16(b[0:]) != 0x0101 || binary.BigEndian.Uint32(b[4:]) != stunMagic || [12]byte(b[8:20]) != tx {
		return netip.AddrPort{}, ErrStun
	}
	n := int(binary.BigEndian.Uint16(b[2:]))
	if 20+n > len(b) {
		return netip.AddrPort{}, ErrStun
	}
	attrs := b[20 : 20+n]
	var mapped netip.AddrPort
	for len(attrs) >= 4 {
		typ, l := binary.BigEndian.Uint16(attrs[0:]), int(binary.BigEndian.Uint16(attrs[2:]))
		if 4+l > len(attrs) {
			return netip.AddrPort{}, ErrStun
		}
		v := attrs[4 : 4+l]
		switch typ {
		case 0x0020: // XOR-MAPPED-ADDRESS
			if ap, ok := stunAddr(v, true, tx); ok {
				return ap, nil
			}
		case 0x0001: // MAPPED-ADDRESS
			if ap, ok := stunAddr(v, false, tx); ok {
				mapped = ap
			}
		}
		pad := (l + 3) &^ 3
		if 4+pad > len(attrs) {
			break
		}
		attrs = attrs[4+pad:]
	}
	if mapped.IsValid() {
		return mapped, nil
	}
	return netip.AddrPort{}, ErrStun
}

func stunAddr(v []byte, xor bool, tx [12]byte) (netip.AddrPort, bool) {
	if len(v) < 8 {
		return netip.AddrPort{}, false
	}
	port := binary.BigEndian.Uint16(v[2:])
	if xor {
		port ^= uint16(stunMagic >> 16)
	}
	switch v[1] {
	case 0x01:
		var a [4]byte
		copy(a[:], v[4:8])
		if xor {
			binary.BigEndian.PutUint32(a[:], binary.BigEndian.Uint32(a[:])^stunMagic)
		}
		return netip.AddrPortFrom(netip.AddrFrom4(a), port), true
	case 0x02:
		if len(v) < 20 {
			return netip.AddrPort{}, false
		}
		var a [16]byte
		copy(a[:], v[4:20])
		if xor {
			var key [16]byte
			binary.BigEndian.PutUint32(key[:], stunMagic)
			copy(key[4:], tx[:])
			for i := range a {
				a[i] ^= key[i]
			}
		}
		return netip.AddrPortFrom(netip.AddrFrom16(a), port), true
	}
	return netip.AddrPort{}, false
}

// stunQuery 在一条已连好的 UDP 连接上做一次 Binding。
func stunQuery(ctx context.Context, c net.Conn, timeout time.Duration) (netip.AddrPort, error) {
	defer c.Close()
	dl := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(dl) {
		dl = d
	}
	_ = c.SetDeadline(dl)
	req, tx := StunRequest()
	if _, err := c.Write(req); err != nil {
		return netip.AddrPort{}, err
	}
	buf := make([]byte, 1500)
	n, err := c.Read(buf)
	if err != nil {
		return netip.AddrPort{}, err
	}
	return ParseStunResponse(buf[:n], tx)
}

// RealIP 持有本机真实公网 IP（m4-plan v2.7 决定 5）：只在内存里，不落盘、不进日志、不进 status。
// 外面只能拿去做“相等”的比较；String、GoString、MarshalJSON 都不输出地址。
type RealIP struct {
	// Dial 经直连拨号模块按域名取一个连到 STUN 服务器的 UDP 连接（envcheck 里用到直连的只有这一处）。
	// DialServer 非 nil 时不用它。
	Dial func(ctx context.Context) (net.Conn, error)
	// DialServer 经直连拨号模块取一个连到 host:port 的 UDP 连接；Measure 按 Servers（nil 用 RealStunServers）
	// 依次试，测到为止。2026-11-28 真机：只问 stun.l.google.com 时国内直连没有应答，真实 IP 一直测不到，08 无法判断。
	DialServer func(ctx context.Context, host string, port uint16) (net.Conn, error)
	Servers    []string

	mu sync.Mutex
	ip netip.Addr
}

// Measure 测一次（启用时、网络变化时）。测不到时清掉旧值。
func (r *RealIP) Measure(ctx context.Context) {
	var ip netip.Addr
	switch {
	case r.DialServer != nil:
		servers := r.Servers
		if servers == nil {
			servers = RealStunServers
		}
		for _, s := range servers {
			if ctx.Err() != nil {
				break
			}
			host, ps, err := net.SplitHostPort(s)
			if err != nil {
				continue
			}
			port, err := strconv.ParseUint(ps, 10, 16)
			if err != nil {
				continue
			}
			c, err := r.DialServer(ctx, host, uint16(port))
			if err != nil {
				continue
			}
			ap, err := stunQuery(ctx, c, 3*time.Second)
			if err == nil {
				ip = ap.Addr().Unmap()
				break
			}
		}
	case r.Dial != nil:
		if c, err := r.Dial(ctx); err == nil {
			if ap, err := stunQuery(ctx, c, 3*time.Second); err == nil {
				ip = ap.Addr().Unmap()
			}
		}
	}
	r.mu.Lock()
	r.ip = ip
	r.mu.Unlock()
}

// String、GoString、MarshalJSON 不输出地址（防止被 fmt、日志、JSON 意外带出去）。
func (r *RealIP) String() string               { return "RealIP(不显示)" }
func (r *RealIP) GoString() string             { return r.String() }
func (r *RealIP) MarshalJSON() ([]byte, error) { return []byte(`"不显示"`), nil }

// Equal 报告 a 是否就是本机真实公网 IP；known 为 false 表示没测到。
func (r *RealIP) Equal(a netip.Addr) (eq, known bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.ip.IsValid() {
		return false, false
	}
	return r.ip == a.Unmap(), true
}

// RealStunServers 是测本机真实公网 IP（直连）用的 STUN 服务器：国内的在前（直连能通），国外的兜底。
var RealStunServers = []string{"stun.miwifi.com:3478", "stun.chat.bilibili.com:3478", "stun.cloudflare.com:3478", "stun.l.google.com:19302"}

// DefaultStunServers 是第 08 项的 STUN 服务器（全球两个、国内两个；可配置）。
var DefaultStunServers = []string{"stun.l.google.com:19302", "stun.cloudflare.com:3478", "stun.miwifi.com:3478", "stun.chat.bilibili.com:3478"}

// STUNDeps 是 08 的依赖。
type STUNDeps struct {
	Servers        []string
	Dial           func(ctx context.Context, network, addr string) (net.Conn, error) // nil 用 ProbeDial
	Real           *RealIP
	Exit           *ExitProbe
	UDPUnsupported func() bool // 主代理已知不支持 UDP
	Timeout        time.Duration
	// Resolve 把服务器的域名解析成真实地址（经主代理的 DoH）；nil 时按域名拨（经系统解析器得到 fake 地址，主代理收到的是域名）。
	// 2026-11-27 真机：主代理是“中转 → SOCKS5”的链路时，目标是域名的 UDP 没有应答（目标是 IP 的照常），08 无法判断、启用回滚。
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// ProxiedUDP 返回网络栈经主代理开的 UDP 流的累计数（nil 时不用）。全部没有应答时，这一轮里它涨了，说明 STUN 请求确实
	// 经主代理发出去了：主代理不转发这类 UDP（2026-11-27 真机：链式主代理只有 DNS 的 UDP 有应答），WebRTC 拿不到地址，
	// 也没有绕开主代理的路（01 已通过），判通过；没涨（请求根本没发出去，例如探测窗口没开）仍是无法判断。
	ProxiedUDP func() int64
}

// stunTarget 把 "host:port" 换成真实地址（Resolve 为 nil、已经是 IP 时原样返回）。
func stunTarget(ctx context.Context, resolve func(context.Context, string) ([]netip.Addr, error), s string) (string, bool) {
	host, port, err := net.SplitHostPort(s)
	if err != nil || resolve == nil {
		return s, err == nil
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return s, true
	}
	addrs, err := resolve(ctx, host)
	if err != nil || len(addrs) == 0 {
		return "", false
	}
	return net.JoinHostPort(addrs[0].Unmap().String(), port), true
}

// STUN 是 08 WebRTC（硬性，要发网络探测）。
func STUN(d STUNDeps) Item {
	return Item{ID: "08", Name: "WebRTC（STUN）", Level: Hard, Network: true, Run: func(ctx context.Context) Outcome {
		dial := d.Dial
		if dial == nil {
			dial = ProbeDial
		}
		servers := d.Servers
		if servers == nil {
			servers = DefaultStunServers
		}
		to := d.Timeout
		if to <= 0 {
			to = 3 * time.Second
		}
		var got []netip.Addr
		var before int64
		if d.ProxiedUDP != nil {
			before = d.ProxiedUDP()
		}
		for _, s := range servers {
			target, ok := stunTarget(ctx, d.Resolve, s)
			if !ok {
				continue
			}
			c, err := dial(ctx, "udp", target)
			if err != nil {
				continue
			}
			if ap, err := stunQuery(ctx, c, to); err == nil {
				got = append(got, ap.Addr().Unmap())
			}
		}
		if len(got) == 0 {
			if d.UDPUnsupported != nil && d.UDPUnsupported() {
				return pass("主代理不支持 UDP，STUN 全部没有应答（WebRTC 不会泄漏）")
			}
			if d.ProxiedUDP != nil && d.ProxiedUDP() > before {
				return pass("STUN 请求都经主代理发出、没有应答（主代理不转发这类 UDP）：WebRTC 拿不到地址，不会泄漏")
			}
			return unk("无法判断：STUN 服务器都没有应答")
		}
		realUnknown, countryUnknown := false, false
		exitCountry := ""
		if d.Exit != nil {
			exitCountry = d.Exit.Report(ctx).Country
		}
		for _, a := range got {
			if d.Real != nil {
				eq, known := d.Real.Equal(a)
				if eq {
					return fail("WebRTC 暴露了本机真实公网 IP", "检查有没有别的软件或浏览器设置让 UDP 绕过了 Claude 出国；关闭浏览器的 WebRTC 或换支持 UDP 的节点。")
				}
				if !known {
					realUnknown = true
				}
			} else {
				realUnknown = true
			}
			if ml, err := exitpolicy.InMainland(a); err == nil && ml {
				return fail("WebRTC 回报的地址在中国大陆", "主代理的 UDP 出口在大陆：换一个节点。")
			}
			if exitCountry == "" || d.Exit == nil {
				countryUnknown = true
				continue
			}
			c, err := d.Exit.Locate(ctx, a)
			switch {
			case err != nil || c == "":
				countryUnknown = true
			case c != exitCountry:
				return fail("WebRTC 回报的地址和出口不在同一个国家", "主代理的 UDP 出口和 TCP 出口不在同一个国家：换一个节点。")
			}
		}
		if realUnknown || countryUnknown {
			return unk("无法判断：本机真实公网 IP 或回报地址的国家没能确定")
		}
		return pass("WebRTC 回报的地址和出口一致（同一个国家，不在大陆，不是本机真实 IP）")
	}}
}
