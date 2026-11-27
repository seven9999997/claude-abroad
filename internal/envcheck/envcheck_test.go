package envcheck

import (
	"context"
	"errors"
	"go/ast"
	"go/token"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/gatesm"
	"claude-abroad/internal/preflight"
	"claude-abroad/internal/rules"
	"claude-abroad/internal/srccheck"
	"claude-abroad/internal/sysnet"
)

var bg = context.Background()
var errX = errors.New("查不了")

func res(t *testing.T, it Item) Outcome {
	t.Helper()
	o := it.Run(bg)
	if strings.Contains(o.Detail, "198.18") || strings.Contains(o.Detail, "203.0.113") {
		t.Fatalf("说明里不应有地址：%s", o.Detail)
	}
	return o
}

func want(t *testing.T, name string, o Outcome, r exitcheck.Result) {
	t.Helper()
	if o.Result != r {
		t.Fatalf("%s：应为 %s，实际 %s（%s）", name, r, o.Result, o.Detail)
	}
	if r == exitcheck.Fail && o.Fix == "" {
		t.Fatalf("%s：不通过时要有修复办法", name)
	}
}

// a. 01 接管就绪。
func TestTakeoverItem(t *testing.T) {
	want(t, "通过", res(t, Takeover(func(context.Context) ([]string, error) { return nil, nil })), exitcheck.Pass)
	want(t, "不通过", res(t, Takeover(func(context.Context) ([]string, error) { return []string{"断网保护失效"}, nil })), exitcheck.Fail)
	want(t, "无法判断", res(t, Takeover(func(context.Context) ([]string, error) { return nil, errX })), exitcheck.Unknown)
}

// a. 06 IPv6：没动过的 unknown 服务不算不通过。
func TestIPv6Item(t *testing.T) {
	snap := func(m map[string]sysnet.IPv6Setting, err error) func(context.Context) (map[string]sysnet.IPv6Setting, error) {
		return func(context.Context) (map[string]sysnet.IPv6Setting, error) { return m, err }
	}
	off := map[string]sysnet.IPv6Setting{"Wi-Fi": {Mode: sysnet.IPv6Off}, "Odd VPN": {Mode: sysnet.IPv6Unknown}}
	refuse := func(context.Context) error { return errors.New("no route") }
	o := res(t, IPv6(IPv6Deps{Snapshot: snap(off, nil), DialV6: refuse}))
	want(t, "通过", o, exitcheck.Pass)
	if !strings.Contains(o.Detail, "Odd VPN") {
		t.Fatal("没动过的服务应单独列出")
	}
	want(t, "服务没关", res(t, IPv6(IPv6Deps{Snapshot: snap(map[string]sysnet.IPv6Setting{"Wi-Fi": {Mode: sysnet.IPv6Automatic}}, nil), DialV6: refuse})), exitcheck.Fail)
	want(t, "连上了", res(t, IPv6(IPv6Deps{Snapshot: snap(off, nil), DialV6: func(context.Context) error { return nil }})), exitcheck.Fail)
	want(t, "无法判断", res(t, IPv6(IPv6Deps{Snapshot: snap(nil, errX), DialV6: refuse})), exitcheck.Unknown)
}

// a. 07 DNS。
func TestDNSItem(t *testing.T) {
	r := func(a []netip.Addr, err error) func(context.Context, string) ([]netip.Addr, error) {
		return func(_ context.Context, n string) ([]netip.Addr, error) {
			if n != ProbeName {
				t.Fatalf("解析的名字不对：%s", n)
			}
			return a, err
		}
	}
	want(t, "通过", res(t, DNS(r([]netip.Addr{netip.MustParseAddr("198.18.3.4")}, nil))), exitcheck.Pass)
	want(t, "不通过", res(t, DNS(r([]netip.Addr{netip.MustParseAddr("203.0.113.9")}, nil))), exitcheck.Fail)
	want(t, "无法判断", res(t, DNS(r(nil, errX))), exitcheck.Unknown)
	want(t, "空应答", res(t, DNS(r(nil, nil))), exitcheck.Unknown)
}

