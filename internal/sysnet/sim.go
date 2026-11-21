package sysnet

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// SimRunner 是命令模拟器（M2-9b-1 决定 1）：实现 Runner，底下是 Fake。收到 route、netstat、scutil、networksetup、
// ifconfig 的命令后，按真实命令的输出格式作答，并把改动落到 Fake 上（改动经 Fake 的方法，所以 FailNext、OnChange 照常生效）。
// 目的是让 MacRoutes、MacDNS、MacIPv6 这些真实实现和状态机、监护一起跑起来。
//
// 局限：输出格式以 testdata 的样本为准，其中一部分（route get 经物理网卡、not in table，IPv6 的 Off、Manual、LinkLocal，
// scutil 键存在时的 show）是构造样本；真机上是否一致由 M2-9c 的用户脚本验证。
// 报错文字只用真实实现识别的那几句：File exists、not in table、No such key、is not a recognized network service。
// 不认识的命令返回错误并记进 Unknown，测试据此断言“没有发出计划外的命令”。
type SimRunner struct {
	F *Fake

	mu      sync.Mutex
	calls   []string            // 每条命令（路径 + 参数 + stdin 首行）
	unknown []string            // 不认识的命令
	ifcfg   map[string][]string // utun 名 → 最近一次 ifconfig 的参数
}

// NewSimRunner 建一个底下是 f 的模拟器。
func NewSimRunner(f *Fake) *SimRunner { return &SimRunner{F: f, ifcfg: map[string][]string{}} }

// ErrSimUnknown 是模拟器不认识的命令。
var ErrSimUnknown = errors.New("模拟器不认识的命令")

var errExit1 = errors.New("exit status 1")

// Calls 返回收到的命令。
func (s *SimRunner) Calls() []string { s.mu.Lock(); defer s.mu.Unlock(); return slices.Clone(s.calls) }

// Unknown 返回不认识的命令。
func (s *SimRunner) Unknown() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.unknown)
}

// Ifconfig 返回某个 utun 最近一次 ifconfig 的参数。
func (s *SimRunner) Ifconfig(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ifcfg[name])
}

// Run 实现 Runner。
func (s *SimRunner) Run(ctx context.Context, path string, args []string, stdin []byte) ([]byte, []byte, error) {
	desc := path + " " + strings.Join(args, " ")
	if stdin != nil {
		desc += " <<" + strings.ReplaceAll(strings.TrimSpace(string(stdin)), "\n", "; ")
	}
	s.mu.Lock()
	s.calls = append(s.calls, desc)
	s.mu.Unlock()
	so, se, err := s.dispatch(ctx, path, args, stdin)
	if errors.Is(err, ErrSimUnknown) {
		s.mu.Lock()
		s.unknown = append(s.unknown, desc)
		s.mu.Unlock()
	}
	return so, se, err
}

func (s *SimRunner) dispatch(ctx context.Context, path string, args []string, stdin []byte) ([]byte, []byte, error) {
	switch path {
	case PathRoute:
		return s.route(ctx, args, stdin)
	case PathPs:
		if slices.Equal(args, []string{"-axo", "comm"}) && stdin == nil {
			return []byte(simPs), nil, nil
		}
	case PathNetstat:
		if slices.Equal(args, []string{"-rn", "-f", "inet"}) && stdin == nil {
			return s.netstat(ctx)
		}
	case PathScutil:
		if len(args) == 0 && stdin != nil {
			return s.scutil(ctx, string(stdin))
		}
		if stdin == nil && slices.Equal(args, []string{"--proxy"}) {
			return []byte(simProxyOff), nil, nil
		}
		if stdin == nil && slices.Equal(args, []string{"--nwi"}) {
			return []byte(simNwi), nil, nil
		}
		if stdin == nil && slices.Equal(args, []string{"--dns"}) {
			var b strings.Builder
			b.WriteString("DNS configuration\n\nresolver #1\n")
			for i, a := range s.F.Snapshot().SysDNS {
				fmt.Fprintf(&b, "  nameserver[%d] : %s\n", i, a)
			}
			b.WriteString("  flags    : Request A records\n  reach    : 0x00000002 (Reachable)\n\nDNS configuration (for scoped queries)\n\n")
			return []byte(b.String()), nil, nil
		}
	case PathNetworksetup:
		if stdin == nil {
			return s.networksetup(ctx, args)
		}
	case PathSystemsetup:
		if stdin == nil {
			return s.systemsetup(ctx, args)
		}
	case PathDefaults:
		if stdin == nil {
			return s.defaults(ctx, args)
		}
	case PathLaunchctl:
		if stdin == nil {
			return s.launchctl(ctx, args)
		}
	case PathIfconfig:
		if len(args) == 7 && ValidUtun(args[0]) == nil && args[1] == "inet" && args[4] == "mtu" && args[6] == "up" {
			s.mu.Lock()
			s.ifcfg[args[0]] = slices.Clone(args)
			s.mu.Unlock()
			return nil, nil, nil
		}
	}
	return nil, nil, fmt.Errorf("%w：%s %v", ErrSimUnknown, path, args)
}

