package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"claude-abroad/internal/kmod"
)

// fakeProbe 模拟虚拟机里的命名空间：记下断网规则是否加载、隧道是否断开、端点规则的计数。
type fakeProbe struct {
	mu         sync.Mutex
	cmds       []string
	files      map[string]string
	ks         map[string]bool
	broken     map[string]bool
	counter    map[string]int
	clientUp   bool
	removed    bool
	nftNoOp    bool // 模拟“断网规则加载了但不起作用”
	failModule string
}

func newFakeProbe() *fakeProbe {
	return &fakeProbe{files: map[string]string{}, ks: map[string]bool{}, broken: map[string]bool{}, counter: map[string]int{}}
}

func (f *fakeProbe) Run(_ context.Context, argv ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := strings.Join(argv, " ")
	f.cmds = append(f.cmds, c)
	ns := ""
	for i, a := range argv {
		if (a == "-n" || a == "exec") && i+1 < len(argv) {
			ns = argv[i+1]
		}
	}
	switch {
	case strings.HasSuffix(c, "wg --version"):
		return "wireguard-tools v1.0.20250521", nil
	case strings.HasSuffix(c, "openvpn --version"):
		return "OpenVPN 2.6.20 aarch64-alpine-linux-musl", errors.New("exit status 1")
	case strings.HasSuffix(c, "nft --version"):
		return "nftables v1.1.3", nil
	case strings.HasSuffix(c, "ip -V"):
		return "ip utility, iproute2-6.15.0", nil
	case strings.HasSuffix(c, "locale -a"):
		return "C.UTF-8\nen_US.UTF-8\nja_JP.UTF-8\nzh_CN.UTF-8\n", nil
	case strings.Contains(c, "addr show dev tun0"):
		if ns == nsPeer {
			return "inet 10.203.0.1/24 scope global tun0", nil
		}
		if f.clientUp {
			return "inet 10.203.0.2/24 scope global tun0", nil
		}
		return "", errors.New("没有 tun0")
	case strings.Contains(c, "link show tun0"):
		if f.clientUp {
			return "tun0: <UP>", nil
		}
		return "", errors.New("Device does not exist")
	case strings.Contains(c, "route get 1.1.1.1"):
		if ns == nsWG {
			return "1.1.1.1 dev wg1 table main src 10.202.0.2", nil
		}
		return "1.1.1.1 dev tun0 table main src 10.203.0.2", nil
	case strings.Contains(c, "nft -f"):
		if !f.nftNoOp {
			f.ks[ns] = true
		}
	case strings.Contains(c, "link del wg1"):
		f.broken[ns] = true
	case strings.Contains(c, "nft list chain"):
		if ns == nsWG {
			return fmt.Sprintf("\t\tip daddr 10.201.0.1 udp dport 51820 counter packets %d bytes 0 accept\n", f.counter[ns]), nil
		}
		return fmt.Sprintf("\t\tip daddr 10.201.0.5 tcp dport 1194 counter packets %d bytes 0 accept\n", f.counter[ns]), nil
	}
	return "", nil
}

func (f *fakeProbe) Start(_ context.Context, argv ...string) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := strings.Join(argv, " ")
	f.cmds = append(f.cmds, "START "+c)
	if strings.Contains(c, nsOVPN) {
		f.clientUp = true
		var once sync.Once
		return func() {
			once.Do(func() {
				f.mu.Lock()
				f.clientUp, f.broken[nsOVPN] = false, true
				f.mu.Unlock()
			})
		}, nil
	}
	return func() {}, nil
}

func (f *fakeProbe) ServeEcho(string) (func(), error) { return func() {}, nil }

func (f *fakeProbe) Probe(ns, kind, addr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	host, port, _ := net.SplitHostPort(addr)
	switch {
	case strings.HasPrefix(host, "10.202.") || strings.HasPrefix(host, "10.203."): // 隧道里的地址
		if f.broken[ns] {
			return errors.New("隧道断了")
		}
		return nil
	case strings.HasPrefix(host, "10.201."): // veth 上的对端
		if (port == "51820" && kind == "udp-send") || (port == "1194" && kind == "tcp-connect") {
			if f.ks[ns] {
				f.counter[ns]++
			}
			return nil
		}
		if f.ks[ns] {
			return errors.New("被断网规则拦下")
		}
		return nil
	}
	if f.ks[ns] {
		return errors.New("被断网规则拦下")
	}
	return nil
}

func (f *fakeProbe) LoadModules(names []string) []kmod.Result {
	var out []kmod.Result
	for _, n := range names {
		if n == f.failModule {
			out = append(out, kmod.Result{Name: n, Error: "没有这个模块"})
		} else {
			out = append(out, kmod.Result{Name: n, OK: true})
		}
	}
	return out
}

func (f *fakeProbe) WriteFile(p string, b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[p] = string(b)
	return nil
}

func (f *fakeProbe) RemoveAll(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p == probeDir {
		f.removed = true
	}
	return nil
}

