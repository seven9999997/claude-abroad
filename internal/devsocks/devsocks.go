// Package devsocks 是开发工具 abroad-dev 的本地 SOCKS5 入口（不进发布包）：
// 只听 127.0.0.1，把 CONNECT 和 UDP ASSOCIATE 经 data.sock（wire/yamux）交给虚拟机里的数据面。
// 目标是域名时按域名原样放进帧头，不在本机解析（设计 4.4）。不需要认证（只在本机回环上）。
package devsocks

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"claude-abroad/internal/wire"
)

// App 是开发入口在帧头里填的应用标识。
const App = "dev.claude-abroad.socks"

// Opener 开一条数据流（通常是 (*wire.Dialer).Open）。
type Opener func(ctx context.Context, h wire.Header) (net.Conn, error)

// Server 是本地 SOCKS5 服务。
type Server struct {
	Open        Opener
	Exit        string // 出口标签，默认 main
	OpenTimeout time.Duration
	Logf        func(format string, a ...any)
	// UDPBind 是 UDP ASSOCIATE 的中继地址（零值用 127.0.0.1）；AllowClient 判断 UDP 客户端的地址（nil 只收回环）。
	// 分流出口命名空间里的 socks 服务（M4-7）绑在 veth 的地址上，客户端是根命名空间里的 sing-box。
	UDPBind     netip.Addr
	AllowClient func(netip.Addr) bool
}

func (s *Server) exit() string {
	if s.Exit == "" {
		return "main"
	}
	return s.Exit
}

// SOCKS5 应答码。
const (
	repOK          = 0x00
	repFailure     = 0x01
	repNotAllowed  = 0x02
	repNetUnreach  = 0x03
	repRefused     = 0x05
	repCmdNotSupp  = 0x07
	repAddrNotSupp = 0x08
)

// repFor 把数据面的回应错误码映射成 SOCKS5 应答码。
func repFor(err error) byte {
	var re *wire.ReplyError
	if errors.As(err, &re) {
		switch re.Code {
		case wire.CodeUnknownExit:
			return repNotAllowed
		case wire.CodeExitDown:
			return repNetUnreach
		case wire.CodeDialFailed:
			return repRefused
		case wire.CodeUDPUnsupported:
			return repCmdNotSupp
		}
	}
	return repFailure
}

// Serve 接受连接直到 ctx 结束。
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	stop := context.AfterFunc(ctx, func() { l.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer c.Close()
			cctx, cancel := context.WithCancel(ctx)
			defer cancel()
			stopc := context.AfterFunc(cctx, func() { c.Close() })
			defer stopc()
			s.handle(cctx, c)
		}()
	}
}

func reply(c net.Conn, rep byte, bnd netip.AddrPort) {
	b := []byte{5, rep, 0, 1}
	a := bnd.Addr()
	if !a.Is4() {
		a = netip.IPv4Unspecified()
	}
	a4 := a.As4()
	b = append(b, a4[:]...)
	b = binary.BigEndian.AppendUint16(b, bnd.Port())
	_, _ = c.Write(b)
}

// readAddr 读 ATYP + 地址 + 端口，返回 (host, ip, port)。
func readAddr(r io.Reader) (string, netip.Addr, int, error) {
	var t [1]byte
	if _, err := io.ReadFull(r, t[:]); err != nil {
		return "", netip.Addr{}, 0, err
	}
	var host string
	var ip netip.Addr
	switch t[0] {
	case 1:
		var a [4]byte
		if _, err := io.ReadFull(r, a[:]); err != nil {
			return "", ip, 0, err
		}
		ip = netip.AddrFrom4(a)
	case 4:
		var a [16]byte
		if _, err := io.ReadFull(r, a[:]); err != nil {
			return "", ip, 0, err
		}
		ip = netip.AddrFrom16(a).Unmap()
	case 3:
		var n [1]byte
		if _, err := io.ReadFull(r, n[:]); err != nil {
			return "", ip, 0, err
		}
		b := make([]byte, n[0])
		if _, err := io.ReadFull(r, b); err != nil {
			return "", ip, 0, err
		}
		host = string(b)
		if a, err := netip.ParseAddr(host); err == nil { // ATYP=3 里装的是 IP 字面量：按 IP 处理
			host, ip = "", a.Unmap()
		}
	default:
		return "", ip, 0, fmt.Errorf("地址类型 %d 不支持", t[0])
	}
	var p [2]byte
	if _, err := io.ReadFull(r, p[:]); err != nil {
		return "", ip, 0, err
	}
	return host, ip, int(binary.BigEndian.Uint16(p[:])), nil
}

func (s *Server) header(network, host string, ip netip.Addr, port int) wire.Header {
	h := wire.Header{V: wire.Version, Exit: s.exit(), Net: network, Port: port, App: App}
	if host != "" {
		h.Host = host // 域名原样交给虚拟机，不在本机解析
	} else {
		h.IP = ip.String()
	}
	return h
}

