package vmmgr

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"claude-abroad/internal/gatesm"
)

// ---- 假时钟：Advance 推进；到点的等待者收到时间 ----

type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []fakeWaiter
}

type fakeWaiter struct {
	at time.Time
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, fakeWaiter{c.now.Add(d), ch})
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	keep := c.waiters[:0]
	for _, w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
		} else {
			keep = append(keep, w)
		}
	}
	c.waiters = keep
	c.mu.Unlock()
}

// ---- 假进程 ----

type fakeProc struct {
	id                     int
	l                      *fakeLauncher
	once                   sync.Once
	done                   chan struct{}
	err                    error
	ignoreTerm, ignoreKill bool
	healthy                bool // 健康检查是否通过
	termed, killed         bool
}

func (p *fakeProc) Done() <-chan struct{} { return p.done }
func (p *fakeProc) ExitErr() error {
	p.l.mu.Lock()
	defer p.l.mu.Unlock()
	return p.err
}

func (p *fakeProc) exit(err error) {
	p.once.Do(func() {
		p.l.mu.Lock()
		p.err = err
		p.l.alive--
		p.l.mu.Unlock()
		close(p.done)
	})
}

func (p *fakeProc) Terminate() error {
	p.l.mu.Lock()
	p.termed = true
	ig := p.ignoreTerm
	p.l.mu.Unlock()
	if !ig {
		p.exit(nil)
	}
	return nil
}

func (p *fakeProc) Kill() error {
	p.l.mu.Lock()
	p.killed = true
	ig := p.ignoreKill
	p.l.mu.Unlock()
	if !ig {
		p.exit(errors.New("signal: killed"))
	}
	return nil
}

// ---- 假拉起器：记录次数、同时存活的最大数量；每次拉起前可以改下一个进程的行为 ----

type fakeLauncher struct {
	mu       sync.Mutex
	t        *testing.T
	launches int
	alive    int
	maxAlive int
	failNext int // 接下来这么多次 Launch 直接返回错误
	procs    []*fakeProc
	// 新进程的默认行为
	healthy                bool
	ignoreTerm, ignoreKill bool
	launchTimes            []time.Time
	clock                  *fakeClock
}

func (l *fakeLauncher) Launch() (Proc, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.launches++
	l.launchTimes = append(l.launchTimes, l.clock.Now())
	if l.failNext > 0 {
		l.failNext--
		return nil, errors.New("exec: 找不到 abroad-vm")
	}
	if l.alive != 0 {
		l.t.Errorf("拉起第 %d 个时还有 %d 个在运行（不能同时有两个虚拟机）", l.launches, l.alive)
	}
	l.alive++
	if l.alive > l.maxAlive {
		l.maxAlive = l.alive
	}
	p := &fakeProc{id: l.launches, l: l, done: make(chan struct{}), healthy: l.healthy, ignoreTerm: l.ignoreTerm, ignoreKill: l.ignoreKill}
	l.procs = append(l.procs, p)
	return p, nil
}

func (l *fakeLauncher) last() *fakeProc {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.procs) == 0 {
		return nil
	}
	return l.procs[len(l.procs)-1]
}

func (l *fakeLauncher) set(f func(l *fakeLauncher)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f(l)
}

func (l *fakeLauncher) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.launches
}

// health：当前（最后一个）进程活着且 healthy。
func (l *fakeLauncher) health(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.procs) == 0 {
		return errors.New("没有虚拟机")
	}
	p := l.procs[len(l.procs)-1]
	select {
	case <-p.done:
		return errors.New("ctl.sock 不存在")
	default:
	}
	if !p.healthy {
		return errors.New("health 不通")
	}
	return nil
}

func (l *fakeLauncher) setHealthy(v bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.healthy = v
	if len(l.procs) > 0 {
		l.procs[len(l.procs)-1].healthy = v
	}
}

// ---- 假闸门：记录原因；开着 = 没有原因（这里不模拟放行期，放行只能经 Kick 之后的复查）----

type fakeGate struct {
	mu      sync.Mutex
	reasons map[gatesm.Reason]string
	kicks   int
	log     []string
}

func newFakeGate() *fakeGate { return &fakeGate{reasons: map[gatesm.Reason]string{}} }

func (g *fakeGate) AddReason(r gatesm.Reason, d string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reasons[r] = d
	g.log = append(g.log, "+"+string(r)+":"+d)
}

func (g *fakeGate) ClearReason(r gatesm.Reason) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.reasons, r)
	g.log = append(g.log, "-"+string(r))
}

func (g *fakeGate) Kick() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.kicks++
}

func (g *fakeGate) vmReason() (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	d, ok := g.reasons[gatesm.ReasonVM]
	return d, ok
}

func (g *fakeGate) kickCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.kicks
}

// ---- 组装 ----

var testTiming = Timing{
	StartTimeout:   10 * time.Second,
	ProbeEvery:     time.Second,
	ProbeTimeout:   time.Second,
	PatrolFailures: 3,
	UnhealthyKill:  10 * time.Second,
	StopWait:       5 * time.Second,
	KillWait:       2 * time.Second,
	StableFor:      30 * time.Second,
	BackoffMin:     time.Second,
	BackoffMax:     8 * time.Second,
	MaxFailures:    4,
	StartWait:      time.Hour,
}

type rig struct {
	t      *testing.T
	clock  *fakeClock
	l      *fakeLauncher
	gate   *fakeGate
	m      *Manager
	cancel context.CancelFunc
	done   chan struct{}
}

func newRig(t *testing.T, mods ...func(*Timing)) *rig {
	t.Helper()
	c := newFakeClock()
	l := &fakeLauncher{t: t, clock: c, healthy: true}
	g := newFakeGate()
	tm := testTiming
	for _, f := range mods {
		f(&tm)
	}
	m := &Manager{Launcher: l, Health: l.health, Gate: g, Kick: g, Clock: c, Timing: tm, Logf: t.Logf}
	ctx, cancel := context.WithCancel(context.Background())
	r := &rig{t: t, clock: c, l: l, gate: g, m: m, cancel: cancel, done: make(chan struct{})}
	go func() { m.Run(ctx); close(r.done) }()
	t.Cleanup(func() {
		cancel()
		r.pump(func() bool {
			select {
			case <-r.done:
				return true
			default:
				return false
			}
		}, "Run 退出")
	})
	return r
}

// pump 一边小步推进假时钟一边等条件成立。
func (r *rig) pump(cond func() bool, what string) {
	r.t.Helper()
	for i := 0; i < 20000; i++ {
		if cond() {
			return
		}
		r.clock.Advance(100 * time.Millisecond)
		time.Sleep(50 * time.Microsecond)
	}
	r.t.Fatalf("等不到：%s（阶段 %s，状态 %+v）", what, r.m.Status().Phase, r.m.Status())
}

// settle 只让出时间、不推进时钟，等条件成立。
func (r *rig) settle(cond func() bool, what string) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Microsecond)
	}
	r.t.Fatalf("等不到：%s（状态 %+v）", what, r.m.Status())
}

func (r *rig) phaseIs(p string) func() bool { return func() bool { return r.m.Status().Phase == p } }

// startAsync 在后台调用 Start，返回结果通道。
func (r *rig) startAsync() <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- r.m.Start(context.Background()) }()
	return ch
}

func (r *rig) wantGateClosed(when string) {
	r.t.Helper()
	d, ok := r.gate.vmReason()
	if !ok || d == "" {
		r.t.Fatalf("%s：闸门应该带着虚拟机原因关着（原因表 %v）", when, r.gate.reasons)
	}
}
