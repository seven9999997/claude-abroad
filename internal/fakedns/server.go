package fakedns

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
)

// DNS 服务的常量（设计 4.3）。
const (
	// FakeTTL 是假地址应答的 TTL。
	FakeTTL = 1
	// UpstreamTimeout 是交给主代理解析的单次上限。
	UpstreamTimeout = 5 * time.Second
	// MaxInflightUDP 是同时处理的 UDP 查询上限（超过的丢弃，客户端会重试）。
	MaxInflightUDP = 256
	// TCPIdle 是 TCP 连接两次查询之间的最长空闲。
	TCPIdle = 10 * time.Second
	// MaxTCPConns 是同时服务的 TCP 连接上限（超过的立即关闭，防止耗尽 goroutine 和文件描述符）。
	MaxTCPConns = 64
)

// fakeNet 是整个 fake-IP 网段：其中地址的 PTR 一律 NXDOMAIN（不回显我们分配的域名）。
var fakeNet = netip.MustParsePrefix("198.18.0.0/15")

// Upstream 是交给主代理解析的出口：经虚拟机里主代理的 DoH（真实实现在 M2-5/M2-9 接）。
// 没有任何退回本机解析器的路径。
type Upstream interface {
	Exchange(ctx context.Context, q *dns.Msg) (*dns.Msg, error)
}

// Server 是 198.18.0.2:53 上的 fake-IP DNS（设计 4.3）：
//   - A（不在 Bypass 名单）：分配假地址，TTL 1 秒；池满回 SERVFAIL；
//   - AAAA、HTTPS（65）、SVCB（64）：本地回空的 NOERROR（名单内的域名也一样）；
//   - 198.18.0.0/15 的 PTR：NXDOMAIN；
//   - 名单内域名的 A：交 Upstream，答出的地址记进 Reverse；
//   - 其他类型、不合法的域名：交 Upstream；
//   - Upstream 出错、超时或为 nil：SERVFAIL；
//   - 多问题：FORMERR；非 IN 类、opcode 不是 QUERY、ANY：NOTIMP。都不问 Upstream；
//   - Upstream 的应答里有落在 198.18.0.0/15 的 A（上游也是 fake-IP 解析器）：整个改成 SERVFAIL，不进 Reverse。
//
// 统计只有计数，日志不写域名（本包不打日志）。
type Server struct {
	Pool     *Pool
	Upstream Upstream
	Bypass   *Bypass
	Reverse  *Reverse
	Now      func() time.Time

	st serverStats
}

type serverStats struct {
	a, aaaa, https, ptr, ptrLocal, other, servfail, upstreamFail, poolFull, formErr, upstreamFake, tcpRejected atomic.Int64
}

// ServerStats 是 DNS 服务的计数（不含域名）。
type ServerStats struct {
	A            int64 `json:"a"`
	AAAA         int64 `json:"aaaa"`
	HTTPS        int64 `json:"https_svcb"`
	PTR          int64 `json:"ptr"`
	PTRLocal     int64 `json:"ptr_local"` // 私有网段的反查，本地回 NXDOMAIN
	Other        int64 `json:"other"`
	ServFail     int64 `json:"servfail"`
	UpstreamFail int64 `json:"upstream_fail"`
	PoolFull     int64 `json:"pool_full"`
	FormErr      int64 `json:"formerr"`
	UpstreamFake int64 `json:"upstream_fake"` // 上游应答里有 fake 段地址
	TCPRejected  int64 `json:"tcp_rejected"`  // 超过 MaxTCPConns 被关的连接
}

// Stats 返回计数快照。
func (s *Server) Stats() ServerStats {
	return ServerStats{A: s.st.a.Load(), AAAA: s.st.aaaa.Load(), HTTPS: s.st.https.Load(), PTR: s.st.ptr.Load(),
		Other: s.st.other.Load(), ServFail: s.st.servfail.Load(), UpstreamFail: s.st.upstreamFail.Load(),
		PoolFull: s.st.poolFull.Load(), FormErr: s.st.formErr.Load(), UpstreamFake: s.st.upstreamFake.Load(),
		TCPRejected: s.st.tcpRejected.Load(), PTRLocal: s.st.ptrLocal.Load()}
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) reply(q *dns.Msg, rcode int) *dns.Msg {
	m := new(dns.Msg)
	m.SetRcode(q, rcode)
	m.RecursionAvailable = true
	if rcode == dns.RcodeServerFailure {
		s.st.servfail.Add(1)
	}
	return m
}

