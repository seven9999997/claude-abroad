package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/netip"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"claude-abroad/internal/exitconf"
	"claude-abroad/internal/guestabi"
)

// 分流出口（m4-plan M4-7 v2.8）：PUT /v1/exits 替换全部分流出口（幂等）。
//   - WireGuard、OpenVPN：每个出口一个命名空间（veth 加根命名空间的 NAT），命名空间里的断网规则只放行
//     回环、已建立的连接、隧道接口和服务器端点（53 端口也在内）；命名空间里运行 `abroad-agent socks`，
//     sing-box 加一个 socks 出站指过去；
//   - 订阅 mihomo：每个出口一个 mihomo 进程，监听虚拟机回环上自己的端口；配置按主代理的形状重新生成；
//   - 凭据只落在 /run/claude-abroad/exits/<出口>/（tmpfs，0700，文件 0600），出口删除或改动时整个目录删掉；
//   - 请求体、配置内容、密钥不进任何日志，不出现在 health 里；health 只给出口名、种类、可用与否和原因。

// ExitsDir 是分流出口的凭据和配置目录（tmpfs）。
const ExitsDir = guestabi.RunDir + "/exits"

// OVPNDNSScript 是 agent 自带的固定脚本：只把 OpenVPN 推送的 DNS 写到出口自己的目录（决定 3）。
const OVPNDNSScript = guestabi.RunDir + "/libexec/ovpn-dns.sh"

// ovpnDNSScriptBody 是脚本内容（随 agent 进镜像）。foreign_option_N 的形如 “dhcp-option DNS 10.8.0.1”。
const ovpnDNSScriptBody = `#!/bin/sh
# 由 abroad-agent 写入（design 7.3 v0.14）：只把 OpenVPN 推送的 DNS 原子写到出口自己的目录，不执行别的命令。
# 两种来源：旧式 foreign_option_N="dhcp-option DNS x"，2.6 的 dns_server_N_address_M。这里只留数字和点，
# 是否可用（回环、组播、fake 段等）由读文件的 socks 服务校验，最多用 2 个。
[ -n "$ABROAD_EXIT_DIR" ] || exit 0
umask 077
out="$ABROAD_EXIT_DIR/dns.tmp"
: > "$out"
keep() { case "$1" in *[!0-9.]*|"") ;; *) echo "$1" >> "$out" ;; esac; }
i=1
while [ "$i" -le 64 ]; do
  eval "opt=\${foreign_option_$i}"
  [ -n "$opt" ] || break
  case "$opt" in "dhcp-option DNS "*) keep "${opt#dhcp-option DNS }" ;; esac
  i=$((i+1))
done
n=0
while [ "$n" -le 7 ]; do
  m=1
  while [ "$m" -le 8 ]; do
    eval "v=\${dns_server_${n}_address_${m}}"
    [ -n "$v" ] || break
    keep "$v"
    m=$((m+1))
  done
  n=$((n+1))
done
mv "$out" "$ABROAD_EXIT_DIR/dns"
exit 0
`

// 每个出口的地址：10.200.<n>.1/30（根命名空间）↔ 10.200.<n>.2/30（出口命名空间）；socks 在 .2:1080。
const (
	exitSocksPort   = 1080
	exitClashPortLo = 7800 // 订阅 mihomo 的端口：7800 + n
	maxExits        = 16
)

// ExitNet 是分流出口 veth 的网段（sing-box 的 socks 出站只能指向这里或回环）。
var ExitNet = netip.MustParsePrefix("10.200.0.0/16")

// WGHandshakeMax 是 WireGuard 最近一次握手的最长间隔：超过算不可用（决定 6）。
const WGHandshakeMax = 3 * time.Minute

// ExitSpec 是一个分流出口的下发内容。Conf 是用户的配置原文（只在内存和该出口的目录里）。
type ExitSpec struct {
	Tag  string        `json:"tag"`
	Kind exitconf.Kind `json:"kind"`
	Conf string        `json:"conf"`
}

// ExitStatus 是一个出口的状态（health 用；不含端点地址、不含配置）。
type ExitStatus struct {
	Tag    string        `json:"tag"`
	Kind   exitconf.Kind `json:"kind"`
	Up     bool          `json:"up"`
	Reason string        `json:"reason,omitempty"`
}

// ExitOps 是分流出口要的系统操作（Linux 实现见 exits_linux.go；测试用假的）。
type ExitOps interface {
	Run(ctx context.Context, argv ...string) (string, error)
	MkdirPrivate(path string) error
	WriteFile(path string, data []byte, perm fs.FileMode) error
	RemoveAll(path string) error
	// ResolveAll 在根命名空间里经网关解析服务器的域名（就是网关的代解析：网关答的同时把结果记进宿主的放行名单）。
	ResolveAll(ctx context.Context, host string) ([]netip.Addr, error)
	// LinkUp 报告命名空间里的设备在不在且是 up。
	LinkUp(ns, dev string) bool
	// WGHandshake 返回 WireGuard 接口最近一次握手的时间（零值表示还没有握手）。
	WGHandshake(ctx context.Context, ns, dev string) (time.Time, error)
}

