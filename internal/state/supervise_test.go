package state

import (
	"claude-abroad/internal/gatesm"
	"context"
	"errors"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"claude-abroad/internal/pf"
	"claude-abroad/internal/sysnet"
)

type fclock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fclock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fclock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// newSup 给当前“进程”建监护（时钟注入，从 0 秒开始）。
func (b *bed) newSup() (*Supervisor, *fclock) {
	c := &fclock{t: time.Unix(0, 0)}
	return &Supervisor{M: b.m, Patrol: b.ppf.P, Now: c.Now, OnEvent: b.onEvent}, c
}

// stepPatrol 包一层巡检：数轮次，可在 Step 里阻塞（测试互斥和 Kick；监护不再调用出口检查，不能拿它当信号）。
type stepPatrol struct {
	PatrolStepper
	mu      sync.Mutex
	n       int
	entered chan struct{}
	block   chan struct{}
}

func (p *stepPatrol) Step(ctx context.Context) error {
	p.mu.Lock()
	p.n++
	en, bl := p.entered, p.block
	p.mu.Unlock()
	if en != nil {
		close(en)
		<-bl
	}
	return p.PatrolStepper.Step(ctx)
}

func (p *stepPatrol) count() int { p.mu.Lock(); defer p.mu.Unlock(); return p.n }

func (b *bed) changes() []string {
	var out []string
	for _, s := range b.tl() {
		if isChange(s) {
			out = append(out, s)
		}
	}
	return out
}

func tick(t *testing.T, s *Supervisor) {
	t.Helper()
	if err := s.Tick(bg); err != nil {
		t.Fatal(err)
	}
}

// b. 无事发生。
func TestSuperviseQuiet(t *testing.T) {
	b := newBed(t)
	s, _ := b.newSup()
	tick(t, s)
	if tl := b.tl(); len(tl) != 0 {
		t.Fatalf("RESTORED 时 Tick 有调用：%v", tl)
	}
	mustOK(t, b.m.Enable(bg))
	checks := b.exit.nCalls()
	b.resetTimeline()
	for range 5 {
		tick(t, s)
	}
	if c := b.changes(); len(c) != 0 {
		t.Fatalf("一切正常时有改动：%v", c)
	}
	if !b.gate.IsOpen() || b.td.n != 0 || b.exit.nCalls() != checks {
		t.Fatal("闸门、Teardown 或出口检查不对")
	}
	b.noViolations()
}

func (b *bed) setNoCheck(v bool) { b.mu.Lock(); b.noCheck = v; b.mu.Unlock() }

// c. pf 保护失效：关闸门、Teardown 恰 1 次；恢复引用后重新开闸门。
func TestSupervisePFUnprotected(t *testing.T) {
	b := newBed(t)
	s, _ := b.newSup()
	mustOK(t, b.m.Enable(bg))
	b.setNoCheck(true) // 主规则集被换掉期间锚点不被求值，I2 和泄漏断言本来就无法成立（外部破坏）
	b.fpf.DropMainRef()
	tick(t, s)
	if b.gate.IsOpen() || b.td.n != 1 || b.countEvents(EvGateClosed) != 1 || b.state().State != Enabled {
		t.Fatalf("gate=%v teardown=%d", b.gate.IsOpen(), b.td.n)
	}
	for range 3 {
		tick(t, s)
	}
	if b.td.n != 1 || b.gate.IsOpen() {
		t.Fatal("Teardown 应只 1 次，闸门保持关闭")
	}
	b.fpf.RestoreMainRef()
	b.setNoCheck(false)
	checks, kicks := b.exit.nCalls(), b.re.n()
	tick(t, s)
	// 监护清掉 takeover、立即补一轮复查（不等 60 秒）；复查通过后闸门打开。监护自己不做出口检查。
	if !b.gate.IsOpen() || b.exit.nCalls() != checks || b.re.n() != kicks+1 || b.countEvents(EvTakeoverCleared) != 1 {
		t.Fatal("恢复引用后应清 takeover、立即补一轮复查并重新打开")
	}
	b.noViolations()
}

// d. 锚点被清：巡检当轮修好，闸门不关。
func TestSuperviseAnchorCleared(t *testing.T) {
	b := newBed(t)
	s, _ := b.newSup()
	mustOK(t, b.m.Enable(bg))
	b.fpf.ClearAnchor()
	tick(t, s)
	if !b.fpf.Active(pf.DefaultAnchor) || !b.gate.IsOpen() || b.td.n != 0 {
		t.Fatal("锚点应恢复且闸门不关")
	}
	b.noViolations()
}

