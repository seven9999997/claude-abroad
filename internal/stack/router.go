package stack

import (
	"context"
	"errors"
	"net"
	"net/netip"

	"claude-abroad/internal/directdial"
	"claude-abroad/internal/guestabi"
	"claude-abroad/internal/rules"
	"claude-abroad/internal/sniff"
	"claude-abroad/internal/wire"
)

// 地址判定的结果。
type verdict int

const (
	vReject verdict = iota
	vDNS
	vFlow
)

// precheck 是 AcceptTCP、AcceptUDP 共用的纯判定（只看地址、映射表、闸门和配置，不做 I/O），按拒绝原因计数。
// 闸门关闭时：当前配置不可能直连就在这里拒（SYN 时 RST）；可能直连就先接受，到判定里再拒走代理的（m3-plan v2.6 补充 6）。
func (p *Proxy) precheck(f Flow) verdict {
	dst := f.Dst
	a := dst.Addr().Unmap()
	switch {
	case dst == p.dnsAddr():
		return vDNS
	case a == p.dnsAddr().Addr() || reservedNet.Contains(a):
		p.st.badDst.Add(1)
		return vReject
	case fakeNet.Contains(a):
		if p.Pool == nil {
			p.st.noMapping.Add(1)
			return vReject
		}
		if _, ok := p.Pool.Lookup(a); !ok {
			p.st.noMapping.Add(1)
			return vReject
		}
	}
	for _, b := range badDst {
		if b.Contains(a) {
			p.st.badDst.Add(1)
			return vReject
		}
	}
	// 闸门关着：配置不可能直连、探测窗口也没开时才在这里拒；窗口开着时先接受，到 newRoute 里识别是不是自己。
	if !p.gateOpen() && !p.probeOpen() && !p.config().MayDirect(p.state()) {
		p.st.gateClosed.Add(1)
		return vReject
	}
	return vFlow
}

// AcceptTCP 实现 Handler。
func (p *Proxy) AcceptTCP(f Flow) bool { return p.precheck(f) != vReject }

// AcceptUDP 实现 Handler（在搬运 goroutine 上同步调用：precheck 不阻塞、不做 I/O）。
func (p *Proxy) AcceptUDP(f Flow) bool { return p.precheck(f) != vReject }

// TCP 实现 Handler。
func (p *Proxy) TCP(ctx context.Context, f Flow, conn net.Conn) {
	if f.Dst == p.dnsAddr() {
		p.serveDNSTCP(ctx, conn)
		return
	}
	fctx, entry, done := p.track(ctx)
	defer done()
	r, ok := p.newRoute(fctx, f, wire.TCP)
	if !ok {
		reset(conn)
		return
	}
	defer r.release()
	if r.self { // 探测窗口：abroadd 自己的探测，固定走主代理，不经规则引擎
		up := p.probe(fctx, r)
		if up == nil {
			reset(conn)
			return
		}
		defer up.Close()
		p.pipeTCP(fctx, conn, up, nil, true)
		return
	}
	var prefix []byte
	if r.cfg.NeedSniff(r.who, r.dest, r.env) {
		res, b, err := sniff.PeekTCP(fctx, conn)
		if err != nil && fctx.Err() != nil {
			return
		}
		prefix = b
		r.dest.Sniff, r.dest.SniffName = res.Kind, res.Name
		p.countSniff(res.Kind)
	}
	up, dec := p.dispatch(fctx, entry, r, p.decide(r), false)
	if up == nil {
		reset(conn)
		return
	}
	defer up.Close()
	p.pipeTCP(fctx, conn, up, prefix, dec.Action.Kind == rules.Proxy)
}

// UDP 实现 Handler。每条流（五元组）判定一次，用第一个数据报做首包识别（m3-plan v2.6 补充 3）。
func (p *Proxy) UDP(ctx context.Context, f Flow, pc net.PacketConn) {
	if f.Dst == p.dnsAddr() {
		p.serveDNSUDP(ctx, pc)
		return
	}
	fctx, entry, done := p.track(ctx)
	defer done()
	stopPC := context.AfterFunc(fctx, func() { pc.Close() })
	defer stopPC()
	buf := make([]byte, wire.MaxDatagram)
	n, _, err := pc.ReadFrom(buf) // 第一个数据报（空闲超时由栈的 udpFlow 管）
	if err != nil {
		return
	}
	first := append([]byte(nil), buf[:n]...)
	pend := &udpPending{}
	go p.udpReader(pc, pend) // 判定期间到达的后续数据报先缓存（有上限）
	r, ok := p.newRoute(fctx, f, wire.UDP)
	if !ok {
		pend.fail()
		return
	}
	defer r.release()
	if r.self { // 探测窗口（同 TCP）
		up := p.probe(fctx, r)
		if up == nil {
			pend.fail()
			return
		}
		defer up.Close()
		p.pipeUDP(fctx, pc, up, first, pend, true)
		return
	}
	if r.cfg.NeedSniff(r.who, r.dest, r.env) {
		res := sniff.QUIC(first)
		r.dest.Sniff, r.dest.SniffName = res.Kind, res.Name
		p.countSniff(res.Kind)
	}
	up, dec := p.dispatch(fctx, entry, r, p.decide(r), false)
	if up == nil {
		pend.fail()
		return
	}
	defer up.Close()
	p.pipeUDP(fctx, pc, up, first, pend, dec.Action.Kind == rules.Proxy)
}

