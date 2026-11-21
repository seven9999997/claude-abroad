package managedproxy

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"claude-abroad/internal/guestabi"
	"claude-abroad/internal/mihomocfg"
	"claude-abroad/internal/usererror"
	"claude-abroad/internal/usernet"
)

// VMControllerSocket 是虚拟机里主代理 mihomo 的控制接口（unix socket，只在 /run/claude-abroad 下）。
const VMControllerSocket = guestabi.MihomoDir + "/controller.sock"

// VMRuntime 生成虚拟机里主代理 mihomo 的配置（设计文档 7.2），经控制面下发给 abroad-agent：
//   - 只含路线（Route）上的节点：配置大小有上限，无关节点的凭据也不必进虚拟机；
//   - 只听 127.0.0.1:7890，没有认证（sing-box 的 main 出站从虚拟机回环连过来）；
//   - 规则是 MATCH,CAGE 加 MATCH,REJECT 兜底，mode: rule，没有 DIRECT，不写 tun（出口不支持 UDP 时
//     mihomo 会跳过 MATCH,CAGE，没有兜底就落到 DIRECT，UDP 从本机真实 IP 出去）；
//   - 路线上的节点统一写 udp：只有每一跳的来源都声明了 udp: true、且没有 http 节点时才是 true；
//     第一跳是本机回环（127.0.0.1）时默认 false（VMOptions.LoopbackUDP 打开除外，见设计 4.5）；
//   - DNS：节点服务器的域名只经虚拟机网关的 DNS 解析（default-nameserver、proxy-server-nameserver
//     都是 [192.168.127.1]；网关只应答路线第一跳的域名，设计 4.5，M2-1b）；
//     目标域名一律经主代理解析（nameserver 带 #CAGE），不从本机真实 IP 直连。
//
// 输出必须通过 mihomocfg.Validate（agent 收到时还会再校验一次）。
func (s Store) VMRuntime() ([]byte, error) {
	c, err := s.VMRuntimeConfig()
	if err != nil {
		return nil, err
	}
	return c.YAML, nil
}

// VMConfig 是虚拟机主代理的配置，以及 abroad-vm 需要的出站白名单（--allow-outbound 的值）。
type VMConfig struct {
	YAML  []byte
	Allow []string // "IPv4:端口"；只来自路线第一跳
	UDP   bool     // 路线是否转 UDP；false 时虚拟机里的 UDP 由 MATCH,REJECT 拒绝
	// FirstHop 是 abroad-vm 的 --first-hop 值（"host:port[,port|lo-hi…]"，能被 usernet.ParseFirstHops 原样接受）：
	// 路线第一跳是域名或公网 IPv4 时才有；回环和局域网第一跳走 Allow。
	FirstHop []string
}

// VMOptions 是生成虚拟机主代理配置的选项。
type VMOptions struct {
	// LoopbackUDP：第一跳是本机回环代理时也转 UDP。本机代理的 UDP 中继地址多数不可用（设计 4.5），
	// 默认不打开；目前只给开发工具（abroad-dev apply-main-proxy --loopback-udp）用。
	LoopbackUDP bool
}

// VMRuntimeConfig 同 VMRuntime，另外按路线第一跳推导出站白名单（设计 4.5）：
//   - 只有第一跳由虚拟机直接拨号；后面各跳由前一跳代拨，它们的地址指的是前一跳那台机器，不改写、不进白名单；
//   - 第一跳是 127.0.0.1：服务器改写成 guestabi.HostLoopbackIP（虚拟机里代表本机回环的地址），白名单加 127.0.0.1:端口；
//   - 第一跳是局域网 IPv4（10/8、172.16/12、192.168/16、100.64/10，不含虚拟机子网）：不改写，白名单加 地址:端口；
//   - 第一跳是 localhost、其他 127/8、0.0.0.0、链路本地、虚拟机子网、IPv6 字面量：拒绝，并说明怎么改；
//   - 域名和公网 IP：不动，不进白名单。
func (s Store) VMRuntimeConfig() (VMConfig, error) { return s.VMRuntimeConfigWith(VMOptions{}) }