// e. utun 死亡：一轮内关闸门、重建、重新开闸门。
func TestSuperviseUtunDies(t *testing.T) {
	b := newBed(t)
	s, _ := b.newSup()
	mustOK(t, b.m.Enable(bg))
	old := b.tun.die()
	tick(t, s)
	nu := b.tun.Name()
	if nu == "" || nu == old || b.state().Utun != nu {
		t.Fatalf("没重建：old=%s new=%s file=%s", old, nu, b.state().Utun)
	}
	if b.td.n != 1 || b.countEvents(EvGateClosed) != 1 || b.countEvents(EvUtunRebuilt) != 1 {
		t.Fatal("应关闸门一次并重建")
	}
	for _, a := range ProbeAddrs {
		if via, _ := b.net.Routes().Via(bg, a); via != nu {
			t.Fatalf("%s 经 %s", a, via)
		}
	}
	if !b.net.Snapshot().DNS.IsValid() || !b.gate.IsOpen() {
		t.Fatal("DNS 或闸门")
	}
	ok, err := b.fpf.Eval(pf.DefaultAnchor, pf.Packet{If: old, User: "alice", Proto: "tcp", DstPort: 443, Dst: netip.MustParseAddr("160.79.104.10")})
	if err != nil || ok {
		t.Fatal("旧 utun 上到公网应被拦")
	}
	mustOK(t, b.m.Restore(bg))
	b.assertClean(b.init)
	b.noViolations()
}

// f. 路由被抢。
func TestSuperviseRouteHijack(t *testing.T) {
	t.Run("更具体的 /16", func(t *testing.T) {
		b := newBed(t)
		s, _ := b.newSup()
		mustOK(t, b.m.Enable(bg))
		hj := netip.MustParsePrefix("198.18.0.0/16")
		b.net.SetForeignRoute(hj, "utun9")
		tick(t, s)
		if b.gate.IsOpen() || b.td.n != 1 || b.countEvents(EvStillFailing) != 1 {
			t.Fatal("应关闸门并报修复后仍不通过")
		}
		tick(t, s)
		if b.gate.IsOpen() || b.td.n != 1 {
			t.Fatal("应保持关闭")
		}
		b.net.DeleteRoute(hj)
		tick(t, s)
		if !b.gate.IsOpen() {
			t.Fatal("撤掉后应重新开闸门")
		}
		mustOK(t, b.m.Restore(bg))
		b.assertClean(b.init)
		b.noViolations()
	})
	t.Run("0.0.0.0/1 被改到 utun9", func(t *testing.T) {
		b := newBed(t)
		s, _ := b.newSup()
		mustOK(t, b.m.Enable(bg))
		b.net.SetForeignRoute(netip.MustParsePrefix("1.0.0.0/8"), "utun9")
		tick(t, s)
		if b.net.Snapshot().Routes[netip.MustParsePrefix("1.0.0.0/8")] != b.tun.Name() || !b.gate.IsOpen() || b.countEvents(EvRouteRepaired) != 1 {
			t.Fatal("应改回并重新开闸门")
		}
		mustOK(t, b.m.Restore(bg))
		b.assertClean(b.init)
		b.noViolations()
	})
	t.Run("两条路由被删", func(t *testing.T) {
		b := newBed(t)
		s, _ := b.newSup()
		mustOK(t, b.m.Enable(bg))
		for _, r := range TakeoverRoutes {
			b.net.DeleteRoute(r)
		}
		tick(t, s)
		for _, r := range TakeoverRoutes {
			if b.net.Snapshot().Routes[r] != b.tun.Name() {
				t.Fatalf("%s 没补回", r)
			}
		}
		if !b.gate.IsOpen() {
			t.Fatal("应重新开闸门")
		}
		b.noViolations()
	})
}

// g. DNS 被删：一轮内补回，闸门关过一次。
func TestSuperviseDNSRemoved(t *testing.T) {
	b := newBed(t)
	s, _ := b.newSup()
	mustOK(t, b.m.Enable(bg))
	b.net.RemoveDNS()
	tick(t, s)
	if b.net.Snapshot().DNS != TakeoverDNS || b.td.n != 1 || !b.gate.IsOpen() || b.countEvents(EvDNSRepaired) != 1 {
		t.Fatal("DNS 没补回或闸门不对")
	}
	b.noViolations()
}

