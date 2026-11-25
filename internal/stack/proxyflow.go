package stack

import (
	"context"
	"net"
	"net/netip"

	"claude-abroad/internal/rules"
	"claude-abroad/internal/sniff"
	"claude-abroad/internal/wire"
)

// ProxyFlow 是经 abroadd 本机代理端口进来的一条 TCP 流（M6-3b，设计 10.2 第四层）：Claude Code 被托管设置指向
// 这个端口，它和它的子进程按 HTTP 代理的方式说出目的地（域名或地址）。
type ProxyFlow struct {
	Src   netip.AddrPort // 连接方在回环上的地址（识别进程用）
	Local netip.AddrPort // 代理端口自己的地址
	Host  string         // 目的域名（已校验；为空时用 IP）
	IP    netip.Addr     // 目的地址（Host 为空时）
	Port  uint16
}

// ServeProxyFlow 用和接管流量同一个判定入口（decide）、同一个拨号入口（dispatch，含闸门复查、直连拨号、开代理流）、
// 同一个连接表（track，闸门由开变关时 DropAll 一起断开）处理一条经本机代理端口的流：
//   - 识别连接方进程；识别不出的按 Claude 处理（最严：只走主代理，闸门关时拒绝）；
//   - 目的是域名时按域名判定（本地域名名单照旧生效），开流和直连都把域名交给出口解析，不在本机解析；
//   - 规则要看首包（TLS 名字）时先 accept(true) 让客户端发出首包；
//   - 被拒绝时：还没 accept 过就 accept(false)（调用方回 403），已经 accept 过就直接断开。
//
// 返回是否接通。只处理 TCP（HTTP 代理不转 UDP）。
func (p *Proxy) ServeProxyFlow(ctx context.Context, f ProxyFlow, conn net.Conn, accept func(ok bool) error) bool {
	fctx, entry, done := p.track(ctx)
	defer done()
	gen := p.gen.Load()
	dst := netip.AddrPortFrom(f.IP, f.Port)
	r := &route{cfg: p.config(), env: p.env(), flow: Flow{Src: f.Src, Dst: dst}, network: wire.TCP, release: func() {}, gen: gen}
	r.dest = rules.Dest{Net: wire.TCP}
	if f.Host != "" {
		r.dest.FakeDomain = f.Host
		r.dest.Local = r.cfg.IsLocal(f.Host)
		r.flow.Dst = netip.AddrPortFrom(netip.Addr{}, f.Port)
	} else {
		r.dest.Addr = dst
		if p.Reverse != nil {
			if n, ok := p.Reverse.Lookup(f.IP.Unmap(), p.now()); ok {
				r.dest.Reverse = n
			}
		}
	}
	r.who = rules.Who{App: rules.Unknown, Claude: true} // 识别不出：按 Claude 处理
	if p.Ident != nil {
		if app, claude := p.Ident.Identify(fctx, wire.TCP, f.Src, f.Local); app != rules.Unknown && app != "" {
			r.who = rules.Who{App: app, Claude: claude}
		}
	}
	accepted := false
	var prefix []byte
	if r.cfg.NeedSniff(r.who, r.dest, r.env) {
		if accept(true) != nil {
			return false
		}
		accepted = true
		res, b, err := sniff.PeekTCP(fctx, conn)
		if err != nil && fctx.Err() != nil {
			return false
		}
		prefix = b
		r.dest.Sniff, r.dest.SniffName = res.Kind, res.Name
		p.countSniff(res.Kind)
	}
	up, dec := p.dispatch(fctx, entry, r, p.decide(r), false)
	if up == nil {
		if !accepted {
			_ = accept(false)
		}
		return false
	}
	defer up.Close()
	if !accepted && accept(true) != nil {
		return false
	}
	p.pipeTCP(fctx, conn, up, prefix, dec.Action.Kind == rules.Proxy)
	return true
}