// Exits 管理全部分流出口。
type Exits struct {
	Ops       ExitOps
	Runner    ProcRunner
	DialReady func(ctx context.Context, addr string) error
	Timing    EngineTiming
	Base      context.Context
	SelfExe   string // abroad-agent 自己（命名空间里的 socks 用它）
	// OnChange 在出口集合变了之后调用：用 main + 这些出口重写 sing-box 配置并重启（nil 时不调用）。
	OnChange func(list []SingBoxExit) error
	Now      func() time.Time
	Logf     func(format string, a ...any)
	// LoadModules 在第一次建命名空间前加载隧道要的内核模块（失败只记日志：有的可能编进了内核，真缺时后面的命令会报错）。
	LoadModules func(names []string) error
	// DevUplink 是开发用对端的 veth（根命名空间一侧），只在开发标记打开时设：转发规则多放行到它（不做 NAT）。
	DevUplink string
	// AllowLoopback：服务器地址允许回环（只给开发标记）。
	AllowLoopback bool
	// ResolveEvery 是重新解析服务器域名的间隔（默认 5 分钟，m4-plan v2.12 决定 1）；After 是定时器（测试注入）。
	ResolveEvery time.Duration
	After        func(time.Duration) <-chan time.Time

	mu        sync.Mutex
	cur       map[string]*exitRT
	rootReady bool

	skew atomic.Int64 // 开发用：状态判断用的时钟往后拨（验证 WireGuard 握手超时）

	// 重新解析（x.mu 保护 failed）：建不起来的出口留着，定时重试；refresh 叫醒重新解析的循环。
	failed      map[string]failedExit
	refresh     chan struct{}
	refreshOnce sync.Once
	rebuilds    atomic.Int64 // 因为解析结果变了而重建的次数（测试和诊断用）

	stMu    sync.Mutex
	stCache map[string]statusEntry
}

type statusEntry struct {
	at time.Time
	st ExitStatus
}

// failedExit 是建不起来的出口（原因不含配置内容和地址）。
type failedExit struct {
	spec ExitSpec
	p    parsed
	why  string
}

// DefaultResolveEvery 是重新解析服务器域名的间隔。
const DefaultResolveEvery = 5 * time.Minute

type exitRT struct {
	resolved map[string]string // 服务器域名 → 这次用的地址（建起来时的解析结果）
	spec     ExitSpec
	hash     string
	n        int
	ns       string
	vethA    string
	dev      string
	dir      string
	sb       SingBoxExit
	engines  []*Engine
	cancel   context.CancelFunc
}

func (x *Exits) now() time.Time {
	t := time.Now()
	if x.Now != nil {
		t = x.Now()
	}
	return t.Add(time.Duration(x.skew.Load()))
}

func (x *Exits) logf(format string, a ...any) {
	if x.Logf != nil {
		x.Logf(format, a...)
	}
}

// parsed 是校验过的配置（三种之一）。
type parsed struct {
	wg    *exitconf.WG
	ovpn  *exitconf.OVPN
	clash *exitconf.ClashConf
}

func (p parsed) endpoints() []exitconf.Endpoint {
	switch {
	case p.wg != nil:
		return p.wg.Endpoints()
	case p.ovpn != nil:
		return p.ovpn.Endpoints()
	case p.clash != nil:
		return p.clash.Endpoints()
	}
	return nil
}

// Validate 校验一组下发内容（不改任何东西）：标签合法、不重复、不是 main，配置按 exitconf 的白名单校验。
func Validate(specs []ExitSpec) (map[string]parsed, error) {
	if len(specs) > maxExits {
		return nil, fmt.Errorf("分流出口最多 %d 个", maxExits)
	}
	out := map[string]parsed{}
	for _, s := range specs {
		if !exitTagRe.MatchString(s.Tag) || s.Tag == guestabi.ExitMain {
			return nil, fmt.Errorf("出口标签不合法（小写字母开头，最多 32 位，不能叫 main）")
		}
		if _, dup := out[s.Tag]; dup {
			return nil, fmt.Errorf("出口 %s 重复", s.Tag)
		}
		var p parsed
		var err error
		switch s.Kind {
		case exitconf.WireGuard:
			var w exitconf.WG
			w, err = exitconf.ParseWG([]byte(s.Conf))
			p.wg = &w
		case exitconf.OpenVPN:
			var o exitconf.OVPN
			o, err = exitconf.ParseOVPN([]byte(s.Conf))
			p.ovpn = &o
		case exitconf.Clash:
			var c exitconf.ClashConf
			c, err = exitconf.ParseClash([]byte(s.Conf))
			p.clash = &c
		default:
			return nil, fmt.Errorf("出口 %s：种类只能是 wireguard、openvpn、clash", s.Tag)
		}
		if err != nil {
			return nil, fmt.Errorf("出口 %s 的配置：%w", s.Tag, err)
		}
		out[s.Tag] = p
	}
	return out, nil
}

