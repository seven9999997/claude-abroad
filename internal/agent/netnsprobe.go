package agent

// NetnsProbe 是 M4-1 的探查接口（m4-plan M4-1，风险 R19、R5）：只在内核命令行带 abroad.dev=1 时注册
// POST /v1/dev/netns-probe。在虚拟机里：
//   1. 加载 veth、wireguard、tun、nf_tables 模块，确认 wg、openvpn、nft、ip 都能执行；
//   2. 建对端命名空间 peer 和两个出口命名空间（wg、ovpn），各用一对 veth 连到 peer（peer 模拟“VPN 服务器”）；
//   3. peer 里起 WireGuard、OpenVPN 服务端（密钥和证书在这里现生成，不依赖外部账号）；
//      出口命名空间里起客户端：WireGuard 不用 wg-quick，由这里建接口、配地址和路由，只调 `wg setconf`；
//      OpenVPN 的客户端配置模拟服务商的样子（带 redirect-gateway），按 R5 改写（去掉 redirect-gateway、加 route-nopull）；
//   4. 对照组：没加断网规则时，出口命名空间能直接连到 peer 的普通端口和 53 端口；
//   5. 隧道里的 TCP 回显、经隧道的 53 端口（解析只经隧道）、默认路由是隧道接口；
//   6. 加 nft 断网规则（只放行 lo、隧道接口、到对端端点的那一个端口），隧道仍通、直连 peer 的普通端口和 53 端口都不通；
//   7. 断开隧道并把默认路由退回 veth（模拟隧道掉了、路由回落）：除了对端端点，其他目的全部不通，53 也不通；
//      端点的放行规则计数增长（仍能到达端点）；
//   8. 清理：停进程、删命名空间、删临时文件。
// 报告只有每一步的结果、耗时和出错时的简短说明，不含密钥、证书和配置内容。

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"claude-abroad/internal/kmod"
)

// probeOps 是探查要用的系统操作（真实实现只在 Linux；测试注入假的）。
type probeOps interface {
	// Run 执行一条命令，返回合并的输出（截断）。
	Run(ctx context.Context, argv ...string) (string, error)
	// Start 在后台启动一个进程（OpenVPN），返回停止它的函数。
	Start(ctx context.Context, argv ...string) (stop func(), err error)
	// ServeEcho 在命名空间里起回显服务（TCP 7、UDP 53，所有地址上都听），返回停止它的函数。
	ServeEcho(ns string) (stop func(), err error)
	// Probe 在命名空间里对 addr 做一次探测：kind 是 tcp-echo、udp-echo（收发一次并核对回显）、
	// tcp-connect（只建连接）、udp-send（只发一个包）。
	Probe(ns, kind, addr string) error
	LoadModules(names []string) []kmod.Result
	WriteFile(path string, data []byte) error
	RemoveAll(path string) error
}

// 探查用的名字和地址（只在虚拟机里，不碰宿主）。
const (
	probeDir  = "/run/abroad-probe"
	nsPeer    = "abprobe-peer"
	nsWG      = "abprobe-wg"
	nsOVPN    = "abprobe-ovpn"
	vethWG0   = "abp-w0" // peer 一侧
	vethWG1   = "abp-w1" // 出口一侧
	vethOV0   = "abp-o0"
	vethOV1   = "abp-o1"
	peerWGIP  = "10.201.0.1"
	exitWGIP  = "10.201.0.2"
	peerOVIP  = "10.201.0.5"
	exitOVIP  = "10.201.0.6"
	wgTunPeer = "10.202.0.1"
	wgTunExit = "10.202.0.2"
	ovTunPeer = "10.203.0.1"
	wgPort    = 51820
	ovPort    = 1194
	echoPort  = 7
)

// probeModules 是探查要加载的模块（按顺序）。
// crc32c_generic 不再手工列出：nf_tables 依赖的 libcrc32c 在 modules.softdep 里有 “pre: crc32c”，
// 加载器按软依赖先加载它（m4-plan v2.1、v2.8 决定 7）。
var probeModules = []string{"veth", "wireguard", "tun", "nf_tables"}