// route 是一条流在判定时需要的全部输入。
type route struct {
	cfg     *rules.Config
	env     rules.Env
	who     rules.Who
	dest    rules.Dest
	flow    Flow
	network string
	release func()
	gen     uint64 // 取配置时的代数（规则或状态之后变了，登记判定时要按新的再判）
	self    bool   // 探测窗口打开时识别为 abroadd 自己的连接（M4-6a）
}

// newRoute 组装判定的输入：fake 映射（持有引用直到连接结束）、反查、应用识别（只在配置需要时）。
func (p *Proxy) newRoute(ctx context.Context, f Flow, network string) (*route, bool) {
	gen := p.gen.Load() // 先取代数再取配置：之后的任何变化都会让代数对不上
	r := &route{cfg: p.config(), env: p.env(), flow: f, network: network, release: func() {}, gen: gen}
	r.dest = rules.Dest{Net: network, Addr: f.Dst}
	a := f.Dst.Addr().Unmap()
	if fakeNet.Contains(a) {
		if p.Pool == nil {
			p.st.noMapping.Add(1)
			return nil, false
		}
		m, rel, ok := p.Pool.AcquireMapping(a, p.now())
		if !ok {
			p.st.noMapping.Add(1)
			return nil, false
		}
		r.dest.FakeDomain, r.dest.Local, r.release = m.Name, m.Local, rel
	} else if p.Reverse != nil {
		if n, ok := p.Reverse.Lookup(a, p.now()); ok {
			r.dest.Reverse = n
		}
	}
	r.who = rules.Who{App: rules.Unknown}
	if r.cfg.NeedApp() && p.Ident != nil { // 配置用不到应用时不识别（m3-plan v2.6 补充 4）
		app, claude := p.Ident.Identify(ctx, network, f.Src, f.Dst)
		r.who = rules.Who{App: app, Claude: claude}
	}
	// 探测窗口开着：不管配置用不用得到应用，都识别是不是 abroadd 自己（m4-plan v2.6 决定 2）。
	if p.probeOpen() {
		if p.Self.IsSelf(ctx, network, f.Src, f.Dst) {
			r.self = true
		} else {
			p.st.probeRefused.Add(1)
		}
	}
	return r, true
}

// decide 是判定入口（每条流恰好一次；唯一的另一处 Decide 调用是 dispatch 里带 Refused 的重判）。
func (p *Proxy) decide(r *route) rules.Decision { return r.cfg.Decide(r.who, r.dest, r.env) }

// dispatch 是唯一能开代理流、发起直连拨号的地方（m3-plan v2.6 补充 1）。返回上游连接（拦截或失败时为 nil）
// 和最终生效的判定。直连回 pinned 时带着 Refused 再判一次（唯一允许的第二次判定），结果不可能是直连。
func (p *Proxy) dispatch(ctx context.Context, e *flowEntry, r *route, dec rules.Decision, refused bool) (net.Conn, rules.Decision) {
	dec = p.commit(e, r, dec)
	p.countReason(dec.Reason)
	switch dec.Action.Kind {
	case rules.Proxy:
		return p.open(ctx, r, dec.Action.Exit), dec
	case rules.Direct:
		if refused { // 防御：带着 Refused 的重判不可能是直连（rules 的性质测试保证），万一是就拦截
			return nil, rules.Decision{Action: rules.ActBlock, Reason: rules.RPinnedResolved}
		}
		c, err := p.dialDirect(ctx, r)
		switch {
		case err == nil:
			p.st.directOK.Add(1)
			p.markDirect(e, r.who.App)
			return c, dec
		case errors.Is(err, directdial.ErrPinned):
			p.countDirectFail(err)
			r.dest.Refused = true
			return p.dispatch(ctx, e, r, r.cfg.Decide(r.who, r.dest, r.env), true)
		default:
			p.countDirectFail(err)
			return nil, dec
		}
	}
	return nil, dec // 拦截
}