func specHash(s ExitSpec) string {
	h := sha256.Sum256([]byte(string(s.Kind) + "\x00" + s.Conf))
	return hex.EncodeToString(h[:])
}

// Apply 替换全部分流出口（幂等）：同样的出口不动；改了的只重建它；没有了的拆掉。
// 先整体校验，校验不过什么都不改。某个出口建不起来时拆掉它（不留半成品目录），其余照常，返回错误。
func (x *Exits) Apply(ctx context.Context, specs []ExitSpec) error {
	confs, err := Validate(specs)
	if err != nil {
		return err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.cur == nil {
		x.cur = map[string]*exitRT{}
	}
	want := map[string]ExitSpec{}
	for _, s := range specs {
		want[s.Tag] = s
	}
	if x.failed == nil {
		x.failed = map[string]failedExit{}
	}
	for tag := range x.failed {
		if _, ok := want[tag]; !ok {
			delete(x.failed, tag)
		}
	}
	x.startRefresh()
	changed := false
	for tag, rt := range x.cur {
		s, ok := want[tag]
		if ok && specHash(s) == rt.hash {
			continue
		}
		x.teardown(rt)
		delete(x.cur, tag)
		changed = true
	}
	var errs []error
	tags := make([]string, 0, len(want))
	for t := range want {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	for _, tag := range tags {
		if _, ok := x.cur[tag]; ok {
			continue
		}
		rt, err := x.setup(ctx, want[tag], confs[tag])
		if err != nil {
			errs = append(errs, fmt.Errorf("出口 %s：%w", tag, err))
			x.failed[tag] = failedExit{spec: want[tag], p: confs[tag], why: err.Error()}
			continue
		}
		delete(x.failed, tag)
		x.cur[tag] = rt
		changed = true
	}
	if changed {
		x.stMu.Lock()
		x.stCache = nil
		x.stMu.Unlock()
		if x.OnChange != nil {
			if err := x.OnChange(x.singBoxExits()); err != nil {
				errs = append(errs, fmt.Errorf("更新 sing-box 的出口：%w", err))
			}
		}
	}
	return errors.Join(errs...)
}

func (x *Exits) singBoxExits() []SingBoxExit {
	var out []SingBoxExit
	for _, rt := range x.cur {
		out = append(out, rt.sb)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tag < out[j].Tag })
	return out
}

// SingBoxExits 返回当前的分流出口（sing-box 的出站）。
func (x *Exits) SingBoxExits() []SingBoxExit {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.singBoxExits()
}

func (x *Exits) freeIndex() int {
	used := map[int]bool{}
	for _, rt := range x.cur {
		used[rt.n] = true
	}
	for n := 1; n <= 250; n++ {
		if !used[n] {
			return n
		}
	}
	return 0
}

// sh 依次执行命令，第一条失败就停。
func (x *Exits) sh(ctx context.Context, cmds ...[]string) error {
	for _, c := range cmds {
		// 命令输出不进错误：可能原样带出配置里的值（密码、密钥），脱敏认不全。
		if _, err := x.Ops.Run(ctx, c...); err != nil {
			return fmt.Errorf("%s %s 失败：%v", path.Base(c[0]), strings.Join(c[1:min(len(c), 3)], " "), err)
		}
	}
	return nil
}

// resolveAll 在根命名空间里把端点的域名解析成 IPv4。
func (x *Exits) resolveAll(ctx context.Context, eps []exitconf.Endpoint) (map[string]string, []exitconf.Endpoint, error) {
	res := map[string]string{}
	var out []exitconf.Endpoint
	for _, e := range eps {
		if !e.IsIP() {
			if _, ok := res[e.Host]; !ok {
				addrs, err := x.Ops.ResolveAll(ctx, e.Host)
				if err != nil || len(addrs) == 0 {
					return nil, nil, errors.New("解析不出服务器的地址")
				}
				pick := ""
				for _, a := range addrs {
					if exitconf.ValidServerAddr(a, x.AllowLoopback) == nil {
						pick = a.Unmap().String()
						break
					}
				}
				if pick == "" {
					return nil, nil, errors.New("解析出的服务器地址都不能用（回环、组播、fake 段、Anthropic 地址段或 IPv6）")
				}
				res[e.Host] = pick
			}
			e.Host = res[e.Host]
		} else if a, err := netip.ParseAddr(e.Host); err != nil || exitconf.ValidServerAddr(a, x.AllowLoopback) != nil {
			return nil, nil, errors.New("服务器地址不能用（回环、组播、fake 段、Anthropic 地址段或 IPv6）")
		}
		out = append(out, e)
	}
	return res, out, nil
}

// nftRules 是出口命名空间的断网规则：只放行回环、发往本出口 veth 根命名空间一侧地址的包（socks 给 sing-box 的应答，
// 是本地投递，不会被转发）、隧道接口、服务器端点。
// 不按 conntrack 放行“已建立的连接”：走过隧道的旧连接在隧道断开、默认路由退回 veth 之后会从 veth 出去（M4-7 端到端发现）。
// 53 端口不例外：隧道断开时解析一并失败（v2 必须改 3）。
func nftRules(n int, dev string, eps []exitconf.Endpoint) string {
	var b strings.Builder
	b.WriteString("table inet abroad_exit {\n  chain output {\n    type filter hook output priority 0; policy drop;\n")
	b.WriteString("    oif \"lo\" accept\n")
	b.WriteString(fmt.Sprintf("    oifname \"vx%db\" ip daddr 10.200.%d.1 accept\n", n, n))
	b.WriteString("    oifname \"" + dev + "\" accept\n")
	seen := map[string]bool{}
	for _, e := range eps {
		r := fmt.Sprintf("    ip daddr %s %s dport %d counter accept\n", e.Host, e.Proto, e.Port)
		if !seen[r] {
			seen[r] = true
			b.WriteString(r)
		}
	}
	b.WriteString("    counter drop\n  }\n}\n") // 被拦下的包计数（排错和端到端验证用）
	return b.String()
}

func (x *Exits) setup(ctx context.Context, s ExitSpec, p parsed) (rt *exitRT, err error) {
	n := x.freeIndex()
	if n == 0 {
		return nil, errors.New("没有空闲的出口编号")
	}
	rt = &exitRT{spec: s, hash: specHash(s), n: n, dir: ExitsDir + "/" + s.Tag,
		ns: "ex-" + s.Tag, vethA: fmt.Sprintf("vx%da", n)}
	defer func() {
		if err != nil {
			x.teardown(rt) // 不留半成品目录、命名空间
			rt = nil
		}
	}()
	if err := x.Ops.MkdirPrivate(ExitsDir); err != nil {
		return rt, err
	}
	if err := x.Ops.MkdirPrivate(rt.dir); err != nil {
		return rt, err
	}
	ectx, cancel := context.WithCancel(x.Base)
	rt.cancel = cancel
	if p.clash != nil {
		return rt, x.setupClash(ctx, ectx, rt, p.clash)
	}
	resolved, eps, err := x.resolveAll(ctx, p.endpoints())
	if err != nil {
		return rt, err
	}
	rt.resolved = resolved
	if err := x.setupNetns(ctx, rt, eps); err != nil {
		return rt, err
	}
	switch {
	case p.wg != nil:
		rt.dev = fmt.Sprintf("wg%d", n)
		err = x.setupWG(ctx, rt, p.wg, resolved, eps)
	case p.ovpn != nil:
		rt.dev = fmt.Sprintf("tun%d", n)
		err = x.setupOVPN(ctx, ectx, rt, p.ovpn, resolved, eps)
	}
	if err != nil {
		return rt, err
	}
	sock := fmt.Sprintf("10.200.%d.2:%d", n, exitSocksPort)
	e := x.engine("socks-"+s.Tag, nil, []string{"/sbin/ip", "netns", "exec", rt.ns, x.SelfExe, "socks", "--listen", sock, "--dns-file", rt.dir + "/dns"}, sock, nil)
	rt.engines = append(rt.engines, e)
	e.Start(ectx)
	rt.sb = SingBoxExit{Tag: s.Tag, Port: exitSocksPort, Server: fmt.Sprintf("10.200.%d.2", n)}
	return rt, nil
}

func (x *Exits) engine(name string, check, run []string, ready string, dialReady func(context.Context, string) error) *Engine {
	if dialReady == nil {
		dialReady = x.DialReady
	}
	return &Engine{Name: name, Check: check, Run: run, Dir: "/", ReadyAddr: ready, Log: NewRingLog(RingLogBytes),
		Runner: x.Runner, Timing: x.Timing, DialReady: dialReady, Logf: x.Logf}
}

// exitModules 是命名空间出口要的模块（M4-1 探查验证过前四个；NAT 相关的视内核配置可能已编进内核）。
var exitModules = []string{"veth", "wireguard", "tun", "nf_tables", "nft_ct", "nft_chain_nat", "nft_masq"}

// rootNATRules 是根命名空间的转发和 NAT：只转发出口 veth → eth0 及其应答，源地址伪装成虚拟机自己的地址。
// 其余转发一律丢弃（打开 ip_forward 不能让 eth0 进来的包被转进出口命名空间）。
// 53 端口的转发单独计数（解析只经隧道：正常时这个计数不增长）。devUplink 非空时多放行到开发用对端（不做 NAT）。
func rootNATRules(devUplink string) string {
	out, devAccept := `"`+guestabi.NetIface+`"`, ""
	if devUplink != "" {
		out = `{ "` + guestabi.NetIface + `", "` + devUplink + `" }`
		devAccept = "    iifname \"vx*\" oifname \"" + devUplink + "\" ip saddr " + ExitNet.String() + " accept\n"
	}
	return "table ip abroad_exit_nat {\n" +
		"  chain forward {\n    type filter hook forward priority 0; policy drop;\n" +
		"    iifname \"vx*\" udp dport 53 counter\n" +
		"    iifname \"vx*\" oifname \"" + guestabi.NetIface + "\" ip saddr " + ExitNet.String() + " counter accept\n" +
		devAccept +
		"    iifname " + out + " oifname \"vx*\" ct state established,related accept\n  }\n" +
		"  chain postrouting {\n    type nat hook postrouting priority 100;\n" +
		"    oifname \"" + guestabi.NetIface + "\" ip saddr " + ExitNet.String() + " masquerade\n  }\n}\n"
}

// ensureRoot 第一次建命名空间前：加载模块、写根命名空间的转发和 NAT 规则、打开 ip_forward（调用方持有 x.mu）。
func (x *Exits) ensureRoot(ctx context.Context) error {
	if x.rootReady {
		return nil
	}
	if x.LoadModules != nil {
		if err := x.LoadModules(exitModules); err != nil {
			x.logf("出口：加载模块：%v", err)
		}
	}
	rules := ExitsDir + "/root-nat.conf"
	if err := x.Ops.WriteFile(rules, []byte(rootNATRules(x.DevUplink)), 0o600); err != nil {
		return err
	}
	// 先删再加，保证幂等（agent 重启后表可能还在）。
	_, _ = x.Ops.Run(ctx, "/usr/sbin/nft", "delete", "table", "ip", "abroad_exit_nat")
	if err := x.sh(ctx, []string{"/usr/sbin/nft", "-f", rules}); err != nil {
		return err
	}
	if err := x.Ops.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("打开 ip_forward：%w", err)
	}
	x.rootReady = true
	return nil
}