func TestNetnsProbeFake(t *testing.T) {
	f := newFakeProbe()
	rep := (&NetnsProbe{Ops: f}).Run(context.Background())
	b, _ := json.MarshalIndent(rep, "", " ")
	if !rep.OK {
		t.Fatalf("探查应全部通过：\n%s", b)
	}
	// 关键的步骤都在。
	for _, want := range []string{"对照组", "隧道里的 TCP 回显", "默认路由是隧道接口", "加断网规则（", "经隧道的 53 通", "直连对端的 TCP 7、UDP 53 都不通", "断开后对端的普通端口、53、隧道地址都不通", "仍能到达对端端点"} {
		n := 0
		for _, s := range rep.Steps {
			if strings.Contains(s.Name, want) {
				n++
			}
		}
		if n != 2 {
			t.Fatalf("WireGuard 和 OpenVPN 都应有“%s”这一步（%d）", want, n)
		}
	}
	// 报告里没有密钥和证书。
	js := string(b)
	for p, content := range f.files {
		for _, l := range strings.Split(content, "\n") {
			if strings.HasPrefix(l, "PrivateKey = ") && strings.Contains(js, strings.TrimPrefix(l, "PrivateKey = ")) {
				t.Fatalf("报告里出现了 %s 里的私钥", p)
			}
		}
	}
	if strings.Contains(js, "PRIVATE KEY") || strings.Contains(js, "BEGIN CERTIFICATE") {
		t.Fatal("报告里出现了证书或私钥")
	}
	// WireGuard 不用 wg-quick，只调 wg setconf。
	all := strings.Join(f.cmds, "\n")
	if strings.Contains(all, "wg-quick") || !strings.Contains(all, "wg setconf wg1") {
		t.Fatal("应只调 wg setconf，不用 wg-quick")
	}
	// OpenVPN 的客户端配置按 R5 改写：没有 redirect-gateway，有 route-nopull。
	cc := f.files[probeDir+"/ovpn-client.conf"]
	if strings.Contains(cc, "redirect-gateway") || !strings.Contains(cc, "route-nopull") {
		t.Fatalf("客户端配置没有按 R5 改写：\n%s", cc)
	}
	// 清理：删了三个命名空间和临时目录。
	for _, ns := range []string{nsWG, nsOVPN, nsPeer} {
		if !strings.Contains(all, "ip netns del "+ns) {
			t.Fatalf("没有清理命名空间 %s", ns)
		}
	}
	if !f.removed {
		t.Fatal("没有删临时目录")
	}
}

// 断网规则不起作用时，探查必须判不通过（不能只看命令有没有执行成功）。
func TestNetnsProbeDetectsIneffectiveKillSwitch(t *testing.T) {
	f := newFakeProbe()
	f.nftNoOp = true
	rep := (&NetnsProbe{Ops: f}).Run(context.Background())
	if rep.OK {
		t.Fatal("断网规则不起作用时不应通过")
	}
	failed := 0
	for _, s := range rep.Steps {
		if !s.OK && strings.Contains(s.Name, "都不通") {
			failed++
		}
	}
	if failed < 2 {
		t.Fatalf("应指出“直连不通”的步骤失败：%+v", rep.Steps)
	}
}

func TestNetnsProbeModuleMissing(t *testing.T) {
	f := newFakeProbe()
	f.failModule = "wireguard"
	rep := (&NetnsProbe{Ops: f}).Run(context.Background())
	if rep.OK || len(rep.Modules) != 1 || len(rep.Steps) != 1 || !strings.Contains(rep.Steps[0].Name, "加载模块") {
		t.Fatalf("模块加载不了应在第一步停下：%+v", rep)
	}
}

func TestRewriteOVPNForNetns(t *testing.T) {
	in := "client\nredirect-gateway def1 bypass-dhcp\nroute 10.0.0.0 255.0.0.0\nroute-nopull\nremote x 1194\n"
	out, ch := RewriteOVPNForNetns(in)
	if strings.Contains(out, "redirect-gateway") || strings.Contains(out, "route 10.0.0.0") || strings.Count(out, "route-nopull") != 1 {
		t.Fatalf("改写结果不对：\n%s", out)
	}
	if len(ch) != 3 {
		t.Fatalf("应记下 3 处改写：%v", ch)
	}
}

func TestNetnsProbeHTTP(t *testing.T) {
	// 没有开发标记：不注册（404）。
	h := (&API{State: &State{}}).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/dev/netns-probe", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("没有开发标记时应是 404：%d", rec.Code)
	}
	h = (&API{State: &State{}, NetnsProbe: &NetnsProbe{Ops: newFakeProbe()}}).Handler()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/dev/netns-probe", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 应是 405：%d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/dev/netns-probe", nil))
	var rep ProbeReport
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &rep) != nil || !rep.OK {
		t.Fatalf("POST 应返回通过的报告：%d %s", rec.Code, rec.Body.String())
	}
}