func (s *SimRunner) route(ctx context.Context, args []string, stdin []byte) ([]byte, []byte, error) {
	if stdin != nil || len(args) < 3 || args[0] != "-n" {
		return nil, nil, fmt.Errorf("%w：route %v", ErrSimUnknown, args)
	}
	r := s.F.Routes()
	switch {
	case args[1] == "get" && len(args) == 3 && args[2] == "default":
		def := s.F.Snapshot().Default
		if def == "" {
			return nil, []byte("route: writing to routing socket: not in table\n"), errExit1
		}
		return []byte("   route to: default\ndestination: default\n       mask: default\n    gateway: 192.0.2.1\n  interface: " + def + "\n      flags: <UP,GATEWAY,DONE,STATIC,PRCLONING,GLOBAL>\n recvpipe  sendpipe  ssthresh  rtt,msec    rttvar  hopcount      mtu     expire\n       0         0         0         0         0         0      1500         0 \n"), nil, nil
	case args[1] == "get" && len(args) == 3:
		a, err := netip.ParseAddr(args[2])
		if err != nil {
			return nil, nil, fmt.Errorf("%w：route get %s", ErrSimUnknown, args[2])
		}
		via, err := r.Via(ctx, a)
		if errors.Is(err, ErrNotFound) {
			return nil, []byte("route: writing to routing socket: not in table\n"), errExit1
		}
		if err != nil {
			return nil, []byte("route: " + err.Error() + "\n"), errExit1
		}
		return []byte(fmt.Sprintf("   route to: %s\ndestination: default\n       mask: default\n    gateway: 192.0.2.1\n  interface: %s\n      flags: <UP,GATEWAY,DONE,STATIC,PRCLONING>\n recvpipe  sendpipe  ssthresh  rtt,msec    rttvar  hopcount      mtu     expire\n       0         0         0         0         0         0      1500         0 \n", a, via)), nil, nil
	case (args[1] == "add" || args[1] == "change") && len(args) == 6 && args[2] == "-net" && args[4] == "-interface":
		dst, err := netip.ParsePrefix(args[3])
		if err != nil {
			return nil, nil, fmt.Errorf("%w：route %v", ErrSimUnknown, args)
		}
		_, exists := s.F.Snapshot().Routes[dst.Masked()]
		if args[1] == "add" && exists {
			return nil, []byte("route: writing to routing socket: File exists\nadd net " + args[3] + ": gateway " + args[5] + ": File exists\n"), errExit1
		}
		if args[1] == "change" && !exists {
			return nil, []byte("route: writing to routing socket: not in table\n"), errExit1
		}
		if err := r.Add(ctx, dst, args[5]); err != nil && !errors.Is(err, ErrExists) {
			return nil, []byte("route: " + err.Error() + "\n"), errExit1
		}
		return []byte(args[1] + " net " + args[3] + ": gateway " + args[5] + "\n"), nil, nil
	case args[1] == "delete" && len(args) == 4 && args[2] == "-net":
		dst, err := netip.ParsePrefix(args[3])
		if err != nil {
			return nil, nil, fmt.Errorf("%w：route %v", ErrSimUnknown, args)
		}
		if err := r.Delete(ctx, dst); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, []byte("route: writing to routing socket: not in table\n"), errExit1
			}
			return nil, []byte("route: " + err.Error() + "\n"), errExit1
		}
		return []byte("delete net " + args[3] + "\n"), nil, nil
	}
	return nil, nil, fmt.Errorf("%w：route %v", ErrSimUnknown, args)
}

