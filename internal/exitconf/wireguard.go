package exitconf

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// WG 是校验过的 WireGuard 配置（m4-plan v2 建议改 8 的白名单）：
//   - [Interface]：PrivateKey、Address、DNS、MTU；
//   - [Peer]：PublicKey、PresharedKey、Endpoint、AllowedIPs、PersistentKeepalive。
//
// PreUp、PostUp、PreDown、PostDown、SaveConfig、Table、FwMark、ListenPort 等一律拒绝并指出是哪一行。
// 不用 wg-quick（M4-1 的结论）：交给 `wg setconf` 的配置由 SetConf 重新生成，只含它认识的项。
type WG struct {
	PrivateKey string
	Address    []netip.Prefix // 只留 IPv4（虚拟机只走 IPv4）
	DNS        []netip.Addr   // 只留 IPv4
	MTU        int
	Peers      []WGPeer
}

// WGPeer 是一个对端。
type WGPeer struct {
	PublicKey    string
	PresharedKey string
	Endpoint     Endpoint
	AllowedIPs   []netip.Prefix
	Keepalive    int
}

func wgKey(v string) bool {
	b, err := base64.StdEncoding.DecodeString(v)
	return err == nil && len(b) == 32
}

func prefixes(v string, line int, name string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(v, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			a, aerr := netip.ParseAddr(f)
			if aerr != nil {
				return nil, lineErr(line, name, "不是地址或网段")
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if p.Addr().Is4() {
			out = append(out, p.Masked())
		}
	}
	return out, nil
}

// ParseWG 解析并校验 WireGuard 配置（wg-quick 的 .conf 格式）。
func ParseWG(b []byte) (WG, error) {
	var w WG
	if len(b) > 64<<10 {
		return w, lineErr(0, "", "配置太大")
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 4096), 64<<10)
	section := ""
	var peer *WGPeer
	seen := map[string]bool{}
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			switch strings.ToLower(line) {
			case "[interface]":
				if section != "" {
					return w, lineErr(n, line, "[Interface] 只能有一段，且在最前面")
				}
				section = "interface"
			case "[peer]":
				if section == "" {
					return w, lineErr(n, line, "[Peer] 之前要有 [Interface]")
				}
				section = "peer"
				w.Peers = append(w.Peers, WGPeer{})
				peer = &w.Peers[len(w.Peers)-1]
				seen = map[string]bool{}
			default:
				return w, lineErr(n, "段", "只认 [Interface] 和 [Peer]")
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return w, lineErr(n, "", "不是“键 = 值”的格式")
		}
		key, v := strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		name := strings.TrimSpace(k)
		if section == "" {
			return w, lineErr(n, name, "在任何段之前")
		}
		if seen[section+key] && key != "address" && key != "dns" && key != "allowedips" {
			return w, lineErr(n, name, "重复")
		}
		seen[section+key] = true
		switch section + "." + key {
		case "interface.privatekey":
			if !wgKey(v) {
				return w, lineErr(n, name, "不是合法的密钥")
			}
			w.PrivateKey = v
		case "interface.address":
			p, err := prefixes(v, n, name)
			if err != nil {
				return w, err
			}
			w.Address = append(w.Address, p...)
		case "interface.dns":
			for _, f := range strings.Split(v, ",") {
				a, err := netip.ParseAddr(strings.TrimSpace(f))
				if err != nil {
					return w, lineErr(n, name, "只收 IP 地址（不收搜索域）")
				}
				if a.Is4() {
					w.DNS = append(w.DNS, a)
				}
			}
		case "interface.mtu":
			m, err := strconv.Atoi(v)
			if err != nil || m < 576 || m > 9000 {
				return w, lineErr(n, name, "MTU 要在 576–9000")
			}
			w.MTU = m
		case "peer.publickey":
			if !wgKey(v) {
				return w, lineErr(n, name, "不是合法的公钥")
			}
			peer.PublicKey = v
		case "peer.presharedkey":
			if !wgKey(v) {
				return w, lineErr(n, name, "不是合法的预共享密钥")
			}
			peer.PresharedKey = v
		case "peer.endpoint":
			ep, err := ParseHostPort(v, 0, "udp")
			if err != nil {
				return w, lineErr(n, name, err.Error())
			}
			peer.Endpoint = ep
		case "peer.allowedips":
			p, err := prefixes(v, n, name)
			if err != nil {
				return w, err
			}
			peer.AllowedIPs = append(peer.AllowedIPs, p...)
		case "peer.persistentkeepalive":
			ka, err := strconv.Atoi(v)
			if err != nil || ka < 0 || ka > 65535 {
				return w, lineErr(n, name, "要在 0–65535")
			}
			peer.Keepalive = ka
		default:
			return w, lineErr(n, name, "不允许的指令（只认白名单里的项；PreUp、PostUp、Table 之类一律拒绝）")
		}
	}
	if err := sc.Err(); err != nil {
		return w, lineErr(0, "", "读不出配置")
	}
	switch {
	case w.PrivateKey == "":
		return w, lineErr(0, "PrivateKey", "缺少")
	case len(w.Peers) == 0:
		return w, lineErr(0, "[Peer]", "至少要有一个对端")
	case len(w.Address) == 0:
		return w, lineErr(0, "Address", "缺少 IPv4 地址")
	}
	for i, p := range w.Peers {
		if p.PublicKey == "" || p.Endpoint.Port == 0 {
			return w, lineErr(0, fmt.Sprintf("第 %d 个 [Peer]", i+1), "缺少 PublicKey 或 Endpoint")
		}
		if len(p.AllowedIPs) == 0 {
			w.Peers[i].AllowedIPs = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}
		}
	}
	return w, nil
}

// Endpoints 返回全部对端的端点。
func (w WG) Endpoints() []Endpoint {
	var out []Endpoint
	for _, p := range w.Peers {
		out = append(out, p.Endpoint)
	}
	return out
}

// SetConf 生成交给 `wg setconf` 的配置：只含它认识的项；端点的域名换成 resolved 里给的 IP（agent 先在根命名空间解析）。
func (w WG) SetConf(resolved map[string]string) string {
	var b strings.Builder
	b.WriteString("[Interface]\nPrivateKey = " + w.PrivateKey + "\n")
	for _, p := range w.Peers {
		host := p.Endpoint.Host
		if ip, ok := resolved[host]; ok {
			host = ip
		}
		b.WriteString("\n[Peer]\nPublicKey = " + p.PublicKey + "\n")
		if p.PresharedKey != "" {
			b.WriteString("PresharedKey = " + p.PresharedKey + "\n")
		}
		b.WriteString(fmt.Sprintf("Endpoint = %s:%d\n", host, p.Endpoint.Port))
		var ips []string
		for _, a := range p.AllowedIPs {
			ips = append(ips, a.String())
		}
		b.WriteString("AllowedIPs = " + strings.Join(ips, ", ") + "\n")
		if p.Keepalive > 0 {
			b.WriteString("PersistentKeepalive = " + strconv.Itoa(p.Keepalive) + "\n")
		}
	}
	return b.String()
}
