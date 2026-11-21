package gatesm

import (
	"context"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock：Advance 把时间往前推，并按到期顺序同步触发定时器。
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	c       *fakeClock
	at      time.Time
	f       func()
	stopped bool
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	return was
}

func newClock() *fakeClock { return &fakeClock{now: time.Unix(1_900_000_000, 0)} }

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Stopper {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	end := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		sort.SliceStable(c.timers, func(i, j int) bool { return c.timers[i].at.Before(c.timers[j].at) })
		var next *fakeTimer
		for _, t := range c.timers {
			if !t.stopped && !t.at.After(end) {
				next = t
				break
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

type rec struct {
	mu     sync.Mutex
	closes int
	events []string
}

func (r *rec) gate(c Clock) *Gate {
	return New(Options{Clock: c,
		OnClose: func() { r.mu.Lock(); r.closes++; r.mu.Unlock() },
		OnEvent: func(k, _ string) { r.mu.Lock(); r.events = append(r.events, k); r.mu.Unlock() }})
}

func (r *rec) n() int { r.mu.Lock(); defer r.mu.Unlock(); return r.closes }
func (r *rec) has(k string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e == k {
			return true
		}
	}
	return false
}

func openGate(t *testing.T, g *Gate) {
	t.Helper()
	g.ClearReason(ReasonState)
	if !g.Grant(g.BeginCheck(), DefaultLease) || !g.IsOpen() {
		t.Fatal("清掉 state 后放行应打开")
	}
}

// a. 放行 90 秒、60 秒续期；不续期到期后走关闭路径，有 lease_expired；多原因；各方只清自己的；过期的检查结果不能放行。
func TestLease(t *testing.T) {
	c := newClock()
	r := &rec{}
	g := r.gate(c)
	if g.IsOpen() || g.Grant(g.BeginCheck(), DefaultLease) {
		t.Fatal("初始有 state 原因，不能放行")
	}
	openGate(t, g)
	c.Advance(60 * time.Second)
	g.Grant(g.BeginCheck(), DefaultLease) // 续期到 150 秒
	c.Advance(80 * time.Second)           // 140 秒
	if !g.IsOpen() || r.n() != 0 {
		t.Fatal("续期后应仍开着")
	}
	c.Advance(10*time.Second + time.Millisecond) // 150 秒多一点
	if g.IsOpen() || r.n() != 1 || !r.has(EvLeaseExpired) {
		t.Fatalf("到期应关闭一次并记 lease_expired：open=%v closes=%d", g.IsOpen(), r.n())
	}
	// 到期后下一次放行自然恢复（到期不是原因）。
	if !g.Grant(g.BeginCheck(), DefaultLease) || !g.IsOpen() {
		t.Fatal("到期后放行应恢复")
	}
	// 多原因：清掉一个不开；全清了也要等放行。
	g.AddReason(ReasonTakeover, "x")
	g.AddReason(ReasonExitCheck, "y")
	g.ClearReason(ReasonTakeover)
	if g.IsOpen() || g.Grant(g.BeginCheck(), DefaultLease) {
		t.Fatal("还有 exit_check 时不能打开")
	}
	g.ClearReason(ReasonExitCheck)
	if g.IsOpen() {
		t.Fatal("清掉原因本身不打开，要等放行")
	}
	// 过期的检查结果：检查期间被关过，放行作废。
	ep := g.BeginCheck()
	g.AddReason(ReasonTakeover, "检查期间出了问题")
	g.ClearReason(ReasonTakeover)
	if g.Grant(ep, DefaultLease) {
		t.Fatal("检查期间闸门被关过，这次的结果不能放行")
	}
	if !g.Grant(g.BeginCheck(), DefaultLease) {
		t.Fatal("新的检查可以放行")
	}
	if st := g.Status(); !st.Open || st.LeaseLeftMs != 90_000 || len(st.Reasons) != 0 {
		t.Fatalf("status：%+v", st)
	}
	// 只剩 env_check。
	g.AddReason(ReasonEnvCheck, "z")
	if !g.OnlyEnvCheck() {
		t.Fatal("只剩 env_check")
	}
	g.AddReason(ReasonTakeover, "w")
	if g.OnlyEnvCheck() {
		t.Fatal("还有 takeover")
	}
}

// b. 每次由开变关恰好一次断开；续期及时时不断。
func TestCloseExactlyOnce(t *testing.T) {
	c := newClock()
	r := &rec{}
	g := r.gate(c)
	openGate(t, g)
	g.AddReason(ReasonTakeover, "a")
	g.AddReason(ReasonExitCheck, "b") // 已经关着：不再断开
	if r.n() != 1 {
		t.Fatalf("加原因关闭应恰好断开一次：%d", r.n())
	}
	g.ClearReason(ReasonTakeover)
	g.ClearReason(ReasonExitCheck)
	g.Grant(g.BeginCheck(), DefaultLease)
	for i := 0; i < 10; i++ { // 每 60 秒续期，持续 10 分钟
		c.Advance(60 * time.Second)
		g.Grant(g.BeginCheck(), DefaultLease)
	}
	if r.n() != 1 || !g.IsOpen() {
		t.Fatalf("续期及时时不应断开：%d", r.n())
	}
	c.Advance(91 * time.Second)
	if r.n() != 2 {
		t.Fatalf("到期应再断开一次：%d", r.n())
	}
}

// c. 性质测试：随机事件序列，不少于 5000 组。
func TestGateProperty(t *testing.T) {
	reasons := []Reason{ReasonTakeover, ReasonExitCheck, ReasonEnvCheck, ReasonState}
	for seed := int64(0); seed < 5000; seed++ {
		r := rand.New(rand.NewSource(seed))
		c := newClock()
		closes := 0
		g := New(Options{Clock: c, OnClose: func() { closes++ }})
		var stale []uint64
		for step := 0; step < 40; step++ {
			// 断言只看外部观察到的开关（IsOpen），和断开回调的计数比，不和闸门自己的转换回调比。
			wasOpen, before := g.IsOpen(), closes
			granted := false
			switch r.Intn(6) {
			case 0:
				g.AddReason(reasons[r.Intn(4)], "x")
			case 1:
				g.ClearReason(reasons[r.Intn(4)])
			case 2:
				ep := g.BeginCheck()
				stale = append(stale, ep)
				granted = g.Grant(ep, time.Duration(1+r.Intn(120))*time.Second)
			case 3:
				if len(stale) > 0 { // 过期的放行
					granted = g.Grant(stale[r.Intn(len(stale))], DefaultLease)
				}
			default:
				c.Advance(time.Duration(r.Intn(100)) * time.Second) // 可能跨过到期点：也是一次由开变关
			}
			isOpen := g.IsOpen()
			st := g.Status()
			if isOpen && (len(st.Reasons) > 0 || st.LeaseLeftMs <= 0) {
				t.Fatalf("种子 %d 第 %d 步：显示为开但有原因或放行期已过：%+v", seed, step, st)
			}
			if !wasOpen && isOpen && !granted {
				t.Fatalf("种子 %d 第 %d 步：没有放行却由关变开", seed, step)
			}
			want := before
			if wasOpen && !isOpen {
				want = before + 1
			}
			if closes != want {
				t.Fatalf("种子 %d 第 %d 步：开=%v→%v，断开次数 %d→%d（应为 %d）", seed, step, wasOpen, isOpen, before, closes, want)
			}
		}
	}
}

// d、g. 复查循环。
type fakeCheck struct {
	mu    sync.Mutex
	res   Result
	calls int
	block bool
}

func (f *fakeCheck) Check(ctx context.Context) (Result, string) {
	f.mu.Lock()
	f.calls++
	res, block := f.res, f.block
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return Unknown, "超时"
	}
	return res, ""
}

func (f *fakeCheck) set(r Result) { f.mu.Lock(); f.res = r; f.mu.Unlock() }
func (f *fakeCheck) n() int       { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

func TestRecheckResults(t *testing.T) {
	c := newClock()
	r := &rec{}
	g := r.gate(c)
	g.ClearReason(ReasonState)
	fc := &fakeCheck{res: Pass}
	rc := &Rechecker{Gate: g, Check: fc.Check, Clock: c}
	if rc.RunOnce(context.Background()) != Pass || !g.IsOpen() {
		t.Fatal("通过应放行")
	}
	// 连续无法判断：9 分 59 秒仍开（每 60 秒一轮续期），10 分 1 秒关。
	fc.set(Unknown)
	for elapsed := time.Duration(0); elapsed < 9*time.Minute+59*time.Second; {
		step := 60 * time.Second
		if elapsed+step > 9*time.Minute+59*time.Second {
			step = 9*time.Minute + 59*time.Second - elapsed
		}
		c.Advance(step)
		elapsed += step
		rc.RunOnce(context.Background())
	}
	if !g.IsOpen() {
		t.Fatal("无法判断 9 分 59 秒内应沿用上一次的通过")
	}
	c.Advance(2 * time.Second) // 10 分 1 秒
	rc.RunOnce(context.Background())
	if g.IsOpen() {
		t.Fatal("无法判断超过 10 分钟应关闭")
	}
	// 中间一次通过重新计时。
	fc.set(Pass)
	rc.RunOnce(context.Background())
	fc.set(Unknown)
	c.Advance(9 * time.Minute)
	rc.RunOnce(context.Background())
	if !g.IsOpen() {
		t.Fatal("通过之后重新计时")
	}
	// 不通过：关。
	fc.set(Fail)
	rc.RunOnce(context.Background())
	if g.IsOpen() || g.Status().Reasons[0] != string(ReasonExitCheck) {
		t.Fatal("不通过应加 exit_check")
	}
	// 进程重启（新实例）：无法判断不放行。
	g2 := (&rec{}).gate(c)
	g2.ClearReason(ReasonState)
	rc2 := &Rechecker{Gate: g2, Check: (&fakeCheck{res: Unknown}).Check, Clock: c}
	rc2.RunOnce(context.Background())
	if g2.IsOpen() {
		t.Fatal("重启后必须重新明确通过一次")
	}
}

// d. 探测卡 25 秒（超过一轮的总时限 20 秒）：按无法判断沿用，闸门不关。真实时钟、时限调小。
func TestRecheckStuckProbe(t *testing.T) {
	r := &rec{}
	g := r.gate(RealClock)
	g.ClearReason(ReasonState)
	fc := &fakeCheck{res: Pass}
	rc := &Rechecker{Gate: g, Check: fc.Check, Total: 200 * time.Millisecond, Lease: 5 * time.Second}
	rc.RunOnce(context.Background())
	fc.mu.Lock()
	fc.block = true // 卡住，超过总时限
	fc.mu.Unlock()
	start := time.Now()
	if res := rc.RunOnce(context.Background()); res != Unknown || time.Since(start) > 2*time.Second {
		t.Fatalf("卡住的探测应在总时限内按无法判断返回：%v %s", res, time.Since(start))
	}
	if !g.IsOpen() || r.n() != 0 {
		t.Fatal("探测卡住时闸门不应关")
	}
}

// g. 复查循环只在启用时探测；切到启用后 Kick 立即一轮；复查协程死了，放行期到期闸门自己关。
func TestRecheckLoop(t *testing.T) {
	c := newClock()
	r := &rec{}
	g := r.gate(c)
	fc := &fakeCheck{res: Pass}
	var enabled atomic.Bool
	rc := &Rechecker{Gate: g, Check: fc.Check, Clock: c, Enabled: enabled.Load}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { rc.Run(ctx); close(done) }()
	waitTimers := func() {
		for i := 0; i < 200; i++ {
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
	for i := 0; i < 5; i++ { // 未启用：5 轮都不探测
		waitTimers()
		c.Advance(DefaultInterval)
	}
	time.Sleep(20 * time.Millisecond)
	if fc.n() != 0 {
		t.Fatalf("未启用时不应探测：%d", fc.n())
	}
	enabled.Store(true)
	g.ClearReason(ReasonState)
	rc.Kick() // 切到启用后立即一轮
	for i := 0; i < 200 && !g.IsOpen(); i++ {
		time.Sleep(time.Millisecond)
	}
	if fc.n() != 1 || !g.IsOpen() {
		t.Fatalf("切到启用后应立即一轮并放行：%d", fc.n())
	}
	// 复查协程死了（ctx 取消，Run 退出）：没人续期，放行期到期闸门自己关、断开一次。
	cancel()
	<-done
	c.Advance(DefaultLease + time.Second)
	if g.IsOpen() || r.n() != 1 || !r.has(EvLeaseExpired) {
		t.Fatalf("复查停了，放行期到期应自己关：open=%v closes=%d", g.IsOpen(), r.n())
	}
}

// AfterEnable：30 秒内的通过结果直接放行，不再多查；超过就补一轮。
func TestAfterEnable(t *testing.T) {
	c := newClock()
	g := (&rec{}).gate(c)
	g.ClearReason(ReasonState)
	fc := &fakeCheck{res: Pass}
	rc := &Rechecker{Gate: g, Check: fc.Check, Clock: c}
	rc.AfterEnable(c.Now().Add(-10 * time.Second))
	if !g.IsOpen() || fc.n() != 0 {
		t.Fatal("30 秒内的通过结果应直接放行，不再多查")
	}
	g2 := (&rec{}).gate(c)
	g2.ClearReason(ReasonState)
	rc2 := &Rechecker{Gate: g2, Check: fc.Check, Clock: c}
	rc2.AfterEnable(c.Now().Add(-31 * time.Second))
	if g2.IsOpen() {
		t.Fatal("超过 30 秒不应直接放行")
	}
	select {
	case <-rc2.kickCh():
	default:
		t.Fatal("超过 30 秒应补一轮（Kick）")
	}
}

// h. 分流出口的可用状态（m4-plan v2.12 决定 4）：没登记的不可用；正式出口第一次明确通过之前不可用；
// 四个条件缺一不可；无法判断沿用 10 分钟（9 分 59 秒可用、10 分 1 秒不可用）；UDP 只看 UDP 探测；
// 开流明确失败后 30 秒不可用；开发用的出口沿用“除非开流明确失败，否则可用”。
func TestExitAvail(t *testing.T) {
	c := newClock()
	a := &ExitAvail{Clock: c}
	if a.Up("wg-a") || a.UDPOK("wg-a") {
		t.Fatal("没登记的出口不可用")
	}
	a.Configure([]string{"wg-a"})
	if a.Up("wg-a") {
		t.Fatal("刚登记、没下发的出口不可用")
	}
	a.SetPushed("wg-a", true, "")
	a.SetAgent("wg-a", true, "")
	if a.Up("wg-a") || a.View("wg-a").Reason != "还没有通过出口检查" {
		t.Fatalf("第一次明确通过之前不可用：%+v", a.View("wg-a"))
	}
	a.Update("wg-a", Unknown, Unknown, nil)
	if a.Up("wg-a") {
		t.Fatal("从没通过过时，无法判断也不可用")
	}
	a.Update("wg-a", Pass, Fail, map[string]string{"https": "pass", "udp": "fail"})
	if !a.Up("wg-a") || a.UDPOK("wg-a") {
		t.Fatal("通过后可用；UDP 探测不通过时只有 UDP 回退")
	}
	v := a.View("wg-a")
	if !v.Up || v.Check != "pass" || v.UDP != "fail" || v.Items["https"] != "pass" {
		t.Fatalf("%+v", v)
	}
	// 无法判断：沿用 10 分钟
	c.Advance(time.Minute)
	a.Update("wg-a", Unknown, Unknown, nil)
	c.Advance(8*time.Minute + 59*time.Second) // 距上次通过 9 分 59 秒
	if !a.Up("wg-a") {
		t.Fatal("9 分 59 秒仍可用")
	}
	c.Advance(2 * time.Second) // 10 分 1 秒
	if a.Up("wg-a") || a.View("wg-a").Reason != "出口检查无法判断超过 10 分钟" {
		t.Fatalf("10 分 1 秒不可用：%+v", a.View("wg-a"))
	}
	a.Update("wg-a", Pass, Pass, nil)
	if !a.Up("wg-a") || !a.UDPOK("wg-a") {
		t.Fatal("恢复通过后可用，UDP 也可以")
	}
	a.Update("wg-a", Fail, Pass, nil)
	if a.Up("wg-a") {
		t.Fatal("明确不通过时不可用")
	}
	a.Update("wg-a", Pass, Pass, nil)
	a.MarkDown("wg-a")
	if a.Up("wg-a") || a.Snapshot()["wg-a"] {
		t.Fatal("开流明确失败后 30 秒内不可用")
	}
	c.Advance(31 * time.Second)
	if !a.Up("wg-a") {
		t.Fatal("30 秒后恢复")
	}
	a.SetAgent("wg-a", false, "隧道设备不在")
	if a.Up("wg-a") {
		t.Fatal("agent 报引擎不可用时不可用")
	}
	a.SetAgent("wg-a", true, "")
	a.SetPushed("wg-a", false, "虚拟机没有就绪")
	if a.Up("wg-a") || a.View("wg-a").AgentUp {
		t.Fatal("下发失败时不可用")
	}
	// 去掉的出口连同状态一起没了
	a.Configure(nil)
	if a.Up("wg-a") || len(a.Snapshot()) != 0 {
		t.Fatal("去掉后不可用")
	}
	// 开发用的出口
	a.SetDev("dev-b")
	if !a.Up("dev-b") || !a.UDPOK("dev-b") {
		t.Fatal("开发用的出口除非开流失败否则可用")
	}
	a.Update("dev-b", Fail, Fail, nil) // 检查结果对开发用的出口不起作用
	a.SetPushed("dev-b", false, "x")
	if !a.Up("dev-b") {
		t.Fatal("开发用的出口不看检查和下发")
	}
	a.MarkDown("dev-b")
	if a.Up("dev-b") {
		t.Fatal("开发用的出口开流失败后 30 秒不可用")
	}
	a.Configure([]string{"x"})
	if _, ok := a.Snapshot()["dev-b"]; !ok {
		t.Fatal("Configure 不动开发用的出口")
	}
}

// 检查函数 panic：不让进程退出，记事件，这一轮按无法判断处理（宽限内续期），循环继续下一轮。
func TestRecheckPanic(t *testing.T) {
	c := newClock()
	r := &rec{}
	g := r.gate(c)
	var mu sync.Mutex
	calls, panicking := 0, false
	var evs []string
	rc := &Rechecker{Gate: g, Clock: c, Check: func(context.Context) (Result, string) {
		mu.Lock()
		calls++
		p := panicking
		mu.Unlock()
		if p {
			panic("203.0.113.7 注入的 panic")
		}
		return Pass, ""
	}, OnEvent: func(k, d string) { mu.Lock(); evs = append(evs, k+" "+d); mu.Unlock() }}
	g.ClearReason(ReasonState)
	if rc.RunOnce(context.Background()) != Pass || !g.IsOpen() {
		t.Fatal("第一轮应通过并放行")
	}
	mu.Lock()
	panicking = true
	mu.Unlock()
	if res := rc.RunOnce(context.Background()); res != Unknown {
		t.Fatalf("panic 的一轮应按无法判断：%v", res)
	}
	mu.Lock()
	if len(evs) != 1 || !strings.HasPrefix(evs[0], EvCheckPanic) || strings.Contains(evs[0], "203.0.113.7") {
		t.Fatalf("应记一条不带 panic 值的事件：%q", evs)
	}
	mu.Unlock()
	if !g.IsOpen() {
		t.Fatal("宽限内的无法判断应续期")
	}
	// 循环继续：Run 里连着 panic 两轮后仍在跑，恢复正常后照常放行。
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { rc.Run(ctx); close(done) }()
	waitCalls := func(n int) {
		for i := 0; i < 500; i++ {
			mu.Lock()
			k := calls
			mu.Unlock()
			if k >= n {
				return
			}
			rc.Kick()
			time.Sleep(2 * time.Millisecond)
		}
		t.Fatalf("循环没有继续：calls<%d", n)
	}
	waitCalls(4)
	mu.Lock()
	panicking = false
	mu.Unlock()
	before := rc.Rounds()
	for i := 0; i < 500 && rc.Rounds() == before; i++ {
		rc.Kick()
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done
	if !g.IsOpen() || r.n() != 0 {
		t.Fatalf("panic 之后循环应继续、闸门不应关：open=%v closes=%d", g.IsOpen(), r.n())
	}
}

// c. 探测窗口（M4-6a）：有 takeover 或 exit_check 时打不开；打开后两者任一出现立即失效（之后清掉原因也不恢复）；
// 到时限自动关闭；重复开关安全；窗口不改变闸门的开关。
func TestProbeWindow(t *testing.T) {
	c := newClock()
	r := &rec{}
	g := r.gate(c) // 初始有 state 原因：可以开窗口（启用第 ⑦ 步就是这样）
	if !g.OpenProbe(0) || !g.ProbeOpen() || g.IsOpen() {
		t.Fatal("只有 state 原因时可以打开，闸门仍是关")
	}
	c.Advance(DefaultProbeWindow - time.Second)
	if !g.ProbeOpen() {
		t.Fatal("时限内应有效")
	}
	c.Advance(time.Second)
	if g.ProbeOpen() {
		t.Fatal("到时限应自动关闭")
	}
	g.CloseProbe()
	g.CloseProbe()
	for _, bad := range []Reason{ReasonTakeover, ReasonExitCheck} {
		g.AddReason(bad, "x")
		if g.OpenProbe(time.Minute) || g.ProbeOpen() {
			t.Fatalf("有 %s 时打不开", bad)
		}
		g.ClearReason(bad)
		if !g.OpenProbe(time.Minute) || !g.ProbeOpen() {
			t.Fatal("清掉后可以打开")
		}
		g.AddReason(bad, "y")
		if g.ProbeOpen() {
			t.Fatalf("打开后出现 %s 应立即失效", bad)
		}
		g.ClearReason(bad)
		if g.ProbeOpen() {
			t.Fatal("失效后清掉原因也不自动恢复，要重新打开")
		}
	}
	g.AddReason(ReasonEnvCheck, "env")
	if !g.OpenProbe(time.Minute) || !g.ProbeOpen() || g.IsOpen() {
		t.Fatal("env_check 关着闸门时可以开窗口（下一轮检测要用），闸门仍是关")
	}
	g.OpenProbe(time.Minute)
	g.CloseProbe()
	if g.ProbeOpen() || r.n() != 0 {
		t.Fatal("关闭后无效；窗口开关不触发断开")
	}
}
