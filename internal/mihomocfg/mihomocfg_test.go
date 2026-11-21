package mihomocfg

import (
	"errors"
	"strings"
	"testing"
)

const secret = "SECRET7f3a"

// base 是一份合法配置（单节点）；反例都从它改出来。
const base = `mixed-port: 7890
bind-address: 127.0.0.1
allow-lan: false
mode: rule
ipv6: false
log-level: warning
find-process-mode: "off"
geo-auto-update: false
unified-delay: true
profile:
  store-selected: false
external-controller-unix: /run/claude-abroad/mihomo/controller.sock
dns:
  enable: true
  ipv6: false
  use-hosts: false
  use-system-hosts: false
  default-nameserver: ["192.168.127.1"]
  proxy-server-nameserver: ["192.168.127.1"]
  nameserver: [https://1.1.1.1/dns-query#CAGE]
proxies:
  - name: node-` + secret + `
    type: ss
    server: 203.0.113.9
    port: 8388
    cipher: aes-128-gcm
    password: ` + secret + `
proxy-groups:
  - name: CAGE
    type: select
    proxies: [node-` + secret + `]
rules:
  - MATCH,CAGE
  - MATCH,REJECT
`

func TestValidAccepted(t *testing.T) {
	chain := strings.Replace(base, "proxies:\n  - name: node-", "proxies:\n  - name: hop1\n    type: socks5\n    server: 198.51.100.1\n    port: 1080\n  - name: node-", 1)
	chain = strings.Replace(chain, "    password: "+secret+"\n", "    password: "+secret+"\n    dialer-proxy: hop1\n", 1)
	rejectOnly := `mixed-port: 7890
bind-address: 127.0.0.1
allow-lan: false
mode: rule
ipv6: false
dns:
  enable: true
  ipv6: false
  use-hosts: false
  use-system-hosts: false
  default-nameserver: ["192.168.127.1"]
  proxy-server-nameserver: ["192.168.127.1"]
  nameserver: [https://1.1.1.1/dns-query#REJECT]
rules: ["MATCH,REJECT"]
`
	for name, c := range map[string]string{"单节点": base, "链式": chain, "仅 MATCH,REJECT": rejectOnly} {
		if err := Validate([]byte(c)); err != nil {
			t.Errorf("%s 应通过：%v", name, err)
		}
	}
}

