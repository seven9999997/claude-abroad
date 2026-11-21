package exitconf

import (
	"errors"
	"fmt"
	"go.yaml.in/yaml/v3"
	"sort"
	"strings"
	"testing"

	"claude-abroad/internal/mihomocfg"
)

const key = "YW5vdGhlcjMyYnl0ZXNsb25na2V5Zm9ydGVzdGluZzE=" // 32 字节的测试密钥（base64）

var wgOK = `# 服务商给的配置
[Interface]
PrivateKey = ` + key + `
Address = 10.66.0.2/32, fd00::2/128
DNS = 10.66.0.1
MTU = 1420

[Peer]
PublicKey = ` + key + `
PresharedKey = ` + key + `
Endpoint = vpn.example.com:51820
AllowedIPs = 0.0.0.0/0, ::/0
PersistentKeepalive = 25
`

// b. WireGuard：合法样本；每个被禁指令各一例被拒并指出行号；重新生成的只有白名单内的项。
func TestWG(t *testing.T) {
	w, err := ParseWG([]byte(wgOK))
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Address) != 1 || w.DNS[0].String() != "10.66.0.1" || w.MTU != 1420 || w.Peers[0].Endpoint != (Endpoint{"vpn.example.com", 51820, "udp"}) {
		t.Fatalf("%+v", w)
	}
	sc := w.SetConf(map[string]string{"vpn.example.com": "203.0.113.5"})
	for _, bad := range []string{"Address", "DNS", "MTU", "#", "::/0", "vpn.example.com", "服务商"} {
		if strings.Contains(sc, bad) {
			t.Fatalf("交给 wg setconf 的配置里不应有 %q：\n%s", bad, sc)
		}
	}
	if !strings.Contains(sc, "Endpoint = 203.0.113.5:51820") || !strings.Contains(sc, "PersistentKeepalive = 25") {
		t.Fatalf("%s", sc)
	}
	for _, d := range []string{"PreUp = x", "PostUp = iptables -A x", "PreDown = x", "PostDown = x", "SaveConfig = true", "Table = off", "FwMark = 1", "ListenPort = 51820"} {
		conf := strings.Replace(wgOK, "MTU = 1420", d, 1)
		_, err := ParseWG([]byte(conf))
		var le *LineError
		if !errors.As(err, &le) || le.Line != 6 || !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s 应在第 6 行被拒：%v", d, err)
		}
		if strings.Contains(err.Error(), "iptables") {
			t.Fatal("错误里不应回显值")
		}
	}
	for name, conf := range map[string]string{
		"缺 PrivateKey": strings.Replace(wgOK, "PrivateKey = "+key, "", 1),
		"坏密钥":          strings.Replace(wgOK, "PrivateKey = "+key, "PrivateKey = abc", 1),
		"没有 Peer":      wgOK[:strings.Index(wgOK, "[Peer]")],
		"IPv6 端点":      strings.Replace(wgOK, "vpn.example.com:51820", "[2001:db8::1]:51820", 1),
	} {
		if _, err := ParseWG([]byte(conf)); err == nil {
			t.Fatalf("%s 应被拒", name)
		}
	}
}

func ovpnConf(extra string) string {
	return "# 注释\nclient\ndev tun\nproto udp\nremote vpn.example.net 1194\nremote 203.0.113.9 443 tcp\nnobind\npersist-key\nremote-cert-tls server\ncipher AES-256-GCM\n" +
		"redirect-gateway def1\ndhcp-option DNS 10.8.0.1\n" + extra +
		"<ca>\n-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n</ca>\n<auth-user-pass>\nuser\nsecret-pass\n</auth-user-pass>\n"
}

