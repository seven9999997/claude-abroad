package sysnet

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// 虚拟网卡（别的 VPN、TUN）的名字：这些前缀加数字（M2-8 起在 preflight，M2-9a 挪到这里共用）。
var virtualIfRe = regexp.MustCompile(`^(utun|ipsec|ppp|tun|tap|wg|feth|gif|stf)[0-9]+$`)

// IsVirtualIf 报告接口名是不是虚拟网卡。
func IsVirtualIf(name string) bool { return virtualIfRe.MatchString(name) }

// RouteRow 是路由表里的一行（只取判断冲突要用的列）。
type RouteRow struct {
	Dest    string       // 原样的目的写法
	Default bool         // 目的是 default
	Prefix  netip.Prefix // 解析出的目的网段（Default 时为零值）
	OK      bool         // 目的解析成功（或是 default）
	Iface   string
}

// ParseNetDest 解析 netstat 的目的写法：default；`10`、`169.254`、`192.168.1` 这类省略了尾部 0 的网段
// （掩码按给出的段数，每段 8 位）；`2/7`、`128.0/1`、`10.14.16/24` 这类带掩码的；四段的主机地址（/32）。
func ParseNetDest(s string) (p netip.Prefix, isDefault bool, err error) {
	if s == "default" {
		return netip.Prefix{}, true, nil
	}
	addr, bitsStr, hasBits := strings.Cut(s, "/")
	parts := strings.Split(addr, ".")
	if len(parts) < 1 || len(parts) > 4 {
		return netip.Prefix{}, false, ErrUnknownOutput
	}
	var b [4]byte
	for i, x := range parts {
		n, err := strconv.Atoi(x)
		if err != nil || n < 0 || n > 255 || x == "" {
			return netip.Prefix{}, false, ErrUnknownOutput
		}
		b[i] = byte(n)
	}
	bits := 8 * len(parts)
	if hasBits {
		n, err := strconv.Atoi(bitsStr)
		if err != nil || n < 0 || n > 32 {
			return netip.Prefix{}, false, ErrUnknownOutput
		}
		bits = n
	}
	return netip.PrefixFrom(netip.AddrFrom4(b), bits).Masked(), false, nil
}

// ParseNetstat 解析 `netstat -rn -f inet` 的输出。没有 “Internet:” 段和表头时返回 ErrUnknownOutput。
func ParseNetstat(out []byte) ([]RouteRow, error) {
	lines := strings.Split(string(out), "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "Internet:" && i+1 < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i+1]), "Destination") {
			start = i + 2
			break
		}
	}
	if start < 0 {
		return nil, ErrUnknownOutput
	}
	var rows []RouteRow
	for _, l := range lines[start:] {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		if len(f) < 4 {
			return nil, ErrUnknownOutput
		}
		r := RouteRow{Dest: f[0], Iface: f[3]}
		p, def, err := ParseNetDest(f[0])
		r.Prefix, r.Default, r.OK = p, def, err == nil
		rows = append(rows, r)
	}
	return rows, nil
}

// specialRanges 是“不算冲突”的范围：私有、回环、链路本地、组播、保留（公司 VPN 的内网分段路由不该拦着启用）。
var specialRanges = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// fakeIPRange 是我们的 fake-IP 段（设计 4.3）：别人的路由与它重叠就会抢走我们的 DNS 和假地址。
var fakeIPRange = netip.MustParsePrefix("198.18.0.0/15")

func within(p netip.Prefix) bool {
	for _, r := range specialRanges {
		if r.Bits() <= p.Bits() && r.Contains(p.Addr()) {
			return true
		}
	}
	return false
}

// ConflictIfaces 返回占着路由的别的虚拟网卡（M2-9a 决定 1，去重、按出现顺序）：经虚拟网卡、且不是 ours 的路由里，
// 目的是 default、目的解析不了、与 198.18.0.0/15 重叠、或不完全落在私有/回环/链路本地/组播/保留范围内的，算冲突。
func ConflictIfaces(rows []RouteRow, ours string) []string {
	var out []string
	for _, r := range rows {
		if !IsVirtualIf(r.Iface) || r.Iface == ours || slices.Contains(out, r.Iface) {
			continue
		}
		if r.Default || !r.OK || r.Prefix.Overlaps(fakeIPRange) || !within(r.Prefix) {
			out = append(out, r.Iface)
		}
	}
	return out
}

// Conflicts 实现 Routes：netstat -rn -f inet 解析后按 ConflictIfaces 判断。
func (m MacRoutes) Conflicts(ctx context.Context, ours string) ([]string, error) {
	so, se, err := m.R.Run(ctx, PathNetstat, []string{"-rn", "-f", "inet"}, nil)
	if err != nil {
		return nil, fmt.Errorf("读路由表：%w（%s）", err, firstLine(se))
	}
	rows, err := ParseNetstat(so)
	if err != nil {
		return nil, err
	}
	return ConflictIfaces(rows, ours), nil
}
