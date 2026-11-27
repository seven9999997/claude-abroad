package usernet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// Rules 是拨号器需要的策略接口（Policy 实现了它）。
type Rules interface {
	Allows(network string, dst netip.AddrPort) bool
	Whitelisted(dst netip.AddrPort) bool
}

// ErrDenied 是出站策略不允许的目标。
var ErrDenied = errors.New("出站策略不允许这个目标")

// Dialer 是虚拟机出站的唯一拨号器（设计 4.5，经补丁 0004 + 0006 注入 gvisor-tap-vsock）：
//   - 目标必须是 IP 字面量（域名一律拒绝：虚拟机里只有引擎自己用 DoH 解析）；
//   - 拨号前再过一次出站策略（与 gvisor 的 Outbound 双保险）；
//   - 拨号前调用 BeforeDial（收紧模式下是 Outbound.BeforeDial：只放行路线第一跳，M2-1a），
//     它报错就拒绝，计 hop_denied；
//   - 回环白名单目标不绑网卡（由 abroad-vm 代为连本机 127.0.0.1），其余一律用 IP_BOUND_IF
//     绑定当前物理网卡；网卡查询或绑定失败直接返回错误，绝不退回不绑定的拨号。
type Dialer struct {
	Rules Rules
	// Interface 返回当前物理网卡名（必填，由组装方注入；abroad-vm 用 physif.Watcher.Current）。
	// 缓存、后台刷新、失败缓存都在它那一层，拨号器每次拨号都调用它。
	Interface func(ctx context.Context) (string, error)
	// OnBindFailed 在绑定网卡失败时调用（abroad-vm 用 physif.Watcher.Invalidate：网卡可能刚变了，
	// 下次拨号要重新查）。可为 nil。
	OnBindFailed func()
	BeforeDial   func(network string, dst netip.AddrPort) error
	// OnDialFailed 在虚拟机出站拨号（DialContext，已过出站策略和 BeforeDial）失败时调用，class 见 DialFailClasses。
	// 组装方只能记类别和“第几个第一跳”，不能把 dst 写进日志。可为 nil。
	OnDialFailed func(network string, dst netip.AddrPort, class string)
	// Timeout 是单次拨号的上限（默认 DialTimeout）。gvisor 的 TCP 转发器只允许 10 个在途连接，
	// 不设上限的话，节点黑洞时 SYN 各卡约 75 秒，会堵住虚拟机的全部出站。
	Timeout time.Duration

	mu     sync.Mutex
	ifname string // 最近一次查到的网卡名（只用于诊断）

	// 计数（不含目标地址），见 Counters。
	denied, bindFailed, dialOK, dialOKLoopback, hopDenied, resolverDialOK, directDialOK atomic.Int64
	fails                                                                               dialFailCounts // DialContext 的失败，按类别
}

// DialTimeout 是默认的单次拨号上限。
const DialTimeout = 10 * time.Second

// Counters 是拨号器的计数快照：被策略拒绝、绑定网卡失败、拨号成功。
type Counters struct {
	PolicyDenied int64 `json:"policy_denied"`
	BindFailed   int64 `json:"bind_failed"`
	DialOK       int64 `json:"dial_ok"`
	// DialOKLoopback 是其中不绑网卡的回环白名单拨号成功数。dial_ok == dial_ok_loopback 说明
	// 一次绑物理网卡（向外）的拨号都没有。
	DialOKLoopback int64 `json:"dial_ok_loopback"`
	// HopDenied 是收紧模式下因不是路线第一跳被拒的拨号（与 policy_denied 分开）。
	HopDenied int64 `json:"hop_denied"`
	// ResolverDialOK 是 abroad-vm 自己的 DoH 解析器的拨号成功数（只连 DoH 的 IP:443），不计入 dial_ok。
	ResolverDialOK int64 `json:"resolver_dial_ok"`
	// DirectDialOK 是直连拨号模块（design 6.3）的拨号成功数，不计入 dial_ok。
	DirectDialOK int64 `json:"direct_dial_ok"`
	// DialFail 是虚拟机出站拨号（已过策略）失败的计数，按类别（DialFailClasses；dns 是网关为第一跳域名解析失败）。
	DialFail map[string]int64 `json:"dial_fail"`
}

// Counters 返回计数快照。
func (d *Dialer) Counters() Counters {
	return Counters{PolicyDenied: d.denied.Load(), BindFailed: d.bindFailed.Load(), DialOK: d.dialOK.Load(),
		DialOKLoopback: d.dialOKLoopback.Load(), HopDenied: d.hopDenied.Load(), ResolverDialOK: d.resolverDialOK.Load(),
		DirectDialOK: d.directDialOK.Load(), DialFail: d.DialFailCounts()}
}

// physical 取当前物理网卡（经注入的 Interface，失败即拒绝）。
func (d *Dialer) physical(ctx context.Context) (string, error) {
	if d.Interface == nil {
		return "", fmt.Errorf("%w：没有配置物理网卡查询", errPhysical)
	}
	name, err := d.Interface(ctx)
	if err != nil {
		return "", fmt.Errorf("%w：%w", errPhysical, err)
	}
	if name == "" {
		return "", fmt.Errorf("%w：结果为空", errPhysical)
	}
	d.mu.Lock()
	d.ifname = name
	d.mu.Unlock()
	return name, nil
}

