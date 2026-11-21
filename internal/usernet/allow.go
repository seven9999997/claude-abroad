package usernet

import (
	"fmt"
	"net/netip"

	"claude-abroad/internal/usererror"
)

// MaxAllow 是出站白名单的上限。
const MaxAllow = 8

var (
	allowPrivate = []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("100.64.0.0/10"),
	}
	guestSubnet = netip.MustParsePrefix(Subnet)
	loopback    = netip.MustParseAddr("127.0.0.1")
)

// ParseAllow 解析出站白名单（设计 4.5：代理服务器在局域网或本机时，用户确认后加入）。
// 每项是 "IPv4:端口"：回环只收 127.0.0.1；其余只收 RFC1918 和 100.64.0.0/10，
// 且不能落在虚拟机自己的子网里。公网地址不需要白名单（本来就放行），也拒绝。去重，最多 MaxAllow 项。
func ParseAllow(items []string) ([]netip.AddrPort, error) {
	seen := map[netip.AddrPort]bool{}
	var out []netip.AddrPort
	for i, s := range items {
		ap, err := netip.ParseAddrPort(s)
		if err != nil || !ap.Addr().Is4() || ap.Port() == 0 {
			return nil, usererror.New("解析出站白名单", fmt.Sprintf("第 %d 项不是“IPv4 地址:端口”", i+1),
				"请写成 127.0.0.1:7890 或 192.168.1.2:1080 这样的形式。", nil)
		}
		a := ap.Addr()
		ok := a == loopback
		if !ok {
			for _, p := range allowPrivate {
				if p.Contains(a) {
					ok = true
				}
			}
			if guestSubnet.Contains(a) {
				ok = false
			}
		}
		if !ok {
			return nil, usererror.New("解析出站白名单", fmt.Sprintf("第 %d 项的地址不在允许的范围内", i+1),
				"只能是 127.0.0.1 或局域网地址（10/8、172.16/12、192.168/16、100.64/10，不含虚拟机子网 192.168.127.0/24）。", nil)
		}
		if !seen[ap] {
			seen[ap] = true
			out = append(out, ap)
		}
	}
	if len(out) > MaxAllow {
		return nil, usererror.New("解析出站白名单", fmt.Sprintf("超过 %d 项", MaxAllow), "请减少白名单条目。", nil)
	}
	return out, nil
}

// hasLoopback 报告白名单里是否有回环项。
func hasLoopback(allow []netip.AddrPort) bool {
	for _, a := range allow {
		if a.Addr().Unmap() == loopback {
			return true
		}
	}
	return false
}
