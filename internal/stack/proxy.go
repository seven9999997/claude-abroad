package stack

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"claude-abroad/internal/fakedns"
	"claude-abroad/internal/rules"
	"claude-abroad/internal/wire"
)

// Gate 是闸门（设计第 8 节；真实实现在 M4）。Open 必须不阻塞（原子读）：AcceptUDP 在搬运 goroutine 上调用它。
type Gate interface{ Open() bool }

// ProbeGate 是探测窗口（m4-plan v2.6 决定 2）：窗口打开期间，识别为 abroadd 自己进程的连接放行，固定走主代理。
type ProbeGate interface{ ProbeOpen() bool }

// SelfIdentifier 判断一条连接是不是 abroadd 自己发起的（daemon：procinfo 查到的进程号等于自己的进程号）。
// 识别不到时返回 false（不放行）。
type SelfIdentifier interface {
	IsSelf(ctx context.Context, network string, src, dst netip.AddrPort) bool
}

// Opener 开一条到虚拟机数据面的流（*wire.Dialer 满足）。
type Opener interface {
	Open(ctx context.Context, h wire.Header) (net.Conn, error)
}

// AppIdentifier 识别一条流属于哪个应用（procinfo.Identifier 经适配满足）。识别不到返回 rules.Unknown。
type AppIdentifier interface {
	Identify(ctx context.Context, network string, src, dst netip.AddrPort) (app rules.AppID, claude bool)
}

// DirectDialer 是直连拨号模块的客户端（directdial.Client 满足）：host 和 ip 二选一。
// 回 directdial.ErrPinned 时，这条流带着 Refused 再判一次（m3-plan v2.6 补充 1）。
type DirectDialer interface {
	Dial(ctx context.Context, network, host string, ip netip.Addr, port uint16) (net.Conn, error)
}

// Proxy 的默认参数。
var DefaultDNSAddr = netip.MustParseAddrPort("198.18.0.2:53")

const (
	DefaultDNSIdle   = 5 * time.Second
	DefaultMaxDNSTCP = 64
	// DNSPerFlow 是一条 DNS UDP 流里同时处理的查询上限（超出的丢弃，客户端会重试）。
	DNSPerFlow = 16
	// DefaultUDPNegTTL 是“出口不支持 UDP”的负缓存时长。
	DefaultUDPNegTTL = 60 * time.Second
	// UDP 判定期间缓存的后续数据报上限（m3-plan v2.6 补充 3）。
	udpPendingMax      = 8
	udpPendingMaxBytes = 16 << 10
)

var (
	reservedNet = netip.MustParsePrefix("198.18.0.0/24") // utun、DNS 等保留地址
	fakeNet     = netip.MustParsePrefix("198.18.0.0/15")
	badDst      = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("224.0.0.0/4"),
		netip.MustParsePrefix("255.255.255.255/32"),
	}
)

