// Package pf 生成 Claude 出国的 pf 断网保护规则（设计 6.1），并提供一个只认自己生成的语法子集的离线求值器，
// 用来证明“不泄漏”（M2-6a）。加载、启用引用、巡检在 M2-6b。
//
// 规则是逐条展开的规范形式：不用宏、不用花括号列表，每个接口、每个协议、每个目的网段各一行，
// 只保留 <abroad_lan> 这一张表；同样的输入逐字节相同。
//
// 本包在 6a 阶段不执行任何命令、不碰网络（有源码检查测试守着）。
package pf

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
)

// Config 是生成规则的输入。
type Config struct {
	UtunName string         // 我们的 utun；为空时（开机 Boot 时还没有 utun）不输出那一行
	LAN      []netip.Prefix // 允许直连的局域网段；“允许局域网”关闭时为空。只收私有范围（见 lanAllowed）
	VMIfs    []string       // 本机虚拟机网桥（bridge100、vmenet0 …）
	User     string         // 唯一放行去公网的用户（_abroad）
}

// 名字只收明确的形式，挡住 pf 的保留字（any、all、self、inet6、unknown、纯数字……），
// 免得离线求值器把它们当普通名字、而真实 pf 另有语义。
var (
	utunRe = regexp.MustCompile(`^utun[0-9]{1,4}$`)
	// VMIfPrefixes 是虚拟机网桥接口名允许的前缀（R15 实测后再扩）。
	VMIfPrefixes = []string{"bridge", "vmenet", "vmnet", "vnic"}
	userRe       = regexp.MustCompile(`^_[a-z0-9_]{1,30}$`)
)

// ValidVMIf 报告 name 是否是允许的虚拟机网桥接口名（前缀见 VMIfPrefixes，后跟 1–4 位数字）。
func ValidVMIf(name string) bool {
	for _, p := range VMIfPrefixes {
		if rest, ok := strings.CutPrefix(name, p); ok && regexp.MustCompile(`^[0-9]{1,4}$`).MatchString(rest) {
			return true
		}
	}
	return false
}

// userOK：`_` 开头的系统用户，或者恰好是 nobody（开发时的语法检查用，本机没有 _abroad）。
func userOK(u string) bool { return userRe.MatchString(u) || u == "nobody" }

// lanAllowed 是 <abroad_lan> 里的网段必须整个落入的范围：RFC1918、CGNAT、链路本地、组播。
// 直连网段是公网地址时（例如恶意 Wi-Fi 的 DHCP 把 Anthropic 的地址段发成直连网段）不能进表：
// 它比 0/1、128/1 更具体，到它的流量会离开 utun。
var lanAllowed = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/4"),
}

// dhcpDst 是 DHCP（udp 68 → 67）允许的目的地址：广播和私有范围（设计 6.1）。
var dhcpDst = []string{"255.255.255.255", "10.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16"}

// vmNets 是本机到自己的虚拟机允许的目的网段。
var vmNets = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}

// lanOK 报告 p 是否是整个落在 lanAllowed 某一项里的 IPv4 网段。
func lanOK(p netip.Prefix) bool {
	if !p.IsValid() || !p.Addr().Is4() {
		return false
	}
	p = p.Masked()
	for _, a := range lanAllowed {
		if a.Bits() <= p.Bits() && a.Contains(p.Addr()) {
			return true
		}
	}
	return false
}

// LANPrefixes 由当前网卡的直连网段算出 <abroad_lan>：丢掉不在私有范围里的，加上组播 224/4 和链路本地 169.254/16。
// 结果已排序、去重。
func LANPrefixes(direct []netip.Prefix) []netip.Prefix {
	out := []netip.Prefix{netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("169.254.0.0/16")}
	for _, p := range direct {
		if lanOK(p) {
			out = append(out, p.Masked())
		}
	}
	return sortPrefixes(out)
}

func sortPrefixes(in []netip.Prefix) []netip.Prefix {
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b netip.Prefix) int {
		if c := a.Addr().Compare(b.Addr()); c != 0 {
			return c
		}
		return a.Bits() - b.Bits()
	})
	return slices.Compact(out)
}

// Render 生成规则文本。输入不合法时返回错误，输出为空。
func Render(c Config) ([]byte, error) {
	if c.UtunName != "" && !utunRe.MatchString(c.UtunName) {
		return nil, errors.New("utun 名称不合法")
	}
	if !userOK(c.User) {
		return nil, errors.New("放行用户名不合法")
	}
	var lan []netip.Prefix
	for _, p := range c.LAN {
		if !lanOK(p) {
			return nil, fmt.Errorf("局域网段 %s 不在允许的私有范围里", p)
		}
		lan = append(lan, p.Masked())
	}
	lan = sortPrefixes(lan)
	vmifs := slices.Clone(c.VMIfs)
	for _, v := range vmifs {
		if !ValidVMIf(v) {
			return nil, errors.New("虚拟机网桥接口名不合法")
		}
	}
	slices.Sort(vmifs)
	vmifs = slices.Compact(vmifs)

	var b bytes.Buffer
	line := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	if len(lan) > 0 {
		parts := make([]string, len(lan))
		for i, p := range lan {
			parts[i] = p.String()
		}
		line("table <abroad_lan> { %s }", strings.Join(parts, ", "))
	}
	// lo0 排在 inet6 拦截之前：::1 上的本地服务要能连（lo0 上的包不出本机）。
	// lo0 和我们的 utun 是无状态的：有状态规则默认 flags S/SA，只有首个 SYN 能建状态，启用 pf 时已有的
	// 本机回环长连接会被最后一条丢掉（设计 6.1）。这两行不涉及物理网卡。
	line("pass out quick on lo0 all no state")
	line("block drop out quick inet6 all")
	if c.UtunName != "" {
		line("pass out quick on %s all no state", c.UtunName)
	}
	for _, d := range dhcpDst {
		line("pass out quick inet proto udp from any port 68 to %s port 67", d)
	}
	line("pass out quick inet proto tcp all user %s", c.User)
	line("pass out quick inet proto udp all user %s", c.User)
	if len(lan) > 0 {
		line("pass out quick inet to <abroad_lan>")
	}
	for _, v := range vmifs {
		for _, n := range vmNets {
			line("pass out quick on %s inet to %s", v, n)
		}
	}
	line("block drop out quick all")
	return b.Bytes(), nil
}

// Digest 是规则文本的 sha256（十六进制），用于比对。
func Digest(rules []byte) string {
	h := sha256.Sum256(rules)
	return hex.EncodeToString(h[:])
}