// Handle 处理一个查询，总是返回应答（ID 和问题段与请求相同）。
func (s *Server) Handle(ctx context.Context, q *dns.Msg) *dns.Msg {
	if q.Opcode != dns.OpcodeQuery {
		s.st.formErr.Add(1)
		return s.reply(q, dns.RcodeNotImplemented)
	}
	if len(q.Question) != 1 {
		s.st.formErr.Add(1)
		m := new(dns.Msg)
		m.SetRcodeFormatError(q)
		m.RecursionAvailable = true
		return m
	}
	qs := q.Question[0]
	if qs.Qclass != dns.ClassINET {
		s.st.formErr.Add(1)
		return s.reply(q, dns.RcodeNotImplemented)
	}
	switch qs.Qtype {
	case dns.TypeANY: // 会带回真实的 A、AAAA、HTTPS，绕开假地址
		s.st.formErr.Add(1)
		return s.reply(q, dns.RcodeNotImplemented)
	case dns.TypeAAAA:
		s.st.aaaa.Add(1)
		return s.reply(q, dns.RcodeSuccess) // IPv6 已关闭
	case dns.TypeHTTPS, dns.TypeSVCB:
		s.st.https.Add(1)
		return s.reply(q, dns.RcodeSuccess) // 真实记录带 ipv4hint、ECH，会绕开假地址
	case dns.TypePTR:
		s.st.ptr.Add(1)
		if a, ok := ptrAddr(qs.Name); ok && fakeNet.Contains(a) {
			return s.reply(q, dns.RcodeNameError)
		}
		if privatePTR(qs.Name) { // 私有网段的反查不发给主代理（m3-plan M3-4）
			s.st.ptrLocal.Add(1)
			return s.reply(q, dns.RcodeNameError)
		}
		return s.upstream(ctx, q, "")
	case dns.TypeA:
		s.st.a.Add(1)
		name, err := Normalize(qs.Name)
		if err != nil {
			return s.upstream(ctx, q, "") // 不合法的名字不分配假地址
		}
		// 本地域名（local_domains）一律分配假地址，优先于“不使用假地址”的名单：
		// 否则 *.lan、*.local 这类名字会被交给主代理解析，而主代理解析不了内网的名字。
		if s.Bypass.Match(name) && (s.Pool == nil || !s.Pool.IsLocalName(name)) {
			return s.upstream(ctx, q, name)
		}
		if s.Pool == nil {
			return s.reply(q, dns.RcodeServerFailure)
		}
		addr, err := s.Pool.Assign(name, s.now())
		if err != nil {
			if errors.Is(err, ErrPoolFull) {
				s.st.poolFull.Add(1)
			}
			return s.reply(q, dns.RcodeServerFailure)
		}
		m := s.reply(q, dns.RcodeSuccess)
		m.Answer = []dns.RR{&dns.A{
			Hdr: dns.RR_Header{Name: qs.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: FakeTTL},
			A:   addr.AsSlice(),
		}}
		return m
	default:
		s.st.other.Add(1)
		return s.upstream(ctx, q, "")
	}
}

// upstream 交给 Upstream 解析。bypassName 非空时，把应答里的 A 记进 Reverse。
func (s *Server) upstream(ctx context.Context, q *dns.Msg, bypassName string) *dns.Msg {
	if s.Upstream == nil {
		s.st.upstreamFail.Add(1)
		return s.reply(q, dns.RcodeServerFailure)
	}
	ctx, cancel := context.WithTimeout(ctx, UpstreamTimeout)
	defer cancel()
	type result struct {
		m   *dns.Msg
		err error
	}
	ch := make(chan result, 1)
	go func() {
		m, err := s.Upstream.Exchange(ctx, q.Copy())
		ch <- result{m, err}
	}()
	var r result
	select {
	case r = <-ch:
	case <-ctx.Done():
		r.err = ctx.Err()
	}
	if r.err != nil || r.m == nil {
		s.st.upstreamFail.Add(1)
		return s.reply(q, dns.RcodeServerFailure)
	}
	for _, rr := range r.m.Answer {
		if a, ok := rr.(*dns.A); ok {
			if ip, ok := netip.AddrFromSlice(a.A); ok && fakeNet.Contains(ip.Unmap()) {
				// 上游也是 fake-IP 解析器：应用连这个地址时，我们的池会把它当成别的域名。
				s.st.upstreamFake.Add(1)
				return s.reply(q, dns.RcodeServerFailure)
			}
		}
	}
	m := r.m.Copy()
	m.Id, m.Question, m.Response, m.RecursionAvailable, m.Opcode = q.Id, q.Question, true, true, q.Opcode
	if bypassName != "" && s.Reverse != nil {
		now := s.now()
		for _, rr := range m.Answer {
			if a, ok := rr.(*dns.A); ok {
				if ip, ok := netip.AddrFromSlice(a.A); ok {
					s.Reverse.Put(ip, bypassName, time.Duration(a.Hdr.Ttl)*time.Second, now)
				}
			}
		}
	}
	return m
}

