package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"claude-abroad/internal/wire"
)

// ExitTable 告诉数据面一个出口标签是否存在、是否可用。M1 只有 main，M4 再扩。
type ExitTable interface {
	Lookup(tag string) (known, up bool)
}

// DataTiming 是数据面的时长和上限（设计文档 4.4、5.3）。
type DataTiming struct {
	HeaderTimeout   time.Duration // 读帧头的上限
	DialTimeout     time.Duration // 经出口拨号的上限，必须短于宿主的 OpenTimeout（10 秒）
	UDPIdle         time.Duration // UDP 流空闲这么久就关闭
	HalfCloseLinger time.Duration // 一个方向结束后，另一方向最多再保留这么久
	MaxSessions     int           // 同时的 yamux 会话数
	MaxStreams      int           // 同时的流数
}

// DefaultDataTiming 是正式取值。
var DefaultDataTiming = DataTiming{
	HeaderTimeout:   5 * time.Second,
	DialTimeout:     8 * time.Second,
	UDPIdle:         120 * time.Second,
	HalfCloseLinger: 5 * time.Minute,
	MaxSessions:     4,
	MaxStreams:      4096,
}

// DataStats 是 /v1/health 的 data 段。不含目标和应用。
type DataStats struct {
	Sessions      int64            `json:"sessions"`
	ActiveStreams int64            `json:"active_streams"`
	TotalStreams  int64            `json:"total_streams"`
	Rejects       map[string]int64 `json:"rejects"`
	UDPDropped    int64            `json:"udp_dropped"`
}

// DataPlane 是 vsock 1080 上的数据面（设计文档 4.4）：每条连接是一个 yamux 会话，
// 每条流先读帧头，再以 auth_user=<出口> 连 sing-box 的 socks 入站（唯一允许的拨号目标），
// 回应帧之后转发 TCP 字节流或 UDP 数据报。任何失败都不直连、不换出口。
//
// 默认级别的日志不写目标域名、目标 IP、应用，只记错误码计数。
type DataPlane struct {
	Exits   ExitTable
	Socks   func(exit string) SocksDialer
	Session wire.SessionConfig
	Timing  DataTiming
	Logf    func(format string, a ...any)

	sessions, active, total, udpDropped atomic.Int64
	mu                                  sync.Mutex
	rejects                             map[wire.Code]int64
}

// Stats 返回计数快照。
func (d *DataPlane) Stats() DataStats {
	d.mu.Lock()
	r := map[string]int64{}
	for k, v := range d.rejects {
		r[string(k)] = v
	}
	d.mu.Unlock()
	return DataStats{Sessions: d.sessions.Load(), ActiveStreams: d.active.Load(), TotalStreams: d.total.Load(),
		Rejects: r, UDPDropped: d.udpDropped.Load()}
}

func (d *DataPlane) timing() DataTiming {
	t, def := d.Timing, DefaultDataTiming
	for _, f := range []struct{ v, def *time.Duration }{
		{&t.HeaderTimeout, &def.HeaderTimeout}, {&t.DialTimeout, &def.DialTimeout},
		{&t.UDPIdle, &def.UDPIdle}, {&t.HalfCloseLinger, &def.HalfCloseLinger},
	} {
		if *f.v <= 0 {
			*f.v = *f.def
		}
	}
	if t.MaxSessions <= 0 {
		t.MaxSessions = def.MaxSessions
	}
	if t.MaxStreams <= 0 {
		t.MaxStreams = def.MaxStreams
	}
	return t
}

func (d *DataPlane) logf(format string, a ...any) {
	if d.Logf != nil {
		d.Logf(format, a...)
	}
}

// Serve 在 l 上接受宿主的连接，直到 ctx 结束；结束时关掉全部会话、流和出站，等它们退出后返回。
func (d *DataPlane) Serve(ctx context.Context, l net.Listener) error {
	t := d.timing()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		live = map[*yamux.Session]bool{}
	)
	stop := context.AfterFunc(ctx, func() {
		l.Close()
		mu.Lock()
		for s := range live {
			s.Close()
		}
		mu.Unlock()
	})
	defer stop()
	var err error
	for {
		c, aerr := l.Accept()
		if aerr != nil {
			if ctx.Err() == nil && !errors.Is(aerr, net.ErrClosed) {
				var ne net.Error
				if errors.As(aerr, &ne) && ne.Temporary() { //nolint:staticcheck // vsock 监听器把偶发错误标成临时
					time.Sleep(50 * time.Millisecond)
					continue
				}
				err = aerr
				mu.Lock() // 监听器坏了：现有会话也一并关掉，Serve 返回错误
				for s := range live {
					s.Close()
				}
				mu.Unlock()
			}
			break
		}
		if d.sessions.Load() >= int64(t.MaxSessions) {
			d.logf("数据面：会话数已达上限 %d，拒绝新连接", t.MaxSessions)
			c.Close()
			continue
		}
		sess, serr := wire.Server(c, d.Session)
		if serr != nil {
			c.Close()
			continue
		}
		mu.Lock()
		if ctx.Err() != nil {
			mu.Unlock()
			sess.Close()
			break
		}
		live[sess] = true
		mu.Unlock()
		d.sessions.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer d.sessions.Add(-1)
			d.serveSession(ctx, sess, t)
			mu.Lock()
			delete(live, sess)
			mu.Unlock()
		}()
	}
	l.Close()
	wg.Wait()
	return err
}