// b. OpenVPN：脚本类、插件、管理接口、引用外部文件的指令各一例被拒；改路由和 DNS 的去掉；重新生成的只有白名单。
func TestOVPN(t *testing.T) {
	o, err := ParseOVPN([]byte(ovpnConf("")))
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Remotes) != 2 || o.Remotes[0] != (Endpoint{"vpn.example.net", 1194, "udp"}) || o.Remotes[1].Proto != "tcp" {
		t.Fatalf("%+v", o.Remotes)
	}
	if strings.Join(o.Dropped, ",") != "redirect-gateway,dhcp-option" {
		t.Fatalf("%v", o.Dropped)
	}
	gen := o.Generate(GenParams{Dev: "tunx1", DNSScript: "/run/claude-abroad/libexec/ovpn-dns.sh", ExitDir: "/run/claude-abroad/exits/x1",
		Resolved: map[string]string{"vpn.example.net": "198.51.100.3"}})
	for _, bad := range []string{"redirect-gateway", "dhcp-option DNS 10.8.0.1", "# 注释", "vpn.example.net", "route-nopull"} {
		if strings.Contains(gen, bad) {
			t.Fatalf("生成的配置里不应有 %q", bad)
		}
	}
	// v2.9：route-noexec 加推送项白名单，末尾兜底忽略其余；白名单不收路由、redirect-gateway、setenv。
	if !strings.Contains(gen, "route-noexec\npull-filter accept \"ifconfig \"\n") || !strings.HasSuffix(strings.Split(gen, "script-security")[0], "pull-filter ignore \"\"\n") {
		t.Fatalf("推送项过滤：\n%s", gen)
	}
	for _, a := range OVPNPullAccept { // 典型的不该收的推送行：没有一条能被白名单前缀匹配上
		for _, bad := range []string{"route 10.0.0.0 255.0.0.0", "route-gateway 10.8.0.1", "route-ipv6 ::/0", "redirect-gateway def1",
			"redirect-private", "setenv-safe X 1", "ifconfig-ipv6 fd00::2/64 fd00::1", "block-outside-dns", "dhcp-option DOMAIN x", "dns search-domains x"} {
			if strings.HasPrefix(bad, a) {
				t.Fatalf("白名单 %q 会放过 %q", a, bad)
			}
		}
	}
	for _, want := range []string{"route-noexec", "dev tunx1", "up /run/claude-abroad/libexec/ovpn-dns.sh", "remote 198.51.100.3 1194 udp", "<auth-user-pass>\nuser\nsecret-pass\n</auth-user-pass>"} {
		if !strings.Contains(gen, want) {
			t.Fatalf("生成的配置里应有 %q：\n%s", want, gen)
		}
	}
	line := strings.Count(ovpnConf(""), "\n") - strings.Count("<ca>\n-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n</ca>\n<auth-user-pass>\nuser\nsecret-pass\n</auth-user-pass>\n", "\n") + 1
	for _, d := range []string{"up /tmp/evil.sh", "down /tmp/x", "route-up /tmp/x", "script-security 3", "plugin /usr/lib/x.so", "management 127.0.0.1 7505",
		"management-client", "ca /etc/ca.crt", "auth-user-pass /tmp/pw", "tls-auth ta.key 1", "config other.ovpn", "setenv X 1", "pull-filter accept route", "pull-filter ignore redirect-gateway", "log /tmp/l", "daemon", "chroot /x", "dev tap"} {
		_, err := ParseOVPN([]byte(ovpnConf(d + "\n")))
		var le *LineError
		if !errors.As(err, &le) || le.Line != line {
			t.Fatalf("%q 应在第 %d 行被拒：%v", d, line, err)
		}
		if strings.Contains(err.Error(), "/tmp") || strings.Contains(err.Error(), "evil") {
			t.Fatal("错误里不应回显值")
		}
	}
	for name, conf := range map[string]string{
		"不是客户端":    strings.Replace(ovpnConf(""), "client\n", "", 1),
		"没有 CA":    strings.Replace(ovpnConf(""), "<ca>\n-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n</ca>\n", "", 1),
		"内联没结束":    "client\nremote 1.2.3.4\n<ca>\nxx\n",
		"不允许的内联块":  "client\nremote 1.2.3.4\n<dh>\nxx\n</dh>\n",
		"IPv6 服务器": "client\nremote 2001:db8::1 1194\n<ca>\nx\n</ca>\n",
	} {
		if _, err := ParseOVPN([]byte(conf)); err == nil {
			t.Fatalf("%s 应被拒", name)
		}
	}
}