func TestRejects(t *testing.T) {
	rep := func(old, new string) string {
		if !strings.Contains(base, old) {
			t.Fatalf("基础配置里没有 %q", old)
		}
		return strings.Replace(base, old, new, 1)
	}
	cases := map[string]string{
		"MATCH,DIRECT": rep("  - MATCH,CAGE", "  - MATCH,DIRECT"),
		"mode: direct": rep("mode: rule", "mode: direct"),
		"规则为空":         rep("rules:\n  - MATCH,CAGE\n  - MATCH,REJECT\n", "rules: []\n"),
		"缺规则":          rep("rules:\n  - MATCH,CAGE\n  - MATCH,REJECT\n", ""),
		"只有 MATCH,CAGE（UDP 会落到 DIRECT）":       rep("  - MATCH,CAGE\n  - MATCH,REJECT\n", "  - MATCH,CAGE\n"),
		"兜底不是 REJECT":                         rep("  - MATCH,REJECT", "  - MATCH,DIRECT"),
		"兜底在前":                                rep("  - MATCH,CAGE\n  - MATCH,REJECT\n", "  - MATCH,REJECT\n  - MATCH,CAGE\n"),
		"三条规则":                                rep("  - MATCH,REJECT", "  - MATCH,REJECT\n  - MATCH,REJECT"),
		"两条 REJECT":                           rep("  - MATCH,CAGE", "  - MATCH,REJECT"),
		"兜底带空格":                               rep("  - MATCH,REJECT", "  - \"MATCH,REJECT \""),
		"出口名带空格":                              rep("  - MATCH,CAGE", "  - \"MATCH,CAGE \""),
		"两条规则":                                rep("  - MATCH,CAGE", "  - IP-CIDR,1.1.1.1/32,CAGE\n  - MATCH,CAGE"),
		"IP-CIDR 规则":                          rep("  - MATCH,CAGE", "  - IP-CIDR,1.1.1.1/32,CAGE"),
		"MATCH 指向未定义":                         rep("  - MATCH,CAGE", "  - MATCH,nope"),
		"顶层 tun":                              rep("ipv6: false\nlog", "ipv6: false\ntun:\n  enable: false\nlog"),
		"顶层 listeners":                        rep("ipv6: false\nlog", "ipv6: false\nlisteners: []\nlog"),
		"顶层 external-controller":              rep("ipv6: false\nlog", "ipv6: false\nexternal-controller: 127.0.0.1:9090\nlog"),
		"proxy-providers":                     rep("ipv6: false\nlog", "ipv6: false\nproxy-providers: {}\nlog"),
		"缺 mixed-port":                        rep("mixed-port: 7890\n", ""),
		"mixed-port 不对":                       rep("mixed-port: 7890", "mixed-port: 7891"),
		"bind-address 不对":                     rep("bind-address: 127.0.0.1", "bind-address: 0.0.0.0"),
		"allow-lan 是字符串":                      rep("allow-lan: false", `allow-lan: "false"`),
		"allow-lan true":                      rep("allow-lan: false", "allow-lan: true"),
		"ipv6 true":                           rep("mode: rule\nipv6: false", "mode: rule\nipv6: true"),
		"log-level info":                      rep("log-level: warning", "log-level: info"),
		"find-process-mode strict":            rep(`find-process-mode: "off"`, "find-process-mode: strict"),
		"unified-delay 字符串":                   rep("unified-delay: true", `unified-delay: "yes"`),
		"geo-auto-update true":                rep("geo-auto-update: false", "geo-auto-update: true"),
		"controller 不在运行目录":                   rep("/run/claude-abroad/mihomo/controller.sock", "/tmp/c.sock"),
		"controller 带 ..":                     rep("/run/claude-abroad/mihomo/controller.sock", "/run/claude-abroad/../etc/c.sock"),
		"type: direct":                        rep("    type: ss", "    type: direct"),
		"type: rematch":                       rep("    type: ss", "    type: rematch"),
		"Type 大写":                             rep("    type: ss", "    Type: ss"),
		"名为 DIRECT 的节点":                       strings.ReplaceAll(base, "node-"+secret, "DIRECT"),
		"名字含逗号":                               strings.ReplaceAll(base, "node-"+secret, "a,b"),
		"interface_name":                      rep("    cipher:", "    interface_name: en0\n    cipher:"),
		"Routing_Mark 嵌套":                     rep("    cipher:", "    smux:\n      Routing_Mark: 1\n    cipher:"),
		"dialer-proxy 未定义":                    rep("    cipher:", "    dialer-proxy: nope\n    cipher:"),
		"dialer_proxy 变体":                     rep("    cipher:", "    dialer_proxy: CAGE\n    cipher:"),
		"组成员 Proxies:[DIRECT]":                rep("    proxies: [node-"+secret+"]", "    proxies: [node-"+secret+"]\n    Proxies: [DIRECT]"),
		"组成员 DIRECT":                          rep("    proxies: [node-"+secret+"]", "    proxies: [DIRECT]"),
		"组 USE":                               rep("    proxies: [node-"+secret+"]", "    proxies: [node-"+secret+"]\n    USE: [p]"),
		"组 Include_All":                       rep("    proxies: [node-"+secret+"]", "    proxies: [node-"+secret+"]\n    Include_All: true"),
		"组 url-test":                          rep("    type: select", "    type: url-test"),
		"组成员为空":                               rep("    proxies: [node-"+secret+"]", "    proxies: []"),
		"nameserver 不带 #":                     rep("https://1.1.1.1/dns-query#CAGE", "https://1.1.1.1/dns-query"),
		"nameserver #DIRECT":                  rep("https://1.1.1.1/dns-query#CAGE", "https://1.1.1.1/dns-query#DIRECT"),
		"nameserver # 带参数":                    rep("https://1.1.1.1/dns-query#CAGE", "https://1.1.1.1/dns-query#CAGE&h3=true"),
		"nameserver 明文":                       rep("https://1.1.1.1/dns-query#CAGE", "1.1.1.1#CAGE"),
		"nameserver 域名主机":                     rep("https://1.1.1.1/dns-query#CAGE", "https://dns."+secret+".com/dns-query#CAGE"),
		"nameserver system":                   rep("https://1.1.1.1/dns-query#CAGE", "system"),
		"nameserver 带查询串":                     rep("https://1.1.1.1/dns-query#CAGE", "https://1.1.1.1/dns-query?x=1#CAGE"),
		"default-nameserver 带 #":              rep(`default-nameserver: ["192.168.127.1"]`, `default-nameserver: ["192.168.127.1#CAGE"]`),
		"default-nameserver 写成 DoH":           rep(`default-nameserver: ["192.168.127.1"]`, `default-nameserver: [https://1.1.1.1/dns-query]`),
		"default-nameserver 别的 IP":            rep(`default-nameserver: ["192.168.127.1"]`, `default-nameserver: ["223.5.5.5"]`),
		"default-nameserver 多项":               rep(`default-nameserver: ["192.168.127.1"]`, `default-nameserver: ["192.168.127.1", "223.5.5.5"]`),
		"default-nameserver 带端口":              rep(`default-nameserver: ["192.168.127.1"]`, `default-nameserver: ["192.168.127.1:5353"]`),
		"default-nameserver 带协议":              rep(`default-nameserver: ["192.168.127.1"]`, `default-nameserver: ["tcp://192.168.127.1"]`),
		"缺 default-nameserver":                rep("  default-nameserver: [\"192.168.127.1\"]\n", ""),
		"proxy-server 用 dhcp":                 rep(`proxy-server-nameserver: ["192.168.127.1"]`, `proxy-server-nameserver: [dhcp://en0]`),
		"proxy-server 写成 DoH":                 rep(`proxy-server-nameserver: ["192.168.127.1"]`, `proxy-server-nameserver: [https://223.5.5.5/dns-query]`),
		"proxy-server 带 #":                    rep(`proxy-server-nameserver: ["192.168.127.1"]`, `proxy-server-nameserver: ["192.168.127.1#CAGE"]`),
		"proxy-server 多项":                     rep(`proxy-server-nameserver: ["192.168.127.1"]`, `proxy-server-nameserver: ["192.168.127.1", "192.168.127.1"]`),
		"proxy-server 空列表":                    rep(`proxy-server-nameserver: ["192.168.127.1"]`, `proxy-server-nameserver: []`),
		"缺 proxy-server-nameserver":           rep("  proxy-server-nameserver: [\"192.168.127.1\"]\n", ""),
		"dns respect-rules":                   rep("  enable: true", "  enable: true\n  respect-rules: true"),
		"dns nameserver-policy":               rep("  enable: true", "  enable: true\n  nameserver-policy: {\"+.example.com\": \"223.5.5.5\"}"),
		"dns proxy-server-nameserver-policy":  rep("  enable: true", "  enable: true\n  proxy-server-nameserver-policy: {\"+.example.com\": \"223.5.5.5\"}"),
		"dns direct-nameserver":               rep("  enable: true", "  enable: true\n  direct-nameserver: [\"223.5.5.5\"]"),
		"dns direct-nameserver-follow-policy": rep("  enable: true", "  enable: true\n  direct-nameserver-follow-policy: true"),
		"dns fallback-filter":                 rep("  enable: true", "  enable: true\n  fallback-filter: {geoip: false}"),
		"dns listen":                          rep("  enable: true", "  enable: true\n  listen: 127.0.0.1:53"),
		"dns prefer-h3":                       rep("  enable: true", "  enable: true\n  prefer-h3: true"),
		"dns fallback":                        rep("  enable: true", "  enable: true\n  fallback: [https://1.1.1.1/dns-query]"),
		"dns enhanced-mode":                   rep("  enable: true", "  enable: true\n  enhanced-mode: fake-ip"),
		"dns.enable false":                    rep("  enable: true", "  enable: false"),
		"use-system-hosts true":               rep("use-system-hosts: false", "use-system-hosts: true"),
		"缺 use-hosts":                         rep("  use-hosts: false\n", ""),
		"缺 dns":                               rep(base[strings.Index(base, "dns:"):strings.Index(base, "proxies:")], ""),
		"重复键":                                 rep("mode: rule", "mode: rule\nmode: rule"),
		"不是 YAML":                             "{{{",
	}
	for name, c := range cases {
		err := Validate([]byte(c))
		if err == nil {
			t.Errorf("%s：应被拒绝", name)
			continue
		}
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s：错误应包 ErrInvalid：%v", name, err)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "203.0.113.9") {
			t.Errorf("%s：错误文本不应含配置内容：%v", name, err)
		}
	}
}

func TestAnchorsExpandedThenChecked(t *testing.T) {
	c := strings.Replace(base, "proxy-groups:\n  - name: CAGE\n    type: select\n    proxies: [node-"+secret+"]",
		"x-members: &m [DIRECT]\nproxy-groups:\n  - name: CAGE\n    type: select\n    proxies: *m", 1)
	if err := Validate([]byte(c)); err == nil {
		t.Fatal("经锚点引入的 DIRECT 也应被拒（以及未知顶层键）")
	}
}