func (d *DataPlane) serveSession(ctx context.Context, sess *yamux.Session, t DataTiming) {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer sess.Close()
	go func() { // 会话断开时取消这个会话的全部出站
		select {
		case <-sess.CloseChan():
			cancel()
		case <-sctx.Done():
		}
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		st, err := sess.AcceptStream()
		if err != nil {
			cancel()
			return
		}
		d.total.Add(1)
		if d.active.Add(1) > int64(t.MaxStreams) {
			d.active.Add(-1)
			go d.reject(st, wire.CodeDialFailed)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer d.active.Add(-1)
			d.serveStream(sctx, st, t)
		}()
	}
}

func (d *DataPlane) count(code wire.Code) {
	d.mu.Lock()
	if d.rejects == nil {
		d.rejects = map[wire.Code]int64{}
	}
	d.rejects[code]++
	d.mu.Unlock()
}

func (d *DataPlane) reject(st net.Conn, code wire.Code) {
	d.count(code)
	_ = st.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = wire.WriteReply(st, wire.Reply{OK: false, Code: code})
	st.Close()
}

type readResult struct {
	b   []byte
	err error
}

func (d *DataPlane) serveStream(ctx context.Context, st net.Conn, t DataTiming) {
	defer func() {
		// 先打断可能还挂着的读（回应前起的那个 goroutine）：yamux 的 Close 只是半关闭，唤不醒本端的 Read。
		_ = st.SetDeadline(time.Now())
		st.Close()
	}()
	_ = st.SetReadDeadline(time.Now().Add(t.HeaderTimeout))
	h, err := wire.ReadHeader(st)
	_ = st.SetReadDeadline(time.Time{})
	if err != nil {
		d.reject(st, wire.CodeBadRequest)
		return
	}
	known, up := d.Exits.Lookup(h.Exit)
	if !known {
		d.reject(st, wire.CodeUnknownExit)
		return
	}
	if !up {
		d.reject(st, wire.CodeExitDown)
		return
	}
	var ip netip.Addr
	if h.Host == "" {
		ip, err = netip.ParseAddr(h.IP)
		if err != nil {
			d.reject(st, wire.CodeBadRequest)
			return
		}
		ip = ip.Unmap() // ::ffff:1.2.3.4 按 IPv4 处理（ATYP=1，回包来源比较也一致）
	}
	sd := d.Socks(h.Exit)

	// 回应帧之前：盯着流。读到 EOF 或出错就是放弃，立即取消拨号；读到数据先缓存，接通后先发。
	dctx, cancel := context.WithTimeout(ctx, t.DialTimeout)
	defer cancel()
	first := make(chan readResult, 1)
	go func() {
		buf := make([]byte, 32<<10)
		n, err := st.Read(buf)
		if err != nil {
			cancel()
		}
		first <- readResult{buf[:n], err}
	}()

	switch h.Net {
	case wire.TCP:
		out, err := sd.Connect(dctx, h.Host, ip, h.Port)
		if err != nil {
			if d.abandoned(first) {
				d.count("abandoned")
				return
			}
			d.reject(st, wire.CodeDialFailed)
			return
		}
		defer out.Close()
		if d.abandoned(first) {
			d.count("abandoned")
			return
		}
		if err := wire.WriteReply(st, wire.Reply{OK: true}); err != nil {
			return
		}
		d.proxyTCP(ctx, st, out, first, t)
	case wire.UDP:
		u, err := sd.Associate(dctx)
		if err != nil {
			if d.abandoned(first) {
				d.count("abandoned")
				return
			}
			var se *SocksError
			if errors.As(err, &se) && se.Rep == SocksRepCommandNotSupported {
				d.reject(st, wire.CodeUDPUnsupported)
			} else {
				d.reject(st, wire.CodeDialFailed)
			}
			return
		}
		defer u.Close()
		if d.abandoned(first) {
			d.count("abandoned")
			return
		}
		if err := wire.WriteReply(st, wire.Reply{OK: true}); err != nil {
			return
		}
		d.proxyUDP(ctx, st, u, h.Host, ip, h.Port, first, t)
	}
}

// abandoned 看回应帧之前流上是否已经读到 EOF 或错误（不阻塞）。读到的是数据就放回去。
func (d *DataPlane) abandoned(first chan readResult) bool {
	select {
	case r := <-first:
		first <- r
		return r.err != nil && len(r.b) == 0
	default:
		return false
	}
}

type closeWriter interface{ CloseWrite() error }