// a. 11 系统代理与其他接管。
func TestConflictsItem(t *testing.T) {
	f := func(fs ...preflight.Finding) func(context.Context) []preflight.Finding {
		return func(context.Context) []preflight.Finding { return fs }
	}
	want(t, "通过", res(t, Conflicts(f())), exitcheck.Pass)
	want(t, "不通过", res(t, Conflicts(f(preflight.Finding{ID: "system_proxy", Title: "系统代理已开启", Fix: "关掉"}))), exitcheck.Fail)
	want(t, "无法判断", res(t, Conflicts(f(preflight.Finding{ID: "system_proxy", Title: "无法判断", Undecidable: true}))), exitcheck.Unknown)
}

// a. 09 Claude 去向：三种判定各有不通过的例子。
func TestClaudeRouteItem(t *testing.T) {
	cfg := func(y string) func() *rules.Config {
		c, err := rules.Parse([]byte(y))
		if err != nil {
			t.Fatal(err)
		}
		return func() *rules.Config { return c }
	}
	up := func(x string) bool { return x == "wg-a" }
	want(t, "默认", res(t, ClaudeRoute(RouteDeps{Config: cfg("rules: []\n"), ExitUp: up})), exitcheck.Pass)
	want(t, "Claude 走可用的分流出口", res(t, ClaudeRoute(RouteDeps{Config: cfg("rules:\n  - {app: com.anthropic.claudefordesktop, action: \"proxy:wg-a\"}\n"), ExitUp: up})), exitcheck.Pass)
	// 1. Claude 的出口不可用（判定回退到主代理也算不通过？不：回退后走的是主代理，可用。这里用不会回退的判定模拟“出口不可用”）。
	down := func(c *rules.Config, w rules.Who, d rules.Dest, e rules.Env) rules.Decision {
		if w.Claude {
			return rules.Decision{Action: rules.Action{Kind: rules.Proxy, Exit: "wg-a"}}
		}
		return c.Decide(w, d, e)
	}
	want(t, "Claude 的出口不可用", res(t, ClaudeRoute(RouteDeps{Config: cfg("rules: []\n"), ExitUp: func(string) bool { return false }, Decide: down})), exitcheck.Fail)
	// 2. 绕过校验把 Claude 设成直连（直接构造配置对象）。
	bad := &rules.Config{Default: rules.ActMain, Rules: []rules.Rule{{App: "com.anthropic.claudefordesktop", Action: rules.ActDirect}}}
	want(t, "Claude 设成直连", res(t, ClaudeRoute(RouteDeps{Config: func() *rules.Config { return bad }, ExitUp: up})), exitcheck.Fail)
	// 3. 普通直连应用访问置顶域名没有被改走主代理（模拟规则引擎出错）。
	broken := func(c *rules.Config, w rules.Who, d rules.Dest, e rules.Env) rules.Decision {
		if w.App == localProbeApp {
			return rules.Decision{Action: rules.ActDirect}
		}
		return c.Decide(w, d, e)
	}
	want(t, "置顶没生效", res(t, ClaudeRoute(RouteDeps{Config: cfg("rules: []\n"), ExitUp: up, Decide: broken})), exitcheck.Fail)
	want(t, "无法判断", res(t, ClaudeRoute(RouteDeps{Config: func() *rules.Config { return nil }})), exitcheck.Unknown)
}

// ---- 整轮 ----

type countItem struct {
	Item
	n atomic.Int32
}

func fakeItem(id string, lvl Level, network bool, r func() exitcheck.Result) *countItem {
	c := &countItem{}
	c.Item = Item{ID: id, Name: "项目" + id, Level: lvl, Network: network, Run: func(context.Context) Outcome {
		c.n.Add(1)
		return Outcome{Result: r(), Detail: "x", Fix: "修 " + id}
	}}
	return c
}

func items(cs ...*countItem) []Item {
	var out []Item
	for _, c := range cs {
		out = append(out, c.Item)
	}
	return out
}

func val(r *atomic.Value) func() exitcheck.Result {
	return func() exitcheck.Result { return r.Load().(exitcheck.Result) }
}