// f. 监护只管 takeover（m4-plan v2.2 决定 5）：出口检查不通过期间，监护清掉自己的原因不会打开闸门；
// 监护不启动虚拟机、不做出口检查（调用记录）；故障消除后立即补一轮复查。
func TestSuperviseOnlyTakeover(t *testing.T) {
	b := newBed(t)
	s, _ := b.newSup()
	mustOK(t, b.m.Enable(bg))
	b.gate.AddReason(gatesm.ReasonExitCheck, "出口检查不通过")
	b.re.setOK(false)
	vm0, ex0 := b.vm.starts(), b.exit.nCalls()
	b.setNoCheck(true)
	b.fpf.DropMainRef()
	tick(t, s)
	if !b.gate.has(gatesm.ReasonTakeover) || !b.gate.has(gatesm.ReasonExitCheck) {
		t.Fatal("接管故障时应加 takeover，exit_check 保持")
	}
	b.fpf.RestoreMainRef()
	b.setNoCheck(false)
	kicks := b.re.n()
	tick(t, s)
	if b.gate.has(gatesm.ReasonTakeover) || !b.gate.has(gatesm.ReasonExitCheck) || b.gate.IsOpen() {
		t.Fatal("修复后只清 takeover；exit_check 还在，闸门不应打开")
	}
	if b.re.n() != kicks+1 {
		t.Fatal("故障消除后应立即补一轮复查")
	}
	for range 10 {
		tick(t, s)
	}
	if b.vm.starts() != vm0 || b.exit.nCalls() != ex0 {
		t.Fatalf("监护不应启动虚拟机或做出口检查：vm %d→%d check %d→%d", vm0, b.vm.starts(), ex0, b.exit.nCalls())
	}
	// 复查通过后（清 exit_check 并放行）才打开。
	b.gate.ClearReason(gatesm.ReasonExitCheck)
	b.re.setOK(true)
	b.re.Kick()
	if !b.gate.IsOpen() {
		t.Fatal("复查通过后应打开")
	}
	b.noViolations()
}

// h. Recover 到 ENABLED 后复查没通过：监护不接手开闸门，由复查循环负责。
func TestSuperviseAfterRecover(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	b.crash()
	b.re.setOK(false)
	mustOK(t, b.m.Recover(bg))
	s, _ := b.newSup()
	tick(t, s)
	if b.gate.IsOpen() {
		t.Fatal("监护不应开闸门")
	}
	b.re.setOK(true)
	b.re.Kick()
	if !b.gate.IsOpen() {
		t.Fatal("复查通过后应打开")
	}
	b.noViolations()
}

// i. DISABLED：修复但不开闸门，没有出口检查和虚拟机启动。
func TestSuperviseDisabled(t *testing.T) {
	b := newBed(t)
	s, _ := b.newSup()
	mustOK(t, b.m.Enable(bg))
	mustOK(t, b.m.Disable(bg, true))
	opens, checks := b.gate.Opens(), b.exit.nCalls()
	b.resetTimeline()
	old := b.tun.die()
	tick(t, s)
	if b.tun.Name() == "" || b.tun.Name() == old || b.state().Utun != b.tun.Name() {
		t.Fatal("utun 没重建")
	}
	b.net.RemoveDNS()
	tick(t, s)
	for _, r := range TakeoverRoutes {
		b.net.DeleteRoute(r)
	}
	tick(t, s)
	b.assertDisabled()
	if b.gate.Opens() != opens || b.exit.nCalls() != checks || index(b.tl(), pre("vm start")) >= 0 {
		t.Fatal("DISABLED 下不应开闸门、做出口检查或启动虚拟机")
	}
	b.noViolations()
}

