package daemon

import (
	"claude-abroad/internal/directdial"
	"claude-abroad/internal/gatesm"
	"claude-abroad/internal/guestabi"
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"

	"claude-abroad/internal/fakedns"
	"claude-abroad/internal/rules"
	"claude-abroad/internal/stack"
	"claude-abroad/internal/tun"
	"claude-abroad/internal/wire"
)

// StreamOpener 开一条到虚拟机数据面的流，并能关掉当前会话（*wire.Dialer 满足：Close 之后的 Open 会重新连接）。
type StreamOpener interface {
	Open(ctx context.Context, h wire.Header) (net.Conn, error)
	Close() error
}

// DataPlane 是数据面（M2-9b-2 决定 1）：
//   - 进程级（daemon 存续期间一份）：fake-IP 地址池、DNS 服务（含直连名单和反查表）、经主代理的 DoH、拨号器、代理处理器。
//     utun 重建后，应用缓存的假地址仍能用；
//   - 设备级（每个 utun 一份）：网络栈。Attach 在 utun 打开的钩子里建，Detach 在关闭的钩子里拆。
//
// 闸门由开变关时按固定顺序断开（决定 3）：Proxy.DropAll → DoH Close → 拨号器 Close，每个关闭期间只做一次。
// 拨号器 Close 之后下一次 Open 会重新连接，所以重新开闸门后新连接能建立。
// 数据面除了经 Opener（unix socket 的 data.sock）之外没有任何拨号或监听（决定 10）。
type DataPlane struct {
	Opener StreamOpener
	pool   *fakedns.Pool
	srv    *fakedns.Server
	doh    *fakedns.DoHUpstream
	// probeDoH 只给环境检测用（07 回显、08 解析 STUN 服务器）：闸门开着或探测窗口开着时都查。启用第 ⑦ 步时闸门还关着，
	// 经系统解析器的查询会被我们的 DNS 答 SERVFAIL（2026-11-27 真机：07、08 无法判断、启用回滚）。只在 abroadd 进程里调用，
	// 别的应用的 DNS 仍按闸门。
	probeDoH *fakedns.DoHUpstream
	proxy    *stack.Proxy
	gate     *lateGate

	opens atomic.Int64 // 经 Opener 开流的次数（测试和诊断用）

	mu     sync.Mutex
	st     *stack.Stack
	dev    tun.Device
	kick   func()
	onSlow func()       // 拆网络栈超过 DetachWait 时调用（记事件）
	trace  func(string) // 测试：记下断开的调用顺序

	tornDown atomic.Bool // 本次关闭期间已经断开过
	openLog  []string    // 每次开流的种类（mu 保护）

	// M3-7
	direct   DirectConfigurer       // 直连拨号模块的客户端（nil：没有直连）
	origDNS  func() []netip.Addr    // 启用前记下的本机 DNS（daemon 从状态文件读）
	dcMu     sync.Mutex             // 保护 dc、dcPinned
	dc       directdial.Config      // 直连拨号模块的当前配置（带代数，m3-plan v2.9）
	dcPinned []string               // 当前规则的置顶追加名单
	avail    *gatesm.ExitAvail      // 分流出口的可用状态（m4-plan M4-3 决定 7、M4-8）
	counts   map[string]*ExitCounts // 按出口的开流计数（网络栈的开流，含 main；mu 保护）
	now      func() time.Time       // 时钟（测试注入）
	syncFail func() error           // 测试：注入“同步名单失败”
}

// ExitDownFor 是分流出口开流回 unknown_exit、exit_down 后按不可用处理的时长（gatesm.ExitAvail 的输入之一；
// M4-8 接上正式的分流出口检查后收口）。
const ExitDownFor = gatesm.DefaultExitDownFor

// DirectConfigurer 是直连拨号模块的客户端（directdial.Client 满足）。
// 拨号时带上当前配置的代数；直连拨号模块回 stale_config 时由它自己补发（m3-plan v2.9）。
type DirectConfigurer interface {
	stack.DirectDialer
	SetConfigSource(func() directdial.Config)
	Configure(ctx context.Context, cfg directdial.Config) error
	ConfiguredGen() (uint64, bool)
}