func (s *SimRunner) netstat(ctx context.Context) ([]byte, []byte, error) {
	// 经 Fake 的 Conflicts 走一遍（让 FailNext(OpRoutesConf) 生效、记调用），表本身按快照生成。
	if _, err := s.F.Routes().Conflicts(ctx, ""); err != nil {
		return nil, []byte("netstat: " + err.Error() + "\n"), errExit1
	}
	st := s.F.Snapshot()
	var b strings.Builder
	b.WriteString("Routing tables\n\nInternet:\nDestination        Gateway            Flags               Netif Expire\n")
	if st.Default != "" {
		fmt.Fprintf(&b, "%-18s %-18s %-19s %9s\n", "default", "192.0.2.1", "UGScg", st.Default)
	}
	dsts := slices.Collect(maps.Keys(st.Routes))
	slices.SortFunc(dsts, func(a, c netip.Prefix) int { return a.Addr().Compare(c.Addr()) })
	for _, d := range dsts {
		fmt.Fprintf(&b, "%-18s %-18s %-19s %9s\n", d.String(), "198.18.0.1", "UGSc", st.Routes[d])
	}
	return []byte(b.String()), nil, nil
}

func (s *SimRunner) scutil(ctx context.Context, script string) ([]byte, []byte, error) {
	var lines []string
	for _, l := range strings.Split(script, "\n") {
		if l = strings.TrimSpace(l); l != "" && l != "quit" {
			lines = append(lines, l)
		}
	}
	d := s.F.DNS()
	switch {
	case len(lines) == 1 && lines[0] == "show "+DNSKey:
		ok, err := d.Present(ctx)
		if err != nil {
			return nil, []byte(err.Error() + "\n"), errExit1
		}
		if !ok {
			return []byte("  No such key\n"), nil, nil
		}
		return []byte("<dictionary> {\n  ServerAddresses : <array> {\n    0 : " + s.F.Snapshot().DNS.String() + "\n  }\n  SupplementalMatchDomains : <array> {\n    0 : \n  }\n}\n"), nil, nil
	case len(lines) == 1 && lines[0] == "remove "+DNSKey:
		if err := d.Remove(ctx); err != nil && !errors.Is(err, ErrNotFound) {
			return nil, []byte(err.Error() + "\n"), errExit1
		}
		return nil, nil, nil
	case len(lines) == 4 && lines[0] == "d.init" && strings.HasPrefix(lines[1], "d.add ServerAddresses * ") &&
		lines[2] == `d.add SupplementalMatchDomains * ""` && lines[3] == "set "+DNSKey:
		a, err := netip.ParseAddr(strings.TrimPrefix(lines[1], "d.add ServerAddresses * "))
		if err != nil {
			return nil, nil, fmt.Errorf("%w：scutil %q", ErrSimUnknown, script)
		}
		if err := d.Set(ctx, a); err != nil && !errors.Is(err, ErrExists) {
			return nil, []byte(err.Error() + "\n"), errExit1
		}
		return nil, nil, nil
	}
	return nil, nil, fmt.Errorf("%w：scutil %q", ErrSimUnknown, script)
}

func notRecognized(svc string) []byte {
	return []byte(svc + " is not a recognized network service.\n** Error: The parameters were not valid.\n")
}

