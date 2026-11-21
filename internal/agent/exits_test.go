package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"claude-abroad/internal/exitconf"
)

const exKey = "YW5vdGhlcjMyYnl0ZXNsb25na2V5Zm9ydGVzdGluZzE="

var exWG = "[Interface]\nPrivateKey = " + exKey + "\nAddress = 10.66.0.2/32\nDNS = 10.66.0.1\n\n[Peer]\nPublicKey = " + exKey +
	"\nEndpoint = vpn.example.com:51820\nAllowedIPs = 0.0.0.0/0\n"

var exOVPN = "client\ndev tun\nproto udp\nremote 203.0.113.9 1194\nnobind\nremote-cert-tls server\nredirect-gateway def1\n" +
	"<ca>\n-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n</ca>\n<auth-user-pass>\nuser\nsecret-pass\n</auth-user-pass>\n"

const exClash = `
proxies:
  - {name: 香港 01, type: ss, server: hk.example.com, port: 8388, cipher: aes-256-gcm, password: sub-secret}
`

// fakeExitOps 记录命令和文件；failOn 命中的命令返回错误。
type fakeExitOps struct {
	mu      sync.Mutex
	cmds    []string
	files   map[string][]byte
	perms   map[string]fs.FileMode
	dirs    map[string]bool
	failOn  string
	links   map[string]bool // ns/dev
	hs      time.Time
	resolve map[string]string
	resolves int
}

func newFakeExitOps() *fakeExitOps {
	return &fakeExitOps{files: map[string][]byte{}, perms: map[string]fs.FileMode{}, dirs: map[string]bool{},
		links: map[string]bool{}, resolve: map[string]string{"vpn.example.com": "203.0.113.5"}}
}

func (o *fakeExitOps) Run(_ context.Context, argv ...string) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	c := strings.Join(argv, " ")
	o.cmds = append(o.cmds, c)
	if o.failOn != "" && strings.Contains(c, o.failOn) {
		return "boom secret-pass", errors.New("exit status 1")
	}
	return "", nil
}

func (o *fakeExitOps) MkdirPrivate(p string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.dirs[p] = true
	return nil
}

func (o *fakeExitOps) WriteFile(p string, data []byte, perm fs.FileMode) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.files[p] = append([]byte(nil), data...)
	o.perms[p] = perm
	return nil
}

func (o *fakeExitOps) RemoveAll(p string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.dirs, p)
	for f := range o.files {
		if strings.HasPrefix(f, p+"/") {
			delete(o.files, f)
		}
	}
	return nil
}

// ResolveAll：resolve 里的值可以是逗号分隔的多个地址。
func (o *fakeExitOps) ResolveAll(_ context.Context, host string) ([]netip.Addr, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.resolves++
	if a, ok := o.resolve[host]; ok {
		var out []netip.Addr
		for _, x := range strings.Split(a, ",") {
			out = append(out, netip.MustParseAddr(x))
		}
		return out, nil
	}
	return nil, errors.New("nx")
}

func (o *fakeExitOps) LinkUp(ns, dev string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.links[ns+"/"+dev]
}

func (o *fakeExitOps) WGHandshake(context.Context, string, string) (time.Time, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.hs, nil
}

func (o *fakeExitOps) take() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	c := o.cmds
	o.cmds = nil
	return c
}

func (o *fakeExitOps) set(f func()) {
	o.mu.Lock()
	defer o.mu.Unlock()
	f()
}

type exitsHarness struct {
	x       *Exits
	ops     *fakeExitOps
	r       *fakeRunner
	changes [][]SingBoxExit
	now     time.Time
	mu      sync.Mutex
}

func newExitsHarness(t *testing.T) *exitsHarness {
	h := &exitsHarness{ops: newFakeExitOps(), r: newFakeRunner(), now: time.Unix(1_800_000_000, 0)}
	h.r.ready.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	h.x = &Exits{Ops: h.ops, Runner: h.r, Timing: testEngineTiming, Base: ctx, SelfExe: "/usr/bin/abroad-agent",
		DialReady: func(context.Context, string) error { return nil },
		OnChange: func(l []SingBoxExit) error {
			h.mu.Lock()
			h.changes = append(h.changes, l)
			h.mu.Unlock()
			return nil
		},
		Now: func() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.now }}
	go func() { // 不让 fakeRunner 的通道塞满
		for range h.r.procs {
		}
	}()
	t.Cleanup(func() { h.x.Close(); cancel() })
	return h
}

