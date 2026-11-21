package managedproxy

import (
	"encoding/json"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"claude-abroad/internal/mihomocfg"
	"claude-abroad/internal/usernet"
)

const vmNodes = `proxies:
 - {name: hop1, type: socks5, server: 9.9.9.10, port: 1080, username: u1, password: HOP1SECRET}
 - {name: exit, type: ss, server: 8.8.4.20, port: 8388, cipher: aes-128-gcm, password: EXITSECRET}
 - {name: unrelated, type: trojan, server: 192.0.2.30, port: 443, password: UNRELATEDSECRET}
`

func vmStore(t *testing.T, chain bool) Store {
	s, _ := Load(t.TempDir())
	if err := s.Import("vm", "", []byte(vmNodes), "src", 0, ""); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, n := range s.Sources[0].Nodes {
		ids[n.Name] = n.ID
	}
	s.Selected = ids["exit"]
	if chain {
		s.Chain = []string{ids["hop1"], ids["exit"]}
	}
	return s
}

func TestVMRuntimeGolden(t *testing.T) {
	for _, chain := range []bool{false, true} {
		s := vmStore(t, chain)
		wire, err := s.VMRuntime()
		if err != nil {
			t.Fatalf("chain=%v：%v", chain, err)
		}
		if err := mihomocfg.Validate(wire); err != nil {
			t.Fatalf("chain=%v：输出应通过二次校验：%v", chain, err)
		}
		text := string(wire)
		for _, bad := range []string{"bind-interface", "interface-name", "routing-mark", "DIRECT", "tun", "authentication",
			"UNRELATEDSECRET", "192.0.2.30", s.Auth, "skip-auth-prefixes"} {
			if bad != "" && strings.Contains(text, bad) {
				t.Errorf("chain=%v：输出里不应有 %q", chain, bad)
			}
		}
		for _, n := range s.Sources[0].Nodes {
			if n.Name == "unrelated" && strings.Contains(text, n.ID) {
				t.Errorf("chain=%v：路线外的节点不应出现", chain)
			}
		}
		var c struct {
			Port       int    `yaml:"mixed-port"`
			Bind       string `yaml:"bind-address"`
			Controller string `yaml:"external-controller-unix"`
			DNS        struct {
				SystemHosts bool     `yaml:"use-system-hosts"`
				Default     []string `yaml:"default-nameserver"`
				Proxy       []string `yaml:"proxy-server-nameserver"`
				Names       []string `yaml:"nameserver"`
			} `yaml:"dns"`
			Proxies []map[string]any `yaml:"proxies"`
			Rules   []string         `yaml:"rules"`
		}
		if err := yaml.Unmarshal(wire, &c); err != nil {
			t.Fatal(err)
		}
		if c.Port != 7890 || c.Bind != "127.0.0.1" || c.DNS.SystemHosts || !strings.HasPrefix(c.Controller, "/run/claude-abroad/") {
			t.Errorf("chain=%v：监听或 DNS 不对：%+v", chain, c)
		}
		if strings.Join(c.Rules, " ") != "MATCH,CAGE MATCH,REJECT" {
			t.Errorf("chain=%v：规则应为 MATCH,CAGE 加 MATCH,REJECT 兜底：%v", chain, c.Rules)
		}
		for _, list := range [][]string{c.DNS.Default, c.DNS.Proxy} {
			if strings.Join(list, " ") != "192.168.127.1" {
				t.Errorf("chain=%v：节点域名只经虚拟机网关的 DNS 解析：%v", chain, list)
			}
		}
		for _, n := range c.DNS.Names {
			if !strings.HasSuffix(n, "#CAGE") {
				t.Errorf("chain=%v：nameserver 应经主代理（#CAGE）：%v", chain, c.DNS.Names)
			}
		}
		route, _ := s.Route()
		if len(c.Proxies) != len(route) {
			t.Errorf("chain=%v：应只含路线上的 %d 个节点，实际 %d", chain, len(route), len(c.Proxies))
		}
	}
}