func (s *SimRunner) networksetup(ctx context.Context, args []string) ([]byte, []byte, error) {
	v := s.F.IPv6()
	if slices.Equal(args, []string{"-listallnetworkservices"}) {
		snap, err := v.Snapshot(ctx)
		if err != nil {
			return nil, []byte(err.Error() + "\n"), errExit1
		}
		var b strings.Builder
		b.WriteString("An asterisk (*) denotes that a network service is disabled.\n")
		for _, k := range slices.Sorted(maps.Keys(snap)) {
			b.WriteString(k + "\n")
		}
		return []byte(b.String()), nil, nil
	}
	if len(args) < 2 {
		return nil, nil, fmt.Errorf("%w：networksetup %v", ErrSimUnknown, args)
	}
	svc := args[1]
	set := func(st IPv6Setting) ([]byte, []byte, error) {
		var err error
		if st.Mode == IPv6Off {
			err = v.Off(ctx, svc)
		} else {
			err = v.Restore(ctx, svc, st)
		}
		if errors.Is(err, ErrNotFound) {
			return notRecognized(svc), nil, nil // networksetup 出错时退出码 0、错误打在 stdout
		}
		if err != nil {
			return []byte("** Error: " + err.Error() + "\n"), nil, nil
		}
		return nil, nil, nil
	}
	switch {
	case args[0] == "-getinfo" && len(args) == 2:
		st, ok := s.F.Snapshot().IPv6[svc]
		if !ok {
			return notRecognized(svc), nil, nil
		}
		return []byte(getinfoText(st)), nil, nil
	case args[0] == "-setv6off" && len(args) == 2:
		return set(IPv6Setting{Mode: IPv6Off})
	case args[0] == "-setv6automatic" && len(args) == 2:
		return set(IPv6Setting{Mode: IPv6Automatic})
	case args[0] == "-setv6LinkLocal" && len(args) == 2:
		return set(IPv6Setting{Mode: IPv6LinkLocal})
	case args[0] == "-setv6manual" && len(args) == 5:
		a, err1 := netip.ParseAddr(args[2])
		n, err2 := strconv.Atoi(args[3])
		r, err3 := netip.ParseAddr(args[4])
		if err1 != nil || err2 != nil || err3 != nil {
			return []byte("** Error: The parameters were not valid.\n"), nil, nil
		}
		return set(IPv6Setting{Mode: IPv6Manual, Address: a, PrefixLen: n, Router: r})
	}
	return nil, nil, fmt.Errorf("%w：networksetup %v", ErrSimUnknown, args)
}

// getinfoText 按 -getinfo 的格式（testdata 的样本）打印 IPv6 部分。unknown 打成一个不认识的模式。
func getinfoText(st IPv6Setting) string {
	head := "DHCP Configuration\nClient ID: \n"
	switch st.Mode {
	case IPv6Automatic:
		return head + "IPv6: Automatic\nIPv6 IP address: none\nIPv6 Router: none\n"
	case IPv6Off:
		return head + "IPv6: Off\n"
	case IPv6LinkLocal:
		return head + "IPv6: Link-local only\nIPv6 IP address: none\nIPv6 Router: none\n"
	case IPv6Manual:
		router := "none"
		if st.Router.IsValid() {
			router = st.Router.String()
		}
		return fmt.Sprintf("%sIPv6: Manual\nIPv6 IP address: %s\nIPv6 Prefix Length: %d\nIPv6 Router: %s\n", head, st.Address, st.PrefixLen, router)
	}
	return head + "IPv6: Teleport\n"
}

// 冲突检查的只读命令用 preflight/testdata 里“没有冲突”的样本作答（M2-9b-2 第 0 步）：
// 系统代理全关、只有 IPv4（en0）、没有代理软件在运行。
const simProxyOff = `<dictionary> {
  HTTPEnable : 0
  HTTPSEnable : 0
  ProxyAutoConfigEnable : 0
  SOCKSEnable : 0
}
`

const simNwi = `Network information

IPv4 network interface information
     en0 : flags      : 0x5 (IPv4,DNS)
           address    : 192.0.2.10
           reach      : 0x00000002 (Reachable)

   REACH : flags 0x00000002 (Reachable)

IPv6 network interface information
   No IPv6 states found


   REACH : flags 0x00000000 (Not Reachable)

Network interfaces: en0
`

const simPs = `COMM
/sbin/launchd
/usr/libexec/logd
/usr/libexec/configd
`