// b. 先后依赖：01 不通过或无法判断时，要发网络探测的项执行 0 次，结果是无法判断并注明原因；只看本机状态的项照常执行。
func TestTakeoverGatesNetworkItems(t *testing.T) {
	for _, r01 := range []exitcheck.Result{exitcheck.Fail, exitcheck.Unknown} {
		var v atomic.Value
		v.Store(r01)
		i01 := fakeItem("01", Hard, false, val(&v))
		net6 := fakeItem("06", Hard, true, func() exitcheck.Result { return exitcheck.Pass })
		net7 := fakeItem("07", Hard, true, func() exitcheck.Result { return exitcheck.Pass })
		loc9 := fakeItem("09", Hard, false, func() exitcheck.Result { return exitcheck.Pass })
		rep := (&Runner{Items: items(net7, loc9, net6, i01)}).Round(bg)
		if net6.n.Load() != 0 || net7.n.Load() != 0 || loc9.n.Load() != 1 || i01.n.Load() != 1 {
			t.Fatalf("01=%s：网络项不应执行", r01)
		}
		for _, it := range rep.Items {
			if (it.ID == "06" || it.ID == "07") && (it.Result != exitcheck.Unknown || !strings.Contains(it.Detail, "接管未就绪")) {
				t.Fatalf("%+v", it)
			}
		}
		if rep.Items[0].ID != "01" {
			t.Fatal("01 应先跑")
		}
		v.Store(exitcheck.Pass)
		rep = (&Runner{Items: items(net7, loc9, net6, i01)}).Round(bg)
		if net6.n.Load() != 1 || rep.Hard != exitcheck.Pass {
			t.Fatal("01 通过后网络项照常执行")
		}
	}
}

type sleepClock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
	hook  func(n int)
}

func (c *sleepClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *sleepClock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.slept = append(c.slept, d)
	n := len(c.slept)
	h := c.hook
	c.mu.Unlock()
	if h != nil {
		h(n)
	}
	return nil
}

// f（envcheck 一侧）. 无法判断按 5、15、45 秒重试，只重跑无法判断的项；其间转为通过则通过；始终无法判断则无法判断。
// 警告项不影响总判定。
func TestRunWithRetry(t *testing.T) {
	var v atomic.Value
	v.Store(exitcheck.Unknown)
	flaky := fakeItem("07", Hard, false, val(&v))
	ok := fakeItem("09", Hard, false, func() exitcheck.Result { return exitcheck.Pass })
	warn := fakeItem("13", Warn, false, func() exitcheck.Result { return exitcheck.Fail })
	c := &sleepClock{now: time.Unix(1_900_000_000, 0)}
	c.hook = func(n int) {
		if n == 2 {
			v.Store(exitcheck.Pass)
		}
	}
	rep := (&Runner{Items: items(flaky, ok, warn), Clock: c}).RunWithRetry(bg)
	if rep.Hard != exitcheck.Pass || len(c.slept) != 2 || c.slept[0] != 5*time.Second || c.slept[1] != 15*time.Second {
		t.Fatalf("%s %v", rep.Hard, c.slept)
	}
	if ok.n.Load() != 1 || flaky.n.Load() != 3 {
		t.Fatalf("只重跑无法判断的项：ok=%d flaky=%d", ok.n.Load(), flaky.n.Load())
	}
	v.Store(exitcheck.Unknown)
	c2 := &sleepClock{}
	rep = (&Runner{Items: items(flaky), Clock: c2}).RunWithRetry(bg)
	if rep.Hard != exitcheck.Unknown || len(c2.slept) != 3 || c2.slept[2] != 45*time.Second {
		t.Fatalf("%s %v", rep.Hard, c2.slept)
	}
	// 明确不通过：不重试。
	bad := fakeItem("11", Hard, false, func() exitcheck.Result { return exitcheck.Fail })
	c3 := &sleepClock{}
	rep = (&Runner{Items: items(bad, flaky), Clock: c3}).RunWithRetry(bg)
	if rep.Hard != exitcheck.Fail || len(c3.slept) != 0 || len(rep.Failed()) != 1 {
		t.Fatal("明确不通过时不重试")
	}
}

// ---- 运行中 ----

type stepClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*stepTimer
}

type stepTimer struct {
	c       *stepClock
	at      time.Time
	f       func()
	stopped bool
}