func TestRuntimeStillHasAllNodes(t *testing.T) {
	s := vmStore(t, false)
	wire, err := s.Runtime(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), "UNRELATEDSECRET") {
		t.Fatal("Cage 的 Runtime 行为不变：仍含全部节点")
	}
}

// storeOf 导入一份节点 YAML，选中 exit；chain 非空时用显式代理链（名字列表）。
func storeOf(t *testing.T, nodes string, chain ...string) Store {
	t.Helper()
	s, _ := Load(t.TempDir())
	if err := s.Import("t", "", []byte(nodes), "src", 0, ""); err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, n := range s.Sources[0].Nodes {
		ids[n.Name] = n.ID
	}
	s.Selected = ids["exit"]
	for _, c := range chain {
		s.Chain = append(s.Chain, ids[c])
	}
	return s
}

func serverOf(t *testing.T, wire []byte, id string) string {
	var c struct{ Proxies []map[string]any }
	if err := yaml.Unmarshal(wire, &c); err != nil {
		t.Fatal(err)
	}
	for _, p := range c.Proxies {
		if p["name"] == id {
			s, _ := p["server"].(string)
			return s
		}
	}
	t.Fatalf("没有节点 %s", id)
	return ""
}

func idOf(s Store, name string) string {
	for _, n := range s.Sources[0].Nodes {
		if n.Name == name {
			return n.ID
		}
	}
	return ""
}

func TestVMRuntimeFirstHop(t *testing.T) {
	cases := []struct {
		name    string
		nodes   string
		chain   []string
		allow   string            // 期望白名单（逗号分隔）
		servers map[string]string // 节点名 → 期望的 server
	}{
		{"单节点 127.0.0.1", "proxies:\n - {name: exit, type: socks5, server: 127.0.0.1, port: 7890}\n", nil,
			"127.0.0.1:7890", map[string]string{"exit": "192.168.127.254"}},
		{"单节点局域网", "proxies:\n - {name: exit, type: socks5, server: 192.168.1.20, port: 1080}\n", nil,
			"192.168.1.20:1080", map[string]string{"exit": "192.168.1.20"}},
		{"局域网（IPv4 映射写法）", "proxies:\n - {name: exit, type: socks5, server: \"::ffff:192.168.1.20\", port: 1080}\n", nil,
			"192.168.1.20:1080", map[string]string{"exit": "192.168.1.20"}},
		{"公网 IP", "proxies:\n - {name: exit, type: ss, server: 8.8.4.5, port: 8388, cipher: aes-128-gcm, password: p}\n", nil,
			"", map[string]string{"exit": "8.8.4.5"}},
		{"域名", "proxies:\n - {name: exit, type: ss, server: node.example.com, port: 8388, cipher: aes-128-gcm, password: p}\n", nil,
			"", map[string]string{"exit": "node.example.com"}},
		{"显式链 [公网 → 127.0.0.1]", "proxies:\n - {name: hop, type: socks5, server: 8.8.4.5, port: 1080}\n - {name: exit, type: socks5, server: 127.0.0.1, port: 7890}\n",
			[]string{"hop", "exit"}, "", map[string]string{"hop": "8.8.4.5", "exit": "127.0.0.1"}},
		{"显式链 [127.0.0.1 → 公网]", "proxies:\n - {name: hop, type: socks5, server: 127.0.0.1, port: 7890}\n - {name: exit, type: socks5, server: 8.8.4.5, port: 1080}\n",
			[]string{"hop", "exit"}, "127.0.0.1:7890", map[string]string{"hop": "192.168.127.254", "exit": "8.8.4.5"}},
		{"导入自带 dialer-proxy [127.0.0.1 → 公网]", "proxies:\n - {name: hop, type: socks5, server: 127.0.0.1, port: 7890}\n - {name: exit, type: socks5, server: 8.8.4.5, port: 1080, dialer-proxy: hop}\n",
			nil, "127.0.0.1:7890", map[string]string{"hop": "192.168.127.254", "exit": "8.8.4.5"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := storeOf(t, c.nodes, c.chain...)
			vc, err := s.VMRuntimeConfig()
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(vc.Allow, ","); got != c.allow {
				t.Errorf("白名单 %q，应为 %q", got, c.allow)
			}
			if _, err := usernet.ParseAllow(vc.Allow); err != nil {
				t.Errorf("白名单应能通过 usernet.ParseAllow：%v", err)
			}
			if err := mihomocfg.Validate(vc.YAML); err != nil {
				t.Errorf("输出应通过校验：%v", err)
			}
			for name, want := range c.servers {
				if got := serverOf(t, vc.YAML, idOf(s, name)); got != want {
					t.Errorf("%s 的 server 是 %q，应为 %q", name, got, want)
				}
			}
		})
	}
}

