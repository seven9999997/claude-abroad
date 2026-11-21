package usernet

import (
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"claude-abroad/internal/usererror"
)

// MaxFirstHops 是 --first-hop 的上限。
const MaxFirstHops = 8

// PortRange 是闭区间 [Lo, Hi]。
type PortRange struct{ Lo, Hi uint16 }

// FirstHop 是路线第一跳（设计 4.5，M2-1）：虚拟机只能连它。
// Host 是公网 IPv4 字面量或主机名（小写、不带末尾的点）；Ports 是允许的端口（hysteria2 的端口跳跃用区间）。
type FirstHop struct {
	Host  string
	Ports []PortRange
}

// Addr 返回 Host 作为 IPv4 字面量时的地址；是域名时 ok 为 false。
func (h FirstHop) Addr() (netip.Addr, bool) {
	a, err := netip.ParseAddr(h.Host)
	return a, err == nil
}

// HasPort 报告端口 p 是否在 Ports 里。
func (h FirstHop) HasPort(p uint16) bool {
	for _, r := range h.Ports {
		if p >= r.Lo && p <= r.Hi {
			return true
		}
	}
	return false
}

var hostLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// validHostname：字母数字和连字符组成的标签，总长不超过 253，最后一个标签不能全是数字。
func validHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	labels := strings.Split(h, ".")
	for _, l := range labels {
		if !hostLabel.MatchString(l) {
			return false
		}
	}
	last := labels[len(labels)-1]
	return strings.Trim(last, "0123456789") != ""
}

// ParseFirstHops 解析 --first-hop 的值：每项 "host:port[,port|lo-hi…]"，可重复，最多 MaxFirstHops 项；
// 同一个 host 出现多次时端口合并。IPv4 字面量必须是公网地址（局域网和本机走 --allow-outbound）。
func ParseFirstHops(items []string) ([]FirstHop, error) {
	bad := func(i int, why string) error {
		return usererror.New("解析路线第一跳", fmt.Sprintf("第 %d 项%s", i+1, why),
			"请写成 node.example.com:443 或 203.0.113.5:443,8000-8010 这样的形式。", nil)
	}
	var out []FirstHop
	index := map[string]int{}
	for i, s := range items {
		host, ports, ok := strings.Cut(s, ":")
		if !ok || ports == "" {
			return nil, bad(i, "缺少端口")
		}
		host = strings.ToLower(strings.TrimSuffix(host, "."))
		if a, err := netip.ParseAddr(host); err == nil {
			if !a.Is4() || a.Zone() != "" {
				return nil, bad(i, "的地址不是 IPv4")
			}
			if !publicV4(a) {
				return nil, bad(i, "的地址不是公网地址（局域网或本机的代理请用 --allow-outbound）")
			}
			host = a.String()
		} else if !validHostname(host) {
			return nil, bad(i, "的主机名无效")
		}
		var rs []PortRange
		for _, part := range strings.Split(ports, ",") {
			lo, hi, isRange := strings.Cut(part, "-")
			l, err1 := strconv.ParseUint(lo, 10, 16)
			h := l
			var err2 error
			if isRange {
				h, err2 = strconv.ParseUint(hi, 10, 16)
			}
			if err1 != nil || err2 != nil || l == 0 || h < l {
				return nil, bad(i, "的端口无效")
			}
			rs = append(rs, PortRange{uint16(l), uint16(h)})
		}
		if j, seen := index[host]; seen {
			out[j].Ports = append(out[j].Ports, rs...)
			continue
		}
		index[host] = len(out)
		out = append(out, FirstHop{Host: host, Ports: rs})
	}
	if len(out) > MaxFirstHops {
		return nil, usererror.New("解析路线第一跳", fmt.Sprintf("超过 %d 项", MaxFirstHops), "请减少第一跳条目。", nil)
	}
	return out, nil
}

// publicV4 报告 a 是否是公网 IPv4（不在 blockedV4 里）。
func publicV4(a netip.Addr) bool {
	a = a.Unmap()
	if !a.Is4() {
		return false
	}
	for _, b := range blockedV4 {
		if b.Contains(a) {
			return false
		}
	}
	return true
}
