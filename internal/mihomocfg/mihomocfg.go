// Package mihomocfg 校验虚拟机里主代理 mihomo 的配置（设计文档 7.2）。
//
// 虚拟机有真实出站之后，`MATCH,DIRECT`、`mode: direct`、空规则、IP 类规则、直连的目标域名解析
// 都会从本机真实 IP 出去，所以只接受 managedproxy.VMRuntime 会生成的形状，按白名单校验：
// 未知的键一律拒绝。agent 收到配置时（二次校验）和宿主生成配置时都用它。
//
// 错误信息只说明是哪一项、第几个，不回显名字、地址、口令等配置内容。
package mihomocfg

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"claude-abroad/internal/guestabi"
)

// GatewayNameserver 是节点域名唯一允许的解析器：虚拟机网关的 DNS（明文 UDP 53）。
var GatewayNameserver = guestabi.DefaultGW

// NodeTypes 是允许的节点类型（与 managedproxy 的导入范围是同一份表）。
var NodeTypes = map[string]bool{
	"http": true, "socks5": true, "ss": true, "ssr": true, "vmess": true, "vless": true, "trojan": true,
	"hysteria": true, "hysteria2": true, "tuic": true, "anytls": true, "mieru": true,
}

// nameRe 是节点名和组名允许的字符（VMRuntime 生成的名字是 24 位十六进制 ID 和 CAGE）。
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// reserved 是 mihomo 的内置出站名，节点和组不能用这些名字。
var reserved = map[string]bool{
	"DIRECT": true, "REJECT": true, "REJECT-DROP": true, "PASS": true, "COMPATIBLE": true, "GLOBAL": true,
}

// OptionKey 按 mihomo 解码节点和组时的规则归一化键名：不区分大小写，'_' 视为 '-'。
func OptionKey(key string) string {
	return strings.ToLower(strings.ReplaceAll(key, "_", "-"))
}

// 允许的顶层键。
var topKeys = map[string]bool{
	"mixed-port": true, "bind-address": true, "allow-lan": true, "mode": true, "ipv6": true,
	"log-level": true, "find-process-mode": true, "external-controller-unix": true, "dns": true,
	"geo-auto-update": true, "profile": true, "proxies": true, "proxy-groups": true, "rules": true,
	"unified-delay": true,
}

var dnsKeys = map[string]bool{
	"enable": true, "ipv6": true, "use-hosts": true, "use-system-hosts": true,
	"default-nameserver": true, "proxy-server-nameserver": true, "nameserver": true,
}

// ErrInvalid 包在所有校验错误外面。
var ErrInvalid = errors.New("主代理配置不符合要求")