func (s *Server) open(ctx context.Context, h wire.Header) (net.Conn, error) {
	t := s.OpenTimeout
	if t <= 0 {
		t = 15 * time.Second
	}
	octx, cancel := context.WithTimeout(ctx, t)
	defer cancel()
	return s.Open(octx, h)
}

func (s *Server) handle(ctx context.Context, c net.Conn) {
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	var g [2]byte
	if _, err := io.ReadFull(c, g[:]); err != nil || g[0] != 5 {
		return
	}
	methods := make([]byte, g[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return
	}
	noAuth := false
	for _, m := range methods {
		noAuth = noAuth || m == 0
	}
	if !noAuth {
		_, _ = c.Write([]byte{5, 0xff})
		return
	}
	_, _ = c.Write([]byte{5, 0})
	var req [3]byte
	if _, err := io.ReadFull(c, req[:]); err != nil || req[0] != 5 {
		return
	}
	host, ip, port, err := readAddr(c)
	if err != nil {
		reply(c, repAddrNotSupp, netip.AddrPort{})
		return
	}
	switch req[1] {
	case 1:
		st, err := s.open(ctx, s.header(wire.TCP, host, ip, port))
		if err != nil {
			reply(c, repFor(err), netip.AddrPort{})
			return
		}
		defer st.Close()
		_ = c.SetDeadline(time.Time{})
		reply(c, repOK, netip.AddrPort{})
		pipe(c, st)
	case 3:
		_ = c.SetDeadline(time.Time{})
		s.associate(ctx, c)
	default:
		reply(c, repCmdNotSupp, netip.AddrPort{})
	}
}

type closeWriter interface{ CloseWrite() error }

// pipe 双向转发；一个方向读到 EOF 就半关闭另一端（yamux 流的 Close 即半关闭）。
func pipe(a, b net.Conn) {
	var wg sync.WaitGroup
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(closeWriter); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	wg.Add(2)
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}

// associate 处理 UDP ASSOCIATE：在 127.0.0.1 上开一个中继端口；每个目标一条数据流（五元组），
// 控制连接断开时全部结束。
func (s *Server) associate(ctx context.Context, c net.Conn) {
	bind := net.IPv4(127, 0, 0, 1)
	if s.UDPBind.IsValid() {
		bind = net.IP(s.UDPBind.AsSlice())
	}
	u, err := net.ListenUDP("udp", &net.UDPAddr{IP: bind})
	if err != nil {
		reply(c, repFailure, netip.AddrPort{})
		return
	}
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer u.Close()
	reply(c, repOK, u.LocalAddr().(*net.UDPAddr).AddrPort())
	go func() { _, _ = io.Copy(io.Discard, c); cancel(); u.Close() }()

	type stream struct {
		dc *wire.DatagramConn
		st net.Conn
	}
	var mu sync.Mutex
	streams := map[string]*stream{}
	defer func() {
		mu.Lock()
		for _, x := range streams {
			x.st.Close()
		}
		mu.Unlock()
	}()
	var client *net.UDPAddr
	buf := make([]byte, 65536)
	for {
		n, from, err := u.ReadFromUDP(buf)
		if err != nil {
			return
		}
		allowed := from.IP.IsLoopback()
		if s.AllowClient != nil {
			a, _ := netip.AddrFromSlice(from.IP)
			allowed = s.AllowClient(a.Unmap())
		}
		if !allowed || (client != nil && !from.IP.Equal(client.IP)) {
			continue
		}
		client = from
		p := buf[:n]
		if len(p) < 4 || p[0] != 0 || p[1] != 0 || p[2] != 0 { // 不支持分片
			continue
		}
		r := &sliceReader{b: p[3:]}
		host, ip, port, err := readAddr(r)
		if err != nil {
			continue
		}
		key := host + "|" + ip.String() + "|" + strconv.Itoa(port)
		mu.Lock()
		x := streams[key]
		mu.Unlock()
		if x == nil {
			st, err := s.open(actx, s.header(wire.UDP, host, ip, port))
			if err != nil {
				continue
			}
			x = &stream{dc: wire.NewDatagramConn(st), st: st}
			mu.Lock()
			streams[key] = x
			mu.Unlock()
			hdr := socksUDPHeader(host, ip, port)
			to := client
			go func() {
				rb := make([]byte, wire.MaxDatagram)
				for {
					m, err := x.dc.ReadDatagram(rb)
					if err != nil {
						// 流结束（例如数据面空闲超时）：关掉并移出，下一个发往同一目标的数据报会重开。
						x.st.Close()
						mu.Lock()
						if streams[key] == x {
							delete(streams, key)
						}
						mu.Unlock()
						return
					}
					_, _ = u.WriteToUDP(append(append([]byte(nil), hdr...), rb[:m]...), to)
				}
			}()
		}
		if err := x.dc.WriteDatagram(r.b); err != nil {
			continue
		}
	}
}

func socksUDPHeader(host string, ip netip.Addr, port int) []byte {
	b := []byte{0, 0, 0}
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

type sliceReader struct{ b []byte }

func (r *sliceReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}