// Proxy 是网络栈的流处理器（设计 4.2、5.2，m3-plan M3-5）。每条流恰好经过一次
// “识别应用 → 组装去向 → 必要时读首包 → rules.Decide → 分发”（router.go）。
//   - 198.18.0.2:53 交给 fake-IP DNS（不看闸门）；
//   - fake 段目的按映射还原成域名（查不到就拒绝，不猜），真实 IP 目的按 IP；
//   - 保留地址、回环/链路本地/组播/广播目的：拒绝；
//   - 走代理时帧头的目的只有两种：fake 映射的域名，或原始的真实 IP；首包里的名字只用于判定；
//   - 直连只经 DirectDialer；连接失败只关这条连接：不重试、不换出口、不退回直连。
//
// M3 的字段都可以不填：不填时等同 M2（全部走主代理）。统计只有计数，本类型不打日志。
type Proxy struct {
	Pool   *fakedns.Pool
	DNS    *fakedns.Server
	Gate   Gate
	Opener Opener
	Now    func() time.Time

	DNSAddr   netip.AddrPort // 默认 198.18.0.2:53
	DNSIdle   time.Duration  // DNS UDP 流的空闲上限，默认 5 秒
	MaxDNSTCP int            // DNS TCP 连接上限，默认 64

	// M3（m3-plan M3-5）
	Rules  func() *rules.Config   // 当前规则（原子读取）；nil 时用 rules.DefaultConfig（全部走主代理）
	State  func() rules.State     // nil 时是 Enabled
	ExitUp func(exit string) bool // 分流出口是否可用；nil 时分流出口一律按不可用（回退主代理）
	// ExitUDP 报告分流出口的 UDP 能不能走它（M4-8：最近一次 UDP 探测明确通过）；nil 时只看负缓存。
	// 返回 false 时只有 UDP 回退主代理，TCP 照走它。
	ExitUDP   func(exit string) bool
	Ident     AppIdentifier    // nil 时应用一律识别不到
	Direct    DirectDialer     // nil 时直连一律失败
	Reverse   *fakedns.Reverse // 反查表（只能让结果更严格）
	UDPNegTTL time.Duration    // 默认 60 秒
	// SyncRules 在 SetRules 换规则之前调用，同步和规则绑定的其他名单（fake-IP DNS 的本地域名名单等，
	// m3-plan v2.7 决定 4）。返回错误时规则保持旧的；它自己要保证失败时不留下一半的改动。
	SyncRules func(c *rules.Config) error
	// M4-6a：探测窗口和自己进程的识别。两个都非 nil 才有放行路径。
	Probe ProbeGate
	Self  SelfIdentifier

	// M6-12：Claude 访问的、不在内置置顶后缀之下的域名交给它记录（只观察，不影响判定）。运行中可换，所以用原子指针。
	claudeDomains atomic.Pointer[func(name string)]

	st     proxyStats
	dnsTCP atomic.Int64
	mu     sync.Mutex
	flows  map[*flowEntry]struct{}
	neg    udpNeg

	cur    atomic.Pointer[rules.Config] // SetRules 设的规则（优先于 Rules）
	stateV atomic.Int32                 // SetEnabled 设的状态（0 表示没设，用 State）
	gen    atomic.Uint64                // 规则或状态每变一次加一
}

// flowEntry 是一条登记中的流。判定之前算“待定”（DropAll 也断它，偏严）；判定为直连后改记为直连。
// 判定之后记下判定的输入和当时的动作（只在内存里，不落盘、不进日志），给 Recheck 用（m3-plan v2.7 决定 1）。
type flowEntry struct {
	cancel  context.CancelFunc
	direct  bool
	app     rules.AppID
	decided bool
	who     rules.Who
	dest    rules.Dest
	network string
	action  rules.Action
	reason  rules.Reason
	flow    Flow
}

func (p *Proxy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Proxy) dnsAddr() netip.AddrPort {
	if p.DNSAddr.IsValid() {
		return p.DNSAddr
	}
	return DefaultDNSAddr
}

func (p *Proxy) gateOpen() bool { return p.Gate != nil && p.Gate.Open() }

func (p *Proxy) probeOpen() bool { return p.Probe != nil && p.Self != nil && p.Probe.ProbeOpen() }

func (p *Proxy) config() *rules.Config {
	if c := p.cur.Load(); c != nil {
		return c
	}
	if p.Rules != nil {
		if c := p.Rules(); c != nil {
			return c
		}
	}
	return rules.DefaultConfig()
}

func (p *Proxy) state() rules.State {
	if v := p.stateV.Load(); v != 0 {
		return rules.State(v)
	}
	if p.State != nil {
		return p.State()
	}
	// 没被告知状态时按停用处理（architect 审 M3-6 的建议改 1）：新写的组装忘了设状态时，
	// 停用状态下的直连规则也不会生效，“停用即断网”仍然成立。
	return rules.Disabled
}