func TestVMRuntimeFirstHopRejects(t *testing.T) {
	for name, server := range map[string]string{
		"localhost": "localhost", "其他回环": "127.0.0.2", "0.0.0.0": "0.0.0.0", "链路本地": "169.254.1.1",
		"虚拟机子网": "192.168.127.9", "IPv6": "\"2001:db8::1\"", "localhost.": "localhost.",
	} {
		s := storeOf(t, "proxies:\n - {name: exit, type: socks5, server: "+server+", port: 1080}\n")
		if len(s.Sources[0].Nodes) == 0 {
			t.Fatalf("%s：导入阶段就被拒了，用例没有走到 VMRuntimeConfig", name)
		}
		if _, err := s.VMRuntimeConfig(); err == nil {
			t.Errorf("%s（%s）作为第一跳应被拒", name, server)
		} else if !strings.Contains(err.Error(), "127.0.0.1") {
			t.Errorf("%s：错误应说明怎么改：%v", name, err)
		}
	}
}

func udpOf(t *testing.T, wire []byte) map[string]any {
	t.Helper()
	var c struct{ Proxies []map[string]any }
	if err := yaml.Unmarshal(wire, &c); err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for _, p := range c.Proxies {
		out[p["name"].(string)] = p["udp"]
	}
	return out
}