// VMRuntimeConfigWith 同 VMRuntimeConfig，带选项。
func (s Store) VMRuntimeConfigWith(o VMOptions) (VMConfig, error) {
	route, err := s.Route()
	if err != nil {
		return VMConfig{}, err
	}
	only := map[string]bool{}
	for _, id := range route {
		only[id] = true
	}
	proxies, err := s.runtimeProxies(only)
	if err != nil {
		return VMConfig{}, err
	}
	udp := true
	for _, id := range route {
		if n, _ := s.Find(id); !n.UDP || n.Type == "http" {
			udp = false
		}
	}
	var allow, hops []string
	for _, p := range proxies {
		if name, _ := p["name"].(string); name != route[0] {
			continue
		}
		h, err := firstHopOf(p)
		if err != nil {
			return VMConfig{}, err
		}
		if h.server != "" {
			p["server"] = h.server
		}
		if h.server == guestabi.HostLoopbackIP && !o.LoopbackUDP {
			udp = false
		}
		if h.allow != "" {
			allow = append(allow, h.allow)
		}
		if h.firstHop != "" {
			hops = append(hops, h.firstHop)
		}
	}
	if _, err := usernet.ParseFirstHops(hops); err != nil {
		// 例如第一跳是文档网段、fake-IP 网段等既不是公网也不是局域网的地址：abroad-vm 不会放行。
		return VMConfig{}, usererror.New("生成虚拟机主代理配置", "第一跳节点的地址不是公网地址或主机名无效", "请检查节点的服务器地址。", err)
	}
	for _, p := range proxies {
		p["udp"] = udp
	}
	dns := map[string]any{
		"enable": true, "ipv6": false, "use-hosts": false, "use-system-hosts": false,
		"default-nameserver":      []string{mihomocfg.GatewayNameserver},
		"proxy-server-nameserver": []string{mihomocfg.GatewayNameserver},
		"nameserver":              []string{"https://1.1.1.1/dns-query#" + Group},
	}
	config := map[string]any{
		"mixed-port": guestabi.MainProxyPort, "bind-address": "127.0.0.1", "allow-lan": false,
		"mode": "rule", "ipv6": false, "log-level": "warning", "find-process-mode": "off",
		"external-controller-unix": VMControllerSocket, "geo-auto-update": false,
		"profile": map[string]any{"store-selected": false}, "unified-delay": true,
		"dns": dns, "proxies": proxies,
		"proxy-groups": []any{map[string]any{"name": Group, "type": "select", "proxies": []string{s.Selected}}},
		"rules":        []string{"MATCH," + Group, "MATCH,REJECT"},
	}
	out, err := yaml.Marshal(config)
	if err != nil {
		return VMConfig{}, err
	}
	if err := mihomocfg.Validate(out); err != nil {
		return VMConfig{}, fmt.Errorf("生成的虚拟机主代理配置没有通过校验：%w", err)
	}
	return VMConfig{YAML: out, Allow: allow, UDP: udp, FirstHop: hops}, nil
}

var (
	lanPrefixes = []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("100.64.0.0/10"),
	}
	vmSubnet  = netip.MustParsePrefix("192.168.127.0/24")
	loopback4 = netip.MustParseAddr("127.0.0.1")
)

type hopInfo struct {
	allow    string // 白名单项（回环、局域网第一跳）
	server   string // 改写后的 server（为空表示不改）
	firstHop string // --first-hop 的值（域名、公网 IPv4 第一跳）
}