// setupNetns 建命名空间、veth、到服务器端点的路由（经 veth）和断网规则。
func (x *Exits) setupNetns(ctx context.Context, rt *exitRT, eps []exitconf.Endpoint) error {
	if err := x.ensureRoot(ctx); err != nil {
		return err
	}
	n := strconv.Itoa(rt.n)
	vb := "vx" + n + "b"
	cmds := [][]string{
		{"/sbin/ip", "netns", "add", rt.ns},
		{"/sbin/ip", "link", "add", rt.vethA, "type", "veth", "peer", "name", vb},
		{"/sbin/ip", "link", "set", vb, "netns", rt.ns},
		{"/sbin/ip", "addr", "add", "10.200." + n + ".1/30", "dev", rt.vethA},
		{"/sbin/ip", "link", "set", rt.vethA, "up"},
		{"/sbin/ip", "-n", rt.ns, "addr", "add", "10.200." + n + ".2/30", "dev", vb},
		{"/sbin/ip", "-n", rt.ns, "link", "set", "lo", "up"},
		{"/sbin/ip", "-n", rt.ns, "link", "set", vb, "up"},
	}
	seen := map[string]bool{}
	for _, e := range eps {
		if !seen[e.Host] {
			seen[e.Host] = true
			cmds = append(cmds, []string{"/sbin/ip", "-n", rt.ns, "route", "replace", e.Host + "/32", "via", "10.200." + n + ".1"})
		}
	}
	if err := x.sh(ctx, cmds...); err != nil {
		return err
	}
	dev := map[bool]string{true: "wg" + n, false: "tun" + n}[rt.spec.Kind == exitconf.WireGuard]
	rules := rt.dir + "/nft.conf"
	if err := x.Ops.WriteFile(rules, []byte(nftRules(rt.n, dev, eps)), 0o600); err != nil {
		return err
	}
	return x.sh(ctx, []string{"/sbin/ip", "netns", "exec", rt.ns, "/usr/sbin/nft", "-f", rules})
}