func TestVMRuntimeUDP(t *testing.T) {
	const (
		ssUDP   = " - {name: exit, type: ss, server: 8.8.4.20, port: 8388, cipher: aes-128-gcm, password: p, udp: true}\n"
		ssNoUDP = " - {name: exit, type: ss, server: 8.8.4.20, port: 8388, cipher: aes-128-gcm, password: p}\n"
	)
	cases := []struct {
		name  string
		nodes string
		chain []string
		opts  VMOptions
		want  bool
	}{
		{"来源没声明 udp", "proxies:\n" + ssNoUDP, nil, VMOptions{}, false},
		{"来源声明 udp: false", "proxies:\n - {name: exit, type: ss, server: 8.8.4.20, port: 8388, cipher: aes-128-gcm, password: p, udp: false}\n", nil, VMOptions{}, false},
		{"来源声明 udp: true", "proxies:\n" + ssUDP, nil, VMOptions{}, true},
		{"键名写法不同（UDP: true）", "proxies:\n - {name: exit, type: ss, server: 8.8.4.20, port: 8388, cipher: aes-128-gcm, password: p, UDP: true}\n", nil, VMOptions{}, true},
		{"http 节点即使声明 udp", "proxies:\n - {name: exit, type: http, server: 8.8.4.20, port: 8080, udp: true}\n", nil, VMOptions{}, false},
		{"链：每跳都 udp", "proxies:\n - {name: hop, type: socks5, server: 9.9.9.1, port: 1080, udp: true}\n" + ssUDP, []string{"hop", "exit"}, VMOptions{}, true},
		{"链：中转没声明 udp", "proxies:\n - {name: hop, type: socks5, server: 9.9.9.1, port: 1080}\n" + ssUDP, []string{"hop", "exit"}, VMOptions{}, false},
		{"链：中转是 http", "proxies:\n - {name: hop, type: http, server: 9.9.9.1, port: 8080, udp: true}\n" + ssUDP, []string{"hop", "exit"}, VMOptions{}, false},
		{"第一跳本机回环（默认）", "proxies:\n - {name: exit, type: socks5, server: 127.0.0.1, port: 7890, udp: true}\n", nil, VMOptions{}, false},
		{"第一跳本机回环 + LoopbackUDP", "proxies:\n - {name: exit, type: socks5, server: 127.0.0.1, port: 7890, udp: true}\n", nil, VMOptions{LoopbackUDP: true}, true},
		{"LoopbackUDP 不越过来源声明", "proxies:\n - {name: exit, type: socks5, server: 127.0.0.1, port: 7890}\n", nil, VMOptions{LoopbackUDP: true}, false},
		{"局域网第一跳不受回环限制", "proxies:\n - {name: exit, type: socks5, server: 192.168.1.20, port: 1080, udp: true}\n", nil, VMOptions{}, true},
		{"中转是本机回环（默认）", "proxies:\n - {name: hop, type: socks5, server: 127.0.0.1, port: 7890, udp: true}\n" + ssUDP, []string{"hop", "exit"}, VMOptions{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := storeOf(t, c.nodes, c.chain...)
			vc, err := s.VMRuntimeConfigWith(c.opts)
			if err != nil {
				t.Fatal(err)
			}
			if vc.UDP != c.want {
				t.Errorf("VMConfig.UDP=%v，应为 %v", vc.UDP, c.want)
			}
			if err := mihomocfg.Validate(vc.YAML); err != nil {
				t.Errorf("输出应通过校验：%v", err)
			}
			for name, v := range udpOf(t, vc.YAML) {
				if v != c.want {
					t.Errorf("节点 %s 的 udp=%v，路线上的节点应统一为 %v", name, v, c.want)
				}
			}
			// Cage 的 Runtime 不受影响：udp 仍一律 false。
			for name, v := range udpOf(t, mustRuntime(t, s)) {
				if v != false {
					t.Errorf("Cage Runtime 里节点 %s 的 udp=%v，应保持 false", name, v)
				}
			}
		})
	}
}

