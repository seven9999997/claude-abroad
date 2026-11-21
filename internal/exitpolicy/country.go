package exitpolicy

import (
	_ "embed"
	"fmt"
	"net/netip"
	"strings"
)

// The list applies to observed EXIT addresses, never destination servers or
// proxy entry points. IPv6 business egress is separately denied by EgressGate.
//
//go:embed data/mainland-ipv4.txt
var mainlandIPv4 string

var mainlandPrefixes, mainlandListError = parseMainlandPrefixes(mainlandIPv4)

func parseMainlandPrefixes(data string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	for _, line := range strings.Fields(data) {
		prefix, err := netip.ParsePrefix(line)
		if err != nil || !prefix.Addr().Is4() || prefix.Bits() < 8 || prefix != prefix.Masked() {
			return nil, fmt.Errorf("invalid bundled mainland prefix %q", line)
		}
		prefixes = append(prefixes, prefix)
	}
	if len(prefixes) == 0 {
		return nil, fmt.Errorf("bundled mainland prefix list is empty")
	}
	return prefixes, nil
}

// InMainland 报告地址是否落在内置的大陆网段里（本地判断，不依赖任何查询；列表不可用时返回 false 和错误）。
func InMainland(a netip.Addr) (bool, error) {
	if mainlandListError != nil {
		return false, mainlandListError
	}
	a = a.Unmap()
	for _, prefix := range mainlandPrefixes {
		if prefix.Contains(a) {
			return true, nil
		}
	}
	return false, nil
}

// CheckExitCountry is mandatory independently of the user-selected IP rule.
// A mainland list match is enough to deny; absence from the list cannot prove
// a non-mainland exit and still requires matching country evidence.
func CheckExitCountry(ip string, location *Location) (State, string) {
	address, err := netip.ParseAddr(ip)
	if err != nil || !address.Is4() || !address.IsGlobalUnicast() || address.IsPrivate() {
		return Unknown, "尚未取得有效的检测出口"
	}
	if mainlandListError != nil {
		return Unknown, "大陆出口网段库不可用，外网保持关闭"
	}
	for _, prefix := range mainlandPrefixes {
		if prefix.Contains(address) {
			return Failed, "检测出口命中中国大陆 IP 网段，Claude 出国不允许使用大陆出口，外网保持关闭"
		}
	}
	if location == nil || location.IP != address.String() {
		return Unknown, "无法确认出口国家或地区，外网保持关闭"
	}
	country := strings.ToUpper(strings.TrimSpace(location.CountryCode))
	if len(country) != 2 || country[0] < 'A' || country[0] > 'Z' || country[1] < 'A' || country[1] > 'Z' || country == "ZZ" || country == "XX" {
		return Unknown, "无法确认出口国家或地区，外网保持关闭"
	}
	if country == "CN" {
		return Failed, "检测出口位于中国大陆，Claude 出国不允许使用大陆出口，外网保持关闭"
	}
	return Passed, "检测出口未命中大陆网段，国家或地区信息符合非大陆出口要求"
}