// env 组装判定用的外部状态。UDPOK 来自负缓存。
func (p *Proxy) env() rules.Env {
	return rules.Env{State: p.state(), GateOpen: p.gateOpen(), ExitUp: p.ExitUp, UDPOK: p.udpOK}
}

// track 登记一条流（待定），返回它的 ctx、登记项和注销函数。
func (p *Proxy) track(ctx context.Context) (context.Context, *flowEntry, func()) {
	fctx, cancel := context.WithCancel(ctx)
	e := &flowEntry{cancel: cancel}
	p.mu.Lock()
	if p.flows == nil {
		p.flows = map[*flowEntry]struct{}{}
	}
	p.flows[e] = struct{}{}
	p.mu.Unlock()
	return fctx, e, func() {
		p.mu.Lock()
		delete(p.flows, e)
		p.mu.Unlock()
		cancel()
	}
}

func (p *Proxy) markDirect(e *flowEntry, app rules.AppID) {
	p.mu.Lock()
	e.direct, e.app = true, app
	p.mu.Unlock()
}

// DropAll 断开所有走代理的流和还没判定完的流（不含 DNS、不含直连），给“停用”第 ② 步和闸门由开变关用。
// 之后新流仍按闸门判定。
func (p *Proxy) DropAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for e := range p.flows {
		if !e.direct {
			e.cancel()
			p.st.dropped.Add(1)
		}
	}
}

// DropDirect 断开 drop 返回真的直连流（例如停用时不在 offline_allow 里的应用，m3-plan v2.6 的 M3-6 补充），
// 返回断开的条数。
func (p *Proxy) DropDirect(drop func(app rules.AppID) bool) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for e := range p.flows {
		if e.direct && drop(e.app) {
			e.cancel()
			n++
		}
	}
	p.st.directDropped.Add(int64(n))
	return n
}

type closeWriter interface{ CloseWrite() error }

// abort 强制断开一条连接：先设一个已过去的期限唤醒阻塞中的读写，再关闭。
// wire 的流（yamux）Close 只关写方向，读会一直等到对端结束，所以强制断开时不能只调 Close。
func abort(c net.Conn) {
	_ = c.SetDeadline(time.Unix(1, 0))
	_ = c.Close()
}

// reset 以 RST 结束应用一侧的 TCP 连接（栈给的连接支持；别的连接退化为 abort）。
func reset(c net.Conn) {
	if r, ok := c.(interface{ Reset() }); ok {
		r.Reset()
		return
	}
	abort(c)
}

func halfClose(c net.Conn) {
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
	} else {
		_ = c.Close()
	}
}

func (p *Proxy) serveDNSTCP(ctx context.Context, conn net.Conn) {
	max := p.MaxDNSTCP
	if max <= 0 {
		max = DefaultMaxDNSTCP
	}
	if p.DNS == nil || !reserve(&p.dnsTCP, max) {
		p.st.dnsTCPRejected.Add(1)
		return
	}
	defer p.dnsTCP.Add(-1)
	p.DNS.ServeConn(ctx, conn)
}

