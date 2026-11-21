package pf

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeNet struct {
	mu   sync.Mutex
	snap NetSnapshot
	err  error
}

func (n *fakeNet) Snapshot(context.Context) (NetSnapshot, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.snap, n.err
}

func (n *fakeNet) set(s NetSnapshot, err error) { n.mu.Lock(); n.snap, n.err = s, err; n.mu.Unlock() }

type patrolEnv struct {
	p      *Patrol
	f      *FakePF
	net    *fakeNet
	utun   atomic.Value // string
	lan    atomic.Bool
	mu     sync.Mutex
	tokens []string
	events []Event
}

func newPatrolEnv(t *testing.T) *patrolEnv {
	t.Helper()
	e := &patrolEnv{f: NewFakePF(), net: &fakeNet{snap: NetSnapshot{Direct: pfx("192.168.1.0/24")}}}
	e.f.UIDs = map[string]string{"_abroad": "250"}
	e.utun.Store("utun5")
	e.lan.Store(true)
	e.p = &Patrol{
		C:        &Controller{R: e.f, Matcher: PrintMatcher{User: "_abroad", UID: "250"}},
		Net:      e.net,
		User:     "_abroad",
		Utun:     func() string { return e.utun.Load().(string) },
		AllowLAN: func() bool { return e.lan.Load() },
		OnToken:  func(tok string) { e.mu.Lock(); e.tokens = append(e.tokens, tok); e.mu.Unlock() },
		OnEvent:  func(ev Event) { e.mu.Lock(); e.events = append(e.events, ev); e.mu.Unlock() },
	}
	return e
}