const clashSub = `
port: 7890
mode: global
rules:
  - DOMAIN-SUFFIX,cn,DIRECT
  - MATCH,DIRECT
proxy-groups:
  - {name: 节点选择, type: select, proxies: [香港 01, DIRECT]}
proxies:
  - {name: 香港 01, type: ss, server: hk.example.com, port: 8388, cipher: aes-256-gcm, password: sub-secret}
  - {name: JP-hy2, type: hysteria2, server: 198.51.100.20, port: 443, password: sub-secret}
`

// b. 订阅 mihomo：只取节点（改名），其余（全局模式、DIRECT 规则、组）一概不用；生成的配置符合主代理的形状。
func TestClash(t *testing.T) {
	c, err := ParseClash([]byte(clashSub))
	if err != nil {
		t.Fatal(err)
	}
	eps := c.Endpoints()
	if len(eps) != 2 || eps[0] != (Endpoint{"hk.example.com", 8388, "tcp"}) || eps[1].Proto != "udp" {
		t.Fatalf("%+v", eps)
	}
	b, err := c.Generate(7801)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, bad := range []string{"DIRECT", "global", "香港", "节点选择", "DOMAIN-SUFFIX"} {
		if strings.Contains(s, bad) {
			t.Fatalf("生成的配置里不应有 %q", bad)
		}
	}
	if err := mihomocfg.ValidatePort(b, 7801); err != nil {
		t.Fatal(err)
	}
	for name, sub := range map[string]string{
		"没有节点":           "proxy-providers: {a: {type: http, url: 'http://x'}}\n",
		"interface-name": "proxies:\n  - {name: a, type: ss, server: 1.2.3.4, port: 1, interface-name: en0}\n",
		"dialer-proxy":   "proxies:\n  - {name: a, type: ss, server: 1.2.3.4, port: 1, dialer-proxy: b}\n",
		"不支持的类型":         "proxies:\n  - {name: a, type: direct, server: 1.2.3.4, port: 1}\n",
		"坏端口":            "proxies:\n  - {name: a, type: ss, server: 1.2.3.4, port: 70000}\n",
		"IPv6 服务器":       "proxies:\n  - {name: a, type: ss, server: '2001:db8::1', port: 1}\n",
		"routing-mark":   "proxies:\n  - {name: a, type: ss, server: 1.2.3.4, port: 1, routing_mark: 3}\n",
	} {
		if _, err := ParseClash([]byte(sub)); err == nil {
			t.Fatalf("%s 应被拒", name)
		}
	}
	// 不在白名单里的字段：被拒，并指出是哪一项、哪个字段。
	_, err = ParseClash([]byte("proxies:\n  - {name: a, type: ss, server: 1.2.3.4, port: 1, cipher: aes-128-gcm, password: x}\n  - {name: b, type: vmess, server: 1.2.3.5, port: 2, uuid: u, ip-version: dual}\n"))
	if err == nil || !strings.Contains(err.Error(), "proxies 第 2 项") || !strings.Contains(err.Error(), "ip-version") {
		t.Fatalf("应指出第 2 项的 ip-version：%v", err)
	}
}

// v2.10：订阅里的 DNS 段、按地址判断的规则、嗅探、TUN、hosts、组一概不进生成的配置；规则只有两条；
// 解析目标域名只能经 EXIT 组（节点），节点域名只经网关。
const clashFull = `
mixed-port: 7890
mode: rule
dns:
  enable: true
  enhanced-mode: fake-ip
  nameserver: [223.5.5.5, 'https://dns.alidns.com/dns-query']
  fallback: [8.8.8.8]
sniffer:
  enable: true
  sniff: {TLS: {ports: [443]}}
tun: {enable: true, stack: system, auto-route: true}
hosts: {claude.ai: 127.0.0.1}
proxy-groups:
  - {name: auto, type: url-test, proxies: [jp], url: 'http://www.gstatic.com/generate_204'}
rules:
  - IP-CIDR,10.0.0.0/8,DIRECT
  - GEOIP,CN,DIRECT
  - DOMAIN-SUFFIX,claude.ai,auto
  - MATCH,DIRECT
proxies:
  - {name: jp, type: trojan, server: 198.51.100.30, port: 443, password: sub-secret, sni: jp.example.com, udp: true}
`