// lateGate 是闸门的只读视图，daemon 启动后才绑定（之前一律关着）。
type lateGate struct{ v atomic.Pointer[GateView] }

func (g *lateGate) Open() bool {
	v := g.v.Load()
	return v != nil && v.Open()
}

// probeOrOpen 在闸门开着或探测窗口开着时算开（只给 probeDoH 用）。
type probeOrOpen struct{ g *lateGate }

func (p probeOrOpen) Open() bool { return p.g.Open() || p.g.ProbeOpen() }

// ProbeLookup 经主代理的 DoH 查 name 的 qtype 记录（只给环境检测用：探测窗口开着时闸门关着也查），返回应答里的记录。
func (p *DataPlane) ProbeLookup(ctx context.Context, name string, qtype uint16) ([]dns.RR, error) {
	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(name), qtype)
	q.RecursionDesired = true
	r, err := p.probeDoH.Exchange(ctx, q)
	if err != nil {
		return nil, err
	}
	if r.Rcode != dns.RcodeSuccess {
		return nil, errors.New("DoH 应答：" + dns.RcodeToString[r.Rcode])
	}
	return r.Answer, nil
}

// ProbeTXT 经 ProbeLookup 查 TXT（07 的回显解析器地址）。
func (p *DataPlane) ProbeTXT(ctx context.Context, name string) ([]string, error) {
	rrs, err := p.ProbeLookup(ctx, name, dns.TypeTXT)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, rr := range rrs {
		if t, ok := rr.(*dns.TXT); ok {
			out = append(out, t.Txt...)
		}
	}
	return out, nil
}

// ProbeA 经 ProbeLookup 查 A 记录（08 把 STUN 服务器解析成真实地址）。
func (p *DataPlane) ProbeA(ctx context.Context, name string) ([]netip.Addr, error) {
	rrs, err := p.ProbeLookup(ctx, name, dns.TypeA)
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, rr := range rrs {
		if a, ok := rr.(*dns.A); ok {
			if ip, ok := netip.AddrFromSlice(a.A.To4()); ok {
				out = append(out, ip)
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("没有 A 记录")
	}
	return out, nil
}

// ProbeOpen 是探测窗口（M4-6a，stack.ProbeGate）。
func (g *lateGate) ProbeOpen() bool {
	v := g.v.Load()
	return v != nil && v.ProbeOpen()
}

// countingOpener 记开流次数和种类（不记目的），经它的流都走 DataPlane.Opener。
type countingOpener struct {
	p    *DataPlane
	kind string // proxy、doh、exit_check
}

func (o countingOpener) Open(ctx context.Context, h wire.Header) (net.Conn, error) {
	o.p.opens.Add(1)
	o.p.mu.Lock()
	o.p.openLog = append(o.p.openLog, o.kind+"/"+h.Exit+"/"+h.Net)
	o.p.mu.Unlock()
	c, err := o.p.Opener.Open(ctx, h)
	if o.kind == "proxy" {
		o.p.countOpen(h.Exit, h.Net, err == nil)
	}
	var re *wire.ReplyError
	if err != nil && h.Exit != guestabi.ExitMain && errors.As(err, &re) && (re.Code == wire.CodeUnknownExit || re.Code == wire.CodeExitDown) {
		o.p.markExitDown(h.Exit) // 分流出口不存在或不可用：30 秒内命中它的新连接回退主代理
	}
	return c, err
}

func (p *DataPlane) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *DataPlane) markExitDown(exit string) { p.avail.MarkDown(exit) }

// exitUp 给网络栈：分流出口按可用状态；主代理不在这里（它不通过时整个闸门关）。
func (p *DataPlane) exitUp(exit string) bool { return p.avail.Up(exit) }

// Exits 返回已知分流出口的可用状态（status 用）。
func (p *DataPlane) Exits() map[string]bool { return p.avail.Snapshot() }

// ExitCounts 是一个出口的开流计数（网络栈的连接；出口检查、DoH 不算）。
type ExitCounts struct {
	TCP    int64 `json:"tcp"`
	UDP    int64 `json:"udp"`
	Failed int64 `json:"failed"`
}

func (p *DataPlane) countOpen(exit, network string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.counts == nil {
		p.counts = map[string]*ExitCounts{}
	}
	c := p.counts[exit]
	if c == nil {
		c = &ExitCounts{}
		p.counts[exit] = c
	}
	switch {
	case !ok:
		c.Failed++
	case network == wire.UDP:
		c.UDP++
	default:
		c.TCP++
	}
}

// OpenCounts 返回各出口的开流计数（拷贝）。
func (p *DataPlane) OpenCounts() map[string]ExitCounts {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]ExitCounts, len(p.counts))
	for k, v := range p.counts {
		out[k] = *v
	}
	return out
}

