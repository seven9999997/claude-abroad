package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// prepareServe 准备 /modloop、/proc/cmdline、/sys/class/net/eth0，并给一个 TCP 回环监听器。
func prepareServe(t *testing.T, f *fakeSys, cmdline string, withNIC bool) string {
	writeModloop(t, filepath.Join(f.root, "/modloop/modules", testKver))
	writeFile(t, filepath.Join(f.root, "/proc/cmdline"), []byte(cmdline+"\n"))
	if withNIC {
		writeFile(t, filepath.Join(f.root, "/sys/class/net/eth0/address"), []byte("52:54:00:00:00:01"))
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.listener = l
	return "http://" + l.Addr().String()
}

func getHealth(t *testing.T, url string) Health {
	t.Helper()
	var h Health
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(url + "/v1/health")
		if err == nil {
			defer resp.Body.Close()
			if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
				t.Fatal(err)
			}
			return h
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServeHappyPathAndShutdown(t *testing.T) {
	f := newFakeSys(t)
	url := prepareServe(t, f, "console=hvc0 abroad.ip=10.0.2.15/24 abroad.gw=10.0.2.2", true)
	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), f.env()) }()

	h := getHealth(t, url)
	for i := 0; i < 200 && !h.OK; i++ { // 等 sing-box 就绪
		time.Sleep(5 * time.Millisecond)
		h = getHealth(t, url)
	}
	if !h.OK || !h.Net.OK || h.Kernel != testKver || h.Engines["sing-box"].State != EngineRunning {
		t.Fatalf("health 不对：%+v", h)
	}
	var names []string
	for _, m := range h.Modules {
		names = append(names, m.Name)
	}
	if got := strings.Join(names, ","); got != "vsock,vmw_vsock_virtio_transport_common,vmw_vsock_virtio_transport,net_failover,virtio_net,virtio_balloon,virtio_rng" {
		t.Errorf("模块顺序：%s", got)
	}
	if f.netCfg == nil || f.netCfg.Addr.String() != "10.0.2.15/24" || f.netCfg.GW.String() != "10.0.2.2" || f.netCfg.Iface != "eth0" {
		t.Errorf("网卡配置：%+v", f.netCfg)
	}

	resp, err := http.Post(url+"/v1/shutdown", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("收到关机请求应正常返回：%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve 没有退出")
	}
}

func TestServeContextCancel(t *testing.T) {
	f := newFakeSys(t)
	url := prepareServe(t, f, "", true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, f.env()) }()
	getHealth(t, url)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve 没有退出")
	}
}