// serveDNSUDP 循环处理一条 DNS UDP 流：同一个源端口可能连发多个查询，所以不能答一次就返回。
// DNSIdle 内没有新包、也没有在途查询时返回（归还名额）。
func (p *Proxy) serveDNSUDP(ctx context.Context, pc net.PacketConn) {
	if p.DNS == nil {
		return
	}
	idle := p.DNSIdle
	if idle <= 0 {
		idle = DefaultDNSIdle
	}
	stop := context.AfterFunc(ctx, func() { pc.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	var inflight atomic.Int32
	sem := make(chan struct{}, DNSPerFlow)
	buf := make([]byte, wire.MaxDatagram)
	for {
		_ = pc.SetReadDeadline(time.Now().Add(idle))
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() && ctx.Err() == nil && inflight.Load() > 0 {
				continue // 还有在途查询：等它们答完
			}
			return
		}
		select {
		case sem <- struct{}{}:
		default:
			p.st.dnsUDPDropped.Add(1)
			continue
		}
		in := append([]byte(nil), buf[:n]...)
		inflight.Add(1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer inflight.Add(-1)
			defer func() { <-sem }()
			if out := p.DNS.HandlePacket(ctx, in); out != nil {
				_, _ = pc.WriteTo(out, from)
			}
		}()
	}
}

// SetEnabled 实现 state.DataState：设定数据面的状态（只分启用和其他），并按新状态重判在途的流
// （进入停用后，不在 offline_allow 里的直连必须断开，m3-plan v2.7 决定 2）。
func (p *Proxy) SetEnabled(enabled bool) {
	s := rules.Disabled
	if enabled {
		s = rules.Enabled
	}
	p.stateV.Store(int32(s))
	p.gen.Add(1)
	p.Recheck()
}

// Enabled 报告当前是否按“启用”判定。
func (p *Proxy) Enabled() bool { return p.state() == rules.Enabled }

// SetRules 原子地换规则（m3-plan v2.7 决定 4）：先经 SyncRules 同步和规则绑定的名单，失败就保持旧规则并返回错误。
// dropAffected 为真时按新规则重判在途的流，判定结果变了的断开；为假时旧连接保持（design 5.2）。
func (p *Proxy) SetRules(c *rules.Config, dropAffected bool) error {
	if c == nil {
		c = rules.DefaultConfig()
	}
	if p.SyncRules != nil {
		if err := p.SyncRules(c); err != nil {
			return err
		}
	}
	p.cur.Store(c)
	p.gen.Add(1)
	if dropAffected {
		p.Recheck()
	}
	return nil
}

// Recheck 按当前规则和状态重判所有已判定的在途流：结果与当时的动作不同的（种类不同或出口不同）断开，
// 返回断开的条数。用记下来的首包结果，不重新读首包（m3-plan v2.7 决定 5）。两处按新配置刷新：
//   - fake 映射的“本地域名”标记按新配置的 local_domains 重算（名单改了，记下的标记就过时了）；
//   - 当时没识别应用（旧配置用不到）、新配置要用的，现场识别一次（流还在，它的五元组还在连接表里）。识别在锁外做。
func (p *Proxy) Recheck() int {
	cfg, env := p.config(), p.env()
	type item struct {
		e    *flowEntry
		who  rules.Who
		dest rules.Dest
		net  string
		act  rules.Action
		flow Flow
	}
	p.mu.Lock()
	var items []item
	for e := range p.flows {
		if e.decided { // 还在判定的流：登记判定时发现规则或状态变了，会按新的再判一次（commit）
			items = append(items, item{e, e.who, e.dest, e.network, e.action, e.flow})
		}
	}
	p.mu.Unlock()
	n := 0
	for _, it := range items {
		if it.dest.FakeDomain != "" {
			it.dest.Local = cfg.IsLocal(it.dest.FakeDomain)
		}
		if it.who.App == rules.Unknown && !it.who.Claude && cfg.NeedApp() && p.Ident != nil {
			app, claude := p.Ident.Identify(context.Background(), it.net, it.flow.Src, it.flow.Dst)
			it.who = rules.Who{App: app, Claude: claude}
		}
		r := &route{cfg: cfg, env: env, who: it.who, dest: it.dest, network: it.net}
		if p.decide(r).Action != it.act {
			p.mu.Lock()
			if _, ok := p.flows[it.e]; ok {
				it.e.cancel()
				n++
			}
			p.mu.Unlock()
		}
	}
	p.st.rechecked.Add(int64(n))
	return n
}

// FlowRecords 返回登记中的流的条数（测试用：流结束后记录要清掉）。
func (p *Proxy) FlowRecords() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.flows)
}
