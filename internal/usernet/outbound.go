package usernet

import (
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// 答过的地址保留多久、每个域名最多记多少个（M2-1a）。
const (
	AnsweredKeep   = time.Hour
	MaxAnsweredPer = 64
)

// ErrNotFirstHop 是收紧模式下不在放行集合里的目标。
var ErrNotFirstHop = errors.New("目标不是路线第一跳")

// Outbound 是收紧模式的放行集合（设计 4.5、R20，M2-1a）。虚拟机只能连三类目标：
//   - 出站白名单（--allow-outbound）里的精确“地址 + 端口”；
//   - IPv4 字面量的第一跳，端口相符；
//   - 网关 DNS 为第一跳域名答过的地址，端口属于这个域名的第一跳。
//
// 其余一律拒绝（拨号器计 hop_denied）。就算哪个引擎隐式退回直连，目标也到不了。
// tcp 和 udp 同样处理。并发安全。
type Outbound struct {
	// allow 是启动时白名单的快照。PolicyRef.Store 能在运行时换白名单，但两者会不一致：
	// 换白名单必须重启 abroad-vm（目前没有任何调用方在运行时换）。
	allow []netip.AddrPort
	// main 是主代理那一组（第一跳和用户确认过的局域网节点）：启动时由 --first-hop 给定，M5-4 起可以整体原子替换
	// （mainhops.go）。读不加锁。
	main atomic.Pointer[mainSet]
	now  func() time.Time

	mu       sync.Mutex
	answered map[string]map[netip.Addr]time.Time // 域名 → 地址 → 最后一次应答的时间

	// 分流出口的端点（M4-8，exitendpoints.go；mu 保护）：整体替换，不碰上面主代理第一跳的部分。
	exits        *exitSet
	exitAnswered map[string]map[netip.Addr]time.Time
	exitLoopback bool
}

// NewOutbound 构造放行集合。now 为 nil 时用 time.Now。
func NewOutbound(allow []netip.AddrPort, hops []FirstHop, now func() time.Time) *Outbound {
	if now == nil {
		now = time.Now
	}
	o := &Outbound{allow: allow, now: now, answered: map[string]map[netip.Addr]time.Time{}}
	o.main.Store(newMainSet(hops, nil))
	return o
}

// Hops 返回第一跳的条数。
func (o *Outbound) Hops() int { return len(o.main.Load().hops) }

// Registered 报告 name（小写、无末尾点）是否是登记过的第一跳域名或分流出口端点的域名（网关只代解析这些）。
func (o *Outbound) Registered(name string) bool {
	if _, ok := o.main.Load().byDom[name]; ok {
		return true
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.exitRegisteredLocked(name)
}

// Learn 记下网关 DNS 为 name 答出的地址（只对登记过的域名有效）。超过 MaxAnsweredPer 时淘汰最旧的。
func (o *Outbound) Learn(name string, addrs []netip.Addr) {
	now := o.now()
	o.mu.Lock()
	defer o.mu.Unlock()
	o.exitLearnLocked(name, addrs, now) // 分流出口端点的域名（过 ValidExitAddr）
	if _, ok := o.main.Load().byDom[name]; !ok {
		return
	}
	m := o.answered[name]
	if m == nil {
		m = map[netip.Addr]time.Time{}
		o.answered[name] = m
	}
	for _, a := range addrs {
		if publicV4(a) { // 第一跳只记公网 IPv4（原来由网关先过滤，现在网关按 Keep 过滤，这里再守一道）
			m[a.Unmap()] = now
		}
	}
	for a, t := range m {
		if now.Sub(t) > AnsweredKeep {
			delete(m, a)
		}
	}
	for len(m) > MaxAnsweredPer {
		var oldest netip.Addr
		var ot time.Time
		for a, t := range m {
			if !oldest.IsValid() || t.Before(ot) {
				oldest, ot = a, t
			}
		}
		delete(m, oldest)
	}
}

// BeforeDial 满足 Dialer.BeforeDial。
func (o *Outbound) BeforeDial(network string, dst netip.AddrPort) error {
	if network != "tcp" && network != "udp" {
		return ErrNotFirstHop
	}
	a, p := dst.Addr().Unmap(), dst.Port()
	for _, w := range o.allow {
		if w.Addr().Unmap() == a && w.Port() == p {
			return nil
		}
	}
	ms := o.main.Load()
	for _, w := range ms.lan {
		if w.Addr() == a && w.Port() == p {
			return nil
		}
	}
	for _, h := range ms.hops {
		if ha, isIP := h.Addr(); isIP && ha == a && h.HasPort(p) {
			return nil
		}
	}
	now := o.now()
	o.mu.Lock()
	defer o.mu.Unlock()
	for name, m := range o.answered {
		i, ok := ms.byDom[name]
		if !ok {
			continue
		}
		if t, ok := m[a]; ok && now.Sub(t) <= AnsweredKeep && ms.hops[i].HasPort(p) {
			return nil
		}
	}
	if o.exitAllowedLocked(network, a, p, now) {
		return nil
	}
	return ErrNotFirstHop
}