func bad(format string, a ...any) error {
	return fmt.Errorf("%w：%s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Validate 校验一份主代理 mihomo 配置（YAML）：mixed-port 必须是主代理的端口。
func Validate(data []byte) error { return ValidatePort(data, guestabi.MainProxyPort) }

// ValidatePort 同 Validate，mixed-port 必须是 port（分流出口的订阅 mihomo 各用自己的端口，M4-7）。
func ValidatePort(data []byte, port int) error {
	var c map[string]any
	if err := yaml.Unmarshal(data, &c); err != nil { // 重复键、语法错误都在这里报
		return bad("不是有效的 YAML（或有重复的键）")
	}
	if c == nil {
		return bad("配置为空")
	}
	for k := range c {
		if !topKeys[k] {
			return bad("不允许的顶层项")
		}
	}

	// 必须显式写出、类型严格的项。
	if v, ok := c["mixed-port"].(int); !ok || v != port {
		return bad("mixed-port 必须是 %d", port)
	}
	if v, ok := c["bind-address"].(string); !ok || v != "127.0.0.1" {
		return bad("bind-address 必须是 127.0.0.1")
	}
	if v, ok := c["allow-lan"].(bool); !ok || v {
		return bad("allow-lan 必须是 false")
	}
	if v, ok := c["mode"].(string); !ok || v != "rule" {
		return bad("mode 必须是 rule")
	}
	if v, ok := c["ipv6"].(bool); !ok || v {
		return bad("ipv6 必须是 false")
	}

	// 可选项。
	if v, ok := c["log-level"]; ok {
		s, _ := v.(string)
		if s != "silent" && s != "error" && s != "warning" {
			return bad("log-level 只能是 silent、error 或 warning（更详细的级别会记录目标域名）")
		}
	}
	if v, ok := c["find-process-mode"]; ok && v != "off" {
		return bad("find-process-mode 只能是 off")
	}
	if v, ok := c["unified-delay"]; ok {
		if _, isBool := v.(bool); !isBool {
			return bad("unified-delay 必须是布尔值")
		}
	}
	if v, ok := c["geo-auto-update"]; ok && v != false {
		return bad("geo-auto-update 只能是 false")
	}
	if v, ok := c["profile"]; ok {
		m, isMap := v.(map[string]any)
		if !isMap {
			return bad("profile 格式不对")
		}
		for k, pv := range m {
			if (k != "store-selected" && k != "store-fake-ip") || pv != false {
				return bad("profile 只允许 store-selected、store-fake-ip 且必须为 false")
			}
		}
	}
	if v, ok := c["external-controller-unix"]; ok {
		s, _ := v.(string)
		if s == "" || strings.Contains(s, "..") || path.Clean(s) != s || !strings.HasPrefix(s, guestabi.RunDir+"/") {
			return bad("external-controller-unix 只能在 %s/ 下", guestabi.RunDir)
		}
	}

	names, groups, err := checkProxies(c["proxies"], c["proxy-groups"])
	if err != nil {
		return err
	}
	known := func(n string) bool { return names[n] || groups[n] }
	if err := checkRules(c["rules"], known); err != nil {
		return err
	}
	return checkDNS(c["dns"], known)
}

func checkProxies(rawProxies, rawGroups any) (names, groups map[string]bool, err error) {
	names, groups = map[string]bool{}, map[string]bool{}
	checkName := func(v any, what string, i int) (string, error) {
		n, ok := v.(string)
		if !ok || !nameRe.MatchString(n) {
			// 字符白名单：mihomo 解析规则时会去掉首尾空格、解析 DoH 地址时会做百分号解码，
			// 带空格或 % 的名字能在校验后“变成” DIRECT。
			return "", bad("%s 第 %d 项：名字只允许字母、数字、_、-（1–64 位）", what, i+1)
		}
		if reserved[strings.ToUpper(n)] {
			return "", bad("%s 第 %d 项：名字不能是内置出站名", what, i+1)
		}
		if names[n] || groups[n] {
			return "", bad("%s 第 %d 项：名字重复", what, i+1)
		}
		return n, nil
	}

	var proxies []any
	if rawProxies != nil {
		var ok bool
		if proxies, ok = rawProxies.([]any); !ok {
			return nil, nil, bad("proxies 格式不对")
		}
	}
	dialers := map[int]string{}
	for i, p := range proxies {
		m, ok := p.(map[string]any)
		if !ok {
			return nil, nil, bad("proxies 第 %d 项格式不对", i+1)
		}
		// name、type 必须是规范键名，且不能有大小写、下划线变体。
		for k := range m {
			if nk := OptionKey(k); (nk == "name" || nk == "type") && k != nk {
				return nil, nil, bad("proxies 第 %d 项：%s 必须用规范写法", i+1, nk)
			}
		}
		n, err := checkName(m["name"], "proxies", i)
		if err != nil {
			return nil, nil, err
		}
		t, _ := m["type"].(string)
		if !NodeTypes[t] {
			return nil, nil, bad("proxies 第 %d 项：不支持的节点类型", i+1)
		}
		if unsafeNodeField(m) {
			return nil, nil, bad("proxies 第 %d 项：不允许 interface-name、routing-mark（也不允许非字符串键的嵌套映射）", i+1)
		}
		seen := 0
		for k, v := range m {
			if OptionKey(k) != "dialer-proxy" {
				continue
			}
			seen++
			if k != "dialer-proxy" || seen > 1 {
				return nil, nil, bad("proxies 第 %d 项：dialer-proxy 只能出现一次且用规范写法", i+1)
			}
			d, ok := v.(string)
			if !ok || d == "" {
				return nil, nil, bad("proxies 第 %d 项：dialer-proxy 格式不对", i+1)
			}
			dialers[i] = d
		}
		names[n] = true
	}

	var groupList []any
	if rawGroups != nil {
		var ok bool
		if groupList, ok = rawGroups.([]any); !ok {
			return nil, nil, bad("proxy-groups 格式不对")
		}
	}
	members := map[int][]string{}
	for i, g := range groupList {
		m, ok := g.(map[string]any)
		if !ok {
			return nil, nil, bad("proxy-groups 第 %d 项格式不对", i+1)
		}
		for k := range m { // 精确键名白名单（mihomo 读组时不区分大小写，黑名单拦不住 Proxies:[DIRECT]）
			if k != "name" && k != "type" && k != "proxies" {
				return nil, nil, bad("proxy-groups 第 %d 项：只允许 name、type、proxies", i+1)
			}
		}
		n, err := checkName(m["name"], "proxy-groups", i)
		if err != nil {
			return nil, nil, err
		}
		if t, _ := m["type"].(string); t != "select" {
			return nil, nil, bad("proxy-groups 第 %d 项：type 只能是 select", i+1)
		}
		list, ok := m["proxies"].([]any)
		if !ok || len(list) == 0 {
			return nil, nil, bad("proxy-groups 第 %d 项：proxies 必须非空", i+1)
		}
		for _, x := range list {
			s, ok := x.(string)
			if !ok {
				return nil, nil, bad("proxy-groups 第 %d 项：成员格式不对", i+1)
			}
			members[i] = append(members[i], s)
		}
		groups[n] = true
	}
	known := func(n string) bool { return names[n] || groups[n] }
	for i, d := range dialers {
		if !known(d) {
			return nil, nil, bad("proxies 第 %d 项：dialer-proxy 指向未定义的节点", i+1)
		}
	}
	for i, ms := range members {
		for _, s := range ms {
			if s != "REJECT" && !known(s) {
				return nil, nil, bad("proxy-groups 第 %d 项：成员只能是已定义的节点、组或 REJECT", i+1)
			}
		}
	}
	return names, groups, nil
}

// CheckNode 校验单个节点（类型在白名单里、没有 interface-name、routing-mark）；分流出口的订阅导入时逐个用（M4-7）。
func CheckNode(m map[string]any) error {
	t, _ := m["type"].(string)
	if !NodeTypes[t] {
		return bad("不支持的节点类型")
	}
	if unsafeNodeField(m) {
		return bad("不允许 interface-name、routing-mark（也不允许非字符串键的嵌套映射）")
	}
	for k := range m {
		if OptionKey(k) == "dialer-proxy" {
			return bad("分流出口的节点不允许 dialer-proxy")
		}
	}
	return nil
}

// unsafeNodeField 递归查找 interface-name、routing-mark（按 mihomo 的规则归一化后比较）。
func unsafeNodeField(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, vv := range x {
			if nk := OptionKey(k); nk == "interface-name" || nk == "routing-mark" || unsafeNodeField(vv) {
				return true
			}
		}
	case map[any]any:
		return true // 含非字符串键的映射：无法可靠检查，直接拒绝
	case []any:
		for _, vv := range x {
			if unsafeNodeField(vv) {
				return true
			}
		}
	}
	return false
}

// checkRules 只接受两种形状（设计 7.2）：
//   - ["MATCH,<已定义的节点或组>", "MATCH,REJECT"]：mihomo 遇到出口不支持 UDP 时会跳过这条规则继续往下匹配，
//     全部跳过就落到 DIRECT，所以必须用 MATCH,REJECT 兜底，否则 UDP 会从本机真实 IP 直连出去；
//   - ["MATCH,REJECT"]。
//
// 只有一条 MATCH,<出口> 的旧形状一律拒绝。
func checkRules(raw any, known func(string) bool) error {
	list, ok := raw.([]any)
	if !ok || len(list) < 1 || len(list) > 2 {
		return bad("rules 只能是 [MATCH,<出口>, MATCH,REJECT] 或 [MATCH,REJECT]")
	}
	if last, _ := list[len(list)-1].(string); last != "MATCH,REJECT" {
		return bad("rules 最后一条必须是 MATCH,REJECT（出口不支持 UDP 时防止落到 DIRECT）")
	}
	if len(list) == 2 {
		s, _ := list[0].(string)
		target, found := strings.CutPrefix(s, "MATCH,")
		if !found || target == "REJECT" || !nameRe.MatchString(target) || !known(target) {
			return bad("rules 第一条必须是 MATCH，指向已定义的节点或组")
		}
	}
	return nil
}

func checkDNS(raw any, known func(string) bool) error {
	m, ok := raw.(map[string]any)
	if !ok {
		return bad("必须有 dns 段")
	}
	for k := range m {
		if !dnsKeys[k] {
			return bad("dns 里有不允许的项")
		}
	}
	if v, ok := m["enable"].(bool); !ok || !v {
		return bad("dns.enable 必须是 true")
	}
	for _, k := range []string{"ipv6", "use-hosts", "use-system-hosts"} {
		if v, ok := m[k].(bool); !ok || v {
			return bad("dns.%s 必须是 false", k)
		}
	}
	// 节点服务器的域名只经虚拟机网关的 DNS 解析（设计 4.5、7.2，M2-1b）：网关只应答路线第一跳的域名，
	// 答出的地址才放行。两个键都必须显式写成且只能写成 [网关地址]：缺省时 mihomo 会用内置的公共 DNS 直连。
	for _, k := range []string{"default-nameserver", "proxy-server-nameserver"} {
		list, ok := m[k].([]any)
		if !ok || len(list) != 1 {
			return bad("dns.%s 必须恰好一项：%s", k, GatewayNameserver)
		}
		if s, _ := list[0].(string); s != GatewayNameserver {
			return bad("dns.%s 只能是 %s（虚拟机网关的 DNS）", k, GatewayNameserver)
		}
	}
	list, ok := m["nameserver"].([]any)
	if !ok || len(list) == 0 {
		return bad("dns.nameserver 必须非空")
	}
	for i, x := range list {
		s, _ := x.(string)
		if err := checkDoH(s, true, known); err != nil {
			return bad("dns.nameserver 第 %d 项：%v", i+1, err)
		}
	}
	return nil
}

// checkDoH：https://<IPv4 字面量>[:端口]/路径，不带 userinfo 和查询串。
// viaProxy 为 true 时（nameserver）必须带 "#<名字>"，名字是已定义的节点、组或 REJECT——
// 目标域名一律经主代理解析，不从本机真实 IP 直连；为 false 时（只解析节点服务器的域名）禁止带 '#'。
func checkDoH(s string, viaProxy bool, known func(string) bool) error {
	base, frag, hasFrag := strings.Cut(s, "#")
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Opaque != "" {
		return errors.New("只允许 https 的 DoH 地址，不带账号和查询串")
	}
	a, err := netip.ParseAddr(u.Hostname())
	if err != nil || !a.Is4() {
		return errors.New("DoH 地址的主机必须是 IPv4 字面量")
	}
	if !strings.HasPrefix(u.Path, "/") {
		return errors.New("DoH 地址缺少路径")
	}
	if !viaProxy {
		if hasFrag {
			return errors.New("这里的 DoH 地址不能带 '#'")
		}
		return nil
	}
	if !hasFrag || frag == "" || strings.ContainsAny(frag, "&=#%") || !(frag == "REJECT" || nameRe.MatchString(frag)) {
		return errors.New("必须带 '#<节点或组>'，经主代理解析")
	}
	if frag != "REJECT" && !known(frag) {
		return errors.New("'#' 后面必须是已定义的节点、组或 REJECT")
	}
	return nil
}