func (x *Exits) setupWG(ctx context.Context, rt *exitRT, w *exitconf.WG, resolved map[string]string, eps []exitconf.Endpoint) error {
	// 没有 PersistentKeepalive 时补 25 秒：状态按“3 分钟内有过握手”判定，没有流量时也要握手。
	for i := range w.Peers {
		if w.Peers[i].Keepalive == 0 {
			w.Peers[i].Keepalive = 25
		}
	}
	conf := rt.dir + "/wg.conf"
	if err := x.Ops.WriteFile(conf, []byte(w.SetConf(resolved)), 0o600); err != nil {
		return err
	}
	var dns []string
	for _, a := range w.DNS {
		dns = append(dns, a.String())
	}
	if err := x.Ops.WriteFile(rt.dir+"/dns", []byte(strings.Join(dns, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	cmds := [][]string{
		{"/sbin/ip", "-n", rt.ns, "link", "add", rt.dev, "type", "wireguard"},
		{"/sbin/ip", "netns", "exec", rt.ns, "/usr/bin/wg", "setconf", rt.dev, conf},
	}
	for _, a := range w.Address {
		cmds = append(cmds, []string{"/sbin/ip", "-n", rt.ns, "addr", "add", a.String(), "dev", rt.dev})
	}
	if w.MTU > 0 {
		cmds = append(cmds, []string{"/sbin/ip", "-n", rt.ns, "link", "set", rt.dev, "mtu", strconv.Itoa(w.MTU)})
	}
	cmds = append(cmds,
		[]string{"/sbin/ip", "-n", rt.ns, "link", "set", rt.dev, "up"},
		[]string{"/sbin/ip", "-n", rt.ns, "route", "replace", "default", "dev", rt.dev})
	return x.sh(ctx, cmds...)
}

func (x *Exits) setupOVPN(ctx, ectx context.Context, rt *exitRT, o *exitconf.OVPN, resolved map[string]string, eps []exitconf.Endpoint) error {
	if err := x.Ops.MkdirPrivate(path.Dir(OVPNDNSScript)); err != nil {
		return err
	}
	if err := x.Ops.WriteFile(OVPNDNSScript, []byte(ovpnDNSScriptBody), 0o700); err != nil {
		return err
	}
	conf := rt.dir + "/client.conf"
	gen := o.Generate(exitconf.GenParams{Dev: rt.dev, DNSScript: OVPNDNSScript, ExitDir: rt.dir, Resolved: resolved})
	if err := x.Ops.WriteFile(conf, []byte(gen), 0o600); err != nil {
		return err
	}
	ns, dev := rt.ns, rt.dev
	// 就绪 = 隧道设备在且是 up；就绪时把默认路由设到隧道上（route-nopull 之后由我们设，只在这个命名空间里）。
	ready := func(ctx context.Context, _ string) error {
		if !x.Ops.LinkUp(ns, dev) {
			return errors.New("隧道设备还没起来")
		}
		_, err := x.Ops.Run(ctx, "/sbin/ip", "-n", ns, "route", "replace", "default", "dev", dev)
		return err
	}
	e := x.engine("openvpn-"+rt.spec.Tag, nil, []string{"/sbin/ip", "netns", "exec", ns, "/usr/sbin/openvpn", "--config", conf}, "tunnel", ready)
	rt.engines = append(rt.engines, e)
	e.Start(ectx)
	// openvpn 在进程内重连会重建隧道设备，设备上的默认路由随之消失：隔一会儿补一次（幂等）。
	// 设备不在时不动路由：没有到别处的路由，断网规则也只放行端点。
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ectx.Done():
				return
			case <-t.C:
				if x.Ops.LinkUp(ns, dev) {
					_, _ = x.Ops.Run(ectx, "/sbin/ip", "-n", ns, "route", "replace", "default", "dev", dev)
				}
			}
		}
	}()
	return nil
}