// commit 在执行动作之前登记判定的输入和动作（给 Recheck 用）。取配置之后规则或状态变了（代数对不上）、
// 而 Recheck 那时这条流还没登记，就按新的规则和状态再判一次，免得漏掉（m3-plan v2.7 决定 2、4）。
func (p *Proxy) commit(e *flowEntry, r *route, dec rules.Decision) rules.Decision {
	p.mu.Lock()
	defer p.mu.Unlock()
	if g := p.gen.Load(); g != r.gen {
		r.cfg, r.env, r.gen = p.config(), p.env(), g
		dec = p.decide(r)
	}
	e.decided, e.who, e.dest, e.network, e.action, e.flow = true, r.who, r.dest, r.network, dec.Action, r.flow
	e.reason = dec.Reason
	return dec
}

// dialDirect 经直连拨号模块拨号：有 fake 映射时传域名，否则传 IP。
func (p *Proxy) dialDirect(ctx context.Context, r *route) (net.Conn, error) {
	if p.Direct == nil {
		return nil, errors.New("没有配置直连拨号模块")
	}
	if r.dest.FakeDomain != "" {
		return p.Direct.Dial(ctx, r.network, r.dest.FakeDomain, netip.Addr{}, r.flow.Dst.Port())
	}
	return p.Direct.Dial(ctx, r.network, "", r.flow.Dst.Addr().Unmap(), r.flow.Dst.Port())
}

// probe 是探测窗口的放行路径（m4-plan v2.6 决定 2）：网络栈里唯一一条不经规则引擎的转发，出口固定是主代理的常量，
// 不可能直连（源码检查守着）。复查窗口（Accept 之后窗口可能失效了）。不登记判定，所以 Recheck 不重判它；DropAll 照样断它。
func (p *Proxy) probe(ctx context.Context, r *route) net.Conn {
	if !p.probeOpen() {
		p.st.gateClosed.Add(1)
		return nil
	}
	if p.Opener == nil {
		p.st.openOther.Add(1)
		return nil
	}
	up, err := p.Opener.Open(ctx, p.header(r, guestabi.ExitMain))
	if err != nil {
		if ctx.Err() == nil {
			p.countOpenErr(err)
		}
		return nil
	}
	p.st.probeProxied.Add(1)
	return up
}

// header 组装开流的帧头。帧头的目的只有两种：fake 映射的域名，或原始的真实 IP（首包里的名字不进帧头）。
func (p *Proxy) header(r *route, exit string) wire.Header {
	h := wire.Header{V: wire.Version, Exit: exit, Net: r.network, Port: int(r.flow.Dst.Port())}
	if len(r.who.App) <= 512 { // 帧头的 app 只用于调试日志，超长时不带（wire 的上限是 512 字节）
		h.App = string(r.who.App)
	}
	if r.dest.FakeDomain != "" {
		h.Host = r.dest.FakeDomain
	} else {
		h.IP = r.flow.Dst.Addr().Unmap().String()
	}
	if h.Exit == "" {
		h.Exit = guestabi.ExitMain
	}
	return h
}

// open 复查闸门、开代理流。帧头的目的只有两种：fake 映射的域名，或原始的真实 IP（首包里的名字不进帧头）。
// UDP 回 udp_unsupported 时记进负缓存。失败时已计数，返回 nil。
func (p *Proxy) open(ctx context.Context, r *route, exit string) net.Conn {
	if !p.gateOpen() { // Accept 之后闸门可能关了
		p.st.gateClosed.Add(1)
		return nil
	}
	if p.Opener == nil {
		p.st.openOther.Add(1)
		return nil
	}
	h := p.header(r, exit)
	up, err := p.Opener.Open(ctx, h)
	if err != nil {
		var re *wire.ReplyError
		if r.network == wire.UDP && errors.As(err, &re) && re.Code == wire.CodeUDPUnsupported {
			p.markUDPUnsupported(h.Exit)
		}
		if ctx.Err() == nil { // DropAll、Stack.Close 打断的不算开流失败（DropAll 已计 dropped）
			p.countOpenErr(err)
		}
		return nil
	}
	if r.network == wire.TCP {
		p.st.tcpProxied.Add(1)
	} else {
		p.st.udpProxied.Add(1)
	}
	return up
}
