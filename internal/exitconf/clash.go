package exitconf

import (
	"fmt"
	"strconv"

	"go.yaml.in/yaml/v3"

	"claude-abroad/internal/guestabi"
	"claude-abroad/internal/mihomocfg"
)

// ClashConf 是订阅 mihomo 出口校验过的节点（只取 proxies，其余一概不用：配置由 Generate 按主代理的形状重新生成）。
type ClashConf struct {
	Proxies []map[string]any // 已改名为 n1、n2…
	eps     []Endpoint
}

// NodeKeys 是订阅节点允许的字段（m4-plan v2.10）：连接节点要的协议参数。不在表里的一律拒绝并指出是哪一项。
// 没有 interface-name、routing-mark、dialer-proxy（绑网卡、改路由、链到别的出站），也没有 ip-version 一类
// 改变节点域名解析方式的字段。嵌套的选项（ws-opts 等）里再查一遍 interface-name、routing-mark（mihomocfg.CheckNode）。
var NodeKeys = map[string]bool{
	"name": true, "type": true, "server": true, "port": true, "udp": true, "tfo": true, "mptcp": true,
	"username": true, "password": true, "uuid": true, "alterid": true, "cipher": true, "auth": true, "auth-str": true,
	"tls": true, "sni": true, "servername": true, "skip-cert-verify": true, "fingerprint": true, "client-fingerprint": true,
	"alpn": true, "ca": true, "ca-str": true, "network": true, "flow": true, "packet-encoding": true, "global-padding": true,
	"authenticated-length": true, "xudp": true, "packet-addr": true,
	"ws-opts": true, "grpc-opts": true, "h2-opts": true, "http-opts": true, "reality-opts": true, "smux": true,
	"plugin": true, "plugin-opts": true, "udp-over-tcp": true, "udp-over-tcp-version": true,
	"protocol": true, "protocol-param": true, "obfs": true, "obfs-param": true, "obfs-password": true,
	"up": true, "down": true, "ports": true, "hop-interval": true, "recv-window-conn": true, "recv-window": true,
	"disable-mtu-discovery": true, "fast-open": true,
	"congestion-controller": true, "udp-relay-mode": true, "reduce-rtt": true, "request-timeout": true,
	"heartbeat-interval": true, "max-udp-relay-packet-size": true, "token": true,
	"idle-session-check-interval": true, "idle-session-timeout": true, "min-idle-session": true,
	"transport": true, "multiplexing": true, "headers": true, "headers-opts": true, "encryption": true,
}

// udpOnly 是只走 UDP 的节点类型。
var udpOnly = map[string]bool{"hysteria": true, "hysteria2": true, "tuic": true}

// ParseClash 解析订阅（Clash/mihomo YAML）：至少一个节点；每个节点类型在白名单里、没有 interface-name、
// routing-mark、dialer-proxy；server、port 合法。
func ParseClash(b []byte) (ClashConf, error) {
	var c ClashConf
	if len(b) > 1<<20 {
		return c, lineErr(0, "", "订阅太大")
	}
	var doc struct {
		Proxies []map[string]any `yaml:"proxies"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return c, lineErr(0, "", "不是有效的 YAML")
	}
	if len(doc.Proxies) == 0 {
		return c, lineErr(0, "proxies", "没有节点（不支持 proxy-providers 一类的远程节点）")
	}
	if len(doc.Proxies) > 200 {
		return c, lineErr(0, "proxies", "节点太多（最多 200 个）")
	}
	for i, p := range doc.Proxies {
		name := fmt.Sprintf("proxies 第 %d 项", i+1)
		if err := mihomocfg.CheckNode(p); err != nil {
			return c, lineErr(0, name, err.Error())
		}
		for k := range p {
			if !NodeKeys[mihomocfg.OptionKey(k)] {
				return c, lineErr(0, name, "字段 "+k+" 不在允许的范围里")
			}
		}
		server, _ := p["server"].(string)
		port, ok := p["port"].(int)
		if !ok {
			if s, isStr := p["port"].(string); isStr {
				port, _ = strconv.Atoi(s)
			}
		}
		proto := "tcp"
		if udpOnly[p["type"].(string)] {
			proto = "udp"
		}
		ep, err := checkEndpoint(server, port, proto)
		if err != nil {
			return c, lineErr(0, name, err.Error())
		}
		q := map[string]any{}
		for k, v := range p {
			q[k] = v
		}
		q["name"] = fmt.Sprintf("n%d", i+1)
		c.Proxies = append(c.Proxies, q)
		c.eps = append(c.eps, ep)
	}
	return c, nil
}

// Endpoints 返回全部节点的端点。
func (c ClashConf) Endpoints() []Endpoint { return append([]Endpoint(nil), c.eps...) }

// ExitGroup 是生成的配置里的节点组名。
const ExitGroup = "EXIT"

// Generate 按主代理的形状生成订阅出口的 mihomo 配置（端口 port）：订阅里除节点以外的内容（DNS 段、规则、组、
// 嗅探、TUN、hosts……）一概不用。只有 select 组 EXIT、规则 [MATCH,EXIT, MATCH,REJECT]（没有任何要 IP 才能判断的规则，
// 所以 mihomo 不需要在本地解析目标域名，域名原样交给节点）；DNS 段只为两件事：节点自己的域名经虚拟机网关的 DNS
// （和主代理同一条路，M2-1），万一要解析目标域名时也只经 EXIT 组（节点）走 DoH，不从虚拟机本地出去。
// 不关掉 DNS 段：关掉后 mihomo 退回系统解析器，任何需要的解析都会从本地出去（design 7.3 v0.15）。再经 mihomocfg 校验一遍。
func (c ClashConf) Generate(port int) ([]byte, error) {
	names := make([]any, 0, len(c.Proxies))
	for _, p := range c.Proxies {
		names = append(names, p["name"])
	}
	proxies := make([]any, 0, len(c.Proxies))
	for _, p := range c.Proxies {
		proxies = append(proxies, p)
	}
	cfg := map[string]any{
		"mixed-port": port, "bind-address": "127.0.0.1", "allow-lan": false, "mode": "rule", "ipv6": false,
		"log-level": "warning", "find-process-mode": "off", "geo-auto-update": false,
		"profile":      map[string]any{"store-selected": false, "store-fake-ip": false},
		"proxies":      proxies,
		"proxy-groups": []any{map[string]any{"name": ExitGroup, "type": "select", "proxies": names}},
		"rules":        []any{"MATCH," + ExitGroup, "MATCH,REJECT"},
		"dns": map[string]any{
			"enable": true, "ipv6": false, "use-hosts": false, "use-system-hosts": false,
			"default-nameserver":      []any{guestabi.DefaultGW},
			"proxy-server-nameserver": []any{guestabi.DefaultGW},
			"nameserver":              []any{"https://1.1.1.1/dns-query#" + ExitGroup},
		},
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	if err := mihomocfg.ValidatePort(b, port); err != nil {
		return nil, err
	}
	return b, nil
}