func (x *Exits) setupClash(ctx, ectx context.Context, rt *exitRT, c *exitconf.ClashConf) error {
	port := exitClashPortLo + rt.n
	b, err := c.Generate(port)
	if err != nil {
		return err
	}
	conf := rt.dir + "/config.yaml"
	if err := x.Ops.WriteFile(conf, b, 0o600); err != nil {
		return err
	}
	ready := fmt.Sprintf("127.0.0.1:%d", port)
	e := x.engine("mihomo-"+rt.spec.Tag, []string{guestabi.MihomoPath, "-t", "-d", rt.dir, "-f", conf},
		[]string{guestabi.MihomoPath, "-d", rt.dir, "-f", conf}, ready, nil)
	rt.engines = append(rt.engines, e)
	e.Start(ectx)
	rt.sb = SingBoxExit{Tag: rt.spec.Tag, Port: port}
	return nil
}

// teardown 停引擎、删命名空间（连同里面的隧道设备和 veth 的一头）、删 veth、删凭据目录。尽力而为。
func (x *Exits) teardown(rt *exitRT) {
	t0 := time.Now()
	defer func() {
		if d := time.Since(t0); d > time.Second {
			x.logf("出口 %s：拆除用时 %s", rt.spec.Tag, d.Round(time.Millisecond))
		}
	}()
	if rt.cancel != nil {
		rt.cancel()
	}
	for _, e := range rt.engines {
		select {
		case <-e.Done():
		case <-time.After(10 * time.Second):
			x.logf("出口 %s：引擎 %s 10 秒内没有停下", rt.spec.Tag, e.Name)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if rt.spec.Kind != exitconf.Clash {
		_, _ = x.Ops.Run(ctx, "/sbin/ip", "netns", "del", rt.ns)
		_, _ = x.Ops.Run(ctx, "/sbin/ip", "link", "del", rt.vethA)
	}
	_ = x.Ops.RemoveAll(rt.dir)
}

// Close 拆掉全部出口（agent 退出时）。
func (x *Exits) Close() {
	x.mu.Lock()
	defer x.mu.Unlock()
	for tag, rt := range x.cur {
		x.teardown(rt)
		delete(x.cur, tag)
	}
}

// status 计算一个出口的状态（决定 6）。
func (x *Exits) status(rt *exitRT) ExitStatus {
	st := ExitStatus{Tag: rt.spec.Tag, Kind: rt.spec.Kind}
	for _, e := range rt.engines {
		if e.Status().State != EngineRunning {
			st.Reason = "引擎没有在运行"
			return st
		}
	}
	switch rt.spec.Kind {
	case exitconf.WireGuard, exitconf.OpenVPN:
		if !x.Ops.LinkUp(rt.ns, rt.dev) {
			st.Reason = "隧道设备不在"
			return st
		}
	}
	if rt.spec.Kind == exitconf.OpenVPN {
		// openvpn 重连会重建隧道设备，设备上的默认路由随之消失：报可用之前先确保默认路由在隧道上（幂等），
		// 否则重连后的一小段时间里出口显示可用却没有路由（M4-7 端到端发现）。
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, err := x.Ops.Run(ctx, "/sbin/ip", "-n", rt.ns, "route", "replace", "default", "dev", rt.dev)
		cancel()
		if err != nil {
			st.Reason = "设不上隧道的默认路由"
			return st
		}
	}
	if rt.spec.Kind == exitconf.WireGuard {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		hs, err := x.Ops.WGHandshake(ctx, rt.ns, rt.dev)
		if err != nil || hs.IsZero() || x.now().Sub(hs) > WGHandshakeMax {
			x.kickRefresh() // 握手失败：重新解析服务器的域名（地址可能变了）
			st.Reason = "3 分钟内没有握手"
			return st
		}
	}
	st.Up = true
	return st
}

// statusCacheFor 是状态的缓存时长（数据面每次开流都要查，不能每次都执行 wg show）。
const statusCacheFor = 2 * time.Second

// Lookup 实现 ExitTable 的一部分：分流出口是否存在、是否可用。
func (x *Exits) Lookup(tag string) (known, up bool) {
	x.mu.Lock()
	rt, ok := x.cur[tag]
	x.mu.Unlock()
	if !ok {
		return false, false
	}
	x.stMu.Lock()
	if e, ok := x.stCache[tag]; ok && x.now().Sub(e.at) < statusCacheFor {
		x.stMu.Unlock()
		return true, e.st.Up
	}
	x.stMu.Unlock()
	st := x.status(rt)
	x.stMu.Lock()
	if x.stCache == nil {
		x.stCache = map[string]statusEntry{}
	}
	x.stCache[tag] = statusEntry{at: x.now(), st: st}
	x.stMu.Unlock()
	return true, st.Up
}

// Status 返回全部出口的状态（health 用）：建起来的按实际状态；建不起来的不可用，带原因（不含地址和配置）。
func (x *Exits) Status() []ExitStatus {
	x.mu.Lock()
	rts := make([]*exitRT, 0, len(x.cur))
	for _, rt := range x.cur {
		rts = append(rts, rt)
	}
	failed := make([]ExitStatus, 0, len(x.failed))
	for tag, f := range x.failed {
		failed = append(failed, ExitStatus{Tag: tag, Kind: f.spec.Kind, Reason: "没有建起来：" + firstLine(Redact(f.why))})
	}
	x.mu.Unlock()
	out := []ExitStatus{}
	for _, rt := range rts {
		out = append(out, x.status(rt))
	}
	out = append(out, failed...)
	sort.Slice(out, func(i, j int) bool { return out[i].Tag < out[j].Tag })
	return out
}

type exitsReq struct {
	Exits []ExitSpec `json:"exits"`
}

// ServeHTTP 是 PUT /v1/exits（替换全部分流出口）和 GET /v1/exits（状态）。请求体不进日志。
func (x *Exits) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"exits": x.Status()})
		return
	case http.MethodPut:
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET、PUT"})
		return
	}
	var req exitsReq
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不对"})
		return
	}
	if _, err := Validate(req.Exits); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 60*time.Second)
	defer cancel()
	if err := x.Apply(ctx, req.Exits); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": err.Error(), "exits": x.Status()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"exits": x.Status()})
}