func (e *patrolEnv) count(kind EventKind) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, ev := range e.events {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

func (e *patrolEnv) pass(t *testing.T, p Packet) bool {
	t.Helper()
	ok, err := e.f.Eval(DefaultAnchor, p)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func (e *patrolEnv) step(t *testing.T) {
	t.Helper()
	if err := e.p.Step(context.Background()); err != nil {
		t.Fatalf("Step：%v", err)
	}
}

func readOnly(calls []string) bool {
	for _, op := range calls {
		if op != "info" && op != "mainrules" && op != "anchorrules" && op != "table" {
			return false
		}
	}
	return true
}

// b. 首轮
func TestPatrolFirstStep(t *testing.T) {
	e := newPatrolEnv(t)
	e.step(t)
	calls := e.f.Calls()
	li := slices.Index(calls, "load")
	if li < 0 || calls[li+1] != "kill" || slices.Index(calls, "enable") < li {
		t.Fatalf("首轮调用序列应为 load、kill、enable：%v", calls)
	}
	if len(e.tokens) != 1 || !e.p.Protected() {
		t.Fatalf("应交出 token 且保护有效：%v %v", e.tokens, e.p.Protected())
	}
	if e.pass(t, publicPkt()) {
		t.Fatal("en0 上普通用户到公网应被拦")
	}
}

// c. 被清、被关；没有扰动只有只读调用
func TestPatrolRecovers(t *testing.T) {
	e := newPatrolEnv(t)
	e.step(t)
	e.f.ResetCalls()
	e.step(t)
	if !readOnly(e.f.Calls()) {
		t.Fatalf("没有扰动时只应有只读调用：%v", e.f.Calls())
	}
	e.f.ClearAnchor()
	if !e.pass(t, publicPkt()) {
		t.Fatal("前提：被清后不拦")
	}
	k := e.f.Kills()
	e.step(t)
	if e.pass(t, publicPkt()) || e.f.Kills() != k+1 {
		t.Fatal("被清后一轮内应恢复（Load + Kill）")
	}
	e.f.Disable()
	e.step(t)
	if len(e.tokens) != 2 || e.pass(t, publicPkt()) {
		t.Fatalf("被关后一轮内应恢复并交出新 token：%v", e.tokens)
	}
}

// d. 连续 3 次
func TestPatrolRepeatedlyCleared(t *testing.T) {
	e := newPatrolEnv(t)
	e.step(t) // 首轮也算一次修复（锚点为空）
	e.step(t) // 干净的一轮：重新计数
	for i := 1; i <= 3; i++ {
		e.f.ClearAnchor()
		e.step(t)
		want := 0
		if i == 3 {
			want = 1
		}
		if got := e.count(EvRepeatedlyCleared); got != want {
			t.Fatalf("第 %d 轮被清后回调次数 %d，应为 %d", i, got, want)
		}
	}
	// 中间隔一轮干净的：不回调
	e2 := newPatrolEnv(t)
	e2.step(t)
	e2.step(t)
	for _, clean := range []bool{false, false, true, false, false} {
		if !clean {
			e2.f.ClearAnchor()
		}
		e2.step(t)
	}
	if e2.count(EvRepeatedlyCleared) != 0 {
		t.Fatal("中间隔一轮干净的不应回调")
	}
	// 第 4、5、6 轮继续被清：再回调一次
	for range 3 {
		e.f.ClearAnchor()
		e.step(t)
	}
	if e.count(EvRepeatedlyCleared) != 2 {
		t.Fatalf("连续 6 轮被清应回调 2 次：%d", e.count(EvRepeatedlyCleared))
	}
}

// e. 换网络
func TestPatrolNetworkChanges(t *testing.T) {
	e := newPatrolEnv(t)
	e.step(t)
	lan1 := pk("en0", "alice", "tcp", 1000, 631, "192.168.1.20")
	lan2 := pk("en0", "alice", "tcp", 1000, 631, "10.0.0.20")
	if !e.pass(t, lan1) || e.pass(t, lan2) {
		t.Fatal("前提：旧网段放行、新网段不放行")
	}
	k := e.f.Kills()
	e.net.set(NetSnapshot{Direct: pfx("10.0.0.0/24")}, nil)
	e.step(t)
	if e.f.Kills() != k || e.pass(t, lan1) || !e.pass(t, lan2) {
		t.Fatal("换网段：一轮内更新、不 Kill；旧网段不放行、新网段放行")
	}
	e.net.set(NetSnapshot{Direct: pfx("160.79.104.0/23")}, nil)
	e.step(t)
	if e.pass(t, pk("en0", "alice", "tcp", 1000, 443, "160.79.104.10")) {
		t.Fatal("公网直连网段始终不放行")
	}
	e.net.set(NetSnapshot{Direct: pfx("192.168.1.0/24")}, nil)
	e.step(t)
	e.lan.Store(false)
	e.step(t)
	if e.pass(t, lan1) {
		t.Fatal("AllowLAN 关闭后到局域网不放行")
	}
	e.utun.Store("utun6")
	e.step(t)
	if e.pass(t, pk("utun5", "alice", "tcp", 1000, 443, "1.1.1.1")) || !e.pass(t, pk("utun6", "alice", "tcp", 1000, 443, "1.1.1.1")) {
		t.Fatal("utun 换名后旧 utun 上到公网不放行，新 utun 放行")
	}
	e.net.set(NetSnapshot{Direct: pfx("192.168.1.0/24"), VMIfs: []string{"any", "en0", "bridge100"}}, nil)
	e.step(t)
	if e.count(EvVMIfDropped) != 2 || !e.pass(t, pk("bridge100", "alice", "tcp", 1000, 22, "192.168.64.2")) {
		t.Fatalf("不合规的网桥应被丢掉并发事件，其余照常：%d", e.count(EvVMIfDropped))
	}
	if !e.p.Protected() {
		t.Fatal("这些都应是成功的轮次")
	}
}

// f. 不放松
func TestPatrolNoLoosening(t *testing.T) {
	e := newPatrolEnv(t)
	e.step(t)
	e.net.set(NetSnapshot{}, errors.New("读不到网卡"))
	e.f.ClearAnchor()
	e.step(t)
	if e.pass(t, publicPkt()) || !e.pass(t, pk("en0", "alice", "tcp", 1000, 631, "192.168.1.20")) || e.count(EvNetError) != 1 {
		t.Fatal("读不到网络时沿用上一次的期望，被清仍能恢复，有 NetError 事件")
	}
	e2 := newPatrolEnv(t)
	e2.net.set(NetSnapshot{}, errors.New("读不到网卡"))
	e2.step(t)
	if e2.pass(t, publicPkt()) || e2.pass(t, pk("en0", "alice", "tcp", 1000, 631, "192.168.1.20")) {
		t.Fatal("从未成功过时用最小配置：到公网、到局域网都不放行")
	}
}

// g. Protected
func TestPatrolProtected(t *testing.T) {
	e := newPatrolEnv(t)
	e.step(t)
	e.f.DropMainRef()
	e.f.ResetCalls()
	if err := e.p.Step(context.Background()); !errors.Is(err, ErrAnchorNotReferenced) || e.p.Protected() || e.count(EvNotReferenced) != 1 {
		t.Fatalf("DropMainRef 后当轮保护失效：%v %v", err, e.p.Protected())
	}
	if !readOnly(e.f.Calls()) {
		t.Fatalf("巡检不应改主规则集（只能有只读调用）：%v", e.f.Calls())
	}

	e2 := newPatrolEnv(t)
	e2.p.C.Matcher = PrintMatcher{User: "_abroad", UID: "999"} // 复核不一致
	if err := e2.p.Step(context.Background()); !errors.Is(err, ErrVerifyFailed) || e2.p.Protected() {
		t.Fatalf("复核不一致当轮保护失效：%v", err)
	}

	e3 := newPatrolEnv(t)
	e3.step(t)
	e3.f.FailNext("info", errors.New("boom"))
	e3.p.Step(context.Background())
	if !e3.p.Protected() {
		t.Fatal("偶发一轮失败仍算有效")
	}
	e3.f.FailNext("info", errors.New("boom"))
	e3.p.Step(context.Background())
	if e3.p.Protected() {
		t.Fatal("连续两轮失败应判失效")
	}
	e3.step(t)
	if !e3.p.Protected() {
		t.Fatal("成功一轮后恢复")
	}
}

// h. OnToken 为 nil
func TestPatrolNeedsTokenSink(t *testing.T) {
	e := newPatrolEnv(t)
	e.p.OnToken = nil
	if err := e.p.Step(context.Background()); !errors.Is(err, ErrNoTokenSink) || slices.Contains(e.f.Calls(), "enable") {
		t.Fatalf("OnToken 为 nil 时应报错且不调用 Enable：%v %v", err, e.f.Calls())
	}
}

// i. Run
func TestPatrolRun(t *testing.T) {
	before := runtime.NumGoroutine()
	e := newPatrolEnv(t)
	e.p.Interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.p.Run(ctx); close(done) }()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				e.p.Step(ctx)
				e.f.ClearAnchor()
			}
		}()
	}
	wg.Wait()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ctx 取消后 1 秒内应退出")
	}
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if runtime.NumGoroutine() > before+1 {
		t.Fatalf("goroutine 泄漏：%d → %d", before, runtime.NumGoroutine())
	}
}