// dpClock 让 ExitAvail 用数据面的时钟（测试注入 p.now）。
type dpClock struct{ p *DataPlane }

func (c dpClock) Now() time.Time                                     { return c.p.clock() }
func (c dpClock) AfterFunc(d time.Duration, f func()) gatesm.Stopper { return time.AfterFunc(d, f) }

// SetDirect 接上直连拨号模块的客户端（真实模式在启动时调用；测试注入假实现）。
func (p *DataPlane) SetDirect(d DirectConfigurer) {
	p.dcMu.Lock()
	if p.dc.Gen == 0 {
		p.dc.Gen = 1 // 从 1 开始：代数 0 留给单独运行（abroad-vm direct --dns）
	}
	p.dcMu.Unlock()
	p.direct = d
	d.SetConfigSource(p.directConfig)
	p.proxy.Direct = d
}

// SetIdent 接上应用识别（真实模式是 procinfo；daemon-sim 和测试是按源端口段的假识别器）。
func (p *DataPlane) SetIdent(id stack.AppIdentifier) { p.proxy.Ident = id }

// SetSelf 设置“是不是 abroadd 自己的连接”的识别（探测窗口用，M4-6a）。不设时没有放行路径。
func (p *DataPlane) SetSelf(id stack.SelfIdentifier) { p.proxy.Self = id }

// UDPUnsupported 报告出口最近是否明确回过 udp_unsupported（环境检测第 08 项用）。
func (p *DataPlane) UDPUnsupported(exit string) bool { return p.proxy.UDPUnsupported(exit) }

// ExitUp 报告出口是否可用（环境检测第 09 项用）。
func (p *DataPlane) ExitUp(exit string) bool { return p.avail.Up(exit) }

// directConfig 返回直连拨号模块的当前配置（拷贝）。
func (p *DataPlane) directConfig() directdial.Config {
	p.dcMu.Lock()
	defer p.dcMu.Unlock()
	return directdial.Config{Gen: p.dc.Gen, DNS: slices.Clone(p.dc.DNS), PinnedExtra: slices.Clone(p.dc.PinnedExtra)}
}

// refreshDirect 重新计算直连拨号模块的配置（本机原 DNS、置顶追加名单）：内容变了代数加一。
// pinned 为 nil 时沿用当前规则的名单。不下发：直连拨号模块下一次回 stale_config 时由客户端补发。
func (p *DataPlane) refreshDirect(pinned []string, setPinned bool) {
	var dns []netip.Addr
	if p.origDNS != nil {
		dns = p.origDNS()
	}
	if len(dns) > directdial.MaxDNSServers {
		dns = dns[:directdial.MaxDNSServers]
	}
	p.dcMu.Lock()
	defer p.dcMu.Unlock()
	if setPinned {
		p.dcPinned = slices.Clone(pinned)
	}
	if !slices.Equal(dns, p.dc.DNS) || !slices.Equal(p.dcPinned, p.dc.PinnedExtra) {
		p.dc = directdial.Config{Gen: p.dc.Gen + 1, DNS: slices.Clone(dns), PinnedExtra: slices.Clone(p.dcPinned)}
	}
}

