// Package directdial 是直连拨号模块（design 6.3、m3-plan M3-4）。
//
// 服务端运行在以专用用户（_abroad）身份运行的进程里，替 abroadd 完成“本机网络”的拨号：
// 绑定物理网卡，拨好之后经 unix socket 用 SCM_RIGHTS 把连接的 fd 交给 abroadd。
// pf 只放行专用用户的出站，所以直连只能经这里发出。
//
// 不泄漏的约束（服务端独立判定，不信任 abroadd 一侧）：
//   - 置顶域名（Claude 相关）一律拒绝，不解析也不拨号；
//   - 目的地址落在 Anthropic 的地址段：拒绝；解析出的任何一个地址落在段内，整个请求拒绝；
//   - 回环、0/8、链路本地、组播、广播、fake 段、IPv6：拒绝；
//   - 域名用启用前记下的本机 DNS 解析；它是回环或 fake 段时拒绝（防止绕回我们自己或别的代理软件）。
//
// 服务端不打带目标的日志，只记各种结果的计数。
package directdial

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"claude-abroad/internal/rules"
)

// Version 是请求的版本号。
const Version = 1

// Request 是一个拨号请求：Host 和 IP 二选一。一条 unix 连接只处理一个请求。
type Request struct {
	V    int    `json:"v"`
	Op   string `json:"op,omitempty"`  // 空：拨号；"config"：下发运行时配置（m3-plan v2.8 决定 1）
	Net  string `json:"net,omitempty"` // 拨号时："tcp" 或 "udp"
	Host string `json:"host,omitempty"`
	IP   string `json:"ip,omitempty"`
	Port uint16 `json:"port,omitempty"`
	// Gen 是配置的代数（m3-plan v2.9）：拨号请求带上“abroadd 认为服务端该持有的代数”，配置请求带上这份配置的代数。
	Gen uint64 `json:"gen,omitempty"`
	// 配置请求的内容。
	DNS         []string `json:"dns,omitempty"`          // 本机原 DNS 服务器，最多 4 个
	PinnedExtra []string `json:"pinned_extra,omitempty"` // 置顶追加名单（"x.example" 或 "*.x.example"）
}

// 请求的大小上限：拨号请求 1 KiB，配置请求 64 KiB。
const (
	MaxDialRequest   = 1 << 10
	MaxConfigRequest = 64<<10 - 1
	MaxDNSServers    = 4
)

// runtimeConfig 是下发的运行时配置。
type runtimeConfig struct {
	gen    uint64
	dns    []netip.Addr
	pinned []string // 已校验、归一的模式
}

// decodeConfig 校验配置请求：DNS 最多 4 个、都要是 IPv4（回环、fake 段等不能用的剔除，不算错）；
// 置顶追加名单的每一项都要是合法的域名或 *.域名。任何一项不合法整体拒绝。
func decodeConfig(r Request) (*runtimeConfig, error) {
	if len(r.DNS) > MaxDNSServers || r.Net != "" || r.Host != "" || r.IP != "" || r.Port != 0 {
		return nil, errBad
	}
	c := &runtimeConfig{gen: r.Gen}
	for _, s := range r.DNS {
		a, err := netip.ParseAddr(s)
		if err != nil || a.Zone() != "" {
			return nil, errBad
		}
		if checkDNSServer(a) == nil {
			c.dns = append(c.dns, a.Unmap())
		}
	}
	for _, p := range r.PinnedExtra {
		wild := strings.HasPrefix(p, "*.")
		n, err := rules.NormalizeName(strings.TrimPrefix(p, "*."))
		if err != nil || strings.Contains(n, "*") {
			return nil, errBad
		}
		if wild {
			n = "*." + n
		}
		c.pinned = append(c.pinned, n)
	}
	return c, nil
}

// Reply 是应答；OK 时随附连接的 fd。
type Reply struct {
	OK   bool   `json:"ok"`
	Code string `json:"code,omitempty"`
}

// 应答码。
const (
	CodeBadRequest    = "bad_request"
	CodePinned        = "pinned"
	CodeForbiddenDst  = "forbidden_dst"
	CodeResolveFailed = "resolve_failed"
	CodeDialFailed    = "dial_failed"
	CodeBusy          = "busy"
	CodeTimeout       = "timeout"
	// CodeStaleConfig：服务端没有配置，或持有的代数与请求里的不一致（例如它刚重启过）。不解析、不拨号；
	// 客户端下发当前配置后重试一次（m3-plan v2.9）。
	CodeStaleConfig = "stale_config"
)

var codes = []string{CodeBadRequest, CodePinned, CodeForbiddenDst, CodeResolveFailed, CodeDialFailed, CodeBusy, CodeTimeout, CodeStaleConfig}