// j. ENABLING、RESTORING（无操作在跑）：只有 pf 巡检；步骤 6 的 RESTORING 没有任何 pfctl 调用。
func TestSuperviseUnfinished(t *testing.T) {
	onlyPF := func(t *testing.T, b *bed) {
		t.Helper()
		for _, s := range b.tl() {
			if !strings.HasPrefix(s, "pf ") {
				t.Fatalf("应只有 pf 巡检的调用：%v", b.tl())
			}
		}
	}
	t.Run("ENABLING", func(t *testing.T) {
		b := newBed(t)
		b.exit.fail = errInjected
		b.vm.failStop = errInjected
		_ = b.m.Enable(bg)
		if b.state().State != Enabling {
			t.Fatal("准备")
		}
		s, _ := b.newSup()
		b.fpf.ClearAnchor()
		b.resetTimeline()
		tick(t, s)
		onlyPF(t, b)
		if !b.fpf.Active(pf.DefaultAnchor) {
			t.Fatal("巡检应把 pf 加载回来")
		}
		b.noViolations()
	})
	t.Run("RESTORING", func(t *testing.T) {
		b := newBed(t)
		mustOK(t, b.m.Enable(bg))
		b.net.FailNext("ipv6.restore", errInjected)
		_ = b.m.Restore(bg)
		if b.state().State != Restoring {
			t.Fatal("准备")
		}
		s, _ := b.newSup()
		b.resetTimeline()
		tick(t, s)
		onlyPF(t, b)
		b.noViolations()
	})
	t.Run("RESTORING 步骤 6", func(t *testing.T) {
		b := newBed(t)
		mustOK(t, b.m.Enable(bg))
		b.failNthWrite(7) // M4-5 起恢复的 ⑤ 多写一次步骤号
		_ = b.m.Restore(bg)
		if f := b.state(); f.State != Restoring || f.Step != StepPFClear {
			t.Fatalf("准备：%+v", f)
		}
		s, _ := b.newSup()
		b.resetTimeline()
		tick(t, s)
		if tl := b.tl(); len(tl) != 0 {
			t.Fatalf("巡检暂停时不应有调用：%v", tl)
		}
		if b.fpf.Active(pf.DefaultAnchor) {
			t.Fatal("锚点应保持为空")
		}
		b.noViolations()
	})
}

// k. 互斥。
func TestSuperviseMutualExclusion(t *testing.T) {
	t.Run("Enable 进行中 Tick 跳过", func(t *testing.T) {
		b := newBed(t)
		s, _ := b.newSup()
		b.pre.entered, b.pre.block = make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		go func() { done <- b.m.Enable(bg) }()
		<-b.pre.entered
		b.resetTimeline()
		tick(t, s)
		if tl := b.tl(); len(tl) != 0 {
			t.Fatalf("Tick 应跳过：%v", tl)
		}
		close(b.pre.block)
		mustOK(t, <-done)
		b.noViolations()
	})
	t.Run("Tick 进行中 Restore 得到 ErrBusy", func(t *testing.T) {
		b := newBed(t)
		s, _ := b.newSup()
		mustOK(t, b.m.Enable(bg))
		sp := &stepPatrol{PatrolStepper: s.Patrol, entered: make(chan struct{}), block: make(chan struct{})}
		s.Patrol = sp
		done := make(chan error, 1)
		go func() { done <- s.Tick(bg) }()
		<-sp.entered
		if err := b.m.Restore(bg); !errors.Is(err, ErrBusy) {
			t.Fatalf("%v", err)
		}
		sp.mu.Lock()
		close(sp.block)
		sp.entered, sp.block = nil, nil
		sp.mu.Unlock()
		mustOK(t, <-done)
		b.noViolations()
	})
	t.Run("Run 与操作并发", func(t *testing.T) {
		b := newBed(t)
		s, _ := b.newSup()
		s.Interval = time.Millisecond
		ctx, cancel := context.WithCancel(bg)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); s.Run(ctx) }()
		retry := func(f func() error) {
			for {
				err := f()
				if !errors.Is(err, ErrBusy) {
					mustOK(t, err)
					return
				}
				time.Sleep(time.Millisecond)
			}
		}
		for range 3 {
			retry(func() error { return b.m.Enable(bg) })
			retry(func() error { return b.m.Disable(bg, false) })
			retry(func() error { return b.m.Enable(bg) })
			retry(func() error { return b.m.Restore(bg) })
		}
		cancel()
		wg.Wait()
		mustOK(t, b.m.Restore(bg))
		b.assertClean(b.init)
		b.noViolations()
	})
}

// l. Run：ctx 取消后 1 秒内退出、不泄漏 goroutine；Kick 让下一轮立刻开始。
func TestSuperviseRun(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	s, c := b.newSup()
	s.Interval = 10 * time.Millisecond
	before := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run 没在 1 秒内退出")
	}
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("goroutine 泄漏：%d → %d", before, n)
	}

	// Kick：间隔很长，第一轮之后只有 Kick 能触发下一轮。
	sp := &stepPatrol{PatrolStepper: b.ppf.P}
	s2 := &Supervisor{M: b.m, Patrol: sp, Now: c.Now, Interval: time.Hour, OnEvent: b.onEvent}
	ctx2, cancel2 := context.WithCancel(bg)
	defer cancel2()
	done2 := make(chan struct{})
	go func() { s2.Run(ctx2); close(done2) }()
	waitFor := func(n int) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for sp.count() < n {
			if time.Now().After(deadline) {
				t.Fatalf("等不到第 %d 轮", n)
			}
			time.Sleep(time.Millisecond)
		}
	}
	base := sp.count()
	waitFor(base + 1) // 第一轮
	c.Add(10 * time.Second)
	s2.Kick()
	waitFor(base + 2) // Kick 触发的一轮
	cancel2()
	<-done2
	b.noViolations()
}

