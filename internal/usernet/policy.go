// Package usernet 是虚拟机的用户态网络在宿主一侧的出站控制（设计文档 4.5）：
// 出站策略、绑定物理网卡的拨号器、一律回 REFUSED 的 DNS，以及 gvisor-tap-vsock 的配置组装。
//
// 底线 6（docs/m1-plan.md）：本包不 import vmhost，vmhost 也不 import 本包；
// 两者只在 abroad-vm 里经网卡 fd 组装在一起。
package usernet

import (
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// 不允许虚拟机访问的 IPv4 网段（除精确白名单外）：本机、局域网、私有、保留、文档、组播、广播。
var blockedV4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), // 链路本地（含云厂商元数据地址）
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"), // 基准测试网段，也是本项目的 fake-IP 网段
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"), // 组播
	netip.MustParsePrefix("240.0.0.0/4"), // 保留，含 255.255.255.255 广播
}

// Policy 是虚拟机的出站策略：只放行公网 IPv4（TCP、UDP）。
// 例外只有精确的“地址 + 端口”白名单（设计 4.5：代理服务器在局域网或本机时，用户确认后加入），
// 只有它能放行私有或回环目标。没有任何“全开”开关，也不读环境变量。
//
// 并发：构造后不可变，可被多个 goroutine 同时调用；要改白名单就整体换一个 Policy。
type Policy struct {
	Allow []netip.AddrPort
	// OwnAddrs 返回本机各网卡自己的地址（它们上面监听的服务等同本机）；为空时用 InterfaceAddrs，缓存 5 秒。
	OwnAddrs func() []netip.Addr
	// InterfaceAddrs 可注入（测试用）；默认 net.InterfaceAddrs。出错时一律当作“是本机地址”（拒绝），且不缓存。
	InterfaceAddrs func() ([]net.Addr, error)

	mu    sync.Mutex
	own   []netip.Addr
	ownAt time.Time
}

// Whitelisted 报告 dst 是否在精确白名单里。
func (p *Policy) Whitelisted(dst netip.AddrPort) bool {
	a := dst.Addr().Unmap().WithZone("")
	for _, w := range p.Allow {
		if w.Addr().Unmap() == a && w.Port() == dst.Port() {
			return true
		}
	}
	return false
}

// Allows 报告虚拟机能否经 network（"tcp" 或 "udp"）访问 dst。
func (p *Policy) Allows(network string, dst netip.AddrPort) bool {
	if network != "tcp" && network != "udp" {
		return false
	}
	if p.Whitelisted(dst) {
		return true
	}
	a := dst.Addr().Unmap().WithZone("")
	if !a.Is4() || dst.Port() == 0 {
		return false // IPv6 一律拒绝（设计 4.5）
	}
	for _, b := range blockedV4 {
		if b.Contains(a) {
			return false
		}
	}
	return !p.isOwn(a)
}

func (p *Policy) isOwn(a netip.Addr) bool {
	var own []netip.Addr
	if p.OwnAddrs != nil {
		own = p.OwnAddrs()
	} else {
		p.mu.Lock()
		if p.ownAt.IsZero() || time.Since(p.ownAt) > 5*time.Second {
			list := p.InterfaceAddrs
			if list == nil {
				list = net.InterfaceAddrs
			}
			addrs, err := list()
			if err != nil {
				p.mu.Unlock()
				return true // 查不到本机地址：失败即拒绝，下次重查
			}
			p.own = p.own[:0]
			for _, ad := range addrs {
				if n, ok := ad.(*net.IPNet); ok {
					if ip, ok := netip.AddrFromSlice(n.IP); ok {
						p.own = append(p.own, ip.Unmap())
					}
				}
			}
			p.ownAt = time.Now()
		}
		own = append(own, p.own...)
		p.mu.Unlock()
	}
	for _, o := range own {
		if o.Unmap() == a {
			return true
		}
	}
	return false
}

// PolicyRef 持有当前的出站策略。gvisor 的 forwarder 把 Outbound 闭包捕获死了，VirtualNetwork 又关不掉，
// 所以 Outbound 包装和 Dialer 都经它取策略：以后换白名单只要 Store 一个新 Policy，不用重启进程。
type PolicyRef struct{ p atomic.Pointer[Policy] }

// NewPolicyRef 返回持有 p 的引用。
func NewPolicyRef(p *Policy) *PolicyRef {
	r := &PolicyRef{}
	r.p.Store(p)
	return r
}

// Load 返回当前策略。
func (r *PolicyRef) Load() *Policy { return r.p.Load() }

// Store 整体替换策略（Policy 构造后不可变）。
// 注意：收紧模式的 Outbound 持有启动时白名单的快照，换白名单必须重启 abroad-vm，不能只调 Store。
func (r *PolicyRef) Store(p *Policy) { r.p.Store(p) }

func (r *PolicyRef) Allows(network string, dst netip.AddrPort) bool {
	p := r.p.Load()
	return p != nil && p.Allows(network, dst)
}

func (r *PolicyRef) Whitelisted(dst netip.AddrPort) bool {
	p := r.p.Load()
	return p != nil && p.Whitelisted(dst)
}
