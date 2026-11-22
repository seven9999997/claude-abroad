package usernet

import (
	"fmt"
	"net/netip"
)

// 主代理第一跳那一组的运行中整体替换（M5-4，m5-plan v2.2 决定 1；design 4.5、R20 的又一处运行中可变）：
// 用户在应用里换节点时，abroadd 经 host.sock 把这一组整体换掉，不用重启 abroad-vm。
//   - 第一跳：公网 IPv4 字面量或主机名，带端口（同 --first-hop 的写法，ParseFirstHops 校验）；
//   - 局域网节点：私有 IPv4:端口，只有带“已经用户确认”的标记才接受（标记由 abroadd 按确认记录填写）；
//   - 回环一律拒绝（本机代理软件不能当主代理，M3 已定）；组播、fake 段、Anthropic 段、链路本地、IPv6 拒绝。
// 整份校验通过才原子替换；分流出口那一组（exitendpoints.go）和启动时的静态白名单不受影响。
// 被移除的第一跳域名连同网关为它答过的地址一起失效。

// MaxMainLAN 是局域网节点的上限（一条链只有一个第一跳，这里留余量）。
const MaxMainLAN = 8

// MainLAN 是一个局域网主代理节点。
type MainLAN struct {
	Addr      string `json:"addr"`      // IPv4:端口
	Confirmed bool   `json:"confirmed"` // 用户确认过
}

// MainHops 是主代理那一组的整份替换请求。
type MainHops struct {
	Hops []string  `json:"hops"` // "host:port[,port|lo-hi…]"
	LAN  []MainLAN `json:"lan"`
}

type mainSet struct {
	hops  []FirstHop
	byDom map[string]int // 域名 → hops 下标
	lan   []netip.AddrPort
}

func newMainSet(hops []FirstHop, lan []netip.AddrPort) *mainSet {
	ms := &mainSet{hops: hops, byDom: map[string]int{}, lan: lan}
	for i, h := range hops {
		if _, isIP := h.Addr(); !isIP {
			ms.byDom[h.Host] = i
		}
	}
	return ms
}

var mainLANPrefixes = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("100.64.0.0/10"),
}
var vmSubnet = netip.MustParsePrefix("192.168.127.0/24")

// ValidateMainHops 校验整份请求，返回解析好的集合；任何一项不对都整体拒绝，错误里指出是哪一项。
func ValidateMainHops(m MainHops) ([]FirstHop, []netip.AddrPort, error) {
	hops, err := ParseFirstHops(m.Hops)
	if err != nil {
		return nil, nil, err
	}
	if len(m.LAN) > MaxMainLAN {
		return nil, nil, fmt.Errorf("局域网节点最多 %d 个", MaxMainLAN)
	}
	var lan []netip.AddrPort
	for i, x := range m.LAN {
		bad := func(why string) error { return fmt.Errorf("第 %d 个局域网节点：%s", i+1, why) }
		ap, err := netip.ParseAddrPort(x.Addr)
		if err != nil || ap.Port() == 0 {
			return nil, nil, bad("要写成 IPv4:端口")
		}
		a := ap.Addr().Unmap()
		if err := ValidExitAddr(a, false); err != nil { // 回环、组播、fake 段、Anthropic 段、链路本地、IPv6
			return nil, nil, bad(err.Error())
		}
		in := false
		for _, p := range mainLANPrefixes {
			if p.Contains(a) {
				in = true
			}
		}
		if !in || vmSubnet.Contains(a) {
			return nil, nil, bad("不是局域网地址（公网节点请放在第一跳里）")
		}
		if !x.Confirmed {
			return nil, nil, bad("局域网节点要先由用户确认")
		}
		lan = append(lan, netip.AddrPortFrom(a, ap.Port()))
	}
	return hops, lan, nil
}

// SetMainHops 整体替换主代理那一组（校验不过不改任何东西）。
func (o *Outbound) SetMainHops(m MainHops) error {
	hops, lan, err := ValidateMainHops(m)
	if err != nil {
		return err
	}
	next := newMainSet(hops, lan)
	o.mu.Lock()
	defer o.mu.Unlock()
	for name := range o.answered {
		if _, keep := next.byDom[name]; !keep {
			delete(o.answered, name) // 被移除的第一跳域名连同学到的地址一起失效
		}
	}
	o.main.Store(next)
	return nil
}

// MainLANCount 是当前局域网主代理节点的个数（诊断用）。
func (o *Outbound) MainLANCount() int { return len(o.main.Load().lan) }

// IsMainHopDomain 报告 name 是不是主代理第一跳的域名。
func (o *Outbound) IsMainHopDomain(name string) bool {
	_, ok := o.main.Load().byDom[name]
	return ok
}
