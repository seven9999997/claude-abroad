package daemonsim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"claude-abroad/internal/daemon"
	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/redact"
	"claude-abroad/internal/stack"
	"claude-abroad/internal/wire"
)

// M4-8 的真实虚拟机阶段（m4-plan v2.11 验收 j）：分流出口经 daemon 的正式接口增加，下发到虚拟机（同机对端：
// agent 的 /v1/dev/exit-peer 建的 WireGuard、OpenVPN 服务端和 socks 节点），三个应用各走一个出口；
// 删除被规则引用的出口被要求先改规则；隧道断开后回退主代理，恢复后走回去。
//
// 同机对端连不到公网，正式的出口检查（HTTPS、出口 IP、大陆段、DoH）在这里没法做：这个阶段用开发用的检查
// （PeerCheck：经该出口连对端带标记的回显，标记对就算 https/ip/mainland/doh 都通过，UDP 同理）。只在 daemon-sim 里用。

// PeerTarget 是同机对端里一个出口的回显地址和标记。
type PeerTarget struct {
	Addr netip.AddrPort
	Tag  string
}

// DefaultPeerTargets 是 agent 开发用对端的布局（internal/agent/devpeer.go）。
var DefaultPeerTargets = map[string]PeerTarget{
	"wg":   {netip.MustParseAddrPort("10.77.0.1:7"), "wg"},
	"ovpn": {netip.MustParseAddrPort("10.78.0.1:7"), "ovpn"},
	"sub":  {netip.MustParseAddrPort("10.199.0.2:7"), "node"},
}

// echoVia 经数据面开一条流到 target，发 payload，读回显，返回读到的内容。
func echoVia(ctx context.Context, op stack.Opener, exit string, network string, target netip.AddrPort) (string, error) {
	h := wire.Header{V: wire.Version, Exit: exit, Net: network, IP: target.Addr().String(), Port: int(target.Port()), App: "daemon-sim-peercheck"}
	c, err := op.Open(ctx, h)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if network == wire.UDP {
		dc := wire.NewDatagramConn(c)
		if err := dc.WriteDatagram([]byte("pc")); err != nil {
			return "", err
		}
		b := make([]byte, 256)
		n, err := dc.ReadDatagram(b)
		return string(b[:n]), err
	}
	if _, err := io.WriteString(c, "pc"); err != nil {
		return "", err
	}
	b := make([]byte, 64)
	n, err := io.ReadAtLeast(c, b, 3)
	return string(b[:n]), err
}

// PeerCheck 返回开发用的按出口检查（见文件头）。
func PeerCheck(op stack.Opener, targets map[string]PeerTarget) func(ctx context.Context, tag string) exitcheck.Report {
	return func(ctx context.Context, tag string) exitcheck.Report {
		t, ok := targets[tag]
		res := func(okv bool) exitcheck.Result {
			if okv {
				return exitcheck.Pass
			}
			return exitcheck.Fail
		}
		tcp, udp := false, false
		if ok {
			got, err := echoVia(ctx, op, tag, wire.TCP, t.Addr)
			tcp = err == nil && strings.HasPrefix(got, t.Tag+":")
			got, err = echoVia(ctx, op, tag, wire.UDP, t.Addr)
			udp = err == nil && strings.HasPrefix(got, t.Tag+":")
		}
		d := "开发用的检查：经该出口连同机对端的回显"
		return exitcheck.Report{Exit: tag, Items: []exitcheck.Item{
			{Name: "https", Result: res(tcp), Detail: d}, {Name: "ip", Result: res(tcp), Detail: d},
			{Name: "mainland", Result: res(tcp), Detail: d}, {Name: "doh", Result: res(tcp), Detail: d},
			{Name: "udp", Result: res(udp), Detail: d}}}
	}
}

// ExitsConfig 是分流出口阶段的配置。
type ExitsConfig struct {
	Config
	ExitsFile string                // agent 的 /v1/dev/exit-peer?action=up 的应答（{"exits":[…]}，含测试密钥，0600）
	Targets   map[string]PeerTarget // 为空时用 DefaultPeerTargets
	Flows     int                   // 每个应用的流数（默认 5）
}

// ExitsRules 是三个应用各走一个出口的规则。
const ExitsRules = `
rules:
  - {app: com.app.a, action: "proxy:wg"}
  - {app: com.app.b, action: "proxy:ovpn"}
  - {app: com.app.c, action: "proxy:sub"}
`

