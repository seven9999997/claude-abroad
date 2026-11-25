//go:build abroaddev

package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"claude-abroad/internal/exitconf"
	"claude-abroad/internal/guestabi"
)

// DevPeer 是 M4-7 虚拟机端到端用的同机对端（m4-plan v2.8 决定 8：对端仍用同一虚拟机里的另一个命名空间）。
// 只在内核命令行带 abroad.dev=1 时注册 /v1/dev/exit-peer：
//
//	POST ?action=up       建对端命名空间 abx-peer（WireGuard、OpenVPN 服务端，socks 节点，带标记的回显，会计数的 DNS，
//	                      进入对端的包按来源计数），返回三个出口（wg、ovpn、sub）的配置，由调用方经 PUT /v1/exits 下发；
//	POST ?action=break&exit=wg|ovpn   断开该出口的隧道，并把它命名空间的默认路由改回 veth（最坏情况），
//	                      再从该命名空间里主动往对端的普通端口、53、公网的 53 发包（应全部被断网规则拦下）；
//	POST ?action=restore&exit=wg|ovpn 恢复；
//	POST ?action=kill&exit=<出口>     杀掉该出口命名空间里的 socks 进程（引擎会重启）；
//	POST ?action=skew&d=4m            状态判断用的时钟往后拨（验证 WireGuard 握手超时；d=0 复原）；
//	POST ?action=down                 拆掉对端；
//	GET                    计数（对端按来源和端口、DNS 查询、回显、根命名空间转发的 53、各出口被拦下的包）、
//	                      残留的命名空间和网卡、出口目录里的文件和权限（只给名字和权限，不给内容）。
//
// 测试用的密钥只在这次开机的内存里，返回给调用方一次。
type DevPeer struct {
	Ops   devPeerOps
	Exits *Exits

	mu       sync.Mutex
	up       bool
	svc      devPeerServices
	stopOVPN func()
	stopNode func()
	pki      probePKI
	ovpnMode string
	stops    []func()
}

type devPeerOps interface {
	probeOps
	InNetns(ns string, fn func() error) error
}

const (
	nsDevPeer    = "abx-peer"
	devUplinkA   = "abxp0" // 根命名空间一侧（不能以 vx 开头：转发规则按 vx* 认出口）
	devUplinkB   = "abxp1"
	devPeerRoot  = "10.199.0.1"
	devPeerIP    = "10.199.0.2"
	devWGTunPeer = "10.77.0.1"
	devWGTunExit = "10.77.0.2"
	devOVTunPeer = "10.78.0.1"
	devNodePort  = 1081
	devPeerDir   = "/run/abroad-devpeer"
	devPubDNS    = "1.1.1.1" // 对端里冒充固定公共解析器（DefaultExitDNS）的地址
)