// ptrAddr 把 d.c.b.a.in-addr.arpa. 换成 IPv4 地址。
func ptrAddr(name string) (netip.Addr, bool) {
	n := strings.ToLower(strings.TrimSuffix(name, "."))
	rest, ok := strings.CutSuffix(n, ".in-addr.arpa")
	if !ok {
		return netip.Addr{}, false
	}
	parts := strings.Split(rest, ".")
	if len(parts) != 4 {
		return netip.Addr{}, false
	}
	var b [4]byte
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 || v > 255 || p != strconv.Itoa(v) {
			return netip.Addr{}, false
		}
		b[3-i] = byte(v)
	}
	return netip.AddrFrom4(b), true
}

// udpSize 是对方能收的 UDP 应答大小（EDNS 声明的，至少 512）。
func udpSize(q *dns.Msg) int {
	if opt := q.IsEdns0(); opt != nil {
		return max(int(opt.UDPSize()), dns.MinMsgSize)
	}
	return dns.MinMsgSize
}

// HandlePacket 处理一个 UDP 查询包：解包 → Handle → 按对方的 EDNS 大小截断（置 TC）→ 打包。
// 解不开或是应答包时返回 nil。给 ServeUDP 和网络栈（M2-5，198.18.0.2:53 的流量来自 gVisor）共用。
func (s *Server) HandlePacket(ctx context.Context, in []byte) []byte {
	q := new(dns.Msg)
	if q.Unpack(in) != nil || q.Response {
		return nil
	}
	m := s.Handle(ctx, q)
	m.Truncate(udpSize(q))
	b, err := m.Pack()
	if err != nil {
		return nil
	}
	return b
}

// ServeUDP 在 pc 上服务，直到 ctx 结束（会关掉 pc）。超过 MaxInflightUDP 的并发查询丢弃。
func (s *Server) ServeUDP(ctx context.Context, pc net.PacketConn) error {
	stop := context.AfterFunc(ctx, func() { pc.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	sem := make(chan struct{}, MaxInflightUDP)
	buf := make([]byte, dns.MaxMsgSize)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		select {
		case sem <- struct{}{}:
		default:
			continue
		}
		in := append([]byte(nil), buf[:n]...)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if b := s.HandlePacket(ctx, in); b != nil {
				_, _ = pc.WriteTo(b, from)
			}
		}()
	}
}

// ServeConn 服务一条 TCP 连接（DNS over TCP：两字节长度前缀），直到对方关闭、出错、空闲超过 TCPIdle
// 或 ctx 结束。不负责关闭 c 之外的东西；返回前关闭 c。
func (s *Server) ServeConn(ctx context.Context, c net.Conn) {
	defer c.Close()
	stopc := context.AfterFunc(ctx, func() { c.Close() })
	defer stopc()
	dc := &dns.Conn{Conn: c}
	for {
		_ = c.SetReadDeadline(time.Now().Add(TCPIdle))
		q, err := dc.ReadMsg()
		if err != nil {
			return
		}
		m := s.Handle(ctx, q)
		_ = c.SetWriteDeadline(time.Now().Add(TCPIdle))
		if dc.WriteMsg(m) != nil {
			return
		}
	}
}

// ServeTCP 在 ln 上服务，直到 ctx 结束（会关掉 ln 和所有连接）。同时最多 MaxTCPConns 条连接，超过的立即关闭。
func (s *Server) ServeTCP(ctx context.Context, ln net.Listener) error {
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	sem := make(chan struct{}, MaxTCPConns)
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		select {
		case sem <- struct{}{}:
		default:
			s.st.tcpRejected.Add(1)
			c.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			s.ServeConn(ctx, c)
		}()
	}
}

// privatePTR 判断反查名是否落在私有网段（10/8、172.16/12、192.168/16、169.254/16），含只给出前几段的区名
// （例如 168.192.in-addr.arpa.）。只给出 172 这一段时不算（它也覆盖公网地址）。
func privatePTR(name string) bool {
	n := strings.ToLower(strings.TrimSuffix(name, "."))
	rest, ok := strings.CutSuffix(n, ".in-addr.arpa")
	if !ok || rest == "" {
		return false
	}
	parts := strings.Split(rest, ".")
	if len(parts) > 4 {
		return false
	}
	oct := make([]int, len(parts)) // oct[0] 是第一段（地址的最高字节）
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 || v > 255 || p != strconv.Itoa(v) {
			return false
		}
		oct[len(parts)-1-i] = v
	}
	switch {
	case oct[0] == 10:
		return true
	case len(oct) >= 2 && oct[0] == 172 && oct[1] >= 16 && oct[1] <= 31:
		return true
	case len(oct) >= 2 && oct[0] == 192 && oct[1] == 168:
		return true
	case len(oct) >= 2 && oct[0] == 169 && oct[1] == 254:
		return true
	}
	return false
}