// firstHopOf 决定路线第一跳的白名单、server 改写和 --first-hop（规则见 VMRuntimeConfig）。
//
// 能改变第一跳实际拨号目标的字段（2026-11-18 对照 mihomo v1.19.34 源码核对，见 docs/m2-plan.md M2-1b）：
//   - hysteria/hysteria2 的 ports、mieru 的 port-range：端口区间并入 --first-hop；
//   - tuic 的 ip：实际连的是 ip，并入 --first-hop（只收公网 IPv4）；
//   - hysteria2 的 realm-opts（会连 server-url 和 STUN 服务器）、vless 的 xhttp-opts.download-settings
//     （下载走另一台服务器）：拒绝。
//
// 漏掉的字段不会泄漏（abroad-vm 只放行 --first-hop），只会连不上；这里的作用是给出明确的错误。
func firstHopOf(node map[string]any) (hopInfo, error) {
	reject := func(why, fix string) (hopInfo, error) {
		return hopInfo{}, usererror.New("生成虚拟机主代理配置", "第一跳节点"+why, fix, nil)
	}
	// mihomo 解码节点时键名不分大小写、'_' 等同 '-'：先按同样的规则建归一化视图（顶层和 xhttp-opts 一层），
	// 否则 realm_opts、Ports、IP 这类写法这里看不到而 mihomo 会用。同一个键两种写法并存时拒绝。
	p, err := normalizedView(node)
	if err != nil {
		return reject("有重复写法的字段", "请每个字段只保留一种写法。")
	}
	if x, ok := p["xhttp-opts"].(map[string]any); ok {
		if p["xhttp-opts"], err = normalizedView(x); err != nil {
			return reject("的 xhttp-opts 有重复写法的字段", "请每个字段只保留一种写法。")
		}
	}
	if v, ok := p["realm-opts"]; ok && v != nil {
		return reject("用了 realm-opts（会另外连接 realm 服务器和 STUN 服务器）", "请换一个不用 realm 的节点。")
	}
	if x, ok := p["xhttp-opts"].(map[string]any); ok {
		if ds, ok := x["download-settings"]; ok && ds != nil {
			return reject("用了 xhttp 的 download-settings（下载走另一台服务器）", "请换一个不用 download-settings 的节点。")
		}
	}
	raw, _ := p["server"].(string)
	host := raw
	typ, _ := p["type"].(string)
	tuicIP := false
	if typ == "tuic" {
		if v, ok := p["ip"]; ok && v != nil && v != "" {
			ip, _ := v.(string)
			tuicIP = true
			a, err := netip.ParseAddr(ip)
			if err != nil || !a.Unmap().Is4() {
				return reject("的 ip 字段不是 IPv4 地址", "请把 ip 改成 IPv4 地址或删掉。")
			}
			host = a.Unmap().String()
		}
	}
	port, hasPort := portOf(p["port"])
	ranges, err := portRanges(p, typ)
	if err != nil {
		return reject("的端口区间无效", "请检查节点的 ports 或 port-range。")
	}
	if !hasPort && len(ranges) == 0 {
		return reject("的端口无效", "请检查节点配置。")
	}
	lanReject := func(why string) (hopInfo, error) {
		return reject("的服务器地址"+why, "本机代理软件请写 127.0.0.1；局域网代理请写它的 IPv4 地址。")
	}
	if strings.EqualFold(strings.TrimRight(host, "."), "localhost") {
		return lanReject("是 localhost")
	}
	hopValue := func(h string) string {
		var parts []string
		if hasPort {
			parts = append(parts, strconv.Itoa(int(port)))
		}
		return h + ":" + strings.Join(append(parts, ranges...), ",")
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		// 域名：由网关 DNS 代为解析，答出的地址才放行。
		return hopInfo{firstHop: hopValue(strings.ToLower(strings.TrimSuffix(host, ".")))}, nil
	}
	a = a.Unmap()
	local := func(server string) (hopInfo, error) {
		if len(ranges) > 0 || !hasPort {
			return reject("在本机或局域网，又用了端口区间", "本机或局域网的节点请写单个端口。")
		}
		if tuicIP { // server 会被改写，ip 不会：mihomo 仍会连 ip，只能拒绝
			return reject("在本机或局域网，又写了 tuic 的 ip 字段", "请删掉 ip，把服务器地址直接写进 server。")
		}
		return hopInfo{allow: netip.AddrPortFrom(a, port).String(), server: server}, nil
	}
	switch {
	case a == loopback4:
		return local(guestabi.HostLoopbackIP)
	case !a.Is4():
		return lanReject("是 IPv6（虚拟机只走 IPv4）")
	case a.IsLoopback():
		return lanReject("是 127.0.0.1 以外的回环地址")
	case a.IsUnspecified():
		return lanReject("是 0.0.0.0")
	case a.IsLinkLocalUnicast():
		return lanReject("是链路本地地址")
	case vmSubnet.Contains(a):
		return lanReject("落在虚拟机自己的子网 192.168.127.0/24 里")
	}
	for _, pre := range lanPrefixes {
		if pre.Contains(a) {
			// 规范成普通 IPv4 写法（例如 ::ffff:192.168.1.20），与白名单一致。
			return local(a.String())
		}
	}
	h := hopInfo{firstHop: hopValue(a.String())}
	if !tuicIP && raw != a.String() {
		h.server = a.String()
	}
	return h, nil
}

// normalizedView 返回按 mihomocfg.OptionKey 归一化键名的浅拷贝；两个键归一化后相同就报错。
func normalizedView(m map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(m))
	for k, v := range m {
		nk := mihomocfg.OptionKey(k)
		if _, dup := out[nk]; dup {
			return nil, fmt.Errorf("字段 %s 有重复写法", nk)
		}
		out[nk] = v
	}
	return out, nil
}

// portRanges 读端口区间（只认对该类型有意义的键：hysteria/hysteria2 的 ports、mieru 的 port-range），
// 规范成 "lo-hi" 或 "p" 列表。
func portRanges(p map[string]any, typ string) ([]string, error) {
	var keys []string
	switch typ {
	case "hysteria", "hysteria2":
		keys = []string{"ports"}
	case "mieru":
		keys = []string{"port-range"}
	}
	var out []string
	for _, k := range keys {
		v, ok := p[k]
		if !ok || v == nil {
			continue
		}
		var s string
		switch x := v.(type) {
		case string:
			s = x
		case int:
			s = strconv.Itoa(x)
		default:
			return nil, fmt.Errorf("%s 格式不对", k)
		}
		for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '/' }) {
			lo, hi, isRange := strings.Cut(strings.TrimSpace(part), "-")
			l, err1 := strconv.Atoi(strings.TrimSpace(lo))
			h := l
			var err2 error
			if isRange {
				h, err2 = strconv.Atoi(strings.TrimSpace(hi))
			}
			if err1 != nil || err2 != nil || l < 1 || h > 65535 || h < l {
				return nil, fmt.Errorf("%s 无效", k)
			}
			if l == h {
				out = append(out, strconv.Itoa(l))
			} else {
				out = append(out, strconv.Itoa(l)+"-"+strconv.Itoa(h))
			}
		}
	}
	return out, nil
}

func portOf(v any) (uint16, bool) {
	var n int
	switch x := v.(type) {
	case int:
		n = x
	case int64:
		n = int(x)
	case float64:
		n = int(x)
		if float64(n) != x {
			return 0, false
		}
	case string:
		var err error
		if n, err = strconv.Atoi(x); err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	if n <= 0 || n > 65535 {
		return 0, false
	}
	return uint16(n), true
}