func apiCall(hs *harness, method, path string, body []byte) (int, string) {
	hc := unixHTTP(hs.d.SocketPath())
	for i := 0; i < 100; i++ {
		req, _ := http.NewRequest(method, "http://abroadd"+path, bytes.NewReader(body))
		resp, err := hc.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusConflict && strings.Contains(string(b), "忙") {
			time.Sleep(100 * time.Millisecond) // 状态机忙（监护、复查）：重试
			continue
		}
		return resp.StatusCode, string(b)
	}
	return 0, "一直忙"
}

func devPeer(vmDir, action string) error {
	hc := unixHTTP(filepath.Join(vmDir, "ctl.sock"))
	req, _ := http.NewRequest(http.MethodPost, "http://abroad-agent/v1/dev/exit-peer?"+action, nil)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// RunExits 跑分流出口的阶段。
func RunExits(ctx context.Context, cfg ExitsConfig) (rep MultiReport) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.Targets == nil {
		cfg.Targets = DefaultPeerTargets
	}
	if cfg.Flows <= 0 {
		cfg.Flows = 5
	}
	fail := func(err error) MultiReport { rep.Err = redact.String(err.Error()); return rep }
	add := func(c Check) { c.OK = c.Got == c.Want; rep.Checks = append(rep.Checks, c) }
	raw, err := os.ReadFile(cfg.ExitsFile)
	if err != nil {
		return fail(err)
	}
	var peer struct {
		Exits []daemon.StoredExit `json:"exits"`
	}
	if err := json.Unmarshal(raw, &peer); err != nil || len(peer.Exits) != 3 {
		return fail(fmt.Errorf("出口文件不对"))
	}
	var hsRef *harness
	cfg.Config.ExitVM = daemon.ExternalVM{Dir: cfg.VMDir}
	cfg.Config.RecheckInterval = 5 * time.Second
	var opMu sync.Mutex
	var opener stack.Opener
	cfg.Config.CheckExit = func(ctx context.Context, tag string) exitcheck.Report {
		opMu.Lock()
		op := opener
		opMu.Unlock()
		return PeerCheck(op, cfg.Targets)(ctx, tag)
	}
	hs, err := newHarness(ctx, cfg.Config, func(dp *daemon.DataPlane) {
		dp.SetIdent(portIdent{})
		opMu.Lock()
		opener = dp.Streams()
		opMu.Unlock()
	})
	if err != nil {
		return fail(err)
	}
	hsRef = hs
	defer hs.close()

	code, _ := hs.op("/v1/enable")
	add(Check{Name: "启用", Want: "200", Got: fmt.Sprint(code)})
	if code != 200 {
		return finishExits(rep, hsRef)
	}
	code, msg := apiCall(hs, http.MethodPut, "/v1/rules?drop_affected=1", []byte(ExitsRules))
	add(Check{Name: "规则引用还不存在的出口被拒", Want: "422 第 1 条", Got: fmt.Sprintf("%d %s", code, firstOf(msg, "第 1 条"))})
	for _, x := range peer.Exits {
		b, _ := json.Marshal(map[string]any{"kind": x.Kind, "conf": x.Conf})
		code, _ := apiCall(hs, http.MethodPut, "/v1/exits/"+x.Tag, b)
		add(Check{Name: "经 daemon 的接口增加出口 " + x.Tag, Want: "200", Got: fmt.Sprint(code)})
	}
	allUp := func(tags ...string) bool {
		st := hs.d.Status()
		up := map[string]bool{}
		for _, x := range st.Exits {
			up[x.Tag] = x.Avail.Up
		}
		for _, t := range tags {
			if !up[t] {
				return false
			}
		}
		return true
	}
	waitUntil := func(d time.Duration, cond func() bool) bool {
		end := time.Now().Add(d)
		for time.Now().Before(end) {
			if cond() {
				return true
			}
			time.Sleep(500 * time.Millisecond)
		}
		return cond()
	}
	add(Check{Name: "三个出口下发、通过检查后可用", Want: "true", Got: fmt.Sprint(waitUntil(120*time.Second, func() bool { return allUp("wg", "ovpn", "sub") }))})
	code, _ = apiCall(hs, http.MethodPut, "/v1/rules?drop_affected=1", []byte(ExitsRules))
	add(Check{Name: "出口都在之后写规则", Want: "200", Got: fmt.Sprint(code)})
	cl, err := hs.client()
	if err != nil {
		return fail(err)
	}
	echo := func(port uint16, dst netip.AddrPort) string {
		c, cancel := context.WithTimeout(ctx, cfg.Timeout)
		defer cancel()
		conn, err := cl.DialTCPFrom(c, port, dst)
		if err != nil {
			return "dial_failed"
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(cfg.Timeout))
		_, _ = io.WriteString(conn, "hi")
		b := make([]byte, 64)
		n, _ := io.ReadAtLeast(conn, b, 3)
		return string(b[:n])
	}
	before := hs.d.Status().Opens
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[string]int{}
	apps := []struct {
		port uint16
		exit string
	}{{portA, "wg"}, {portB, "ovpn"}, {portC, "sub"}}
	for _, a := range apps {
		for i := 0; i < cfg.Flows; i++ {
			wg.Add(1)
			go func(port uint16, exit string) {
				defer wg.Done()
				r := echo(port, cfg.Targets[exit].Addr)
				if strings.HasPrefix(r, cfg.Targets[exit].Tag+":") {
					mu.Lock()
					got[exit]++
					mu.Unlock()
				}
			}(a.port+uint16(10+i), a.exit)
		}
	}
	wg.Wait()
	after := hs.d.Status().Opens
	for _, a := range apps {
		add(Check{Name: "应用经出口 " + a.exit + "（回显来自对应的对端）", Want: fmt.Sprint(cfg.Flows), Got: fmt.Sprint(got[a.exit])})
		add(Check{Name: "status 里出口 " + a.exit + " 的开流计数增量", Want: fmt.Sprint(cfg.Flows), Got: fmt.Sprint(after[a.exit].TCP - before[a.exit].TCP)})
	}
	code, msg = apiCall(hs, http.MethodDelete, "/v1/exits/wg", nil)
	add(Check{Name: "删除被规则引用的出口：要求先改规则", Want: "409 第 1 条", Got: fmt.Sprintf("%d %s", code, firstOf(msg, "第 1 条"))})
	// 隧道断开：wg 变为不可用，A 的新连接回退主代理（主代理是开发上游，回显不是 wg 的）。
	if err := devPeer(cfg.VMDir, "action=break&exit=wg"); err != nil {
		return fail(err)
	}
	add(Check{Name: "隧道断开后出口 wg 不可用", Want: "true", Got: fmt.Sprint(waitUntil(60*time.Second, func() bool { return !allUp("wg") }))})
	b0 := hs.d.Status().Opens
	r := echo(portA+500, cfg.Targets["wg"].Addr)
	b1 := hs.d.Status().Opens
	add(Check{Name: "出口断开后 A 的新连接回退主代理（主代理计数增长、wg 计数不变、回显不是 wg 的）",
		Want: "true", Got: fmt.Sprint(b1["main"].TCP+b1["main"].Failed > b0["main"].TCP+b0["main"].Failed && b1["wg"] == b0["wg"] && !strings.HasPrefix(r, "wg:"))})
	if s := hs.d.Status(); !s.GateOpen {
		add(Check{Name: "分流出口断开不影响闸门", Want: "true", Got: "false"})
	} else {
		add(Check{Name: "分流出口断开不影响闸门", Want: "true", Got: "true"})
	}
	if err := devPeer(cfg.VMDir, "action=restore&exit=wg"); err != nil {
		return fail(err)
	}
	add(Check{Name: "恢复后出口 wg 重新可用", Want: "true", Got: fmt.Sprint(waitUntil(120*time.Second, func() bool { return allUp("wg") }))})
	add(Check{Name: "恢复后 A 的新连接走回 wg", Want: "true", Got: fmt.Sprint(strings.HasPrefix(echo(portA+600, cfg.Targets["wg"].Addr), "wg:"))})
	return finishExits(rep, hsRef)
}

func firstOf(msg, want string) string {
	if strings.Contains(msg, want) {
		return want
	}
	if len(msg) > 120 {
		msg = msg[:120]
	}
	return msg
}

// finishExits 收尾：日志里不能有测试密钥、端点和测试用的应用 ID。
func finishExits(rep MultiReport, hs *harness) MultiReport {
	log := hs.lb.String()
	for _, s := range []string{"PrivateKey", "BEGIN", "10.199.0.2", "10.77.0.1", "10.78.0.1", "com.app"} {
		if strings.Contains(log, s) {
			rep.LogLeak = true
		}
	}
	rep.OK = rep.Err == "" && !rep.LogLeak && len(rep.Checks) > 0
	for _, c := range rep.Checks {
		rep.OK = rep.OK && c.OK
	}
	return rep
}
