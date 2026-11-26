package pf

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"runtime"
	"slices"
	"strings"
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
		if i > 1 && e.f.Kills() != k {
			t.Fatalf("第 %d 轮不应再清状态", i+1)
		}
		if e.p.Protected() {
			t.Fatalf("第 %d 轮 Protected 应为 false", i+1)
		}
	}
	// 同一组内容最多重载两次：第 1 轮加载、第 2 轮再完整重载一次（打印结果和上次一模一样才确认是格式对不上），之后不再重载
	if !slices.Equal(loads, []int{1, 1, 0, 0}) || e.count(EvRepeatedlyCleared) != 0 {
		t.Fatalf("只有前两轮加载，且不误报：%v，回调 %d 次", loads, e.count(EvRepeatedlyCleared))
	}
	e.f.ClearAnchor()
	e.f.ResetCalls()
	e.p.Step(context.Background())
	if !slices.Contains(e.f.Calls(), "load") {
		t.Fatal("锚点被清后仍应重新加载")
	}
}

// M6-4b-3 补审时定：复核时锚点里是别人刚写的内容（不是打印格式对不上），下一轮巡检再完整重载一次就恢复保护。
func TestPatrolVerifyForeignWriteRecovers(t *testing.T) {
	e := newPatrolEnv(t)
	foreign := []byte("pass out quick all\n") // 别人（例如开机时的 guard）写的更宽的规则
	e.f.AfterNextLoad(func() { _ = (&Controller{R: e.f}).Load(context.Background(), foreign) })
	if err := e.p.Step(context.Background()); !errors.Is(err, ErrVerifyFailed) {
		t.Fatalf("第 1 轮复核应看到别人的内容：%v", err)
	}
	if e.p.Protected() {
		t.Fatal("第 1 轮应判保护失效")
	}
	e.f.ResetCalls()
	if err := e.p.Step(context.Background()); err != nil {
		t.Fatalf("第 2 轮应重载成功：%v", err)
	}
	if !slices.Contains(e.f.Calls(), "load") || !e.p.Protected() {
		t.Fatalf("第 2 轮应重载并恢复保护：%v", e.f.Calls())
	}
	if bytes.Equal(e.f.Raw(DefaultAnchor), foreign) {
		t.Fatal("锚点里不应再是别人的内容")
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

// M6-11a：回国拦截模式。从接管规则一次加载换过来（中间没有 Flush、不清状态），只拦那几段；被清掉、pf 被关时巡检
// 重新加载和启用；Protected 一直为假，不调用 OnApplied；Apply 回到接管规则，Flush 退出这一模式。
func TestPatrolRestoredMode(t *testing.T) {
	e := newPatrolEnv(t)
	applied := 0
	e.p.OnApplied = func(Config) { applied++ }
	ctx := context.Background()
	if err := e.p.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	kills, appliedBefore := e.f.Kills(), applied
	e.f.ResetCalls()
	if err := e.p.ApplyRestored(ctx); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(e.f.Calls(), "flush") || e.f.Kills() != kills {
		t.Fatalf("换组不应先清空锚点、不应清状态：%v", e.f.Calls())
	}
	want, _ := Render(ReturnBlockConfig())
	if !bytes.Equal(e.f.Raw(DefaultAnchor), want) {
		t.Fatalf("锚点应恰好是回国拦截那一组：\n%s", e.f.Raw(DefaultAnchor))
	}
	if !e.p.RestoredMode() || !e.p.ReturnBlockActive() || e.p.Protected() || applied != appliedBefore {
		t.Fatalf("模式 %v 生效 %v 保护 %v OnApplied %d→%d", e.p.RestoredMode(), e.p.ReturnBlockActive(), e.p.Protected(), appliedBefore, applied)
	}
	blocked := func(dst string) bool {
		ok, err := e.f.Eval(DefaultAnchor, Packet{If: "en0", User: "alice", Proto: "tcp", DstPort: 443, Dst: netip.MustParseAddr(dst)})
		if err != nil {
			t.Fatal(err)
		}
		return !ok
	}
	if !blocked("160.79.104.10") || !blocked("2607:6bc0::1") || blocked("1.1.1.1") || blocked("2001:db8::1") {
		t.Fatal("应只拦 Anthropic 的地址段")
	}
	// 被清掉、pf 被关：巡检重新加载并启用（新 token 交出去），仍不清状态
	e.f.ClearAnchor()
	e.f.Disable()
	ntok := len(e.tokens)
	if err := e.p.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(e.f.Raw(DefaultAnchor), want) || !e.f.Active(DefaultAnchor) || len(e.tokens) != ntok+1 || e.f.Kills() != kills {
		t.Fatalf("没有恢复：%s tokens=%v kills=%d", e.f.Raw(DefaultAnchor), e.tokens, e.f.Kills())
	}
	// Apply 回到接管规则（同样一次加载）
	e.f.ResetCalls()
	if err := e.p.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(e.f.Calls(), "flush") || e.p.RestoredMode() || !e.p.Protected() || !blocked("160.79.104.10") || !blocked("1.1.1.1") {
		t.Fatalf("回到接管：%v", e.f.Calls())
	}
	// Flush 退出这一模式
	_ = e.p.ApplyRestored(ctx)
	if err := e.p.Flush(ctx); err != nil || e.p.RestoredMode() || e.p.ReturnBlockActive() {
		t.Fatal("Flush 应退出回国拦截模式")
	}
	if err := e.p.Step(ctx); err != nil || e.f.Raw(DefaultAnchor) != nil {
		t.Fatal("暂停期间不应加载")
	}
}

// 打印形式（-n -v -o none 实测，2026-11-26）：pf 给两行都补上 from any，Matcher 判一致。
func TestMatcherReturnBlock(t *testing.T) {
	want, _ := Render(ReturnBlockConfig())
	shown := []byte("block return out quick inet from any to 160.79.104.0/21\nblock return out quick inet6 from any to 2607:6bc0::/32\n")
	m := PrintMatcher{User: "_abroad"}
	if !m.Same(want, Status{AnchorRules: shown}) {
		t.Fatal("应判一致")
	}
	if m.Same(want, Status{AnchorRules: []byte("block return out quick inet from any to 160.79.104.0/21\n")}) {
		t.Fatal("少一行应判不同")
	}
}

// neverSame 是打印和期望永远对不上的比对器（模拟真机打印格式和比对器不一致）。
type neverSame struct{}

func (neverSame) Present(st Status) bool       { return len(ruleLines(st.AnchorRules)) > 0 }
func (neverSame) Same(_ []byte, _ Status) bool { return false }

// architect 2026-11-27：回国拦截模式同样防空转——打印和期望对不上时最多重载两次，之后不再重载；被清掉仍重新加载；
// 别人写了别的内容时下一轮重载恢复。
func TestPatrolRestoredVerifyNoReloadLoop(t *testing.T) {
	e := newPatrolEnv(t)
	ctx := context.Background()
	e.p.C.Matcher = neverSame{}
	var loads []int
	for i := range 4 {
		e.f.ResetCalls()
		var err error
		if i == 0 {
			err = e.p.ApplyRestored(ctx)
		} else {
			err = e.p.Step(ctx)
		}
		if !errors.Is(err, ErrVerifyFailed) {
			t.Fatalf("第 %d 轮应 ErrVerifyFailed：%v", i+1, err)
		}
		n := 0
		for _, op := range e.f.Calls() {
			if op == "load" {
				n++
			}
		}
		loads = append(loads, n)
		if e.p.ReturnBlockActive() {
			t.Fatalf("第 %d 轮不应报告生效", i+1)
		}
	}
	if !slices.Equal(loads, []int{1, 1, 0, 0}) || e.f.Kills() != 0 {
		t.Fatalf("只有前两轮加载、从不清状态：%v kills=%d", loads, e.f.Kills())
	}
	e.f.ClearAnchor()
	e.f.ResetCalls()
	_ = e.p.Step(ctx)
	if !slices.Contains(e.f.Calls(), "load") {
		t.Fatal("锚点被清后仍应重新加载")
	}

	// 别人写了别的内容：下一轮重载恢复
	e2 := newPatrolEnv(t)
	foreign := []byte("pass out quick all\n")
	e2.f.AfterNextLoad(func() { _ = (&Controller{R: e2.f}).Load(ctx, foreign) })
	if err := e2.p.ApplyRestored(ctx); !errors.Is(err, ErrVerifyFailed) {
		t.Fatalf("第 1 轮复核应看到别人的内容：%v", err)
	}
	if err := e2.p.Step(ctx); err != nil || !e2.p.ReturnBlockActive() {
		t.Fatalf("第 2 轮应重载恢复：%v", err)
	}
	want, _ := Render(ReturnBlockConfig())
	if !bytes.Equal(e2.f.Raw(DefaultAnchor), want) {
		t.Fatal("锚点应是回国拦截那一组")
	}
}

// M6-11b 验收 f：定向清状态默认关；打开时只在回国拦截被重新加载（原来不在或被别人改过）后，对每一段各清一次，参数是
// “同族任意地址到这一段”，不清全机的状态。
func TestPatrolReturnBlockKillStates(t *testing.T) {
	if got := KillStatesToArgs(netip.MustParsePrefix("160.79.104.0/21")); !slices.Equal(got, []string{"-k", "0.0.0.0/0", "-k", "160.79.104.0/21"}) {
		t.Fatalf("IPv4 参数：%v", got)
	}
	if got := KillStatesToArgs(netip.MustParsePrefix("2607:6bc0::/32")); !slices.Equal(got, []string{"-k", "::/0", "-k", "2607:6bc0::/32"}) {
		t.Fatalf("IPv6 参数：%v", got)
	}
	ctx := context.Background()
	count := func(calls []string) int {
		n := 0
		for _, c := range calls {
			if strings.HasPrefix(c, "killto ") {
				n++
			}
		}
		return n
	}
	// 默认关
	e := newPatrolEnv(t)
	_ = e.p.ApplyRestored(ctx)
	e.f.ClearAnchor()
	_ = e.p.Step(ctx)
	if n := count(e.f.Calls()); n != 0 {
		t.Fatalf("默认不应清：%v", e.f.Calls())
	}
	// 打开
	e = newPatrolEnv(t)
	e.p.KillReturnBlockStates = true
	_ = e.p.ApplyRestored(ctx) // 第一次加载（锚点原来是空的）
	if c := e.f.Calls(); count(c) != 2 || !slices.Contains(c, "killto 160.79.104.0/21") || !slices.Contains(c, "killto 2607:6bc0::/32") || e.f.Kills() != 0 {
		t.Fatalf("应只对两段各清一次、不清全机：%v kills=%d", c, e.f.Kills())
	}
	e.f.ResetCalls()
	_ = e.p.Step(ctx) // 没变：不加载、不清
	if count(e.f.Calls()) != 0 {
		t.Fatalf("没有重新加载时不应清：%v", e.f.Calls())
	}
	e.f.ClearAnchor()
	_ = e.p.Step(ctx)
	if count(e.f.Calls()) != 2 {
		t.Fatalf("被清掉后重新加载应再清：%v", e.f.Calls())
	}
}
