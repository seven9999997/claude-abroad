package pf

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// Packet 是求值用的一个出站包。地址族由 Dst 得出。Proto 是 tcp、udp、icmp（IPv6 时 icmp6）。
type Packet struct {
	If      string
	User    string
	Proto   string
	SrcPort uint16
	DstPort uint16
	Dst     netip.Addr
}

// rule 是解析后的一条规则（只认 Render 生成的语法子集）。
type rule struct {
	pass    bool
	iface   string // 为空表示任意接口
	family  string // ""、inet、inet6
	proto   string // 为空表示任意协议
	srcPort int    // 0 表示任意
	dst     string // ""=any；"<abroad_lan>"；否则是地址或网段
	dstPort int
	user    string
	line    int
}

// Eval 对 Render 输出的规则文本求值：按 quick 语义取第一条命中的规则。
// 遇到不认识的行返回错误（不猜）；没有任何规则命中时不放行。
func Eval(rules []byte, pkt Packet) (bool, error) {
	rs, table, err := parse(rules)
	if err != nil {
		return false, err
	}
	for _, r := range rs {
		if r.match(pkt, table) {
			return r.pass, nil
		}
	}
	return false, nil
}

func parse(text []byte) ([]rule, []netip.Prefix, error) {
	var rs []rule
	var table []netip.Prefix
	for i, raw := range strings.Split(string(text), "\n") {
		ln := i + 1
		l := strings.TrimSpace(raw)
		if l == "" {
			continue
		}
		bad := func(why string) error { return fmt.Errorf("第 %d 行不认识（%s）：%q", ln, why, l) }
		if strings.HasPrefix(l, "table ") {
			rest, ok := strings.CutPrefix(l, "table <abroad_lan> { ")
			body, ok2 := strings.CutSuffix(rest, " }")
			if !ok || !ok2 {
				return nil, nil, bad("表")
			}
			for _, part := range strings.Split(body, ", ") {
				p, err := netip.ParsePrefix(part)
				if err != nil {
					return nil, nil, bad("表项")
				}
				table = append(table, p)
			}
			continue
		}
		t := strings.Fields(l)
		r := rule{line: ln}
		k := 0
		next := func() string {
			if k < len(t) {
				k++
				return t[k-1]
			}
			return ""
		}
		switch next() {
		case "pass":
			r.pass = true
		case "block":
			if next() != "drop" {
				return nil, nil, bad("block 后应为 drop")
			}
		default:
			return nil, nil, bad("动作")
		}
		if next() != "out" || next() != "quick" {
			return nil, nil, bad("应为 out quick")
		}
		tok := next()
		if tok == "on" {
			r.iface = next()
			if r.iface == "" {
				return nil, nil, bad("缺接口名")
			}
			tok = next()
		}
		if tok == "inet" || tok == "inet6" {
			r.family = tok
			tok = next()
		}
		if tok == "proto" {
			r.proto = next()
			if r.proto != "tcp" && r.proto != "udp" {
				return nil, nil, bad("协议")
			}
			tok = next()
		}
		switch tok {
		case "all":
			tok = next()
			// no state 只认 `pass out quick on <接口> all no state` 这一种位置。
			if tok == "no" {
				if !r.pass || r.iface == "" || r.family != "" || r.proto != "" || next() != "state" {
					return nil, nil, bad("no state 的位置不对")
				}
				tok = next()
			}
		case "from":
			if next() != "any" {
				return nil, nil, bad("from 只认 any")
			}
			tok = next()
			if tok == "port" {
				p, err := strconv.Atoi(next())
				if err != nil || p <= 0 || p > 65535 {
					return nil, nil, bad("源端口")
				}
				r.srcPort = p
				tok = next()
			}
			if tok != "to" {
				return nil, nil, bad("缺 to")
			}
			tok = "to"
			fallthrough
		case "to":
			if tok == "to" {
				d := next()
				switch {
				case d == "any":
				case d == "<abroad_lan>":
					r.dst = d
				default:
					if _, err := netip.ParsePrefix(d); err != nil {
						if _, err := netip.ParseAddr(d); err != nil {
							return nil, nil, bad("目的地址")
						}
					}
					r.dst = d
				}
				tok = next()
				if tok == "port" {
					p, err := strconv.Atoi(next())
					if err != nil || p <= 0 || p > 65535 {
						return nil, nil, bad("目的端口")
					}
					r.dstPort = p
					tok = next()
				}
			}
		default:
			return nil, nil, bad("缺 all、from 或 to")
		}
		if tok == "user" {
			r.user = next()
			if r.user == "" {
				return nil, nil, bad("缺用户名")
			}
			tok = next()
		}
		if tok != "" {
			return nil, nil, bad("多余的内容 " + tok)
		}
		if (r.srcPort != 0 || r.dstPort != 0) && r.proto == "" {
			return nil, nil, bad("端口条件需要 proto")
		}
		rs = append(rs, r)
	}
	return rs, table, nil
}

func (r rule) match(p Packet, table []netip.Prefix) bool {
	if r.iface != "" && r.iface != p.If {
		return false
	}
	is6 := p.Dst.Is6() && !p.Dst.Is4In6()
	switch r.family {
	case "inet":
		if is6 {
			return false
		}
	case "inet6":
		if !is6 {
			return false
		}
	}
	if r.proto != "" && r.proto != p.Proto {
		return false
	}
	if r.srcPort != 0 && r.srcPort != int(p.SrcPort) {
		return false
	}
	if r.dstPort != 0 && r.dstPort != int(p.DstPort) {
		return false
	}
	if r.dst != "" {
		a := p.Dst.Unmap()
		switch r.dst {
		case "<abroad_lan>":
			in := false
			for _, t := range table {
				in = in || t.Contains(a)
			}
			if !in {
				return false
			}
		default:
			if pre, err := netip.ParsePrefix(r.dst); err == nil {
				if !pre.Contains(a) {
					return false
				}
			} else if netip.MustParseAddr(r.dst) != a {
				return false
			}
		}
	}
	if r.user != "" {
		// pf 的 user 条件只对 tcp、udp 生效（只有它们有套接字归属）。
		if (p.Proto != "tcp" && p.Proto != "udp") || p.User != r.user {
			return false
		}
	}
	return true
}
