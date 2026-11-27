package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/pf"
	"claude-abroad/internal/sysnet"
	"claude-abroad/internal/wire"
)

var ipv4Re = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)

// fakeNetStats 是假的 abroad-vm 网络计数（故意带一个地址字段，快照里不能出现）。
type fakeNetStats struct{ err error }

func (f fakeNetStats) NetStats(context.Context) (map[string]any, error) {
	if f.err != nil {
		return nil, f.err
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(`{"interface":"en0","outbound_mode":"first_hop","dial_ok":0,"hop_denied":0,"bind_failed":0,
		"policy_denied":0,"dial_fail":{"perm":0,"unreach":0,"timeout":3,"refused":0,"dns":0,"bind":0,"other":0},
		"leak":"8.8.4.4","first_hop_count":1}`), &m)
	return m, nil
}

// snapPF 是加载了接管规则（局域网 192.168.1.0/24）的假 pf，计数按 counters 给。
func snapPF(t *testing.T, counters map[int][4]int64) *pf.Controller {
	t.Helper()
	f := pf.NewFakePF()
	f.UIDs = map[string]string{"_abroad": "250"}
	c := &pf.Controller{R: f, Matcher: pf.PrintMatcher{User: "_abroad", UID: "250"}}
	rules, err := pf.Render(pf.Config{UtunName: "utun7", LAN: pf.LANPrefixes([]netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")}),
		VMIfs: []string{"bridge100"}, User: "_abroad"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Ensure(context.Background(), rules, nil); err != nil {
		t.Fatal(err)
	}
	f.SetRuleCounters(counters)
	return c
}

// 启用失败的现场快照：假 pf、假路由；只有接口名和计数，没有地址。
func TestEnableSnapshotFakes(t *testing.T) {
	ctl := snapPF(t, map[int][4]int64{5: {12, 0, 0, 0}})
	net := sysnet.NewFake(sysnet.FakeState{Default: "en0", Routes: map[netip.Prefix]string{}})
	rt := net.Routes()
	_ = rt.Add(context.Background(), netip.MustParsePrefix("1.0.0.0/8"), "utun7")
	_ = rt.Add(context.Background(), netip.MustParsePrefix("128.0.0.0/1"), "utun7")
	net.SetForeignRoute(netip.MustParsePrefix("8.8.0.0/16"), "utun3") // 别的隧道占着一段公网路由
	s := TakeEnableSnapshot(context.Background(), SnapshotDeps{PF: ctl, Routes: rt, OurUtun: func() string { return "utun7" },
		FirstHop: func() (string, error) { return "8.8.4.4", nil }, VM: fakeNetStats{},
		Now: func() time.Time { return time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC) }})
	if s.PF == nil || !s.PF.Enabled || len(s.PF.Rules) == 0 {
		t.Fatalf("pf：%+v，错误 %v", s.PF, s.Errors)
	}
	if s.FirstHopKind != "ip" || s.FirstHopRouteIface != "utun3" {
		t.Fatalf("去第一跳：%s %s", s.FirstHopKind, s.FirstHopRouteIface)
	}
	if s.HalfRouteIfaces["1/8–64/2"] != "utun7" || s.HalfRouteIfaces["128/1"] != "utun7" {
		t.Fatalf("两条 /1：%v", s.HalfRouteIfaces)
	}
	if len(s.ForeignTunnels) != 1 || s.ForeignTunnels[0] != "utun3" || s.DialBoundIface != "en0" {
		t.Fatalf("隧道、绑定：%v %s", s.ForeignTunnels, s.DialBoundIface)
	}
	if _, ok := s.VMNetStats["leak"]; ok {
		t.Fatal("netstats 里的字符串字段不能带进来")
	}
	sum := s.Summary()
	for _, want := range []string{"pf 已启用", "放行专用用户 tcp", "其余全部拦截", "拨号绑定 en0", "dial_fail_timeout=3", "utun3"} {
		if !strings.Contains(sum, want) {
			t.Errorf("摘要缺 %q：%s", want, sum)
		}
	}
	js, _ := json.Marshal(s)
	for _, text := range []string{string(js), sum} {
		if m := ipv4Re.FindAllString(text, -1); len(m) > 0 {
			t.Errorf("快照里有地址 %v：%s", m, text)
		}
	}
	// 第一跳是域名：不解析，不查去它的路由。
	s2 := TakeEnableSnapshot(context.Background(), SnapshotDeps{Routes: rt, FirstHop: func() (string, error) { return "node.example.com", nil }})
	if s2.FirstHopKind != "domain" || s2.FirstHopRouteIface != "" || !strings.Contains(s2.Summary(), "pf：没取到") {
		t.Fatalf("域名：%+v", s2)
	}
	// 取不到的部分记在 Errors 里，也不带地址。
	s3 := TakeEnableSnapshot(context.Background(), SnapshotDeps{VM: fakeNetStats{err: errors.New("连 8.8.4.4 失败")}})
	if len(s3.Errors) != 1 || strings.Contains(strings.Join(s3.Errors, ""), "8.8.4.4") {
		t.Fatalf("错误：%v", s3.Errors)
	}
}

// 走完整的启用：出口检查失败时，回滚之前留下快照，事件和诊断包里都有，且没有地址。
func TestEnableFailureSnapshotRecorded(t *testing.T) {
	e := newEnv(t)
	d := e.start(e.opts())
	e.sys.ExitCheck.SetFail(errors.New("假的出口检查失败"))
	if code, _ := call(t, d, "POST", "/v1/enable", nil); code == 200 {
		t.Fatal("出口检查失败时启用应失败")
	}
	found := false
	for _, ev := range status(t, d).Events {
		if ev.Source == "enable" && ev.Kind == "snapshot" {
			found = true
			if !strings.Contains(ev.Detail, "pf 已启用") || !strings.Contains(ev.Detail, "1/8–64/2 走 utun") {
				t.Errorf("快照事件：%s", ev.Detail)
			}
		}
	}
	if !found {
		t.Fatal("应有 enable/snapshot 事件")
	}
	_, b := call(t, d, "GET", "/v1/diagnostics", nil)
	var dg struct {
		Snap *EnableSnapshot `json:"enable_failure_snapshot"`
	}
	if err := json.Unmarshal(b, &dg); err != nil || dg.Snap == nil || dg.Snap.PF == nil || len(dg.Snap.PF.Rules) == 0 {
		t.Fatalf("诊断包里应有快照：%v %s", err, b)
	}
	if !strings.Contains(string(b), "enable_failure_snapshot") {
		t.Fatal("诊断包")
	}
	for _, r := range dg.Snap.PF.Rules {
		if ipv4Re.MatchString(r.Rule) {
			t.Errorf("规则原文没脱敏：%s", r.Rule)
		}
	}
}

// eofOpener 开的每条流对端立刻关闭（复现真机：经主代理的请求都是 EOF）。
type eofOpener struct{}

func (eofOpener) Open(context.Context, wire.Header) (net.Conn, error) {
	a, b := net.Pipe()
	b.Close()
	return a, nil
}

// 空原因 bug（2026-11-27 真机“启用失败：出口检查失败：。”）：各项都连接失败、各项都无法判断时文字都不为空。
func TestExitCheckReasonNotEmpty(t *testing.T) {
	item := func(name string, r exitcheck.Result, c exitcheck.Cause) exitcheck.Item {
		return exitcheck.Item{Name: name, Result: r, Cause: c}
	}
	allConnect := exitcheck.Report{Verdict: exitcheck.Unknown, Items: []exitcheck.Item{
		item("https", exitcheck.Unknown, exitcheck.CauseConnect), item("ip", exitcheck.Unknown, exitcheck.CauseConnect),
		item("mainland", exitcheck.Unknown, exitcheck.CauseConnect), item("rule", exitcheck.Unknown, exitcheck.CauseConnect),
		item("doh", exitcheck.Unknown, exitcheck.CauseConnect), item("udp", exitcheck.Unknown, exitcheck.CauseTimeout)}}
	if got := checkFailReason(allConnect); got != AllConnectFailedReason {
		t.Fatalf("全部连接失败：%q", got)
	}
	allUnknown := exitcheck.Report{Verdict: exitcheck.Unknown, Items: []exitcheck.Item{
		item("https", exitcheck.Unknown, exitcheck.CauseTimeout), item("ip", exitcheck.Unknown, exitcheck.CauseRateLimited),
		item("mainland", exitcheck.Unknown, exitcheck.CauseConnect), item("doh", exitcheck.Unknown, exitcheck.CauseBadFormat)}}
	if got := checkFailReason(allUnknown); got == "" || got == AllConnectFailedReason || !strings.Contains(got, "：") {
		t.Fatalf("全部无法判断：%q", got)
	}
	if got := checkFailReason(exitcheck.Report{Verdict: exitcheck.Unknown}); got == "" {
		t.Fatal("没有项时也不能为空")
	}
	// 经真实的检查器：每条流都是 EOF。
	dir := t.TempDir()
	ck := &exitcheck.Checker{Opener: eofOpener{}, HTTPSURL: "https://check.example.com/generate_204",
		Total: 3 * time.Second, HTTPS: time.Second, IP: time.Second, Geo: time.Second, UDP: 200 * time.Millisecond, DoH: time.Second}
	err := FullExitCheck{Checker: ck, Baseline: &BaselineService{Path: filepath.Join(dir, BaselineFile)}}.Check(context.Background())
	if err == nil {
		t.Fatal("应失败")
	}
	msg := err.Error()
	if strings.Contains(msg, "失败：。") || !strings.Contains(msg, AllConnectFailedReason) {
		t.Fatalf("文字：%q", msg)
	}
}