// CodeError 是服务端拒绝或失败时客户端得到的错误，可以用 errors.Is 和下面的变量比较。
type CodeError struct{ Code string }

func (e *CodeError) Error() string { return "直连拨号失败：" + e.Code }

// Is 按应答码比较。
func (e *CodeError) Is(target error) bool {
	t, ok := target.(*CodeError)
	return ok && t.Code == e.Code
}

// 可以用 errors.Is 判断的错误。ErrPinned 要被网络栈单独识别（改走主代理，M3-5）。
var (
	ErrBadRequest    = &CodeError{CodeBadRequest}
	ErrPinned        = &CodeError{CodePinned}
	ErrForbiddenDst  = &CodeError{CodeForbiddenDst}
	ErrResolveFailed = &CodeError{CodeResolveFailed}
	ErrDialFailed    = &CodeError{CodeDialFailed}
	ErrBusy          = &CodeError{CodeBusy}
	ErrTimeout       = &CodeError{CodeTimeout}
	ErrStaleConfig   = &CodeError{CodeStaleConfig}
)

// target 是校验过的请求目标。
type target struct {
	net  string
	host string     // 归一后的域名；空表示目标是 IP
	ip   netip.Addr // host 为空时有效
	port uint16
	gen  uint64 // 请求方认为服务端该持有的配置代数
}

var errBad = errors.New(CodeBadRequest)

// decodeRequest 严格解码：未知字段、多余内容、版本不对、Host 和 IP 同时给或都不给、端口为 0 都拒绝。
// 返回的错误是应答码（errBad、或 pinned/forbidden_dst 由调用方另判）。
func decodeRequest(b []byte) (target, error) {
	r, err := parseRequest(b)
	if err != nil || r.Op != "" || len(b) > MaxDialRequest {
		return target{}, errBad
	}
	return decodeDial(r)
}

// parseRequest 严格解码 JSON（未知字段、多余内容拒绝），不做语义校验。
func parseRequest(b []byte) (Request, error) {
	var r Request
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&r); err != nil || d.More() || r.V != Version {
		return Request{}, errBad
	}
	return r, nil
}

func decodeDial(r Request) (target, error) {
	if len(r.DNS) > 0 || len(r.PinnedExtra) > 0 {
		return target{}, errBad
	}
	if r.V != Version || (r.Net != "tcp" && r.Net != "udp") || r.Port == 0 || (r.Host == "") == (r.IP == "") {
		return target{}, errBad
	}
	t := target{net: r.Net, port: r.Port, gen: r.Gen}
	if r.Host != "" {
		if _, err := netip.ParseAddr(strings.Trim(r.Host, "[]")); err == nil {
			return target{}, errBad // IP 要放在 IP 字段
		}
		n, err := rules.NormalizeName(r.Host)
		if err != nil {
			return target{}, errBad
		}
		t.host = n
		return t, nil
	}
	a, err := netip.ParseAddr(r.IP)
	if err != nil || a.Zone() != "" {
		return target{}, errBad
	}
	t.ip = a.Unmap()
	return t, nil
}

var (
	fakeNet      = netip.MustParsePrefix("198.18.0.0/15")
	zeroNet      = netip.MustParsePrefix("0.0.0.0/8")
	linkLocalNet = netip.MustParsePrefix("169.254.0.0/16")
	broadcast    = netip.MustParseAddr("255.255.255.255")
)

// checkDst 判定一个目的地址：Anthropic 地址段回 pinned；回环、0/8、链路本地、组播、广播、fake 段、
// IPv6 回 forbidden_dst；其余（含私有网段：公司内网）放行。
func checkDst(a netip.Addr) string {
	a = a.Unmap()
	switch {
	case !a.IsValid():
		return CodeForbiddenDst
	case rules.InAnthropicNet(a):
		return CodePinned
	case !a.Is4(), a.IsLoopback(), a.IsUnspecified(), zeroNet.Contains(a), linkLocalNet.Contains(a),
		a.IsMulticast(), a == broadcast, fakeNet.Contains(a):
		return CodeForbiddenDst
	}
	return ""
}

// checkDNSServer 判定配置的本机 DNS 服务器能不能用：回环、fake 段、0/8、IPv6 不能用。
func checkDNSServer(a netip.Addr) error {
	a = a.Unmap()
	if !a.IsValid() || !a.Is4() || a.IsLoopback() || a.IsUnspecified() || zeroNet.Contains(a) || fakeNet.Contains(a) ||
		a.IsMulticast() || a == broadcast {
		return fmt.Errorf("本机 DNS 服务器不可用于直连解析")
	}
	return nil
}