// pushDirect 主动下发一次当前配置（abroadd 启动后的预热；失败不算错，直连时会按需补发）。
func (p *DataPlane) pushDirect(ctx context.Context) error {
	if p.direct == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return p.direct.Configure(ctx, p.directConfig())
}

// DirectStatus 报告直连拨号模块：是否接上、它最近一次收下的配置是不是当前代数。
func (p *DataPlane) DirectStatus() (attached, configured bool) {
	if p.direct == nil {
		return false, false
	}
	g, ok := p.direct.ConfiguredGen()
	return true, ok && g == p.directConfig().Gen
}

// Stats 返回网络栈的计数。
func (p *DataPlane) Stats() stack.ProxyStats { return p.proxy.Stats() }

// OpenLog 返回每次开流的种类（“种类/出口/协议”，不含目的），按时间顺序。
func (p *DataPlane) OpenLog() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.openLog...)
}

// gatedUpstream 在闸门关闭时直接返回错误（fake-IP DNS 答 SERVFAIL），不调用内层上游。
type gatedUpstream struct {
	inner fakedns.Upstream
	g     stack.Gate
}

func (u gatedUpstream) Exchange(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
	if !u.g.Open() {
		return nil, fakedns.ErrGateClosed
	}
	return u.inner.Exchange(ctx, q)
}

// NewDataPlane 建进程级的数据面。upstream 为 nil 时用经 Opener 的 DoH（1.1.1.1:443，出口 main）。
func NewDataPlane(op StreamOpener, upstream fakedns.Upstream) *DataPlane {
	p := &DataPlane{Opener: op, gate: &lateGate{}}
	p.avail = &gatesm.ExitAvail{Clock: dpClock{p}, DownFor: ExitDownFor}
	p.pool = fakedns.DefaultPool()
	p.doh = &fakedns.DoHUpstream{Opener: countingOpener{p, "doh"}, Gate: p.gate}
	p.probeDoH = &fakedns.DoHUpstream{Opener: countingOpener{p, "doh"}, Gate: probeOrOpen{p.gate}}
	if upstream == nil {
		upstream = p.doh
	}
	p.srv = &fakedns.Server{Pool: p.pool, Bypass: fakedns.NewBypass(), Reverse: fakedns.NewReverse(), Upstream: gatedUpstream{upstream, p.gate}}
	p.proxy = &stack.Proxy{Pool: p.pool, DNS: p.srv, Gate: p.gate, Opener: countingOpener{p, "proxy"}, Probe: p.gate}
	// 换规则时同步 fake-IP DNS 的本地域名名单（m3-plan v2.7 决定 4）。直连拨号模块那一侧的置顶追加名单
	// 在 M3-6 先固定为只有内置名单（abroadd 一侧的判定已经包含追加名单），下发留给 M3-7。
	p.proxy.SyncRules = func(c *rules.Config) error {
		if p.syncFail != nil {
			if err := p.syncFail(); err != nil {
				return err // 还没改任何东西
			}
		}
		if err := p.srv.Bypass.SetUser(c.BypassExtra, c.PinnedExtra); err != nil { // 补齐三：用户追加的不分配假地址名单
			return err // 还没改任何东西
		}
		p.pool.SetLocalMatcher(c.IsLocal)
		return nil
	}
	p.proxy.ExitUp = p.exitUp
	p.proxy.ExitUDP = p.avail.UDPOK
	p.proxy.SetEnabled(false) // 初始按停用判定，直到状态机通知“启用”（m3-plan v2.7 决定 3）
	return p
}

// SetEnabled 实现 state.DataState：转给网络栈（按新状态重判在途的流）。启用时重新计算直连拨号模块的配置
// （启用前的本机 DNS 刚写进状态文件），不下发：下一次直连时按代数补发（m3-plan v2.9）。
func (p *DataPlane) SetEnabled(enabled bool) {
	if enabled {
		p.refreshDirect(nil, false)
	}
	p.proxy.SetEnabled(enabled)
}

// Enabled 报告数据面当前是否按“启用”判定。
func (p *DataPlane) Enabled() bool { return p.proxy.Enabled() }