func (h *exitsHarness) nChanges() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.changes)
}

func (h *exitsHarness) advance(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	h.mu.Unlock()
}

func (h *exitsHarness) started(argv string) bool {
	h.r.mu.Lock()
	defer h.r.mu.Unlock()
	for _, a := range h.r.argvs {
		if strings.Join(a, " ") == argv {
			return true
		}
	}
	return false
}

func hasCmd(cmds []string, sub string) bool {
	for _, c := range cmds {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

// 等引擎都进入运行状态（假 runner 立即就绪）。
func waitExitUp(t *testing.T, h *exitsHarness, tag string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.advance(3 * time.Second) // 让状态缓存过期
		if _, up := h.x.Lookup(tag); up {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("出口 %s 没有变为可用：%+v", tag, h.x.Status())
}

// a/c. WireGuard 出口：命名空间、veth、到端点的路由、断网规则、wg setconf；凭据 0600 在自己的目录；sing-box 指向 veth。
func TestExitsWireGuardSetup(t *testing.T) {
	h := newExitsHarness(t)
	h.ops.set(func() { h.ops.links["ex-hk/wg1"] = true; h.ops.hs = h.now })
	if err := h.x.Apply(context.Background(), []ExitSpec{{Tag: "hk", Kind: exitconf.WireGuard, Conf: exWG}}); err != nil {
		t.Fatal(err)
	}
	cmds := h.ops.take()
	for _, want := range []string{
		"nft -f " + ExitsDir + "/root-nat.conf", "ip netns add ex-hk", "veth peer name vx1b", "addr add 10.200.1.1/30 dev vx1a", "-n ex-hk addr add 10.200.1.2/30 dev vx1b",
		"route replace 203.0.113.5/32 via 10.200.1.1", "nft -f " + ExitsDir + "/hk/nft.conf",
		"link add wg1 type wireguard", "wg setconf wg1 " + ExitsDir + "/hk/wg.conf", "addr add 10.66.0.2/32 dev wg1", "route replace default dev wg1",
	} {
		if !hasCmd(cmds, want) {
			t.Fatalf("缺少命令 %q：\n%s", want, strings.Join(cmds, "\n"))
		}
	}
	wg := string(h.ops.files[ExitsDir+"/hk/wg.conf"])
	if !strings.Contains(wg, "Endpoint = 203.0.113.5:51820") || !strings.Contains(wg, "PersistentKeepalive = 25") || strings.Contains(wg, "vpn.example.com") {
		t.Fatalf("wg.conf：\n%s", wg)
	}
	if string(h.ops.files["/proc/sys/net/ipv4/ip_forward"]) != "1\n" {
		t.Fatal("应打开 ip_forward")
	}
	root := string(h.ops.files[ExitsDir+"/root-nat.conf"])
	for _, want := range []string{"policy drop", `iifname "vx*" oifname "eth0" ip saddr 10.200.0.0/16 counter accept`, "masquerade"} {
		if !strings.Contains(root, want) {
			t.Fatalf("根命名空间规则缺 %q：\n%s", want, root)
		}
	}
	nft := string(h.ops.files[ExitsDir+"/hk/nft.conf"])
	for _, want := range []string{"policy drop", `oif "lo" accept`, `oifname "vx1b" ip daddr 10.200.1.1 accept`, `oifname "wg1" accept`, "ip daddr 203.0.113.5 udp dport 51820"} {
		if !strings.Contains(nft, want) {
			t.Fatalf("断网规则缺 %q：\n%s", want, nft)
		}
	}
	if strings.Contains(nft, "ct state") {
		t.Fatal("不应按 conntrack 放行：隧道断开后旧连接会从 veth 出去")
	}
	if strings.Contains(nft, "dport 53") {
		t.Fatal("53 端口不应单独放行")
	}
	if strings.TrimSpace(string(h.ops.files[ExitsDir+"/hk/dns"])) != "10.66.0.1" {
		t.Fatalf("dns：%q", h.ops.files[ExitsDir+"/hk/dns"])
	}
	for p, m := range h.ops.perms {
		if strings.HasPrefix(p, ExitsDir+"/") && m != 0o600 {
			t.Fatalf("%s 的权限 %o", p, m)
		}
	}
	if h.nChanges() != 1 || len(h.changes[0]) != 1 || h.changes[0][0] != (SingBoxExit{Tag: "hk", Port: 1080, Server: "10.200.1.2"}) {
		t.Fatalf("sing-box 出口：%+v", h.changes)
	}
	// socks 在命名空间里运行，解析读出口自己的 dns 文件
	waitExitUp(t, h, "hk")
	if !h.started("/sbin/ip netns exec ex-hk /usr/bin/abroad-agent socks --listen 10.200.1.2:1080 --dns-file " + ExitsDir + "/hk/dns") {
		t.Fatal("socks 的命令不对")
	}
}

// h. 状态：隧道设备不在、3 分钟内没有握手都算不可用；health 里没有端点和配置。
func TestExitsStatus(t *testing.T) {
	h := newExitsHarness(t)
	h.ops.set(func() { h.ops.links["ex-hk/wg1"] = true; h.ops.hs = h.now })
	if err := h.x.Apply(context.Background(), []ExitSpec{{Tag: "hk", Kind: exitconf.WireGuard, Conf: exWG}}); err != nil {
		t.Fatal(err)
	}
	waitExitUp(t, h, "hk")
	h.advance(WGHandshakeMax + time.Second)
	if _, up := h.x.Lookup("hk"); up {
		t.Fatal("超过 3 分钟没有握手应不可用")
	}
	h.ops.set(func() { h.ops.hs = h.now })
	h.advance(3 * time.Second)
	if _, up := h.x.Lookup("hk"); !up {
		t.Fatal("刚握手应可用")
	}
	h.ops.set(func() { delete(h.ops.links, "ex-hk/wg1") })
	h.advance(3 * time.Second)
	if known, up := h.x.Lookup("hk"); !known || up {
		t.Fatal("隧道设备不在应不可用")
	}
	if known, _ := h.x.Lookup("jp"); known {
		t.Fatal("没有的出口")
	}
	b, _ := json.Marshal(h.x.Status())
	for _, bad := range []string{"203.0.113", "vpn.example", exKey, "10.66"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("状态里不应有 %q：%s", bad, b)
		}
	}
	// 状态有缓存：缓存期内不重复查握手
	h.ops.set(func() { h.ops.links["ex-hk/wg1"] = true })
	h.advance(3 * time.Second)
	h.x.Lookup("hk")
	h.ops.set(func() { delete(h.ops.links, "ex-hk/wg1") })
	if _, up := h.x.Lookup("hk"); !up {
		t.Fatal("缓存期内应沿用上次的结果")
	}
}

// i. 幂等：同样的内容再下发什么都不做；改一个只重建那一个；删掉的拆干净。
func TestExitsIdempotent(t *testing.T) {
	h := newExitsHarness(t)
	specs := []ExitSpec{{Tag: "hk", Kind: exitconf.WireGuard, Conf: exWG}, {Tag: "jp", Kind: exitconf.Clash, Conf: exClash}}
	if err := h.x.Apply(context.Background(), specs); err != nil {
		t.Fatal(err)
	}
	h.ops.take()
	if err := h.x.Apply(context.Background(), specs); err != nil {
		t.Fatal(err)
	}
	if c := h.ops.take(); len(c) != 0 || h.nChanges() != 1 {
		t.Fatalf("重复下发不应有动作：%v，%d", c, h.nChanges())
	}
	// 改 hk
	specs[0].Conf = strings.Replace(exWG, "10.66.0.2/32", "10.66.0.3/32", 1)
	if err := h.x.Apply(context.Background(), specs); err != nil {
		t.Fatal(err)
	}
	c := h.ops.take()
	if !hasCmd(c, "netns del ex-hk") || !hasCmd(c, "addr add 10.66.0.3/32") || hasCmd(c, "jp") {
		t.Fatalf("只应重建 hk：\n%s", strings.Join(c, "\n"))
	}
	if h.nChanges() != 2 {
		t.Fatal("出口变了应更新 sing-box")
	}
	// 删光
	if err := h.x.Apply(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if hasCmd(h.ops.take(), "netns del ex-jp") {
		t.Fatal("订阅出口没有命名空间")
	}
	for d := range h.ops.dirs {
		if d != ExitsDir {
			t.Fatalf("目录没删：%s", d)
		}
	}
	for f := range h.ops.files {
		if strings.HasPrefix(f, ExitsDir+"/") && f != ExitsDir+"/root-nat.conf" {
			t.Fatalf("文件没删：%s", f)
		}
	}
	if h.nChanges() != 3 || len(h.changes[2]) != 0 {
		t.Fatalf("%+v", h.changes)
	}
}

// f. 配置不合法：整体拒绝，什么都不改；建不起来的出口拆干净、不留目录，其他出口照常。
func TestExitsFailures(t *testing.T) {
	h := newExitsHarness(t)
	bad := strings.Replace(exWG, "[Peer]", "PostUp = iptables -F\n[Peer]", 1)
	err := h.x.Apply(context.Background(), []ExitSpec{{Tag: "hk", Kind: exitconf.WireGuard, Conf: exWG}, {Tag: "jp", Kind: exitconf.WireGuard, Conf: bad}})
	if err == nil || len(h.ops.take()) != 0 || len(h.ops.dirs) != 0 || h.nChanges() != 0 {
		t.Fatalf("不合法应整体拒绝：%v", err)
	}
	if strings.Contains(err.Error(), "iptables") {
		t.Fatal("错误里不应回显配置内容")
	}
	for _, s := range []ExitSpec{{Tag: "main", Kind: exitconf.WireGuard, Conf: exWG}, {Tag: "Bad", Kind: exitconf.WireGuard, Conf: exWG}, {Tag: "x", Kind: "socks", Conf: ""}} {
		if err := h.x.Apply(context.Background(), []ExitSpec{s}); err == nil {
			t.Fatalf("%+v 应被拒", s)
		}
	}
	if err := h.x.Apply(context.Background(), []ExitSpec{{Tag: "a", Kind: exitconf.WireGuard, Conf: exWG}, {Tag: "a", Kind: exitconf.WireGuard, Conf: exWG}}); err == nil {
		t.Fatal("重复的标签应被拒")
	}

	h.ops.set(func() { h.ops.failOn = "wg setconf wg1" })
	err = h.x.Apply(context.Background(), []ExitSpec{{Tag: "hk", Kind: exitconf.WireGuard, Conf: exWG}, {Tag: "jp", Kind: exitconf.OpenVPN, Conf: exOVPN}})
	if err == nil || !strings.Contains(err.Error(), "出口 hk") {
		t.Fatalf("hk 应失败：%v", err)
	}
	if strings.Contains(err.Error(), "secret-pass") {
		t.Fatal("错误里的命令输出要脱敏")
	}
	c := h.ops.take()
	if !hasCmd(c, "netns del ex-hk") || !hasCmd(c, "link del vx1a") {
		t.Fatalf("失败的出口要拆掉：\n%s", strings.Join(c, "\n"))
	}
	if h.ops.dirs[ExitsDir+"/hk"] {
		t.Fatal("失败的出口不应留目录")
	}
	if l := h.x.SingBoxExits(); len(l) != 1 || l[0].Tag != "jp" || l[0].Server != "10.200.1.2" {
		t.Fatalf("jp 应照常建立：%+v", l)
	}

	// 域名解析失败
	h.ops.set(func() { h.ops.failOn = ""; h.ops.resolve = map[string]string{} })
	if err := h.x.Apply(context.Background(), []ExitSpec{{Tag: "hk", Kind: exitconf.WireGuard, Conf: exWG}}); err == nil || !strings.Contains(err.Error(), "解析") {
		t.Fatalf("%v", err)
	}
}

// d. OpenVPN：配置重新生成（route-noexec 加推送项过滤、DNS 脚本、出口目录），在命名空间里运行；隧道起来后设默认路由。
func TestExitsOpenVPN(t *testing.T) {
	h := newExitsHarness(t)
	h.ops.set(func() { h.ops.links["ex-vpn/tun1"] = true })
	if err := h.x.Apply(context.Background(), []ExitSpec{{Tag: "vpn", Kind: exitconf.OpenVPN, Conf: exOVPN}}); err != nil {
		t.Fatal(err)
	}
	conf := string(h.ops.files[ExitsDir+"/vpn/client.conf"])
	for _, want := range []string{"route-noexec", `pull-filter ignore ""`, "dev tun1", "up " + OVPNDNSScript, "setenv ABROAD_EXIT_DIR " + ExitsDir + "/vpn"} {
		if !strings.Contains(conf, want) {
			t.Fatalf("client.conf 缺 %q：\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "redirect-gateway") {
		t.Fatal("redirect-gateway 应去掉")
	}
	if h.ops.perms[OVPNDNSScript] != 0o700 || !strings.Contains(string(h.ops.files[OVPNDNSScript]), "ABROAD_EXIT_DIR") {
		t.Fatal("DNS 脚本")
	}
	waitExitUp(t, h, "vpn")
	if !hasCmd(h.ops.take(), "-n ex-vpn route replace default dev tun1") {
		t.Fatal("隧道起来后应设默认路由")
	}
	if !h.started("/sbin/ip netns exec ex-vpn /usr/sbin/openvpn --config " + ExitsDir + "/vpn/client.conf") {
		t.Fatal("openvpn 应在命名空间里运行")
	}
}

// e. 订阅 mihomo：端口 7800+n，配置按主代理的形状生成，不建命名空间。
func TestExitsClash(t *testing.T) {
	h := newExitsHarness(t)
	if err := h.x.Apply(context.Background(), []ExitSpec{{Tag: "sub", Kind: exitconf.Clash, Conf: exClash}}); err != nil {
		t.Fatal(err)
	}
	if hasCmd(h.ops.take(), "netns") {
		t.Fatal("订阅出口不建命名空间")
	}
	conf := string(h.ops.files[ExitsDir+"/sub/config.yaml"])
	if !strings.Contains(conf, "7801") || strings.Contains(conf, "香港") {
		t.Fatalf("config.yaml：\n%s", conf)
	}
	if l := h.x.SingBoxExits(); len(l) != 1 || l[0] != (SingBoxExit{Tag: "sub", Port: 7801}) {
		t.Fatalf("%+v", l)
	}
	waitExitUp(t, h, "sub")
}

// HTTP：PUT 校验失败 422 且不回显；坏请求体 400；GET 给状态。
func TestExitsHTTP(t *testing.T) {
	h := newExitsHarness(t)
	put := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.x.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/v1/exits", strings.NewReader(body)))
		return w
	}
	if w := put(`{"exits":[],"x":1}`); w.Code != 400 {
		t.Fatal(w.Code)
	}
	body, _ := json.Marshal(exitsReq{Exits: []ExitSpec{{Tag: "hk", Kind: exitconf.WireGuard, Conf: strings.Replace(exWG, "[Peer]", "PostUp = iptables -F\n[Peer]", 1)}}})
	if w := put(string(body)); w.Code != 422 || strings.Contains(w.Body.String(), "iptables") {
		t.Fatal(w.Code, w.Body.String())
	}
	body, _ = json.Marshal(exitsReq{Exits: []ExitSpec{{Tag: "sub", Kind: exitconf.Clash, Conf: exClash}}})
	if w := put(string(body)); w.Code != 200 || strings.Contains(w.Body.String(), "sub-secret") || !strings.Contains(w.Body.String(), `"tag":"sub"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w := httptest.NewRecorder()
	h.x.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/exits", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"kind":"clash"`) {
		t.Fatal(w.Body.String())
	}
}

func TestExitsTable(t *testing.T) {
	h := newExitsHarness(t)
	if err := h.x.Apply(context.Background(), []ExitSpec{{Tag: "sub", Kind: exitconf.Clash, Conf: exClash}}); err != nil {
		t.Fatal(err)
	}
	waitExitUp(t, h, "sub")
	tb := exitsTable{base: staticTable{"main": true}, ex: h.x}
	if k, u := tb.Lookup("sub"); !k || !u {
		t.Fatal("sub")
	}
	if k, u := tb.Lookup("main"); !k || !u {
		t.Fatal("main")
	}
	if k, _ := tb.Lookup("nope"); k {
		t.Fatal("nope")
	}
}

type staticTable map[string]bool

func (s staticTable) Lookup(tag string) (bool, bool) { up, ok := s[tag]; return ok, up }

// l. 服务器域名的重新解析：每 5 分钟（定时器注入）；地址变了就重建一次（断网规则和引擎配置用新地址）；没变不动；
// 握手失败触发重新解析；解析出的地址落在被禁范围时跳过，全被禁时出口不可用；之后能解析出好地址时自动建起来。
func TestExitsReResolve(t *testing.T) {
	h := newExitsHarness(t)
	tick := make(chan time.Time)
	h.x.After = func(time.Duration) <-chan time.Time { return tick }
	h.ops.set(func() { h.ops.links["ex-hk/wg1"] = true; h.ops.hs = h.now; h.ops.resolve["vpn.example.com"] = "127.0.0.1,198.18.0.1,203.0.113.5" })
	if err := h.x.Apply(context.Background(), []ExitSpec{{Tag: "hk", Kind: exitconf.WireGuard, Conf: exWG}}); err != nil {
		t.Fatal(err)
	}
	if nft := string(h.ops.files[ExitsDir+"/hk/nft.conf"]); !strings.Contains(nft, "ip daddr 203.0.113.5 udp dport 51820") || strings.Contains(nft, "127.0.0.1") {
		t.Fatalf("应跳过回环和 fake 段，用第一个能放行的地址：\n%s", nft)
	}
	h.ops.take()
	// 5 分钟到了，地址没变：不重建
	tick <- time.Now()
	waitCond(t, "第一次重新解析", func() bool { h.ops.mu.Lock(); defer h.ops.mu.Unlock(); return h.ops.resolves >= 2 })
	time.Sleep(20 * time.Millisecond)
	if h.x.rebuilds.Load() != 0 || hasCmd(h.ops.take(), "netns del") {
		t.Fatal("地址没变时不重建")
	}
	// 地址变了：重建一次，断网规则和 wg 配置用新地址
	h.ops.set(func() { h.ops.resolve["vpn.example.com"] = "203.0.113.77" })
	tick <- time.Now()
	waitCond(t, "重建", func() bool { return h.x.rebuilds.Load() == 1 })
	h.ops.mu.Lock()
	nft := string(h.ops.files[ExitsDir+"/hk/nft.conf"])
	wg := string(h.ops.files[ExitsDir+"/hk/wg.conf"])
	h.ops.mu.Unlock()
	if !strings.Contains(nft, "203.0.113.77") || strings.Contains(nft, "203.0.113.5 ") || !strings.Contains(wg, "Endpoint = 203.0.113.77:51820") {
		t.Fatalf("新地址：\n%s\n%s", nft, wg)
	}
	if c := h.ops.take(); !hasCmd(c, "netns del ex-hk") || !hasCmd(c, "wg setconf") {
		t.Fatal("重建：拆掉再建（引擎重启一次）")
	}
	// 握手失败触发重新解析（不用等 5 分钟）
	h.ops.mu.Lock()
	r0 := h.ops.resolves
	h.ops.mu.Unlock()
	h.advance(WGHandshakeMax + time.Second)
	h.x.Lookup("hk")
	waitCond(t, "握手失败触发重新解析", func() bool { h.ops.mu.Lock(); defer h.ops.mu.Unlock(); return h.ops.resolves > r0 })
	// 解析出的地址全被禁：出口不可用，状态里有原因（不含地址）
	h.ops.set(func() { h.ops.resolve["vpn.example.com"] = "127.0.0.1,160.79.104.5" })
	tick <- time.Now()
	waitCond(t, "全被禁后不可用", func() bool { k, _ := h.x.Lookup("hk"); return !k })
	st := h.x.Status()
	if len(st) != 1 || st[0].Up || !strings.Contains(st[0].Reason, "都不能用") || strings.Contains(st[0].Reason, "127.0.0.1") {
		t.Fatalf("%+v", st)
	}
	if hasCmd(h.ops.take(), "160.79.104.5") {
		t.Fatal("被禁的地址不能进任何规则")
	}
	// 之后解析出好地址：自动建起来
	h.ops.set(func() { h.ops.resolve["vpn.example.com"] = "203.0.113.88"; h.ops.hs = h.now })
	tick <- time.Now()
	waitCond(t, "重试后建起来", func() bool { k, _ := h.x.Lookup("hk"); return k })
}

func waitCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等不到：%s", what)
}