func mustRuntime(t *testing.T, s Store) []byte {
	t.Helper()
	b, err := s.Runtime(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestNodeUDPPersisted(t *testing.T) {
	dir := t.TempDir()
	s, _ := Load(dir)
	if err := s.Import("t", "", []byte("proxies:\n - {name: a, type: ss, server: 8.8.4.20, port: 8388, cipher: aes-128-gcm, password: p, udp: true}\n - {name: b, type: ss, server: 8.8.4.21, port: 8388, cipher: aes-128-gcm, password: p}\n"), "src", 0, ""); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, n := range s.Sources[0].Nodes {
		got[n.Name] = n.UDP
		if n.Config["udp"] != false {
			t.Errorf("%s：Config[udp] 应仍为 false", n.Name)
		}
	}
	if !got["a"] || got["b"] {
		t.Errorf("Node.UDP 应来自来源声明：%v", got)
	}
	// 旧存储（JSON 里没有 udp 字段）按 false。
	var n Node
	if err := json.Unmarshal([]byte(`{"id":"x","name":"x","type":"ss","config":{}}`), &n); err != nil || n.UDP {
		t.Errorf("旧存储应按 false：%v %v", n.UDP, err)
	}
}

func TestVMRuntimeFirstHopArg(t *testing.T) {
	cases := []struct {
		name, nodes string
		chain       []string
		hop, allow  string
	}{
		{"域名", "proxies:\n - {name: exit, type: ss, server: Node.Example.com, port: 8388, cipher: aes-128-gcm, password: p}\n", nil, "node.example.com:8388", ""},
		{"公网 IP", "proxies:\n - {name: exit, type: ss, server: 8.8.4.4, port: 8388, cipher: aes-128-gcm, password: p}\n", nil, "8.8.4.4:8388", ""},
		{"回环走白名单", "proxies:\n - {name: exit, type: socks5, server: 127.0.0.1, port: 7890}\n", nil, "", "127.0.0.1:7890"},
		{"局域网走白名单", "proxies:\n - {name: exit, type: socks5, server: 192.168.1.20, port: 1080}\n", nil, "", "192.168.1.20:1080"},
		{"hysteria2 端口跳跃", "proxies:\n - {name: exit, type: hysteria2, server: hy.example.com, port: 443, ports: \"20000-30000/40000\", password: p}\n", nil, "hy.example.com:443,20000-30000,40000", ""},
		{"hysteria2 只有 ports", "proxies:\n - {name: exit, type: hysteria2, server: hy.example.com, ports: \"20000-30000\", password: p}\n", nil, "hy.example.com:20000-30000", ""},
		{"hysteria2 mport 别名", "proxies:\n - {name: exit, type: hysteria2, server: hy.example.com, port: 443, mport: \"5000-5010\", password: p}\n", nil, "hy.example.com:443,5000-5010", ""},
		{"mieru port-range", "proxies:\n - {name: exit, type: mieru, server: mi.example.com, port-range: 2090-2099, username: u, password: p, transport: TCP}\n", nil, "mi.example.com:2090-2099", ""},
		{"tuic 的 ip", "proxies:\n - {name: exit, type: tuic, server: tu.example.com, ip: 8.8.8.8, port: 443, uuid: 00000000-0000-4000-8000-000000000000, password: p}\n", nil, "8.8.8.8:443", ""},
		{"Ports 大写（hysteria）", "proxies:\n - {name: exit, type: hysteria, server: hy.example.com, port: 443, Ports: \"2000-3000\", auth-str: p}\n", nil, "hy.example.com:443,2000-3000", ""},
		{"port_range 写法（mieru）", "proxies:\n - {name: exit, type: mieru, server: mi.example.com, port_range: 2090-2099, username: u, password: p, transport: TCP}\n", nil, "mi.example.com:2090-2099", ""},
		{"tuic 的 IP 大写", "proxies:\n - {name: exit, type: tuic, server: tu.example.com, IP: 8.8.8.8, port: 443, uuid: 00000000-0000-4000-8000-000000000000, password: p}\n", nil, "8.8.8.8:443", ""},
		{"非跳跃类型的 ports 不放宽端口", "proxies:\n - {name: exit, type: ss, server: ss.example.com, port: 8388, ports: \"1-65535\", cipher: aes-128-gcm, password: p}\n", nil, "ss.example.com:8388", ""},
		{"链式只取第一跳", "proxies:\n - {name: hop, type: socks5, server: hop.example.com, port: 1080}\n - {name: exit, type: ss, server: exit.example.com, port: 8388, cipher: aes-128-gcm, password: p}\n",
			[]string{"hop", "exit"}, "hop.example.com:1080", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := storeOf(t, c.nodes, c.chain...)
			vc, err := s.VMRuntimeConfig()
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(vc.FirstHop, " "); got != c.hop {
				t.Errorf("FirstHop %q，应为 %q", got, c.hop)
			}
			if got := strings.Join(vc.Allow, " "); got != c.allow {
				t.Errorf("Allow %q，应为 %q", got, c.allow)
			}
			// 往返：输出必须能被 usernet.ParseFirstHops 原样接受。
			hops, err := usernet.ParseFirstHops(vc.FirstHop)
			if err != nil || len(hops) != len(vc.FirstHop) {
				t.Fatalf("ParseFirstHops 应接受：%v %v", hops, err)
			}
			if err := mihomocfg.Validate(vc.YAML); err != nil {
				t.Errorf("输出应通过校验：%v", err)
			}
		})
	}
}

func TestVMRuntimeFirstHopArgRejects(t *testing.T) {
	for name, nodes := range map[string]string{
		"文档网段":                            "proxies:\n - {name: exit, type: ss, server: 203.0.113.5, port: 8388, cipher: aes-128-gcm, password: p}\n",
		"fake-IP 网段":                      "proxies:\n - {name: exit, type: ss, server: 198.18.0.9, port: 8388, cipher: aes-128-gcm, password: p}\n",
		"tuic ip 指向局域网":                   "proxies:\n - {name: exit, type: tuic, server: tu.example.com, ip: 192.168.1.2, port: 443, uuid: 00000000-0000-4000-8000-000000000000, password: p}\n",
		"tuic ip 不是 IP":                   "proxies:\n - {name: exit, type: tuic, server: tu.example.com, ip: x.example.com, port: 443, uuid: 00000000-0000-4000-8000-000000000000, password: p}\n",
		"realm-opts":                      "proxies:\n - {name: exit, type: hysteria2, server: hy.example.com, port: 443, password: p, realm-opts: {enable: true, server-url: \"https://realm.example.com\"}}\n",
		"download-settings":               "proxies:\n - {name: exit, type: vless, server: v.example.com, port: 443, uuid: 00000000-0000-4000-8000-000000000000, network: xhttp, xhttp-opts: {download-settings: {server: d.example.com, port: 443}}}\n",
		"回环加端口区间":                         "proxies:\n - {name: exit, type: hysteria2, server: 127.0.0.1, port: 443, ports: \"20000-30000\", password: p}\n",
		"端口区间反了":                          "proxies:\n - {name: exit, type: mieru, server: mi.example.com, port-range: 2099-2090, username: u, password: p, transport: TCP}\n",
		"realm_opts 写法":                   "proxies:\n - {name: exit, type: hysteria2, server: hy.example.com, port: 443, password: p, realm_opts: {enable: true, server-url: \"https://realm.example.com\"}}\n",
		"xhttp_opts.download_settings 写法": "proxies:\n - {name: exit, type: vless, server: v.example.com, port: 443, uuid: 00000000-0000-4000-8000-000000000000, network: xhttp, XHTTP_OPTS: {Download_Settings: {server: d.example.com, port: 443}}}\n",
		"tuic IP 大写指向局域网":                 "proxies:\n - {name: exit, type: tuic, server: tu.example.com, IP: 192.168.1.2, port: 443, uuid: 00000000-0000-4000-8000-000000000000, password: p}\n",
		"tuic 的 ip 与 server 同为回环":         "proxies:\n - {name: exit, type: tuic, server: 127.0.0.1, ip: 127.0.0.1, port: 443, uuid: 00000000-0000-4000-8000-000000000000, password: p}\n",
		"ports 和 Ports 并存":                "proxies:\n - {name: exit, type: hysteria, server: hy.example.com, port: 443, ports: \"2000-3000\", Ports: \"4000-5000\", auth-str: p}\n",
		"Ports 大写的反区间":                    "proxies:\n - {name: exit, type: hysteria, server: hy.example.com, port: 443, Ports: \"3000-2000\", auth-str: p}\n",
	} {
		s, _ := Load(t.TempDir())
		if err := s.Import("t", "", []byte(nodes), "src", 0, ""); err != nil {
			t.Logf("%s：导入阶段就被拒了（%v），也算拒绝", name, err)
			continue
		}
		s.Selected = s.Sources[0].Nodes[0].ID
		if _, err := s.VMRuntimeConfig(); err == nil {
			t.Errorf("%s 作为第一跳应被拒", name)
		}
	}
}