func (t *stepTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	w := !t.stopped
	t.stopped = true
	return w
}
func (c *stepClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *stepClock) AfterFunc(d time.Duration, f func()) gatesm.Stopper {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &stepTimer{c: c, at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}
func (c *stepClock) Advance(d time.Duration) {
	c.mu.Lock()
	end := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var next *stepTimer
		for _, t := range c.timers {
			if !t.stopped && !t.at.After(end) && (next == nil || t.at.Before(next.at)) {
				next = t
			}
		}
		if next == nil {
			c.now = end
			c.mu.Unlock()
			return
		}
		next.stopped = true
		if next.at.After(c.now) {
			c.now = next.at
		}
		c.mu.Unlock()
		next.f()
	}
}

func newGate(c gatesm.Clock) *gatesm.Gate {
	g := gatesm.New(gatesm.Options{Clock: c})
	g.ClearReason(gatesm.ReasonState)
	return g
}

func has(g *gatesm.Gate, r gatesm.Reason) bool {
	for _, x := range g.Status().Reasons {
		if x == string(r) {
			return true
		}
	}
	return false
}

// g（envcheck 一侧）. 不通过 → env_check；通过 → 清掉并补出口复查；无法判断沿用 10 分钟（9:59 不关、10:01 关）；
// 新实例（重启）无法判断直接关；闸门因环境检测关着时，下一轮的探测在探测窗口里发出。
func TestMonitor(t *testing.T) {
	c := &stepClock{now: time.Unix(1_900_000_000, 0)}
	g := newGate(c)
	var v atomic.Value
	v.Store(exitcheck.Pass)
	sawWindow := atomic.Bool{}
	probeItem := Item{ID: "07", Name: "需要发探测的项", Level: Hard, Network: true, Run: func(context.Context) Outcome {
		sawWindow.Store(g.ProbeOpen())
		return Outcome{Result: v.Load().(exitcheck.Result), Detail: "x", Fix: "修"}
	}}
	t01 := Takeover(func(context.Context) ([]string, error) { return nil, nil })
	passes := 0
	m := &Monitor{Runner: &Runner{Items: []Item{t01, probeItem}}, Gate: g, Clock: c, AfterPass: func() { passes++ }}
	m.RunOnce(bg)
	if has(g, gatesm.ReasonEnvCheck) || passes != 1 || !sawWindow.Load() || g.ProbeOpen() {
		t.Fatal("通过：不加原因、补出口复查；探测在窗口里发出，结束后窗口关闭")
	}
	v.Store(exitcheck.Fail)
	m.RunOnce(bg)
	if !has(g, gatesm.ReasonEnvCheck) {
		t.Fatal("不通过应加 env_check")
	}
	v.Store(exitcheck.Pass)
	sawWindow.Store(false)
	m.RunOnce(bg)
	if has(g, gatesm.ReasonEnvCheck) || !sawWindow.Load() || passes != 2 {
		t.Fatal("闸门因环境检测关着时，下一轮的探测也要经窗口发出；通过后清掉原因、补出口复查")
	}
	v.Store(exitcheck.Unknown)
	c.Advance(10*time.Minute - time.Second)
	m.RunOnce(bg)
	if has(g, gatesm.ReasonEnvCheck) {
		t.Fatal("9 分 59 秒内无法判断沿用")
	}
	c.Advance(2 * time.Second)
	m.RunOnce(bg)
	if !has(g, gatesm.ReasonEnvCheck) {
		t.Fatal("超过 10 分钟无法判断应关")
	}
	// 重启（新实例）：没有明确通过过，无法判断直接关。
	g2 := newGate(c)
	m2 := &Monitor{Runner: &Runner{Items: []Item{t01, probeItem}}, Gate: g2, Clock: c}
	m2.RunOnce(bg)
	if !has(g2, gatesm.ReasonEnvCheck) {
		t.Fatal("重启后必须重新明确通过")
	}
}

