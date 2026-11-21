// Package exitconf 是分流出口的配置（m4-plan M4-7 v2.8 决定 1）：WireGuard、OpenVPN、订阅 mihomo 三种配置的
// 指令白名单、服务器端点的提取，以及从校验过的结构重新生成配置（不把用户的原文交给引擎）。
// 纯计算，各平台都能编。daemon 收下配置时（M4-8）和 agent 下发前用同一份规则。
//
// 错误只说是第几行、哪个指令、为什么，不回显值（值里可能有密钥、账号）。
package exitconf

import (
	"claude-abroad/internal/rules"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// Kind 是出口的种类。
type Kind string

const (
	WireGuard Kind = "wireguard"
	OpenVPN   Kind = "openvpn"
	Clash     Kind = "clash" // 订阅 mihomo
)

// ErrInvalid 包在所有校验错误外面。
var ErrInvalid = errors.New("出口配置不符合要求")

// LineError 是某一行的问题（Line 从 1 开始；0 表示整份配置）。
type LineError struct {
	Line   int
	Name   string // 指令或键名
	Reason string
}

func (e *LineError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("第 %d 行（%s）：%s", e.Line, e.Name, e.Reason)
	}
	if e.Name != "" {
		return e.Name + "：" + e.Reason
	}
	return e.Reason
}

func (e *LineError) Unwrap() error { return ErrInvalid }

func lineErr(line int, name, reason string) error {
	return &LineError{Line: line, Name: name, Reason: reason}
}

// Endpoint 是出口的服务器端点：IPv4 字面量或域名，端口，协议（udp、tcp）。
type Endpoint struct {
	Host  string `json:"host"`
	Port  int    `json:"port"`
	Proto string `json:"proto"`
}

// IsIP 报告 Host 是不是 IP 字面量。
func (e Endpoint) IsIP() bool { _, err := netip.ParseAddr(e.Host); return err == nil }

// ErrIPv6Endpoint 是 IPv6 端点（暂不支持：虚拟机只走 IPv4，design 4.5）。
var ErrIPv6Endpoint = errors.New("暂不支持 IPv6 的服务器端点")

var domainRe = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// ParseHostPort 解析 host:port（或只有 host，用 defPort）。IPv6 一律拒绝。
func ParseHostPort(s string, defPort int, proto string) (Endpoint, error) {
	s = strings.TrimSpace(s)
	host, port := s, defPort
	if strings.HasPrefix(s, "[") {
		return Endpoint{}, ErrIPv6Endpoint
	}
	if h, p, err := net.SplitHostPort(s); err == nil {
		n, err := strconv.Atoi(p)
		if err != nil {
			return Endpoint{}, errors.New("端口不是数字")
		}
		host, port = h, n
	} else if strings.Count(s, ":") > 1 {
		return Endpoint{}, ErrIPv6Endpoint
	}
	return checkEndpoint(host, port, proto)
}

func checkEndpoint(host string, port int, proto string) (Endpoint, error) {
	if port <= 0 || port > 65535 {
		return Endpoint{}, errors.New("端口不在 1–65535")
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if !a.Is4() {
			return Endpoint{}, ErrIPv6Endpoint
		}
		if a.IsUnspecified() || a.IsMulticast() || a.IsLoopback() {
			return Endpoint{}, errors.New("服务器地址不对")
		}
		return Endpoint{Host: a.String(), Port: port, Proto: proto}, nil
	}
	if len(host) > 253 || !domainRe.MatchString(host) {
		return Endpoint{}, errors.New("服务器不是 IPv4 地址也不是合法的域名")
	}
	return Endpoint{Host: strings.ToLower(host), Port: port, Proto: proto}, nil
}

// ValidServerAddr 判断一个地址能不能作为分流出口的服务器（M4-8，m4-plan v2.12 决定 1）：只收 IPv4；
// 拒绝回环（allowLoopback 时例外，只给开发标记）、组播、未指定、广播、链路本地、fake-IP 段（198.18.0.0/15）、
// Anthropic 的地址段。私有地址可以（局域网里的服务器）。宿主的出站白名单（usernet）和虚拟机里的 agent 用同一份规则。
func ValidServerAddr(a netip.Addr, allowLoopback bool) error {
	a = a.Unmap()
	switch {
	case !a.Is4():
		return errors.New("只支持 IPv4")
	case a.IsLoopback() && !allowLoopback:
		return errors.New("不能是回环地址")
	case a.IsMulticast():
		return errors.New("不能是组播地址")
	case a.IsUnspecified(), a == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		return errors.New("不能是未指定或广播地址")
	case a.IsLinkLocalUnicast():
		return errors.New("不能是链路本地地址")
	case fakeIPNet.Contains(a):
		return errors.New("不能是 fake-IP 段的地址")
	case rules.InAnthropicNet(a):
		return errors.New("不能是 Anthropic 的地址")
	}
	return nil
}

// fakeIPNet 是宿主网络栈分配给域名的 fake-IP 段。
var fakeIPNet = netip.MustParsePrefix("198.18.0.0/15")