// 7a 第 0 步：复核不一致时不反复重载、清状态。
func TestPatrolVerifyFailedNoReloadLoop(t *testing.T) {
	e := newPatrolEnv(t)
	e.p.C.Matcher = PrintMatcher{User: "_abroad", UID: "999"} // 故意配错 UID
	var loads []int
	for i := range 4 {
		e.f.ResetCalls()
		k := e.f.Kills()
		if err := e.p.Step(context.Background()); !errors.Is(err, ErrVerifyFailed) {
			t.Fatalf("第 %d 轮应 ErrVerifyFailed：%v", i+1, err)
		}
		n := 0
		for _, op := range e.f.Calls() {
			if op == "load" {
				n++
			}
		}
		loads = append(loads, n)
		if i > 0 && e.f.Kills() != k {
			t.Fatalf("第 %d 轮不应再清状态", i+1)
		}
		if e.p.Protected() {
			t.Fatalf("第 %d 轮 Protected 应为 false", i+1)
		}
	}
	if !slices.Equal(loads, []int{1, 0, 0, 0}) || e.count(EvRepeatedlyCleared) != 0 {
		t.Fatalf("只有第 1 轮加载，且不误报：%v，回调 %d 次", loads, e.count(EvRepeatedlyCleared))
	}
	e.f.ClearAnchor()
	e.f.ResetCalls()
	e.p.Step(context.Background())
	if !slices.Contains(e.f.Calls(), "load") {
		t.Fatal("锚点被清后仍应重新加载")
	}
}

// 7a 第 0 步：确认坏状态但修复失败，当轮判失效；读不到状态才要两轮。
func TestPatrolRepairFailureImmediate(t *testing.T) {
	for _, c := range []struct {
		name    string
		disturb func(f *FakePF)
		op      string
	}{
		{"load 失败", func(f *FakePF) { f.ClearAnchor() }, "load"},
		{"enable 失败", func(f *FakePF) { f.Disable() }, "enable"},
	} {
		e := newPatrolEnv(t)
		e.step(t)
		c.disturb(e.f)
		e.f.FailNext(c.op, errors.New("boom"))
		e.p.Step(context.Background())
		if e.p.Protected() {
			t.Errorf("%s：一轮后 Protected 应为 false", c.name)
		}
	}
	e := newPatrolEnv(t)
	e.step(t)
	e.f.FailNext("info", errors.New("boom"))
	if err := e.p.Step(context.Background()); !errors.Is(err, ErrStatusUnavailable) || !e.p.Protected() {
		t.Fatalf("读不到状态一轮仍算有效：%v", err)
	}
}