// g. 每 10 分钟一轮（时钟注入）；Kick 立即补一轮；不是启用状态时不跑。
func TestMonitorLoop(t *testing.T) {
	c := &stepClock{now: time.Unix(1_900_000_000, 0)}
	g := newGate(c)
	it := fakeItem("09", Hard, false, func() exitcheck.Result { return exitcheck.Pass })
	var enabled atomic.Bool
	enabled.Store(true)
	m := &Monitor{Runner: &Runner{Items: items(it)}, Gate: g, Clock: c, Enabled: enabled.Load}
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	wait := func(n int) {
		t.Helper()
		for i := 0; i < 500 && m.Rounds() < n; i++ {
			time.Sleep(2 * time.Millisecond)
		}
		if m.Rounds() != n {
			t.Fatalf("轮数 %d，应为 %d", m.Rounds(), n)
		}
	}
	waitTimer := func() {
		for i := 0; i < 500; i++ {
			c.mu.Lock()
			n := 0
			for _, t := range c.timers {
				if !t.stopped {
					n++
				}
			}
			c.mu.Unlock()
			if n > 0 {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitTimer()
	c.Advance(10*time.Minute - time.Second)
	time.Sleep(20 * time.Millisecond)
	wait(0)
	c.Advance(time.Second)
	wait(1)
	m.Kick()
	wait(2)
	enabled.Store(false)
	m.Kick()
	time.Sleep(20 * time.Millisecond)
	wait(2)
	cancel()
	<-done
}

// h. 警告项的开关：关闭后“已跳过”、不参与判定；硬性项关不掉；文件 0600，重新读取后保留。
func TestSettings(t *testing.T) {
	dir := t.TempDir()
	s := &Settings{Path: filepath.Join(dir, SettingsFileName)}
	hard := fakeItem("09", Hard, false, func() exitcheck.Result { return exitcheck.Pass })
	warn := fakeItem("13", Warn, false, func() exitcheck.Result { return exitcheck.Fail })
	all := items(hard, warn)
	if err := s.SetDisabled([]string{"09"}, all); !errors.Is(err, ErrHardItem) {
		t.Fatal("硬性项不能关")
	}
	if err := s.SetDisabled([]string{"99"}, all); !errors.Is(err, ErrHardItem) {
		t.Fatal("不认识的项不能关")
	}
	if _, err := os.Stat(s.Path); !os.IsNotExist(err) {
		t.Fatal("被拒时不应写文件")
	}
	if err := s.SetDisabled([]string{"13", "13"}, all); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(s.Path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("权限：%v", fi.Mode())
	}
	s2 := &Settings{Path: s.Path} // 重启后重新读
	rep := (&Runner{Items: all, Disabled: s2.Disabled}).Round(bg)
	if warn.n.Load() != 0 || rep.Items[1].Result != Skipped || rep.Hard != exitcheck.Pass {
		t.Fatalf("关闭的警告项应跳过：%+v", rep.Items)
	}
	os.Chmod(s.Path, 0o644)
	if len(s2.Disabled()) != 0 {
		t.Fatal("别人可读的设置文件按没有处理")
	}
}

// 决定 8：Claude 的域名字面量只允许出现在规则引擎的置顶名单和第 09、10 项里。唯一的例外是开发用的 daemon-sim（测试工具）；
// fakedns 的受保护名单从内置置顶名单推导（m4-plan v2.7 第 0 步）。
func TestClaudeLiteralsOnlyInAllowedFiles(t *testing.T) {
	root := filepath.Join("..", "..")
	allowed := map[string]bool{
		filepath.Join("internal", "rules", "pinned.go"):          true,
		filepath.Join("internal", "envcheck", "claude_route.go"): true,
		filepath.Join("internal", "daemonsim", "multi.go"):       true, // 开发用模拟器
	}
	var hits []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "third_party", "out", ".git", ".claude", "testdata", "node_modules", "guest":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		af := srccheck.Parse(t, p)
		ast.Inspect(af, func(n ast.Node) bool {
			bl, ok := n.(*ast.BasicLit)
			if !ok || bl.Kind != token.STRING {
				return true
			}
			v, _ := strconv.Unquote(bl.Value)
			lv := strings.ToLower(v)
			if (strings.Contains(lv, "claude.ai") || strings.Contains(lv, "anthropic.com") || strings.Contains(lv, "claude.com")) && !allowed[rel] {
				hits = append(hits, rel+"："+v)
			}
			return true
		})
		return nil
	})
	sort.Strings(hits)
	if len(hits) > 0 {
		t.Fatalf("Claude 的域名字面量出现在不允许的文件里：\n%s", strings.Join(hits, "\n"))
	}
}