// closeWrite 半关闭：yamux 的流 Close 就是半关闭（wire 包要求 yamux ≥ v0.1.2）。
func closeWrite(c net.Conn) {
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// proxyTCP 双向转发。一个方向结束就半关闭另一端的写方向；之后另一方向最多再保留 HalfCloseLinger。
func (d *DataPlane) proxyTCP(ctx context.Context, st, out net.Conn, first chan readResult, t DataTiming) {
	done := make(chan struct{}, 2)
	// 流 → 出站：先发回应前缓存的数据。
	go func() {
		defer func() { done <- struct{}{} }()
		r := <-first
		if len(r.b) > 0 {
			if _, err := out.Write(r.b); err != nil {
				out.Close()
				return
			}
		}
		if r.err == nil {
			if _, err := io.Copy(out, st); err != nil && !errors.Is(err, io.EOF) {
				out.Close()
				return
			}
		}
		closeWrite(out)
	}()
	// 出站 → 流。
	go func() {
		defer func() { done <- struct{}{} }()
		if _, err := io.Copy(st, out); err != nil {
			st.Close()
			return
		}
		closeWrite(st) // yamux 流：Close 即半关闭
	}()
	finished := 0
	select {
	case <-done:
		finished++
	case <-ctx.Done():
	}
	if finished == 1 { // 一个方向结束：另一方向最多再保留 HalfCloseLinger
		linger := time.NewTimer(t.HalfCloseLinger)
		select {
		case <-done:
			finished++
		case <-linger.C:
		case <-ctx.Done():
		}
		linger.Stop()
	}
	out.Close()
	_ = st.SetDeadline(time.Now()) // 打断还在读的一方
	st.Close()
	for ; finished < 2; finished++ {
		<-done
	}
}

// proxyUDP 在一条流上转发一个五元组的数据报（2 字节长度 + 数据，见 wire.DatagramConn）。
// 目标是 IP 时只收来源等于目标的回包；目标是域名时不过滤来源。空闲 UDPIdle 后关闭；
// 控制连接断开时结束。
func (d *DataPlane) proxyUDP(ctx context.Context, st net.Conn, u *SocksUDP, host string, ip netip.Addr, port int,
	first chan readResult, t DataTiming) {
	uctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var last atomic.Int64
	touch := func() { last.Store(time.Now().UnixNano()) }
	touch()
	hdr := AppendUDPHeader(nil, host, ip, port)
	want := netip.AddrPortFrom(ip, uint16(port))

	go func() { // 控制连接断开（或 sing-box 关掉关联）就结束
		_, _ = io.Copy(io.Discard, u.Ctrl)
		cancel()
	}()
	go func() { // 空闲超时
		tick := time.NewTicker(min(t.UDPIdle/4, time.Second))
		defer tick.Stop()
		for {
			select {
			case <-uctx.Done():
				return
			case <-tick.C:
				if time.Since(time.Unix(0, last.Load())) >= t.UDPIdle {
					cancel()
					return
				}
			}
		}
	}()
	go func() { // 流 → 出站
		defer cancel()
		// 回应前缓存的那一段按数据报流解析：拼回流的前面。
		r := <-first
		var src io.Reader = st
		if len(r.b) > 0 {
			src = io.MultiReader(&sliceReader{b: r.b}, st)
		} else if r.err != nil {
			return
		}
		dc := wire.NewDatagramConn(struct {
			io.Reader
			io.Writer
		}{src, io.Discard})
		buf := make([]byte, wire.MaxDatagram)
		pkt := make([]byte, 0, MaxSocksUDPPayload)
		for {
			n, err := dc.ReadDatagram(buf)
			if err != nil {
				return
			}
			touch()
			if len(hdr)+n > MaxSocksUDPPayload {
				d.udpDropped.Add(1)
				continue
			}
			pkt = append(append(pkt[:0], hdr...), buf[:n]...)
			if _, err := u.Relay.Write(pkt); err != nil {
				return
			}
		}
	}()
	go func() { // 出站 → 流
		defer cancel()
		dc := wire.NewDatagramConn(st)
		buf := make([]byte, 65536)
		for {
			n, err := u.Relay.Read(buf)
			if err != nil {
				return
			}
			from, data, err := ParseUDPPacket(buf[:n])
			if err != nil {
				d.udpDropped.Add(1)
				continue
			}
			from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
			if host == "" && from != want {
				d.udpDropped.Add(1) // 目标是 IP：只收来自目标的回包
				continue
			}
			touch()
			if err := dc.WriteDatagram(data); err != nil {
				return
			}
		}
	}()
	<-uctx.Done()
	u.Close()
	_ = st.SetDeadline(time.Now())
	st.Close()
}

// MainExits 是 M1 的出口表：只有 main，sing-box 和主代理 mihomo 都在运行时可用。
type MainExits struct{ SingBox, Mihomo *Engine }

func (m MainExits) Lookup(tag string) (known, up bool) {
	if tag != "main" {
		return false, false
	}
	return true, m.SingBox.Status().State == EngineRunning && m.Mihomo.Status().State == EngineRunning
}