func TestServeOptionalFailuresNotFatal(t *testing.T) {
	f := newFakeSys(t)
	f.loadFail["virtio_balloon"] = errBoom
	url := prepareServe(t, f, "", false) // 没有网卡
	env := f.env()
	now := time.Unix(0, 0)
	env.Now = func() time.Time { return now }
	env.Sleep = func(d time.Duration) { now = now.Add(d) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Serve(ctx, env) }()
	h := getHealth(t, url)
	if h.OK || h.Net.OK || !strings.Contains(h.Net.Error, "eth0") {
		t.Fatalf("没有网卡应记进 health 但不致命：%+v", h)
	}
	failed := false
	for _, m := range h.Modules {
		if m.Name == "virtio_balloon" && !m.OK && strings.Contains(m.Error, "boom") {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("模块失败应记进 health：%+v", h.Modules)
	}
}

func TestServeFatalErrors(t *testing.T) {
	f := newFakeSys(t)
	f.loadFail["vsock"] = errBoom
	prepareServe(t, f, "", true)
	if err := Serve(context.Background(), f.env()); err == nil || !strings.Contains(err.Error(), "vsock") {
		t.Fatalf("vsock 模块失败应报错：%v", err)
	}

	f = newFakeSys(t)
	prepareServe(t, f, "", true)
	f.listenErr = map[uint32]error{1081: errBoom}
	if err := Serve(context.Background(), f.env()); err == nil || !strings.Contains(err.Error(), "1081") {
		t.Fatalf("控制面监听失败应报错：%v", err)
	}
}

func TestParseNetCmdline(t *testing.T) {
	c, err := ParseNetCmdline("console=hvc0 quiet")
	if err != nil || c.Addr.String() != "192.168.127.2/24" || c.GW.String() != "192.168.127.1" || c.Iface != "eth0" {
		t.Fatalf("默认值：%+v %v", c, err)
	}
	c, err = ParseNetCmdline("abroad.ip=172.30.5.9/16 abroad.gw=172.30.0.1")
	if err != nil || c.Addr.String() != "172.30.5.9/16" || c.GW.String() != "172.30.0.1" {
		t.Fatalf("显式值：%+v %v", c, err)
	}
	for _, bad := range []string{
		"abroad.ip=nonsense",
		"abroad.ip=192.168.127.2",          // 没有前缀长度
		"abroad.ip=fd00::2/64",             // IPv6
		"abroad.ip=10.0.0.2/31",            // 前缀太长
		"abroad.gw=fe80::1",                // IPv6 网关
		"abroad.gw=10.9.9.9",               // 不在网段里
		"abroad.gw=192.168.127.2",          // 等于自己
		"abroad.ip=10.0.0.2/24 abroad.gw=", // 空网关
	} {
		if _, err := ParseNetCmdline(bad); err == nil {
			t.Errorf("%q 应报错", bad)
		}
	}
}

func TestServeDataPlaneFatalAndOrder(t *testing.T) {
	f := newFakeSys(t)
	prepareServe(t, f, "", true)
	f.listenErr = map[uint32]error{1080: errBoom}
	if err := Serve(context.Background(), f.env()); err == nil || !strings.Contains(err.Error(), "1080") {
		t.Fatalf("数据面监听失败应报错：%v", err)
	}

	g := newFakeSys(t)
	url := prepareServe(t, g, "", true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, g.env()) }()
	h := getHealth(t, url)
	if h.Data == nil {
		t.Fatal("health 应有 data 段")
	}
	calls := strings.Join(g.Calls(), "\n")
	if strings.Index(calls, "listen vsock 1080") < 0 || strings.Index(calls, "listen vsock 1080") > strings.Index(calls, "listen vsock 1081") {
		t.Fatalf("数据面应先于控制面监听：\n%s", calls)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	dl := g.extra[0]
	g.mu.Unlock()
	if _, err := net.Dial("tcp", dl.Addr().String()); err == nil {
		t.Fatal("退出后数据面应不再监听")
	}
}

func TestServeStartsAndStopsEngines(t *testing.T) {
	f := newFakeSys(t)
	url := prepareServe(t, f, "", false) // 没有网卡：lo 也要拉起来
	env := f.env()
	now := time.Unix(0, 0)
	var nowMu sync.Mutex
	env.Now = func() time.Time { nowMu.Lock(); defer nowMu.Unlock(); return now }
	env.Sleep = func(d time.Duration) { nowMu.Lock(); now = now.Add(d); nowMu.Unlock() }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, env) }()
	getHealth(t, url)

	calls := strings.Join(f.Calls(), "\n")
	if !strings.Contains(calls, "lo up") {
		t.Fatal("没有网卡时也应拉起 lo")
	}
	if strings.Index(calls, "lo up") > strings.Index(calls, "listen vsock") {
		t.Error("lo 应在控制面监听之前拉起")
	}
	for p, want := range map[string]os.FileMode{
		"/run/claude-abroad": 0o700, "/run/claude-abroad/mihomo": 0o700, "/run/claude-abroad/sing-box.json": 0o600,
	} {
		fi, err := os.Stat(filepath.Join(f.root, p))
		if err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s 权限应为 %o：%v", p, want, err)
		}
	}
	cfg, _ := os.ReadFile(filepath.Join(f.root, "/run/claude-abroad/sing-box.json"))
	if _, err := SingBoxConfigCheck(cfg); err != nil {
		t.Errorf("sing-box.json 不对：%v", err)
	}
	r := f.runner
	var p *fakeProc
	select {
	case p = <-r.procs:
	case <-time.After(3 * time.Second):
		t.Fatal("sing-box 没有启动")
	}
	r.mu.Lock()
	argv := strings.Join(r.argvs[0], " ")
	r.mu.Unlock()
	if argv != "/usr/bin/sing-box run -c /run/claude-abroad/sing-box.json -D /run/claude-abroad/sing-box" {
		t.Errorf("sing-box 启动参数：%s", argv)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s := p.signals(); len(s) == 0 || s[0] != syscall.SIGTERM {
		t.Fatalf("serve 退出时应停掉 sing-box：%v", s)
	}
}

func TestServeDisablesIPv6(t *testing.T) {
	f := newFakeSys(t)
	url := prepareServe(t, f, "", true)
	for _, k := range []string{"all", "default", "eth0"} {
		writeFile(t, filepath.Join(f.root, "/proc/sys/net/ipv6/conf", k, "disable_ipv6"), []byte("0\n"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Serve(ctx, f.env()) }()
	getHealth(t, url)
	for _, k := range []string{"all", "default", "eth0"} {
		b, _ := os.ReadFile(filepath.Join(f.root, "/proc/sys/net/ipv6/conf", k, "disable_ipv6"))
		if string(b) != "1\n" {
			t.Errorf("%s 的 disable_ipv6 应为 1：%q", k, b)
		}
	}
}