// SetRules 原子地换分流规则（同步本地名单）；dropAffected 时断开判定结果变了的在途流。
// 成功后更新直连拨号模块的配置（置顶追加名单变了代数加一）。
func (p *DataPlane) SetRules(c *rules.Config, dropAffected bool) error {
	if err := p.proxy.SetRules(c, dropAffected); err != nil {
		return err
	}
	p.refreshDirect(c.PinnedExtra, true)
	return nil
}

// bind 绑定闸门和监护的 Kick（daemon 启动时调用）。
func (p *DataPlane) bind(g GateView, kick func()) {
	p.gate.v.Store(&g)
	p.mu.Lock()
	p.kick = kick
	p.mu.Unlock()
}

// Opens 返回经 Opener 开流的次数。
func (p *DataPlane) Opens() int64 { return p.opens.Load() }

// Attach 在 utun 打开（配好地址）之后挂网络栈（realTun 的 OnTunOpen）。网络栈意外退出时 Kick 监护。
func (p *DataPlane) Attach(dev tun.Device) error {
	st, err := stack.New(dev, p.proxy, stack.Options{})
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.st, p.dev = st, dev
	p.mu.Unlock()
	go func() {
		<-st.Done()
		p.mu.Lock()
		kick, cur := p.kick, p.st
		p.mu.Unlock()
		if cur == st && kick != nil {
			kick() // 不必等到下一个监护间隔（决定 2）
		}
	}()
	return nil
}

// Detach 在 utun 关闭之前拆网络栈（realTun 的 OnTunClose）。
func (p *DataPlane) Detach(dev tun.Device) {
	p.mu.Lock()
	st := p.st
	if p.dev == dev {
		p.st, p.dev = nil, nil
	}
	p.mu.Unlock()
	if st == nil {
		return
	}
	// 有界等待：-race 下偶见设备被外部先关掉后，gVisor 的 Stack.Wait 迟迟不返回（处理协程一直停在可运行状态）。
	// 设备已经关了，旧栈不会再有流量；不让它挡住重建和恢复，留在后台收尾，并记一条事件。
	done := make(chan struct{})
	go func() { _ = st.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(DetachWait):
		p.note("detach_slow")
		p.mu.Lock()
		ev := p.onSlow
		p.mu.Unlock()
		if ev != nil {
			ev()
		}
	}
}

// DetachWait 是拆网络栈时最多等多久。
var DetachWait = 2 * time.Second

// StackAlive 报告当前设备上的网络栈是否还在运行（realTun 的 Alive 要包含它，决定 2）。
func (p *DataPlane) StackAlive() bool {
	p.mu.Lock()
	st := p.st
	p.mu.Unlock()
	if st == nil {
		return false
	}
	select {
	case <-st.Done():
		return false
	default:
		return true
	}
}

func (p *DataPlane) note(s string) {
	p.mu.Lock()
	tr := p.trace
	p.mu.Unlock()
	if tr != nil {
		tr(s)
	}
}

// Teardown 实现 state.Teardown：断开所有走代理的连接，顺序固定；每个关闭期间只做一次（闸门的钩子和状态机都会调用）。
func (p *DataPlane) Teardown(context.Context) {
	if p.tornDown.Swap(true) {
		return
	}
	p.proxy.DropAll()
	p.note("drop_all")
	p.doh.Close()
	p.note("doh_close")
	_ = p.Opener.Close()
	p.note("dialer_close")
}

// gateChanged 是闸门的钩子：由开变关时断开，由关变开时允许下一次断开。
func (p *DataPlane) gateChanged(open bool) {
	if open {
		p.tornDown.Store(false)
		return
	}
	p.Teardown(context.Background())
}

// ErrNoStack 是还没有网络栈（utun 没有打开）。
var ErrNoStack = errors.New("网络栈没有运行")

// Streams 返回经数据面计数的开流入口（出口检查用它，开流次数一起算）。
func (p *DataPlane) Streams() stack.Opener { return countingOpener{p, "exit_check"} }