// ProbeStep 是报告的一步。
type ProbeStep struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Ms     int64  `json:"ms"`
	Detail string `json:"detail,omitempty"`
}

// ProbeReport 是探查报告。
type ProbeReport struct {
	OK       bool        `json:"ok"`
	Steps    []ProbeStep `json:"steps"`
	Modules  []string    `json:"modules_failed,omitempty"`
	Rewrites []string    `json:"ovpn_rewrites"` // OpenVPN 配置改写了哪些指令（R5）
}

// NetnsProbe 实现 POST /v1/dev/netns-probe。同一时间只跑一个。
type NetnsProbe struct {
	Ops     probeOps
	Timeout time.Duration // 默认 120 秒
	mu      sync.Mutex
}

func (p *NetnsProbe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "只支持 POST", http.StatusMethodNotAllowed)
		return
	}
	if !p.mu.TryLock() {
		http.Error(w, "已有一次探查在进行", http.StatusConflict)
		return
	}
	defer p.mu.Unlock()
	t := p.Timeout
	if t <= 0 {
		t = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), t)
	defer cancel()
	rep := p.Run(ctx)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rep)
}

type probeRun struct {
	ops          probeOps
	ctx          context.Context
	rep          *ProbeReport
	stops        []func() // 都要可以重复调用
	ovClientStop func()
}