// CurrentInterface 返回最近一次查到的物理网卡名（没查过时为空）。网卡名不算敏感信息。
func (d *Dialer) CurrentInterface() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ifname
}

// bindFailedNow 计一次绑定失败，并通知组装方让网卡查询结果作废。
func (d *Dialer) bindFailedNow() {
	d.bindFailed.Add(1)
	if d.OnBindFailed != nil {
		d.OnBindFailed()
	}
}

// DialContext 满足 gvisor-tap-vsock 的 types.DialContext。
func (d *Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "udp" {
		return nil, fmt.Errorf("不支持的网络类型 %q", network)
	}
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return nil, fmt.Errorf("目标必须是 IP 地址加端口：%w", ErrDenied)
	}
	ap = netip.AddrPortFrom(ap.Addr().Unmap().WithZone(""), ap.Port())
	if d.Rules == nil || !d.Rules.Allows(network, ap) {
		d.denied.Add(1)
		return nil, ErrDenied
	}
	if d.BeforeDial != nil {
		if err := d.BeforeDial(network, ap); err != nil {
			d.hopDenied.Add(1)
			return nil, fmt.Errorf("拨号前检查：%w", err)
		}
	}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = DialTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	nd := net.Dialer{}
	if !(ap.Addr().IsLoopback() && d.Rules.Whitelisted(ap)) {
		ifname, err := d.physical(ctx)
		if err != nil {
			d.bindFailed.Add(1)
			d.noteFail(network, ap, DialFailBind)
			return nil, err
		}
		ctl, err := bindControl(ifname)
		if err != nil {
			d.bindFailedNow()
			d.noteFail(network, ap, DialFailBind)
			return nil, err
		}
		nd.Control = ctl
	}
	c, err := nd.DialContext(ctx, network, ap.String())
	switch {
	case err == nil:
		d.dialOK.Add(1)
		if nd.Control == nil {
			d.dialOKLoopback.Add(1)
		}
	case nd.Control != nil && errors.Is(err, errBind):
		d.bindFailedNow()
		d.noteFail(network, ap, DialFailBind)
	default:
		d.noteFail(network, ap, ClassifyDialError(err))
	}
	return c, err
}

// DialResolver 是 abroad-vm 自己的 DoH 解析器专用的拨号：只允许 tcp 连 allowed 里的地址，
// 绑定物理网卡（失败即拒绝），不经出站策略和 BeforeDial，计 resolver_dial_ok 而不是 dial_ok。
func (d *Dialer) DialResolver(ctx context.Context, network, addr string, allowed []netip.AddrPort) (net.Conn, error) {
	ap, err := netip.ParseAddrPort(addr)
	if network != "tcp" || err != nil {
		return nil, ErrDenied
	}
	ap = netip.AddrPortFrom(ap.Addr().Unmap().WithZone(""), ap.Port())
	ok := false
	for _, a := range allowed {
		ok = ok || a == ap
	}
	if !ok {
		return nil, ErrDenied
	}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = DialTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ifname, err := d.physical(ctx)
	if err != nil {
		d.bindFailed.Add(1)
		return nil, err
	}
	ctl, err := bindControl(ifname)
	if err != nil {
		d.bindFailedNow()
		return nil, err
	}
	nd := net.Dialer{Control: ctl}
	c, err := nd.DialContext(ctx, network, ap.String())
	switch {
	case err == nil:
		d.resolverDialOK.Add(1)
	case errors.Is(err, errBind):
		d.bindFailedNow()
	}
	return c, err
}

// errBind 包在绑定网卡失败的错误外面，便于识别。
var errBind = errors.New("绑定物理网卡失败")

// errPhysical 包在识别物理网卡失败的错误外面（拨号失败的类别里和绑定失败算一类）。
var errPhysical = errors.New("识别物理网卡")

// DialDirect 是直连拨号模块（design 6.3、m3-plan M3-4）专用的拨号：一律用 IP_BOUND_IF 绑定当前物理网卡，
// 网卡查询或绑定失败直接返回错误，绝不退回不绑定的拨号；不经虚拟机的出站策略和 BeforeDial
// （目标是否允许由直连拨号模块自己判定）。计 direct_dial_ok，不计入 dial_ok。
func (d *Dialer) DialDirect(ctx context.Context, network string, dst netip.AddrPort) (net.Conn, error) {
	if network != "tcp" && network != "udp" {
		return nil, fmt.Errorf("不支持的网络类型 %q", network)
	}
	dst = netip.AddrPortFrom(dst.Addr().Unmap().WithZone(""), dst.Port())
	if !dst.Addr().Is4() || dst.Port() == 0 {
		return nil, ErrDenied
	}
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = DialTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ifname, err := d.physical(ctx)
	if err != nil {
		d.bindFailed.Add(1)
		return nil, err
	}
	ctl, err := bindControl(ifname)
	if err != nil {
		d.bindFailedNow()
		return nil, err
	}
	nd := net.Dialer{Control: ctl}
	c, err := nd.DialContext(ctx, network, dst.String())
	switch {
	case err == nil:
		d.directDialOK.Add(1)
	case errors.Is(err, errBind):
		d.bindFailedNow()
	}
	return c, err
}