func (p *DevPeer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 90*time.Second)
	defer cancel()
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, p.stats(ctx))
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "只支持 GET、POST"})
		return
	}
	q := r.URL.Query()
	var (
		out any = map[string]bool{"ok": true}
		err error
	)
	switch q.Get("action") {
	case "up":
		out, err = p.setup(ctx)
	case "break":
		out, err = p.breakExit(ctx, q.Get("exit"))
	case "restore":
		err = p.restore(ctx, q.Get("exit"))
	case "kill":
		err = p.kill(ctx, q.Get("exit"))
	case "ovpn-mode": // 换对端 OpenVPN 服务端的推送方式并重启它；客户端由 ping-restart 重连
		err = p.setOVPNMode(ctx, q.Get("mode"))
	case "skew":
		var d time.Duration
		if d, err = time.ParseDuration(q.Get("d")); err == nil {
			p.Exits.skew.Store(int64(d))
			p.Exits.stMu.Lock()
			p.Exits.stCache = nil
			p.Exits.stMu.Unlock()
		}
	case "node-down": // 停掉订阅出口的节点（R20：节点不可用时开流失败，不改走直连）
		p.mu.Lock()
		if p.stopNode != nil {
			p.stopNode()
			p.stopNode = nil
		}
		p.mu.Unlock()
	case "node-up":
		p.mu.Lock()
		if p.stopNode == nil {
			err = p.startNodeLocked(ctx)
		}
		p.mu.Unlock()
	case "down":
		p.teardown()
	default:
		err = errors.New("不认识的 action")
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (p *DevPeer) sh(ctx context.Context, cmds ...[]string) error {
	for _, c := range cmds {
		if out, err := p.Ops.Run(ctx, c...); err != nil {
			return fmt.Errorf("%s：%v：%s", strings.Join(c, " "), err, firstLine(out))
		}
	}
	return nil
}

// peerNFT 是对端命名空间的计数规则（只计数，不拦）：来自出口命名空间（10.200.0.0/16，转发时不做 NAT）的包按目的端口计数，
// 其余（不该出现的）单独计数。
func peerNFT() string {
	return fmt.Sprintf(`table inet abx_peer {
  chain input {
    type filter hook input priority 0; policy accept;
    ip saddr %[1]s udp dport 51820 counter comment "wg_endpoint"
    ip saddr %[1]s udp dport 1194 counter comment "ovpn_endpoint"
    ip saddr %[1]s udp dport 51820 accept
    ip saddr %[1]s udp dport 1194 accept
    ip saddr %[1]s counter comment "exitns_other"
    iifname "%[2]s" ip saddr %[3]s counter comment "root_ns"
  }
}
`, ExitNet, devUplinkB, devPeerRoot)
}

type devPeerUp struct {
	Exits []ExitSpec `json:"exits"`
}

func (p *DevPeer) setup(ctx context.Context) (devPeerUp, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.up {
		return devPeerUp{}, errors.New("对端已经建好")
	}
	p.teardownLocked()
	if err := os.MkdirAll(devPeerDir, 0o700); err != nil {
		return devPeerUp{}, err
	}
	p.Ops.LoadModules(exitModules)
	if err := p.sh(ctx,
		[]string{"/sbin/ip", "netns", "add", nsDevPeer},
		[]string{"/sbin/ip", "link", "add", devUplinkA, "type", "veth", "peer", "name", devUplinkB},
		[]string{"/sbin/ip", "link", "set", devUplinkB, "netns", nsDevPeer},
		[]string{"/sbin/ip", "addr", "add", devPeerRoot + "/30", "dev", devUplinkA},
		[]string{"/sbin/ip", "link", "set", devUplinkA, "up"},
		[]string{"/sbin/ip", "-n", nsDevPeer, "addr", "add", devPeerIP + "/30", "dev", devUplinkB},
		[]string{"/sbin/ip", "-n", nsDevPeer, "link", "set", "lo", "up"},
		[]string{"/sbin/ip", "-n", nsDevPeer, "link", "set", devUplinkB, "up"},
		[]string{"/sbin/ip", "-n", nsDevPeer, "route", "add", ExitNet.String(), "via", devPeerRoot},
	); err != nil {
		return devPeerUp{}, err
	}
	if err := p.Ops.WriteFile(devPeerDir+"/peer.nft", []byte(peerNFT())); err != nil {
		return devPeerUp{}, err
	}
	if err := p.sh(ctx, []string{"/sbin/ip", "netns", "exec", nsDevPeer, "/usr/sbin/nft", "-f", devPeerDir + "/peer.nft"}); err != nil {
		return devPeerUp{}, err
	}
	// 根命名空间自己发出的包（mihomo 等在根命名空间里运行的进程）按“发往 eth0 的 53 端口”和“发往 eth0 的全部”计数，
	// 只计数不拦：订阅出口不应在本地解析、不应直连（v2.10 验收 b、c）。
	if err := p.Ops.WriteFile(devPeerDir+"/root-out.nft", []byte(rootOutNFT)); err != nil {
		return devPeerUp{}, err
	}
	_, _ = p.Ops.Run(ctx, "/usr/sbin/nft", "delete", "table", "inet", "abx_root_out")
	if err := p.sh(ctx, []string{"/usr/sbin/nft", "-f", devPeerDir + "/root-out.nft"}); err != nil {
		return devPeerUp{}, err
	}
	p.Exits.devSetUplink(devUplinkA)

	// WireGuard 服务端。
	peerPriv, peerPub, err := wgKeyPair()
	if err != nil {
		return devPeerUp{}, err
	}
	exitPriv, exitPub, err := wgKeyPair()
	if err != nil {
		return devPeerUp{}, err
	}
	if err := p.Ops.WriteFile(devPeerDir+"/wg0.conf", []byte(fmt.Sprintf("[Interface]\nPrivateKey = %s\nListenPort = 51820\n\n[Peer]\nPublicKey = %s\nAllowedIPs = %s/32\n", peerPriv, exitPub, devWGTunExit))); err != nil {
		return devPeerUp{}, err
	}
	if err := p.sh(ctx,
		[]string{"/sbin/ip", "-n", nsDevPeer, "link", "add", "wg0", "type", "wireguard"},
		[]string{"/sbin/ip", "netns", "exec", nsDevPeer, "/usr/bin/wg", "setconf", "wg0", devPeerDir + "/wg0.conf"},
		[]string{"/sbin/ip", "-n", nsDevPeer, "addr", "add", devWGTunPeer + "/24", "dev", "wg0"},
		[]string{"/sbin/ip", "-n", nsDevPeer, "link", "set", "wg0", "up"},
	); err != nil {
		return devPeerUp{}, err
	}
	wgConf := fmt.Sprintf("[Interface]\nPrivateKey = %s\nAddress = %s/32\nDNS = %s\n\n[Peer]\nPublicKey = %s\nEndpoint = %s:51820\nAllowedIPs = 0.0.0.0/0\n",
		exitPriv, devWGTunExit, devWGTunPeer, peerPub, devPeerIP)

	// OpenVPN 服务端（UDP，推送 DNS）。
	pki, err := newProbePKI()
	if err != nil {
		return devPeerUp{}, err
	}
	p.pki, p.ovpnMode = pki, "normal"
	if err := p.startOVPNLocked(ctx); err != nil {
		return devPeerUp{}, err
	}
	ovpnConf := strings.Join([]string{
		"client", "dev tun", "proto udp", "remote " + devPeerIP + " 1194", "nobind", "remote-cert-tls server",
		"redirect-gateway def1", "verb 3", // verb 3：日志里有“推送项被过滤”的记录（验收 l）
		"<ca>\n" + pki.ca + "</ca>", "<cert>\n" + pki.clientCert + "</cert>", "<key>\n" + pki.clientKey + "</key>",
	}, "\n") + "\n"

	// socks 节点（订阅 mihomo 出口的节点）：就是 abroad-agent socks，在对端命名空间里直连。
	// 节点没有 DNS 文件，解析目标域名时退到 1.1.1.1——在对端里就是冒充的那个解析器，所以经节点解析的查询记在 pub 上。
	if err := p.startNodeLocked(ctx); err != nil {
		return devPeerUp{}, err
	}
	clashConf := fmt.Sprintf("proxies:\n  - {name: dev-node, type: socks5, server: %s, port: %d, udp: true}\n", devPeerIP, devNodePort)

	// 回显和 DNS：wg、ovpn 的隧道地址，节点的地址（订阅出口经节点连回显），以及冒充固定公共解析器的 1.1.1.1
	// （挂在对端的 lo 上：推送的 DNS 不可用时，出口退到 1.1.1.1，查询经隧道到这里，验收 m）。
	if err := p.sh(ctx, []string{"/sbin/ip", "-n", nsDevPeer, "addr", "add", devPubDNS + "/32", "dev", "lo"}); err != nil {
		return devPeerUp{}, err
	}
	err = p.Ops.InNetns(nsDevPeer, func() error {
		for _, s := range []struct {
			tag string
			ip  string
			dns bool
		}{{"wg", devWGTunPeer, true}, {"ovpn", devOVTunPeer, true}, {"node", devPeerIP, true}, {"pub", devPubDNS, true}} {
			if err := p.svc.add(s.tag, netip.MustParseAddr(s.ip), s.dns); err != nil {
				return fmt.Errorf("%s 的回显：%w", s.tag, err)
			}
		}
		// 冒充 DoH（1.1.1.1:443）：只计数到达的连接。订阅出口的 mihomo 万一要解析目标域名，按配置只经节点走 DoH，
		// 连接会经节点到这里（对端里没有真的 DoH 服务，解析会失败，但能看出走的是哪条路）。
		return p.svc.addAt("doh443", netip.AddrPortFrom(netip.MustParseAddr(devPubDNS), 443))
	})
	if err != nil {
		return devPeerUp{}, err
	}
	p.up = true
	return devPeerUp{Exits: []ExitSpec{
		{Tag: "wg", Kind: exitconf.WireGuard, Conf: wgConf},
		{Tag: "ovpn", Kind: exitconf.OpenVPN, Conf: ovpnConf},
		{Tag: "sub", Kind: exitconf.Clash, Conf: clashConf},
	}}, nil
}

// ovpnServerConf 是对端 OpenVPN 服务端的配置。每种模式都额外推送 setenv-safe、route、redirect-gateway
// （客户端的推送项白名单应把它们都忽略，验收 l）。DNS 按模式：
//   - normal：旧式 dhcp-option DNS（推送 10.78.0.1）；
//   - dns26：2.6 的 dns 推送项（push "dns server 0 address …"；只在服务端配置里写 dns 不会推送）；
//   - bad：只推不可用的地址（回环、fake 段、出口 veth 网段），客户端应退到固定的公共解析器（验收 m）。
func ovpnServerConf(pki probePKI, mode string) (string, error) {
	lines := []string{
		"port 1194", "proto udp", "local " + devPeerIP, "dev tun0", "topology subnet",
		"server 10.78.0.0 255.255.255.0", "dh none", "keepalive 2 10", "verb 1",
		`push "setenv-safe ABROAD_PUSHED 1"`, `push "route 10.99.0.0 255.255.0.0"`, `push "redirect-gateway def1"`,
	}
	switch mode {
	case "normal":
		lines = append(lines, `push "dhcp-option DNS `+devOVTunPeer+`"`)
	case "dns26":
		lines = append(lines, `push "dns server 0 address `+devOVTunPeer+`"`)
	case "bad":
		lines = append(lines, `push "dhcp-option DNS 127.0.0.1"`, `push "dhcp-option DNS 198.18.0.5"`, `push "dhcp-option DNS 10.200.2.1"`)
	default:
		return "", errors.New("模式只能是 normal、dns26、bad")
	}
	lines = append(lines, "<ca>\n"+pki.ca+"</ca>", "<cert>\n"+pki.serverCert+"</cert>", "<key>\n"+pki.serverKey+"</key>")
	return strings.Join(lines, "\n") + "\n", nil
}

func (p *DevPeer) startOVPNLocked(ctx context.Context) error {
	conf, err := ovpnServerConf(p.pki, p.ovpnMode)
	if err != nil {
		return err
	}
	if err := p.Ops.WriteFile(devPeerDir+"/ovpn-server.conf", []byte(conf)); err != nil {
		return err
	}
	stop, err := p.Ops.Start(ctx, "/sbin/ip", "netns", "exec", nsDevPeer, "/usr/sbin/openvpn", "--config", devPeerDir+"/ovpn-server.conf")
	if err != nil {
		return fmt.Errorf("起 OpenVPN 服务端：%w", err)
	}
	p.stopOVPN = stop
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := p.Ops.Run(ctx, "/sbin/ip", "-n", nsDevPeer, "-o", "addr", "show", "dev", "tun0")
		if strings.Contains(out, devOVTunPeer+"/") {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("OpenVPN 服务端的 tun0 没有起来")
}

// breakResult 是断开隧道后主动发包的结果：每一项都应失败（被断网规则拦下或没有路由）。
type breakResult struct {
	Attempts map[string]string `json:"attempts"` // 目标 → 错误（空表示居然成功了）
}

func (p *DevPeer) breakExit(ctx context.Context, tag string) (breakResult, error) {
	info, ok := p.Exits.devInfo(tag)
	if !ok || info.Dev == "" {
		return breakResult{}, errors.New("没有这个隧道出口")
	}
	switch tag {
	case "wg":
		if err := p.sh(ctx, []string{"/sbin/ip", "-n", info.NS, "link", "del", info.Dev}); err != nil {
			return breakResult{}, err
		}
	case "ovpn":
		p.mu.Lock()
		if p.stopOVPN != nil {
			p.stopOVPN()
			p.stopOVPN = nil
		}
		p.mu.Unlock()
		// 客户端发现服务端没了会重连（引擎可能重启它）；隧道设备删掉，模拟隧道已断。
		_, _ = p.Ops.Run(ctx, "/sbin/ip", "-n", info.NS, "link", "del", info.Dev)
	default:
		return breakResult{}, errors.New("只能断开 wg、ovpn")
	}
	gw := fmt.Sprintf("10.200.%d.1", info.N)
	// 最坏情况：默认路由退回 veth。
	if err := p.sh(ctx, []string{"/sbin/ip", "-n", info.NS, "route", "replace", "default", "via", gw}); err != nil {
		return breakResult{}, err
	}
	res := breakResult{Attempts: map[string]string{}}
	for _, a := range []struct{ kind, addr string }{
		{"tcp-connect", devPeerIP + ":7"}, {"udp-send", devPeerIP + ":53"}, {"udp-send", devPeerIP + ":7"},
		{"udp-send", "1.1.1.1:53"}, {"tcp-connect", "1.1.1.1:443"}, {"tcp-connect", devWGTunPeer + ":7"}, {"udp-send", devOVTunPeer + ":53"},
	} {
		err := p.Ops.Probe(info.NS, a.kind, a.addr)
		msg := ""
		if err != nil {
			msg = firstLine(err.Error())
		}
		res.Attempts[a.kind+" "+a.addr] = msg
	}
	return res, nil
}

func (p *DevPeer) restore(ctx context.Context, tag string) error {
	switch tag {
	case "wg":
		return p.Exits.devRebuild(ctx, "wg")
	case "ovpn":
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.stopOVPN == nil {
			if err := p.startOVPNLocked(ctx); err != nil {
				return err
			}
		}
		return nil
	}
	return errors.New("只能恢复 wg、ovpn")
}

func (p *DevPeer) kill(ctx context.Context, tag string) error {
	info, ok := p.Exits.devInfo(tag)
	if !ok || info.Dev == "" {
		return errors.New("没有这个隧道出口")
	}
	// 只匹配这个出口的 socks 进程（监听地址唯一）。
	_, err := p.Ops.Run(ctx, "/usr/bin/pkill", "-f", fmt.Sprintf("socks --listen 10.200.%d.2:", info.N))
	return err
}

func (p *DevPeer) teardown() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.teardownLocked()
}

func (p *DevPeer) teardownLocked() {
	p.svc.close()
	if p.stopOVPN != nil {
		p.stopOVPN()
		p.stopOVPN = nil
	}
	if p.stopNode != nil {
		p.stopNode()
		p.stopNode = nil
	}
	for _, f := range p.stops {
		f()
	}
	p.stops = nil
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = p.Ops.Run(ctx, "/sbin/ip", "netns", "del", nsDevPeer)
	_, _ = p.Ops.Run(ctx, "/sbin/ip", "link", "del", devUplinkA)
	_, _ = p.Ops.Run(ctx, "/usr/sbin/nft", "delete", "table", "inet", "abx_root_out")
	_ = p.Ops.RemoveAll(devPeerDir)
	p.up = false
}

// devPeerStats 是 GET 的应答。
type devPeerStats struct {
	Up        bool                    `json:"up"`
	Peer      map[string]int64        `json:"peer"`       // 对端的计数（注释名 → 包数）
	DNS       map[string]int64        `json:"dns"`        // 对端各地址收到的 DNS 查询
	Echo      map[string]int64        `json:"echo"`       // 对端各回显收到的连接和数据报
	RootDNS53 int64                   `json:"root_dns53"` // 根命名空间从出口 veth 转发的 53 端口包
	RootEth0  int64                   `json:"root_eth0"`  // 根命名空间从出口 veth 转发到 eth0 的包（端到端里对端都在虚拟机内，应为 0）
	RootOut53 int64                   `json:"root_out53"` // 根命名空间自己发往 eth0 的 53 端口包（mihomo 本地解析会记在这里）
	RootOut   int64                   `json:"root_out"`   // 根命名空间自己发往 eth0 的全部包（直连会记在这里）
	Routes    map[string][]string     `json:"routes"`     // 各出口命名空间的路由表
	OVPNDNS   []string                `json:"ovpn_dns"`   // ovpn 出口目录里的 dns 文件（只有地址）
	PushLog   []string                `json:"push_log"`   // ovpn 客户端日志里和推送项相关的行（已脱敏）
	Dropped   map[string]int64        `json:"dropped"`    // 各出口命名空间被断网规则拦下的包
	Netns     []string                `json:"netns"`      // 现存的命名空间
	Links     []string                `json:"links"`      // 根命名空间里 vx*、abxp* 网卡
	Files     map[string]string       `json:"files"`      // 出口目录里的文件 → 权限（不含内容）
	Status    []ExitStatus            `json:"status"`
	Engines   map[string]EngineStatus `json:"engines"` // 各出口引擎（重启次数、起始时间）
}

var nftCounterRe = regexp.MustCompile(`counter packets (\d+) bytes \d+(?: comment "([a-z_0-9]+)")?`)

// parseNFTCounters 取 `nft list` 输出里的计数：有注释的按注释名，没有的按出现顺序编号（#0、#1…）。
func parseNFTCounters(out string) map[string]int64 {
	m := map[string]int64{}
	i := 0
	for _, mm := range nftCounterRe.FindAllStringSubmatch(out, -1) {
		v, _ := strconv.ParseInt(mm[1], 10, 64)
		name := mm[2]
		if name == "" {
			name = "#" + strconv.Itoa(i)
			i++
		}
		m[name] += v
	}
	return m
}

func (p *DevPeer) stats(ctx context.Context) devPeerStats {
	st := devPeerStats{Peer: map[string]int64{}, Dropped: map[string]int64{}, Files: map[string]string{}}
	p.mu.Lock()
	st.Up = p.up
	p.mu.Unlock()
	st.Echo, st.DNS = p.svc.counts()
	if out, err := p.Ops.Run(ctx, "/sbin/ip", "netns", "exec", nsDevPeer, "/usr/sbin/nft", "list", "table", "inet", "abx_peer"); err == nil {
		st.Peer = parseNFTCounters(out)
	}
	if out, err := p.Ops.Run(ctx, "/usr/sbin/nft", "list", "table", "ip", "abroad_exit_nat"); err == nil {
		c := parseNFTCounters(out)
		st.RootDNS53, st.RootEth0 = c["#0"], c["#1"]
	}
	if out, err := p.Ops.Run(ctx, "/usr/sbin/nft", "list", "table", "inet", "abx_root_out"); err == nil {
		c := parseNFTCounters(out)
		st.RootOut53, st.RootOut = c["out53"], c["out_all"]
	}
	if out, err := p.Ops.Run(ctx, "/sbin/ip", "netns", "list"); err == nil {
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Fields(l); len(f) > 0 {
				st.Netns = append(st.Netns, f[0])
			}
		}
	}
	for _, ns := range st.Netns {
		if !strings.HasPrefix(ns, "ex-") {
			continue
		}
		if out, err := p.Ops.Run(ctx, "/sbin/ip", "netns", "exec", ns, "/usr/sbin/nft", "list", "table", "inet", "abroad_exit"); err == nil {
			c := parseNFTCounters(out) // 最后一个计数是 counter drop；端点的放行计数在前面
			st.Dropped[strings.TrimPrefix(ns, "ex-")] = c["#"+strconv.Itoa(len(c)-1)]
		}
	}
	if out, err := p.Ops.Run(ctx, "/sbin/ip", "-o", "link", "show"); err == nil {
		for _, l := range strings.Split(out, "\n") {
			f := strings.Fields(l)
			if len(f) < 2 {
				continue
			}
			name := strings.TrimSuffix(f[1], ":")
			if i := strings.IndexByte(name, '@'); i >= 0 {
				name = name[:i]
			}
			if strings.HasPrefix(name, "vx") || strings.HasPrefix(name, "abxp") {
				st.Links = append(st.Links, name)
			}
		}
	}
	_ = filepath.WalkDir(ExitsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			kind := "f"
			if d.IsDir() {
				kind = "d"
			}
			st.Files[path] = fmt.Sprintf("%s%04o", kind, fi.Mode().Perm())
		}
		return nil
	})
	sort.Strings(st.Netns)
	sort.Strings(st.Links)
	st.Status = p.Exits.Status()
	st.Engines = p.Exits.devEngines()
	st.Routes = map[string][]string{}
	for _, ns := range st.Netns {
		if strings.HasPrefix(ns, "ex-") {
			if out, err := p.Ops.Run(ctx, "/sbin/ip", "-n", ns, "route", "show"); err == nil {
				st.Routes[strings.TrimPrefix(ns, "ex-")] = strings.Split(strings.TrimSpace(out), "\n")
			}
		}
	}
	if b, err := os.ReadFile(ExitsDir + "/ovpn/dns"); err == nil {
		st.OVPNDNS = strings.Fields(string(b))
	}
	for _, l := range p.Exits.devEngineLog("openvpn-ovpn") {
		if strings.Contains(l, "filter") || strings.Contains(l, "PUSH") || strings.Contains(l, "OPTIONS IMPORT") {
			st.PushLog = append(st.PushLog, l)
		}
	}
	if len(st.PushLog) > 60 {
		st.PushLog = st.PushLog[len(st.PushLog)-60:]
	}
	return st
}

func (p *DevPeer) setOVPNMode(ctx context.Context, mode string) error {
	if _, err := ovpnServerConf(p.pki, mode); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopOVPN != nil {
		p.stopOVPN()
		p.stopOVPN = nil
	}
	p.ovpnMode = mode
	return p.startOVPNLocked(ctx)
}

// rootOutNFT 是根命名空间 output 的计数规则（只计数，不拦）。
const rootOutNFT = `table inet abx_root_out {
  chain output {
    type filter hook output priority 0; policy accept;
    oifname "eth0" meta l4proto { tcp, udp } th dport 53 counter comment "out53"
    oifname "eth0" counter comment "out_all"
  }
}
`

func (p *DevPeer) startNodeLocked(ctx context.Context) error {
	stop, err := p.Ops.Start(ctx, "/sbin/ip", "netns", "exec", nsDevPeer, guestabi.AgentPath, "socks",
		"--listen", fmt.Sprintf("%s:%d", devPeerIP, devNodePort), "--dns-file", devPeerDir+"/none")
	if err != nil {
		return fmt.Errorf("起 socks 节点：%w", err)
	}
	p.stopNode = stop
	return nil
}