// step 记一步；fn 返回的错误只取第一行、截断，作为说明。
func (r *probeRun) step(name string, fn func() (string, error)) bool {
	start := time.Now()
	detail, err := fn()
	s := ProbeStep{Name: name, OK: err == nil, Ms: time.Since(start).Milliseconds(), Detail: detail}
	if err != nil {
		s.Detail = firstLine(err.Error())
	}
	r.rep.Steps = append(r.rep.Steps, s)
	return err == nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// sh 依次执行多条命令，任一失败就返回（带命令名，不带参数里可能的路径以外的东西）。
func (r *probeRun) sh(cmds ...[]string) error {
	for _, c := range cmds {
		if out, err := r.ops.Run(r.ctx, c...); err != nil {
			return fmt.Errorf("%s：%v：%s", strings.Join(c, " "), err, firstLine(out))
		}
	}
	return nil
}

// Run 跑完整的探查（包括清理），返回报告。
func (p *NetnsProbe) Run(ctx context.Context) ProbeReport {
	rep := ProbeReport{}
	r := &probeRun{ops: p.Ops, ctx: ctx, rep: &rep}
	defer r.cleanup()
	r.cleanup() // 上一次没清干净的残留
	ok := r.run()
	rep.OK = ok
	for _, s := range rep.Steps {
		rep.OK = rep.OK && s.OK
	}
	return rep
}

func (r *probeRun) cleanup() {
	for i := len(r.stops) - 1; i >= 0; i-- {
		r.stops[i]()
	}
	r.stops = nil
	cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, ns := range []string{nsWG, nsOVPN, nsPeer} {
		_, _ = r.ops.Run(cctx, "/sbin/ip", "netns", "del", ns) // veth 随命名空间一起删掉
	}
	_ = r.ops.RemoveAll(probeDir)
}

func (r *probeRun) run() bool {
	// 1. 模块和程序。
	if !r.step("加载模块（veth、wireguard、tun、nf_tables；crc32c 经软依赖）", func() (string, error) {
		var failed []string
		var softs []string
		for _, res := range r.ops.LoadModules(probeModules) {
			switch {
			case res.Soft:
				softs = append(softs, res.Name+map[bool]string{true: " 成功", false: " 失败"}[res.OK])
			case !res.OK:
				failed = append(failed, res.Name+"："+res.Error)
			}
		}
		r.rep.Modules = failed
		if len(failed) > 0 {
			return "", errors.New(strings.Join(failed, "；"))
		}
		return "软依赖：" + strings.Join(softs, "、"), nil
	}) {
		return false
	}
	if !r.step("程序能执行（wg、openvpn、nft、ip）", func() (string, error) {
		var vers []string
		for _, c := range [][]string{{"/usr/bin/wg", "--version"}, {"/usr/sbin/openvpn", "--version"}, {"/usr/sbin/nft", "--version"}, {"/sbin/ip", "-V"}} {
			out, err := r.ops.Run(r.ctx, c...)
			if err != nil && !(c[0] == "/usr/sbin/openvpn" && strings.Contains(out, "OpenVPN")) { // openvpn --version 的退出码是 1
				return "", fmt.Errorf("%s：%v：%s", c[0], err, firstLine(out))
			}
			vers = append(vers, firstLine(out))
		}
		return strings.Join(vers, " | "), nil
	}) {
		return false
	}
	// 地区（M4-5）要用的：时区数据和 musl 的语言环境。只记录，不影响隧道部分。
	r.step("地区要用的文件（tzdata、musl-locales）", func() (string, error) {
		if _, err := r.ops.Run(r.ctx, "/bin/ls", "/usr/share/zoneinfo/Asia/Tokyo", "/usr/share/zoneinfo/America/Los_Angeles"); err != nil {
			return "", fmt.Errorf("tzdata：%w", err)
		}
		out, err := r.ops.Run(r.ctx, "/usr/bin/locale", "-a")
		if err != nil {
			return "", fmt.Errorf("locale -a：%w", err)
		}
		var have []string
		for _, l := range []string{"en_US", "ja_JP", "zh_CN"} {
			if strings.Contains(out, l) {
				have = append(have, l)
			}
		}
		if len(have) < 2 {
			return "", fmt.Errorf("locale -a 里没有常用的语言环境：%s", firstLine(out))
		}
		return "locale -a 有 " + strings.Join(have, "、"), nil
	})
	if err := r.ops.WriteFile(probeDir+"/.keep", nil); err != nil {
		r.step("建临时目录", func() (string, error) { return "", err })
		return false
	}
	// 2. 命名空间和 veth。
	if !r.step("建命名空间和 veth", func() (string, error) {
		return "", r.sh(
			[]string{"/sbin/ip", "netns", "add", nsPeer},
			[]string{"/sbin/ip", "netns", "add", nsWG},
			[]string{"/sbin/ip", "netns", "add", nsOVPN},
			[]string{"/sbin/ip", "link", "add", vethWG0, "type", "veth", "peer", "name", vethWG1},
			[]string{"/sbin/ip", "link", "add", vethOV0, "type", "veth", "peer", "name", vethOV1},
			[]string{"/sbin/ip", "link", "set", vethWG0, "netns", nsPeer},
			[]string{"/sbin/ip", "link", "set", vethOV0, "netns", nsPeer},
			[]string{"/sbin/ip", "link", "set", vethWG1, "netns", nsWG},
			[]string{"/sbin/ip", "link", "set", vethOV1, "netns", nsOVPN},
			[]string{"/sbin/ip", "-n", nsPeer, "addr", "add", peerWGIP + "/30", "dev", vethWG0},
			[]string{"/sbin/ip", "-n", nsPeer, "addr", "add", peerOVIP + "/30", "dev", vethOV0},
			[]string{"/sbin/ip", "-n", nsWG, "addr", "add", exitWGIP + "/30", "dev", vethWG1},
			[]string{"/sbin/ip", "-n", nsOVPN, "addr", "add", exitOVIP + "/30", "dev", vethOV1},
			[]string{"/sbin/ip", "-n", nsPeer, "link", "set", "lo", "up"},
			[]string{"/sbin/ip", "-n", nsWG, "link", "set", "lo", "up"},
			[]string{"/sbin/ip", "-n", nsOVPN, "link", "set", "lo", "up"},
			[]string{"/sbin/ip", "-n", nsPeer, "link", "set", vethWG0, "up"},
			[]string{"/sbin/ip", "-n", nsPeer, "link", "set", vethOV0, "up"},
			[]string{"/sbin/ip", "-n", nsWG, "link", "set", vethWG1, "up"},
			[]string{"/sbin/ip", "-n", nsOVPN, "link", "set", vethOV1, "up"},
		)
	}) {
		return false
	}
	// peer 里的回显服务：TCP 7、UDP 53（模拟“对端的普通服务”和“解析器”，所有地址上都听）。
	if !r.step("对端起回显服务（TCP 7、UDP 53）", func() (string, error) {
		stop, err := r.ops.ServeEcho(nsPeer)
		if err != nil {
			return "", err
		}
		r.stops = append(r.stops, stop)
		return "", nil
	}) {
		return false
	}
	wgOK := r.wireguard()
	ovOK := r.openvpn()
	return wgOK && ovOK
}

// ---- WireGuard ----

func (r *probeRun) wireguard() bool {
	var peerPriv, exitPriv, peerPub, exitPub string
	if !r.step("WireGuard：生成密钥（X25519）", func() (string, error) {
		var err error
		if peerPriv, peerPub, err = wgKeyPair(); err != nil {
			return "", err
		}
		exitPriv, exitPub, err = wgKeyPair()
		return "", err
	}) {
		return false
	}
	peerConf := fmt.Sprintf("[Interface]\nPrivateKey = %s\nListenPort = %d\n\n[Peer]\nPublicKey = %s\nAllowedIPs = %s/32\n", peerPriv, wgPort, exitPub, wgTunExit)
	exitConf := fmt.Sprintf("[Interface]\nPrivateKey = %s\n\n[Peer]\nPublicKey = %s\nEndpoint = %s:%d\nAllowedIPs = 0.0.0.0/0\n", exitPriv, peerPub, peerWGIP, wgPort)
	if !r.step("WireGuard：对端和出口的接口（不用 wg-quick，只调 wg setconf）", func() (string, error) {
		if err := r.ops.WriteFile(probeDir+"/wg-peer.conf", []byte(peerConf)); err != nil {
			return "", err
		}
		if err := r.ops.WriteFile(probeDir+"/wg-exit.conf", []byte(exitConf)); err != nil {
			return "", err
		}
		return "", r.sh(
			[]string{"/sbin/ip", "-n", nsPeer, "link", "add", "wg0", "type", "wireguard"},
			[]string{"/sbin/ip", "netns", "exec", nsPeer, "/usr/bin/wg", "setconf", "wg0", probeDir + "/wg-peer.conf"},
			[]string{"/sbin/ip", "-n", nsPeer, "addr", "add", wgTunPeer + "/24", "dev", "wg0"},
			[]string{"/sbin/ip", "-n", nsPeer, "link", "set", "wg0", "up"},
			[]string{"/sbin/ip", "-n", nsWG, "link", "add", "wg1", "type", "wireguard"},
			[]string{"/sbin/ip", "netns", "exec", nsWG, "/usr/bin/wg", "setconf", "wg1", probeDir + "/wg-exit.conf"},
			[]string{"/sbin/ip", "-n", nsWG, "addr", "add", wgTunExit + "/24", "dev", "wg1"},
			[]string{"/sbin/ip", "-n", nsWG, "link", "set", "wg1", "up"},
			[]string{"/sbin/ip", "-n", nsWG, "route", "replace", "default", "dev", "wg1"},
		)
	}) {
		return false
	}
	return r.tunnelChecks("WireGuard", nsWG, "wg1", peerWGIP, wgTunPeer,
		fmt.Sprintf("ip daddr %s udp dport %d counter accept", peerWGIP, wgPort),
		func() error {
			return r.sh(
				[]string{"/sbin/ip", "-n", nsWG, "link", "del", "wg1"},
				[]string{"/sbin/ip", "-n", nsWG, "route", "replace", "default", "via", peerWGIP, "dev", vethWG1},
			)
		},
		func() error { // 端点仍能到达：向端点发一个 UDP 包，放行规则的计数增长
			return r.ops.Probe(nsWG, "udp-send", net.JoinHostPort(peerWGIP, strconv.Itoa(wgPort)))
		})
}

// wgKeyPair 生成 WireGuard 的密钥对（base64）。
func wgKeyPair() (priv, pub string, err error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(k.Bytes()), base64.StdEncoding.EncodeToString(k.PublicKey().Bytes()), nil
}

// ---- OpenVPN ----

func (r *probeRun) openvpn() bool {
	var pki probePKI
	if !r.step("OpenVPN：生成证书（ECDSA P-256，自签 CA）", func() (string, error) {
		var err error
		pki, err = newProbePKI()
		return "", err
	}) {
		return false
	}
	server := strings.Join([]string{
		"port " + strconv.Itoa(ovPort), "proto tcp-server", "local " + peerOVIP, "dev tun", "topology subnet",
		"server 10.203.0.0 255.255.255.0", "dh none", "keepalive 2 10", "verb 3",
		`push "redirect-gateway def1"`, `push "dhcp-option DNS ` + ovTunPeer + `"`,
		"<ca>\n" + pki.ca + "</ca>", "<cert>\n" + pki.serverCert + "</cert>", "<key>\n" + pki.serverKey + "</key>",
	}, "\n") + "\n"
	// 模拟服务商给的客户端配置：带 redirect-gateway。按 R5 改写后再用。
	provider := strings.Join([]string{
		"client", "dev tun", "proto tcp-client", fmt.Sprintf("remote %s %d", peerOVIP, ovPort), "nobind",
		"remote-cert-tls server", "redirect-gateway def1", "verb 3",
		"<ca>\n" + pki.ca + "</ca>", "<cert>\n" + pki.clientCert + "</cert>", "<key>\n" + pki.clientKey + "</key>",
	}, "\n") + "\n"
	client, rewrites := RewriteOVPNForNetns(provider)
	r.rep.Rewrites = rewrites
	if !r.step("OpenVPN：对端服务端、出口客户端（配置按 R5 改写）", func() (string, error) {
		if err := r.ops.WriteFile(probeDir+"/ovpn-server.conf", []byte(server)); err != nil {
			return "", err
		}
		if err := r.ops.WriteFile(probeDir+"/ovpn-client.conf", []byte(client)); err != nil {
			return "", err
		}
		stopS, err := r.ops.Start(r.ctx, "/sbin/ip", "netns", "exec", nsPeer, "/usr/sbin/openvpn", "--config", probeDir+"/ovpn-server.conf")
		if err != nil {
			return "", fmt.Errorf("起服务端：%w", err)
		}
		r.stops = append(r.stops, stopS)
		if err := r.waitAddr(nsPeer, "tun0", ovTunPeer, 20*time.Second); err != nil {
			return "", fmt.Errorf("服务端的 tun0 没有起来：%w", err)
		}
		stopC, err := r.ops.Start(r.ctx, "/sbin/ip", "netns", "exec", nsOVPN, "/usr/sbin/openvpn", "--config", probeDir+"/ovpn-client.conf")
		if err != nil {
			return "", fmt.Errorf("起客户端：%w", err)
		}
		r.stops = append(r.stops, stopC)
		r.ovClientStop = stopC
		if err := r.waitAddr(nsOVPN, "tun0", "10.203.0.", 30*time.Second); err != nil {
			return "", fmt.Errorf("客户端的 tun0 没有拿到地址：%w", err)
		}
		// route-nopull 之后由我们设默认路由（只在这个命名空间里）。
		return "", r.sh([]string{"/sbin/ip", "-n", nsOVPN, "route", "replace", "default", "dev", "tun0"})
	}) {
		return false
	}
	return r.tunnelChecks("OpenVPN", nsOVPN, "tun0", peerOVIP, ovTunPeer,
		fmt.Sprintf("ip daddr %s tcp dport %d counter accept", peerOVIP, ovPort),
		func() error {
			if r.ovClientStop != nil {
				r.ovClientStop()
			}
			if err := r.waitGone(nsOVPN, "tun0", 10*time.Second); err != nil {
				return err
			}
			return r.sh([]string{"/sbin/ip", "-n", nsOVPN, "route", "replace", "default", "via", peerOVIP, "dev", vethOV1})
		},
		func() error { // 端点仍能到达：TCP 连 1194 能建成（服务端还在听）
			return r.ops.Probe(nsOVPN, "tcp-connect", net.JoinHostPort(peerOVIP, strconv.Itoa(ovPort)))
		})
}

// RewriteOVPNForNetns 是 R5 的改写：去掉影响路由的 redirect-gateway、route 指令，加 route-nopull
// （服务端推送的路由也不要），默认路由由 agent 只在出口命名空间里设。返回改写后的配置和改了什么。
func RewriteOVPNForNetns(conf string) (string, []string) {
	var out, changes []string
	for _, l := range strings.Split(conf, "\n") {
		f := strings.Fields(l)
		if len(f) > 0 && (f[0] == "redirect-gateway" || f[0] == "route" || f[0] == "route-ipv6" || f[0] == "route-nopull") {
			if f[0] != "route-nopull" {
				changes = append(changes, "去掉 "+f[0])
			}
			continue
		}
		out = append(out, l)
	}
	out = append(out, "route-nopull")
	changes = append(changes, "加 route-nopull（不接受服务端推送的路由和 redirect-gateway）")
	return strings.Join(out, "\n"), changes
}

// ---- 隧道的通用检查 ----

// tunnelChecks：对照组 → 隧道通、默认路由是隧道接口、经隧道的 53 → 加断网规则 → 隧道仍通、直连不通
// → 断开隧道、路由回落 → 只有端点能到，53 也不通。
func (r *probeRun) tunnelChecks(name, ns, dev, peerVeth, peerTun, endpointRule string, breakTunnel, hitEndpoint func() error) bool {
	ok := true
	ok = r.step(name+"：对照组（没加断网规则时能直连对端的 TCP 7 和 UDP 53）", func() (string, error) {
		if err := r.tcpEcho(ns, peerVeth); err != nil {
			return "", fmt.Errorf("TCP：%w", err)
		}
		return "", r.udpEcho(ns, peerVeth)
	}) && ok
	ok = r.step(name+"：隧道里的 TCP 回显", func() (string, error) { return "", r.tcpEcho(ns, peerTun) }) && ok
	ok = r.step(name+"：默认路由是隧道接口", func() (string, error) {
		out, err := r.ops.Run(r.ctx, "/sbin/ip", "-n", ns, "route", "get", "1.1.1.1")
		if err != nil {
			return "", err
		}
		if !strings.Contains(out, "dev "+dev) {
			return "", fmt.Errorf("1.1.1.1 的路由不经 %s：%s", dev, firstLine(out))
		}
		return "", nil
	}) && ok
	rules := fmt.Sprintf("table inet abroad_ks {\n  chain output {\n    type filter hook output priority 0; policy drop;\n"+
		"    oifname \"lo\" accept\n    oifname \"%s\" accept\n    %s\n  }\n}\n", dev, endpointRule)
	if !r.step(name+"：加断网规则（只放行 lo、隧道、对端端点）", func() (string, error) {
		f := probeDir + "/ks-" + ns + ".nft"
		if err := r.ops.WriteFile(f, []byte(rules)); err != nil {
			return "", err
		}
		return "", r.sh([]string{"/sbin/ip", "netns", "exec", ns, "/usr/sbin/nft", "-f", f})
	}) {
		return false
	}
	ok = r.step(name+"：加规则后隧道仍通，经隧道的 53 通", func() (string, error) {
		if err := r.tcpEcho(ns, peerTun); err != nil {
			return "", fmt.Errorf("TCP：%w", err)
		}
		return "", r.udpEcho(ns, peerTun)
	}) && ok
	ok = r.step(name+"：加规则后直连对端的 TCP 7、UDP 53 都不通", func() (string, error) {
		return "", r.mustFail(ns, peerVeth)
	}) && ok
	if !r.step(name+"：断开隧道、默认路由退回 veth", func() (string, error) { return "", breakTunnel() }) {
		return false
	}
	ok = r.step(name+"：断开后对端的普通端口、53、隧道地址都不通", func() (string, error) {
		if err := r.mustFail(ns, peerVeth); err != nil {
			return "", err
		}
		if r.tcpEcho(ns, peerTun) == nil {
			return "", errors.New("隧道断开后隧道地址仍然能连上")
		}
		if r.udpEcho(ns, "1.1.1.1") == nil {
			return "", errors.New("隧道断开后公网 53 竟然有应答")
		}
		return "", nil
	}) && ok
	ok = r.step(name+"：断开后仍能到达对端端点（放行规则的计数增长）", func() (string, error) {
		before := r.counter(ns, endpointRule)
		if err := hitEndpoint(); err != nil {
			return "", err
		}
		after := r.counter(ns, endpointRule)
		if after <= before {
			return "", fmt.Errorf("端点规则的计数没有增长（%d → %d）", before, after)
		}
		return fmt.Sprintf("计数 %d → %d", before, after), nil
	}) && ok
	return ok
}

var counterRe = regexp.MustCompile(`counter packets (\d+)`)

// counter 读断网规则里端点那条的包计数（读不到时 -1）。
func (r *probeRun) counter(ns, rule string) int {
	out, err := r.ops.Run(r.ctx, "/sbin/ip", "netns", "exec", ns, "/usr/sbin/nft", "list", "chain", "inet", "abroad_ks", "output")
	if err != nil {
		return -1
	}
	key := strings.Fields(rule)
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, key[2]) && strings.Contains(l, "dport "+key[len(key)-3]) { // 端点地址和端口
			if m := counterRe.FindStringSubmatch(l); m != nil {
				n, _ := strconv.Atoi(m[1])
				return n
			}
		}
	}
	return -1
}