// 7d 第 0 步：查询出错连续 2 轮才算不通过；中间成功一轮清零。
func TestSuperviseQueryErrors(t *testing.T) {
	for _, op := range []string{sysnet.OpRoutesVia, sysnet.OpDNSPresent} {
		t.Run(op, func(t *testing.T) {
			b := newBed(t)
			s, _ := b.newSup()
			mustOK(t, b.m.Enable(bg))
			b.net.FailNext(op, errInjected)
			tick(t, s)
			if !b.gate.IsOpen() || b.td.n != 0 {
				t.Fatal("查询出错一次不应关闸门")
			}
			tick(t, s) // 成功一轮，清零
			b.net.FailNext(op, errInjected)
			tick(t, s)
			if !b.gate.IsOpen() || b.td.n != 0 {
				t.Fatal("清零后再出错一次不应关闸门")
			}
			// 连续两轮出错：关闸门。
			b.net.FailNext(op, errInjected)
			tick(t, s)
			// 关过一次闸门；修复后复核时查询已恢复，同一轮重新开闸门。
			if b.td.n != 1 || b.countEvents(EvGateClosed) != 1 {
				t.Fatalf("连续两轮出错应关闸门：teardown=%d", b.td.n)
			}
			if !b.gate.IsOpen() {
				t.Fatal("查询恢复后应重新开闸门")
			}
			b.noViolations()
		})
	}
}

// 7d 第 0 步：utun 死了但名字还在，一轮内重建出新名字。
func TestSuperviseUtunDeadKeepsName(t *testing.T) {
	b := newBed(t)
	s, _ := b.newSup()
	mustOK(t, b.m.Enable(bg))
	old := b.tun.dieKeepName()
	tick(t, s)
	if nu := b.tun.Name(); nu == old || !b.tun.Alive() || b.state().Utun != nu || !b.gate.IsOpen() {
		t.Fatalf("没重建：old=%s new=%s", old, nu)
	}
	mustOK(t, b.m.Restore(bg))
	b.assertClean(b.init)
	b.noViolations()
}

// M2-9a c：别的软件加一条 8/6 经 utun9（比我们的 8/5 更具体，地址探测抓不到；v0.72e 起 8/5 是我们自己的）：一轮内关闸门、修复后仍不通过、不删改它；撤掉后重开。
// 加 10/8 经 utun9（内网分段）：闸门不关。
func TestSuperviseRouteTableConflict(t *testing.T) {
	b := newBed(t)
	s, _ := b.newSup()
	mustOK(t, b.m.Enable(bg))
	p85 := netip.MustParsePrefix("8.0.0.0/6")
	b.net.SetForeignRoute(p85, "utun9")
	for _, a := range ProbeAddrs {
		if via, _ := b.net.Routes().Via(bg, a); via != b.tun.Name() {
			t.Fatalf("前提：地址探测应抓不到（%s 经 %s）", a, via)
		}
	}
	tick(t, s)
	if b.gate.IsOpen() || b.td.n != 1 || b.countEvents(EvStillFailing) != 1 {
		t.Fatalf("应关闸门并报修复后仍不通过：gate=%v teardown=%d", b.gate.IsOpen(), b.td.n)
	}
	if b.net.Snapshot().Routes[p85] != "utun9" {
		t.Fatal("不应删改别人的路由")
	}
	b.net.DeleteRoute(p85)
	tick(t, s)
	if !b.gate.IsOpen() {
		t.Fatal("撤掉后应重新开闸门")
	}
	p10 := netip.MustParsePrefix("10.0.0.0/8")
	b.net.SetForeignRoute(p10, "utun9")
	tick(t, s)
	if !b.gate.IsOpen() || b.td.n != 1 {
		t.Fatal("内网分段经别的 utun 不应关闸门")
	}
	b.net.DeleteRoute(p10)
	mustOK(t, b.m.Restore(bg))
	b.assertClean(b.init)
	b.noViolations()
}
