package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ruleConfig 是生成 pf 规则所需的输入。规则按设计文档 6.1 的顺序排列，
// 有意保留"block inet6 在 lo0 放行之前"，用来验证 ::1 回环是否会被拦。
type ruleConfig struct {
	User  string   // 放行去公网的用户
	LAN   []string // 允许局域网的网段；空表示关闭"允许局域网"
	VMIfs []string // 本机虚拟机网桥接口
}

var pfName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func buildRules(cfg ruleConfig) (string, error) {
	if !pfName.MatchString(cfg.User) {
		return "", fmt.Errorf("用户名不合法: %q", cfg.User)
	}
	for _, c := range cfg.LAN {
		if _, _, err := net.ParseCIDR(c); err != nil {
			return "", fmt.Errorf("网段不合法: %q", c)
		}
	}
	for _, i := range cfg.VMIfs {
		if !pfName.MatchString(i) {
			return "", fmt.Errorf("接口名不合法: %q", i)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "table <abroad_lan> { %s }\n", strings.Join(cfg.LAN, ", "))
	b.WriteString(`block drop out quick inet6 all label "m0_block_inet6"` + "\n")
	b.WriteString(`pass out quick on lo0 all label "m0_lo0"` + "\n")
	b.WriteString(`pass out quick proto udp from any port 68 to any port 67 label "m0_dhcp"` + "\n")
	fmt.Fprintf(&b, `pass out quick proto { tcp udp } user %s label "m0_user"`+"\n", cfg.User)
	b.WriteString(`pass out quick to <abroad_lan> label "m0_lan"` + "\n")
	if len(cfg.VMIfs) > 0 {
		fmt.Fprintf(&b, `pass out quick on { %s } to { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 } label "m0_vmif"`+"\n",
			strings.Join(cfg.VMIfs, " "))
	}
	b.WriteString(`block drop out quick all label "m0_block_all"` + "\n")
	return b.String(), nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func cmdRules(args []string) error {
	fs := flag.NewFlagSet("rules", flag.ExitOnError)
	user := fs.String("user", "", "放行的用户")
	lan := fs.String("lan", "", "局域网段，逗号分隔；空表示关闭允许局域网")
	vmif := fs.String("vmif", "", "虚拟机网桥接口，逗号分隔")
	_ = fs.Parse(args)
	s, err := buildRules(ruleConfig{User: *user, LAN: splitList(*lan), VMIfs: splitList(*vmif)})
	if err != nil {
		return err
	}
	fmt.Print(s)
	return nil
}

// lanCIDR 返回地址列表里第一个 IPv4 地址所在的网段（不含回环和链路本地）。
func lanCIDR(addrs []net.Addr) (string, error) {
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip4 := ipn.IP.To4()
		if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
			continue
		}
		n := &net.IPNet{IP: ip4.Mask(ipn.Mask), Mask: ipn.Mask}
		return n.String(), nil
	}
	return "", errors.New("没有可用的 IPv4 地址")
}

func cmdLAN(args []string) error {
	fs := flag.NewFlagSet("lan", flag.ExitOnError)
	iface := fs.String("iface", "", "网卡名")
	_ = fs.Parse(args)
	ifi, err := net.InterfaceByName(*iface)
	if err != nil {
		return err
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return err
	}
	c, err := lanCIDR(addrs)
	if err != nil {
		return err
	}
	fmt.Println(c)
	return nil
}

var vmIfPattern = regexp.MustCompile(`^(bridge[1-9][0-9]{2,}|vmenet[0-9]+)$`)

// filterVMIfs 选出本机虚拟机用的网桥接口。bridge0 通常是雷雳网桥（物理接口），不算。
func filterVMIfs(names []string) []string {
	var out []string
	for _, n := range names {
		if vmIfPattern.MatchString(n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

func cmdVMIfs([]string) error {
	ifs, err := net.Interfaces()
	if err != nil {
		return err
	}
	names := make([]string, 0, len(ifs))
	for _, i := range ifs {
		names = append(names, i.Name)
	}
	fmt.Println(strings.Join(filterVMIfs(names), ","))
	return nil
}

// labelCounters 是 `pfctl -sl` 一行里的计数：评估次数、包数、状态数。
// 同名标签（例如 `proto { tcp udp }` 展开成的两条规则）会相加。
type labelCounters struct {
	Evals, Packets, States uint64
}

func parseLabels(r io.Reader) (map[string]labelCounters, error) {
	out := map[string]labelCounters{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 {
			continue
		}
		nums := make([]uint64, 0, len(f)-1)
		for _, s := range f[1:] {
			n, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("无法解析 pfctl -sl 行 %q", sc.Text())
			}
			nums = append(nums, n)
		}
		c := out[f[0]]
		c.Evals += nums[0]
		c.Packets += nums[1]
		c.States += nums[len(nums)-1]
		out[f[0]] = c
	}
	return out, sc.Err()
}

// diffLabels 输出包数有变化的标签，形如 "m0_user:+3"，按标签名排序；没有变化时输出 "-"。
func diffLabels(before, after map[string]labelCounters) string {
	var parts []string
	for name, a := range after {
		d := int64(a.Packets) - int64(before[name].Packets)
		if d != 0 {
			parts = append(parts, fmt.Sprintf("%s:%+d", name, d))
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func cmdLabelDiff(args []string) error {
	if len(args) != 2 {
		return errors.New("用法：labeldiff before.txt after.txt")
	}
	read := func(p string) (map[string]labelCounters, error) {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return parseLabels(f)
	}
	b, err := read(args[0])
	if err != nil {
		return err
	}
	a, err := read(args[1])
	if err != nil {
		return err
	}
	fmt.Println(diffLabels(b, a))
	return nil
}