func (r *probeRun) mustFail(ns, peer string) error {
	if r.tcpEcho(ns, peer) == nil {
		return fmt.Errorf("TCP 7 竟然连通了")
	}
	if r.udpEcho(ns, peer) == nil {
		return fmt.Errorf("UDP 53 竟然有应答")
	}
	return nil
}

func (r *probeRun) tcpEcho(ns, ip string) error {
	return r.ops.Probe(ns, "tcp-echo", net.JoinHostPort(ip, strconv.Itoa(echoPort)))
}

func (r *probeRun) udpEcho(ns, ip string) error {
	return r.ops.Probe(ns, "udp-echo", net.JoinHostPort(ip, "53"))
}

func (r *probeRun) waitAddr(ns, dev, prefix string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		out, err := r.ops.Run(r.ctx, "/sbin/ip", "-n", ns, "-4", "addr", "show", "dev", dev)
		if err == nil && strings.Contains(out, "inet "+prefix) {
			return nil
		}
		if time.Now().After(deadline) || r.ctx.Err() != nil {
			return fmt.Errorf("%s 的 %s 在 %s 内没有地址", ns, dev, d)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (r *probeRun) waitGone(ns, dev string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		if _, err := r.ops.Run(r.ctx, "/sbin/ip", "-n", ns, "link", "show", dev); err != nil {
			return nil
		}
		if time.Now().After(deadline) || r.ctx.Err() != nil {
			return fmt.Errorf("%s 的 %s 在 %s 内没有消失", ns, dev, d)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// ---- 探查用的 PKI（只在内存和临时目录里） ----

type probePKI struct{ ca, serverCert, serverKey, clientCert, clientKey string }

func newProbePKI() (probePKI, error) {
	var p probePKI
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return p, err
	}
	now := time.Now().Add(-time.Hour)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "abroad-probe-ca"},
		NotBefore: now, NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		return p, err
	}
	caCert, _ := x509.ParseCertificate(caDER)
	leaf := func(serial int64, cn string, eku x509.ExtKeyUsage) (string, string, error) {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return "", "", err
		}
		tpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn},
			NotBefore: now, NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyAgreement,
			ExtKeyUsage: []x509.ExtKeyUsage{eku}}
		der, err := x509.CreateCertificate(rand.Reader, tpl, caCert, &k.PublicKey, caKey)
		if err != nil {
			return "", "", err
		}
		kd, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			return "", "", err
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
			string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd})), nil
	}
	p.ca = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	if p.serverCert, p.serverKey, err = leaf(2, "abroad-probe-server", x509.ExtKeyUsageServerAuth); err != nil {
		return p, err
	}
	p.clientCert, p.clientKey, err = leaf(3, "abroad-probe-client", x509.ExtKeyUsageClientAuth)
	return p, err
}
