package usernet

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"claude-abroad/internal/exitconf"
)

// 分流出口的服务器端点（M4-8，m4-plan v2.11 决定 4、v2.12 决定 1；design 4.5、R20 的有意放宽）：
// 放行集合在“主代理第一跳”（启动时给定、不变）之外多一组可以整体原子替换的端点，精确到“主机、协议、端口”。
//   - 主机是 IPv4 字面量：放行这个地址的这个协议和端口；
//   - 主机是域名：登记进网关 DNS 的代解析名单（agent 在虚拟机里查它，就是代解析），网关答出的地址过
//     ValidExitAddr 后记下，之后放行这些地址的这个协议和端口；被剔掉的地址不放行。
// 替换只碰这一组：主代理第一跳那一组的内容任何时候都不变。被移除的域名连同学到的地址一起失效。

// MaxExitEndpoints 是分流出口端点的上限（16 个出口 × 每个最多 8 个端点）。
const MaxExitEndpoints = 128

var exitHostRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

// ExitEndpoint 是一个分流出口的服务器端点。Proto 是 tcp 或 udp。
type ExitEndpoint struct {
	Host  string `json:"host"`
	Proto string `json:"proto"`
	Port  uint16 `json:"port"`
}

// ValidExitAddr 判断一个地址能不能作为分流出口的服务器放行（和虚拟机里的 agent 同一份规则：exitconf.ValidServerAddr）。
func ValidExitAddr(a netip.Addr, allowLoopback bool) error {
	return exitconf.ValidServerAddr(a, allowLoopback)
}

type protoPort struct {
	proto string
	port  uint16
}

// exitSet 是一次替换后的端点集合（替换时整体换掉，不原地改）。
type exitSet struct {
	ips  map[netip.Addr][]protoPort // IPv4 字面量 → 协议和端口
	doms map[string][]protoPort     // 域名 → 协议和端口
}

// SetExitEndpoints 整体替换分流出口的端点。任何一项不合法都整体拒绝（不改任何东西），错误里指出是第几项。
// allowLoopback 只给开发标记（同机对端）。
func (o *Outbound) SetExitEndpoints(eps []ExitEndpoint, allowLoopback bool) error {
	if len(eps) > MaxExitEndpoints {
		return fmt.Errorf("分流出口的端点最多 %d 个", MaxExitEndpoints)
	}
	next := &exitSet{ips: map[netip.Addr][]protoPort{}, doms: map[string][]protoPort{}}
	for i, e := range eps {
		bad := func(why string) error { return fmt.Errorf("第 %d 个端点：%s", i+1, why) }
		if e.Proto != "tcp" && e.Proto != "udp" {
			return bad("协议只能是 tcp 或 udp")
		}
		if e.Port == 0 {
			return bad("端口不能是 0")
		}
		pp := protoPort{e.Proto, e.Port}
		if a, err := netip.ParseAddr(e.Host); err == nil {
			if err := ValidExitAddr(a, allowLoopback); err != nil {
				return bad(err.Error())
			}
			next.ips[a.Unmap()] = append(next.ips[a.Unmap()], pp)
			continue
		}
		h := strings.ToLower(strings.TrimSuffix(e.Host, "."))
		if h != e.Host || len(h) > 253 || !exitHostRe.MatchString(h) {
			return bad("主机要是 IPv4 地址或小写的域名")
		}
		next.doms[h] = append(next.doms[h], pp)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for name := range o.exitAnswered {
		if _, keep := next.doms[name]; !keep {
			delete(o.exitAnswered, name) // 被移除的域名连同学到的地址一起失效
		}
	}
	o.exits = next
	o.exitLoopback = allowLoopback
	return nil
}

// ExitEndpoints 返回当前端点的个数（诊断用）。
func (o *Outbound) ExitEndpoints() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.exits == nil {
		return 0
	}
	n := 0
	for _, v := range o.exits.ips {
		n += len(v)
	}
	for _, v := range o.exits.doms {
		n += len(v)
	}
	return n
}

// exitRegisteredLocked 报告 name 是不是分流出口端点的域名。调用方持锁。
func (o *Outbound) exitRegisteredLocked(name string) bool {
	if o.exits == nil {
		return false
	}
	_, ok := o.exits.doms[name]
	return ok
}

// exitLearnLocked 记下网关为分流出口端点域名答出的地址（过 ValidExitAddr）。调用方持锁。
func (o *Outbound) exitLearnLocked(name string, addrs []netip.Addr, now time.Time) {
	if !o.exitRegisteredLocked(name) {
		return
	}
	if o.exitAnswered == nil {
		o.exitAnswered = map[string]map[netip.Addr]time.Time{}
	}
	m := o.exitAnswered[name]
	if m == nil {
		m = map[netip.Addr]time.Time{}
		o.exitAnswered[name] = m
	}
	for _, a := range addrs {
		if ValidExitAddr(a, o.exitLoopback) != nil {
			continue // 落在被禁范围的地址不进放行名单
		}
		m[a.Unmap()] = now
	}
	pruneAnswered(m, now)
}

// exitAllowedLocked 报告 network/dst 是否命中分流出口的端点。调用方持锁。
func (o *Outbound) exitAllowedLocked(network string, a netip.Addr, p uint16, now time.Time) bool {
	if o.exits == nil {
		return false
	}
	has := func(list []protoPort) bool {
		for _, x := range list {
			if x.proto == network && x.port == p {
				return true
			}
		}
		return false
	}
	if has(o.exits.ips[a]) {
		return true
	}
	for name, m := range o.exitAnswered {
		if t, ok := m[a]; ok && now.Sub(t) <= AnsweredKeep && has(o.exits.doms[name]) {
			return true
		}
	}
	return false
}

// pruneAnswered 去掉过期的、超过上限时淘汰最旧的。
func pruneAnswered(m map[netip.Addr]time.Time, now time.Time) {
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

// Keep 报告网关为 name 答出的地址 a 要不要留下（交给虚拟机、记进放行名单）：第一跳域名要公网 IPv4；
// 分流出口端点的域名要过 ValidExitAddr（私有地址可以）。两者都是时任一满足即可。
func (o *Outbound) Keep(name string, a netip.Addr) bool {
	if _, ok := o.byDom[name]; ok && publicV4(a) {
		return true
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.exitRegisteredLocked(name) && ValidExitAddr(a, o.exitLoopback) == nil
}