// exitsTable 是带分流出口的出口表：分流出口优先，其余交给 base（main、开发出口）。
type exitsTable struct {
	base    ExitTable
	ex      *Exits
	singBox *Engine // 分流出口经 sing-box 转发：sing-box 没在运行时都不可用（nil 时不看）
}

func (t exitsTable) Lookup(tag string) (known, up bool) {
	if known, up := t.ex.Lookup(tag); known {
		return true, up && (t.singBox == nil || t.singBox.Status().State == EngineRunning)
	}
	return t.base.Lookup(tag)
}

// devExitInfo 是开发用端到端要的出口内部信息（命名空间、编号、隧道设备）。
type devExitInfo struct {
	NS  string
	N   int
	Dev string
}

func (x *Exits) devInfo(tag string) (devExitInfo, bool) {
	x.mu.Lock()
	defer x.mu.Unlock()
	rt, ok := x.cur[tag]
	if !ok {
		return devExitInfo{}, false
	}
	return devExitInfo{NS: rt.ns, N: rt.n, Dev: rt.dev}, true
}

// devRebuild 拆掉一个出口再按原样建起来（开发用：断开隧道之后恢复）。
func (x *Exits) devRebuild(ctx context.Context, tag string) error {
	x.mu.Lock()
	rt, ok := x.cur[tag]
	x.mu.Unlock()
	if !ok {
		return errors.New("没有这个出口")
	}
	spec := rt.spec
	var keep []ExitSpec
	x.mu.Lock()
	for t, r := range x.cur {
		if t != tag {
			keep = append(keep, r.spec)
		}
	}
	x.mu.Unlock()
	if err := x.Apply(ctx, keep); err != nil {
		return err
	}
	return x.Apply(ctx, append(keep, spec))
}