func TestClashGenerateWhitelist(t *testing.T) {
	c, err := ParseClash([]byte(clashFull))
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Generate(7802)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := yaml.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := "allow-lan,bind-address,dns,find-process-mode,geo-auto-update,ipv6,log-level,mixed-port,mode,profile,proxies,proxy-groups,rules"
	if strings.Join(keys, ",") != want {
		t.Fatalf("顶层键：%s", strings.Join(keys, ","))
	}
	if r := fmt.Sprint(got["rules"]); r != "[MATCH,EXIT MATCH,REJECT]" {
		t.Fatalf("规则：%s", r)
	}
	s := string(b)
	for _, bad := range []string{"enhanced-mode", "223.5.5.5", "alidns", "fallback", "8.8.8.8", "sniff", "stack: system", "auto-route",
		"claude.ai", "url-test", "gstatic", "GEOIP", "IP-CIDR", "DIRECT", "DOMAIN-SUFFIX", "7890"} {
		if strings.Contains(s, bad) {
			t.Fatalf("生成的配置里不应有 %q：\n%s", bad, s)
		}
	}
	dns := got["dns"].(map[string]any)
	for _, ns := range dns["nameserver"].([]any) {
		if !strings.HasSuffix(ns.(string), "#"+ExitGroup) {
			t.Fatalf("解析目标域名只能经节点：%v", ns)
		}
	}
	for _, k := range []string{"default-nameserver", "proxy-server-nameserver"} {
		if fmt.Sprint(dns[k]) != "["+mihomocfg.GatewayNameserver+"]" {
			t.Fatalf("%s 只能是网关：%v", k, dns[k])
		}
	}
	if !strings.Contains(s, "sni: jp.example.com") || !strings.Contains(s, "name: n1") {
		t.Fatalf("节点的协议参数应保留、名字改为 n1：\n%s", s)
	}
}

// b. 端点提取：IP、域名、带端口、IPv6（拒绝并说明暂不支持）。
func TestEndpoints(t *testing.T) {
	for in, want := range map[string]Endpoint{
		"203.0.113.5:51820":   {"203.0.113.5", 51820, "udp"},
		"VPN.Example.com:443": {"vpn.example.com", 443, "udp"},
		"vpn.example.com":     {"vpn.example.com", 1194, "udp"},
	} {
		got, err := ParseHostPort(in, 1194, "udp")
		if err != nil || got != want {
			t.Fatalf("%s → %+v %v", in, got, err)
		}
	}
	for _, in := range []string{"[2001:db8::1]:51820", "2001:db8::1", "::1"} {
		if _, err := ParseHostPort(in, 1, "udp"); !errors.Is(err, ErrIPv6Endpoint) || !strings.Contains(err.Error(), "暂不支持") {
			t.Fatalf("%s：%v", in, err)
		}
	}
	for _, in := range []string{"127.0.0.1:1", "0.0.0.0:1", "a b:1", "x:0", "x:99999", "-bad-.com:1"} {
		if _, err := ParseHostPort(in, 1, "udp"); err == nil {
			t.Fatalf("%s 应被拒", in)
		}
	}
}

// b. 模糊测试：任意输入不崩溃（每个跑 20 秒，三个合计 60 秒：go test -fuzz=FuzzWG -fuzztime=20s 等）。
func FuzzWG(f *testing.F) {
	f.Add([]byte(wgOK))
	f.Fuzz(func(t *testing.T, b []byte) {
		if w, err := ParseWG(b); err == nil {
			_ = w.SetConf(nil)
		}
	})
}

func FuzzOVPN(f *testing.F) {
	f.Add([]byte(ovpnConf("")))
	f.Fuzz(func(t *testing.T, b []byte) {
		if o, err := ParseOVPN(b); err == nil {
			_ = o.Generate(GenParams{Dev: "tun0"})
		}
	})
}

func FuzzClash(f *testing.F) {
	f.Add([]byte(clashSub))
	f.Fuzz(func(t *testing.T, b []byte) {
		if c, err := ParseClash(b); err == nil {
			_, _ = c.Generate(7801)
		}
	})
}
