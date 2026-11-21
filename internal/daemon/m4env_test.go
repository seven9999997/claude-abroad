package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"claude-abroad/internal/envcheck"
	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/gatesm"
	"claude-abroad/internal/preflight"
	"claude-abroad/internal/sysnet"
)

// ---- M4-6a：环境检测（m4-plan v2.6 验收 d、f、g、h、i）----

// selfIdent：源端口在 [25000, 26000) 的连接是 abroadd 自己。
type selfIdent struct{}

func (selfIdent) IsSelf(_ context.Context, _ string, src, _ netip.AddrPort) bool {
	return src.Port() >= 25000 && src.Port() < 26000
}

type envKnobs struct {
	v6Leak    atomic.Bool
	dnsErr    atomic.Bool
	probeN    atomic.Int32 // 需要发探测的假项目跑过几次
	probeOK   atomic.Int32 // 其中探测发出去并回显的次数
	port      atomic.Int32
	warnFail  atomic.Bool
	probeSkip atomic.Bool // 快速推进假时钟的用例：探测窗口在假时钟里会先到期，不发探测
}

type envEnv struct {
	*m3Env
	k     *envKnobs
	clk   *stepClock
	world *envWorld
}

// newEnvEnv 是带环境检测的 M3 环境（真实的数据面和网络栈；检测项里的本机应用用同一个客户端，自己的源端口是 25000 起）。
func newEnvEnv(t *testing.T, withClock bool, extra ...func(*Options)) *envEnv {
	t.Helper()
	ee := &envEnv{k: &envKnobs{}}
	ee.k.port.Store(25000)
	var holder atomic.Pointer[m3Env]
	lookup := func(ctx context.Context, name string) (netip.Addr, error) {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		cl, err := holder.Load().getClient()
		if err != nil {
			return netip.Addr{}, err
		}
		return cl.LookupA(ctx, name, netip.MustParseAddrPort("198.18.0.2:53"))
	}
	deps := &EnvDeps{
		DialV6: func(context.Context) error {
			if ee.k.v6Leak.Load() {
				return nil
			}
			return errors.New("no route to host")
		},
		Resolve: func(ctx context.Context, name string) ([]netip.Addr, error) {
			if ee.k.dnsErr.Load() {
				return nil, errors.New("超时")
			}
			a, err := lookup(ctx, name)
			return []netip.Addr{a}, err
		},
		Conflicts: func(context.Context) []preflight.Finding { return nil },
		Extra: []envcheck.Item{
			{ID: "91", Name: "需要发探测的假项目", Level: envcheck.Hard, Network: true, Run: func(ctx context.Context) envcheck.Outcome {
				ee.k.probeN.Add(1)
				if ee.k.probeSkip.Load() {
					return envcheck.Outcome{Result: exitcheck.Pass, Detail: "这个用例不发探测"}
				}
				a, err := lookup(ctx, "probe.example.org")
				if err != nil {
					return envcheck.Outcome{Result: exitcheck.Unknown, Detail: "解析失败"}
				}
				cl, _ := holder.Load().getClient()
				dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
				defer cancel()
				c, err := cl.DialTCPFrom(dctx, uint16(ee.k.port.Add(1)), netip.AddrPortFrom(a, 443))
				if err == nil {
					err = echoConn(c)
					c.Close()
				}
				if err != nil {
					return envcheck.Outcome{Result: exitcheck.Fail, Detail: "探测没有发出去", Fix: "修探测"}
				}
				ee.k.probeOK.Add(1)
				return envcheck.Outcome{Result: exitcheck.Pass, Detail: "探测经主代理回显"}
			}},
			{ID: "90", Name: "测试用的警告项", Level: envcheck.Warn, Run: func(context.Context) envcheck.Outcome {
				if ee.k.warnFail.Load() {
					return envcheck.Outcome{Result: exitcheck.Fail, Detail: "警告", Fix: "看看"}
				}
				return envcheck.Outcome{Result: exitcheck.Pass}
			}},
		},
	}
	ee.world = newEnvWorld(t)
	ee.world.deps6b(deps)
	if withClock {
		ee.clk = &stepClock{now: time.Now()}
	}
	fake := &m3Direct{pinnedSet: map[string]bool{}}
	pr := &fakeProber{rep: geoReport("US", "America/Los_Angeles")}
	ee.m3Env = newM3DaemonCore(t, m3Rules, fake, func(e *m3Env, o *Options) {
		holder.Store(e)
		o.Sys.Env = deps
		// 02 要按出口基准比对：启动前先建好（经假的检查器；出口 IP、国家与假服务一致）。
		bs := NewBaselineService(pr, o.StateDir)
		if err := os.MkdirAll(o.StateDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := bs.Establish(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
		o.Sys.Baseline = bs
		o.Sys.Data.SetSelf(selfIdent{})
		if ee.clk != nil {
			o.Clock = ee.clk
		}
		for _, x := range extra {
			x(o)
		}
	})
	ee.direct = fake
	return ee
}

func envReport(t *testing.T, d *Daemon) envcheck.Report {
	t.Helper()
	s := status(t, d)
	if s.Env == nil {
		t.Fatal("status 里应有环境检测")
	}
	return *s.Env
}

func itemOf(rep envcheck.Report, id string) envcheck.ItemResult {
	for _, it := range rep.Items {
		if it.ID == id {
			return it
		}
	}
	return envcheck.ItemResult{}
}

// f. 启用第 ⑦ 步：全部通过 → 启用成功，探测在窗口里发出（闸门那时还关着），第 ⑦ 步结束时窗口已关；status 有每项的结果。
func TestEnvEnableStep7(t *testing.T) {
	e := newEnvEnv(t, false)
	if s := waitGateOpen(t, e.d); !s.GateOpen || s.State != "ENABLED" {
		t.Fatalf("%+v", s.Gate)
	}
	if e.k.probeOK.Load() != 1 || e.d.gate.ProbeOpen() {
		t.Fatalf("探测应在窗口里发出一次，结束后窗口关闭：ok=%d", e.k.probeOK.Load())
	}
	rep := envReport(t, e.d)
	for _, id := range []string{"01", "06", "07", "09", "11", "10", "91"} {
		if it := itemOf(rep, id); it.Result != exitcheck.Pass {
			t.Fatalf("%s：%+v", id, it)
		}
	}
	if st := status(t, e.d); st.Stack == nil || st.Stack.ProbeProxied != 1 {
		t.Fatalf("放行路径应单独计数：%+v", st.Stack)
	}
}

// f. 某个硬性项不通过：启用失败并回滚干净，错误里有修复办法，闸门从未打开；警告项不通过不影响启用。
func TestEnvEnableFails(t *testing.T) {
	e := newEnvEnv(t, false)
	post(t, e.d, "/v1/restore", 200)
	e.k.v6Leak.Store(true)
	e.k.warnFail.Store(true)
	var opened atomic.Bool
	e.d.gate.SetOnChange(func(o bool) {
		if o {
			opened.Store(true)
		}
	})
	code, b := callOp(t, e.d, "/v1/enable")
	if code == 200 || !strings.Contains(string(b), "06 IPv6") || !strings.Contains(string(b), "恢复本机网络") {
		t.Fatalf("应失败并列出 06 和修复办法：%d %s", code, b)
	}
	if s := status(t, e.d); s.State != "RESTORED" || opened.Load() || !e.sys.Net.Snapshot().Equal(e.init) {
		t.Fatalf("应回滚干净、闸门从未打开：%s", s.State)
	}
	e.k.v6Leak.Store(false)
	post(t, e.d, "/v1/enable", 200) // 警告项（13）照样不通过：不影响
	if it := itemOf(envReport(t, e.d), "90"); it.Result != exitcheck.Fail {
		t.Fatalf("%+v", it)
	}
}

// f. 某项无法判断：按 5、15、45 秒重试（时钟注入），始终无法判断则失败并回滚；其间转为通过则成功。
func TestEnvEnableRetries(t *testing.T) {
	e := newEnvEnv(t, true)
	waitGateOpen(t, e.d)
	post(t, e.d, "/v1/restore", 200)
	e.k.probeSkip.Store(true)
	e.k.dnsErr.Store(true)
	done := make(chan []byte, 1)
	go func() { _, b := confirmCall(t, e.d, "/v1/enable"); done <- b }()
	var b []byte
	for i := 0; i < 400; i++ {
		select {
		case b = <-done:
		default:
			e.clk.Advance(5 * time.Second)
			time.Sleep(5 * time.Millisecond)
			continue
		}
		break
	}
	if !strings.Contains(string(b), "07 DNS") || status(t, e.d).State != "RESTORED" {
		t.Fatalf("始终无法判断应失败并回滚：%s", b)
	}
	// 第一次重试之后转为通过。
	go func() { _, b := confirmCall(t, e.d, "/v1/enable"); done <- b }()
	time.Sleep(50 * time.Millisecond)
	e.k.dnsErr.Store(false)
	for i := 0; i < 400; i++ {
		select {
		case b = <-done:
		default:
			e.clk.Advance(5 * time.Second)
			time.Sleep(5 * time.Millisecond)
			continue
		}
		break
	}
	if s := status(t, e.d); s.State != "ENABLED" {
		t.Fatalf("其间转为通过应成功：%s %s", s.State, b)
	}
}

// g. 运行中：每 10 分钟一轮；06 不通过 → 闸门关（原因 env_check），已有的走代理长连接 1 秒内被断、直连保持；
// 闸门关着时下一轮的探测经探测窗口发出；修复后通过 → 原因清除、出口复查重新放行。
func TestEnvRuntimeGate(t *testing.T) {
	e := newEnvEnv(t, true)
	waitGateOpen(t, e.d)
	plain := netip.AddrPortFrom(e.lookup(t, "plain.example.org"), 443)
	held, err := e.dial(20001, plain)
	if err != nil || echoConn(held) != nil {
		t.Fatal(err)
	}
	defer held.Close()
	direct, err := e.dial(22001, plain)
	if err != nil || echoConn(direct) != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	rounds := e.d.env.Rounds()
	e.k.v6Leak.Store(true)
	e.clk.Advance(envcheck.DefaultInterval)
	waitFor(t, func() bool { return e.d.env.Rounds() > rounds })
	st := status(t, e.d)
	if st.GateOpen || !strings.Contains(strings.Join(st.Gate.Reasons, ","), string(gatesm.ReasonEnvCheck)) {
		t.Fatalf("06 不通过应关闸门：%+v", st.Gate)
	}
	mustDropped(t, held, "环境检测关闸门")
	if err := echoConn(direct); err != nil {
		t.Fatalf("直连保持：%v", err)
	}
	ok := e.k.probeOK.Load()
	e.k.v6Leak.Store(false)
	e.d.env.Kick()
	waitFor(t, func() bool { return e.k.probeOK.Load() > ok })
	waitGateOpen(t, e.d)
	if s := status(t, e.d); !s.GateOpen || len(s.Gate.Reasons) != 0 {
		t.Fatalf("修复后应清掉原因并重新放行：%+v", s.Gate)
	}
}

// g. 四个触发补跑的时机：Claude 的去向变化（去向不变时不补跑）、切换地区、应用主代理、监护清掉接管故障。
func TestEnvKickTriggers(t *testing.T) {
	e := newEnvEnv(t, false, func(o *Options) { o.SuperviseInterval = 20 * time.Millisecond })
	waitGateOpen(t, e.d)
	next := func(name string, f func()) {
		t.Helper()
		n := e.d.env.Rounds()
		f()
		for i := 0; i < 400 && e.d.env.Rounds() <= n; i++ {
			time.Sleep(5 * time.Millisecond)
		}
		if e.d.env.Rounds() <= n {
			t.Fatalf("%s 之后应补一轮环境检测", name)
		}
	}
	claudeRules := "rules:\n  - {app: com.app.a, action: \"proxy:wg-a\"}\n  - {app: com.anthropic.claudefordesktop, action: \"proxy:wg-a\"}\n"
	next("Claude 的去向变化", func() {
		if code, b := putRules(t, e.d, claudeRules, false); code != 200 {
			t.Fatalf("%d %s", code, b)
		}
	})
	n := e.d.env.Rounds()
	if code, b := putRules(t, e.d, claudeRules+"  - {app: com.app.b, action: \"proxy:clash-b\"}\n", false); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	time.Sleep(60 * time.Millisecond)
	if e.d.env.Rounds() != n {
		t.Fatal("Claude 的去向没变时不补跑")
	}
	// busyRetry：监护每 20 毫秒会短暂占住忙标记（409 且没有确认码），重试。
	busyRetry := func(f func() (int, []byte)) (int, []byte) {
		code, b := f()
		for i := 0; i < 100 && code == http.StatusConflict && !strings.Contains(string(b), `"confirm"`); i++ {
			time.Sleep(10 * time.Millisecond)
			code, b = f()
		}
		return code, b
	}
	next("切换地区", func() {
		code, b := busyRetry(func() (int, []byte) {
			return putRegion(t, e.d, `{"mode":"manual","timezone":"Asia/Tokyo","lang":"ja-JP"}`)
		})
		if code == http.StatusConflict {
			cn := confirmOf(t, b)
			code, b = busyRetry(func() (int, []byte) {
				return withBody(t, "PUT", e.d, "/v1/region", RegionRequest{Mode: "manual", Timezone: "Asia/Tokyo", Lang: "ja-JP", Confirm: cn.Confirm})
			})
		}
		if code != 200 {
			t.Fatalf("%d %s", code, b)
		}
	})
	next("应用主代理", func() {
		if code, b := busyRetry(func() (int, []byte) { return call(t, e.d, "POST", "/v1/main-proxy/apply", nil) }); code != 200 {
			t.Fatalf("%d %s", code, b)
		}
	})
	e.sys.PF.DropMainRef() // 监护因保护失效加 takeover
	for i := 0; i < 300 && status(t, e.d).GateOpen; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	next("监护清掉接管故障", func() { e.sys.PF.RestoreMainRef() })
}

// h. 警告项的开关：关闭后“已跳过”、不参与判定；硬性项关不掉（422）；设置文件 0600，重启后保留。
func TestEnvSettingsAPI(t *testing.T) {
	e := newEnvEnv(t, false)
	waitGateOpen(t, e.d)
	if code, _ := withBody(t, "PUT", e.d, "/v1/envcheck/settings", EnvSettingsRequest{Disabled: []string{"06"}}); code != http.StatusUnprocessableEntity {
		t.Fatalf("硬性项关不掉：%d", code)
	}
	if code, b := withBody(t, "PUT", e.d, "/v1/envcheck/settings", EnvSettingsRequest{Disabled: []string{"90"}}); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	fi, err := os.Stat(filepath.Join(e.dir, envcheck.SettingsFileName))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("设置文件应 0600：%v", err)
	}
	e.k.warnFail.Store(true)
	if code, _ := call(t, e.d, "POST", "/v1/envcheck/run", nil); code != 200 {
		t.Fatal("补跑")
	}
	if it := itemOf(envReport(t, e.d), "90"); it.Result != envcheck.Skipped {
		t.Fatalf("关闭的警告项应“已跳过”：%+v", it)
	}
	// 重启后保留：新的实例（重新读文件）。
	if !(&envcheck.Settings{Path: filepath.Join(e.dir, envcheck.SettingsFileName)}).Disabled()["90"] {
		t.Fatal("重启后应保留")
	}
	// 打开回来：再跑一轮是“不通过”（警告，不影响闸门）。
	withBody(t, "PUT", e.d, "/v1/envcheck/settings", EnvSettingsRequest{Disabled: []string{}})
	call(t, e.d, "POST", "/v1/envcheck/run", nil)
	if it := itemOf(envReport(t, e.d), "90"); it.Result != exitcheck.Fail || !status(t, e.d).GateOpen {
		t.Fatalf("%+v", it)
	}
}

// l. status 里 17 项都有结果（6a 的五项、6b 的十二项）。
func TestEnvAllItems(t *testing.T) {
	e := newEnvEnv(t, false)
	waitGateOpen(t, e.d)
	rep := envReport(t, e.d)
	for i := 1; i <= 17; i++ {
		id := fmt.Sprintf("%02d", i)
		it := itemOf(rep, id)
		if it.ID == "" || it.Result == "" {
			t.Fatalf("%s 没有结果", id)
		}
		if it.Level == envcheck.Hard && it.Result != exitcheck.Pass {
			t.Fatalf("硬性项 %s 应通过：%+v", id, it)
		}
	}
}

// g. 本机真实 IP：一轮检测后，状态目录下所有文件、status、日志、事件里都没有它；网络变化后重测、补一轮检测。
func TestEnvRealIPNotLeaked(t *testing.T) {
	e := newEnvEnv(t, false, func(o *Options) { o.SuperviseInterval = 20 * time.Millisecond })
	waitGateOpen(t, e.d)
	call(t, e.d, "POST", "/v1/envcheck/run", nil)
	const real = "198.51.100.77"
	if eq, known := e.d.realIP.Equal(netip.MustParseAddr(real)); !eq || !known {
		t.Fatal("启用时应测到本机真实 IP（只在内存）")
	}
	var all strings.Builder
	filepath.WalkDir(e.dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, err := os.ReadFile(p); err == nil {
				all.Write(b)
			}
		}
		return nil
	})
	_, st := call(t, e.d, "GET", "/v1/status", nil)
	_, ev := call(t, e.d, "GET", "/v1/envcheck", nil)
	all.Write(st)
	all.Write(ev)
	all.WriteString(e.logBuf.String())
	for _, r := range e.d.events.List() {
		all.WriteString(r.Detail)
	}
	if strings.Contains(all.String(), real) {
		t.Fatal("本机真实 IP 出现在了文件、status、日志或事件里")
	}
	// 换网络：直连网段变了 → 重测、补一轮。
	dials, rounds := e.world.realDials.Load(), e.d.env.Rounds()
	e.ifaces.Store(&[]sysnet.IfaceInfo{
		{Name: "en0", Up: true, Addrs: []netip.Prefix{netip.MustParsePrefix("10.20.3.4/16")}},
		{Name: "bridge100", Up: true, Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.64.1/24")}},
	})
	waitFor(t, func() bool { return e.world.realDials.Load() > dials && e.d.env.Rounds() > rounds })
	if !hasEvent(e.d, "envcheck", "network_changed") {
		t.Fatal("应有网络变化的事件")
	}
}

// j. 不接地区同步（--dev-skip-region）：04、05 已跳过，命令记录里没有任何读写时区和语言的命令。
func TestEnvSkipRegion(t *testing.T) {
	e := newEnvEnv(t, false, func(o *Options) { o.Sys.VMRegion, o.Sys.Locale = nil, nil })
	waitGateOpen(t, e.d)
	rep := envReport(t, e.d)
	for _, id := range []string{"04", "05", "15", "16", "17"} {
		if it := itemOf(rep, id); it.Result != envcheck.Skipped {
			t.Fatalf("%s 应已跳过：%+v", id, it)
		}
	}
	for _, c := range e.m3Env.dataEnv.k.log.all() {
		for _, x := range []string{sysnet.PathSystemsetup, sysnet.PathDefaults, sysnet.PathLaunchctl} {
			if strings.HasPrefix(c, x) {
				t.Fatalf("不应有地区命令：%s", c)
			}
		}
	}
}