// devSetRootUplink 设开发用对端的 veth，并让根命名空间的规则下次重写。
func (x *Exits) devSetUplink(dev string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.DevUplink = dev
	x.rootReady = false
}

// devEngines 返回各出口引擎的状态（开发用：验证重复下发不重启引擎）。
func (x *Exits) devEngines() map[string]EngineStatus {
	x.mu.Lock()
	defer x.mu.Unlock()
	out := map[string]EngineStatus{}
	for _, rt := range x.cur {
		for _, e := range rt.engines {
			out[e.Name] = e.Status()
		}
	}
	return out
}

// devEngineLog 返回某个出口引擎的日志（已脱敏；开发用）。
func (x *Exits) devEngineLog(name string) []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	for _, rt := range x.cur {
		for _, e := range rt.engines {
			if e.Name == name {
				lines, _ := e.Log.Tail(0)
				return lines
			}
		}
	}
	return nil
}

// ---- 重新解析服务器的域名（m4-plan v2.12 决定 1）----

// startRefresh 第一次下发时起重新解析的循环（随 x.Base 结束）。调用方持有 x.mu。
func (x *Exits) startRefresh() {
	x.refreshOnce.Do(func() {
		x.refresh = make(chan struct{}, 1)
		go x.refreshLoop()
	})
}

// kickRefresh 叫醒重新解析的循环（隧道握手失败时）。
func (x *Exits) kickRefresh() {
	if x.refresh == nil {
		return
	}
	select {
	case x.refresh <- struct{}{}:
	default:
	}
}

func (x *Exits) after(d time.Duration) <-chan time.Time {
	if x.After != nil {
		return x.After(d)
	}
	return time.After(d)
}

func (x *Exits) refreshLoop() {
	every := x.ResolveEvery
	if every <= 0 {
		every = DefaultResolveEvery
	}
	for {
		select {
		case <-x.Base.Done():
			return
		case <-x.after(every):
		case <-x.refresh:
		}
		ctx, cancel := context.WithTimeout(x.Base, 60*time.Second)
		x.RefreshNow(ctx)
		cancel()
	}
}

// RefreshNow 重新解析各出口服务器的域名：结果变了的出口重建（断网规则和引擎配置用新地址，引擎重启一次）；
// 之前建不起来的出口再试一次。sing-box 的出站有变化时通知 OnChange。
func (x *Exits) RefreshNow(ctx context.Context) {
	x.mu.Lock()
	defer x.mu.Unlock()
	changed := false
	tags := make([]string, 0, len(x.cur))
	for t := range x.cur {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	for _, tag := range tags {
		rt := x.cur[tag]
		if len(rt.resolved) == 0 {
			continue // 服务器是 IP 字面量，或是订阅（mihomo 自己经网关解析）
		}
		confs, err := Validate([]ExitSpec{rt.spec})
		if err != nil {
			continue
		}
		p := confs[tag]
		res, _, err := x.resolveAll(ctx, p.endpoints())
		if err == nil && fmt.Sprint(res) == fmt.Sprint(rt.resolved) {
			continue // 没变
		}
		// 变了（或解析失败）：拆掉重建；建不起来就记进 failed，出口不可用，不放宽任何规则。
		x.teardown(rt)
		delete(x.cur, tag)
		changed = true
		x.rebuilds.Add(1)
		nrt, serr := x.setup(ctx, rt.spec, p)
		if serr != nil {
			x.failed[tag] = failedExit{spec: rt.spec, p: p, why: serr.Error()}
			x.logf("出口 %s：重新解析后没有建起来：%v", tag, serr)
			continue
		}
		x.cur[tag] = nrt
		x.logf("出口 %s：服务器地址变了，已用新地址重建", tag) // 不写地址
	}
	ftags := make([]string, 0, len(x.failed))
	for t := range x.failed {
		ftags = append(ftags, t)
	}
	sort.Strings(ftags)
	for _, tag := range ftags {
		f := x.failed[tag]
		if _, ok := x.cur[tag]; ok {
			delete(x.failed, tag)
			continue
		}
		nrt, err := x.setup(ctx, f.spec, f.p)
		if err != nil {
			f.why = err.Error()
			x.failed[tag] = f
			continue
		}
		delete(x.failed, tag)
		x.cur[tag] = nrt
		changed = true
	}
	if changed {
		x.stMu.Lock()
		x.stCache = nil
		x.stMu.Unlock()
		if x.OnChange != nil {
			if err := x.OnChange(x.singBoxExits()); err != nil {
				x.logf("更新 sing-box 的出口：%v", err)
			}
		}
	}
}
