package daemon

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"claude-abroad/internal/gatesm"
	"claude-abroad/internal/state"
	"claude-abroad/internal/vmmgr"
)

// ---- M6-2a 由 abroadd 管理虚拟机：daemon 里的端到端（假的 abroad-vm 进程、真实时钟、很短的时长）----

type dmProc struct {
	once sync.Once
	done chan struct{}
}

func (p *dmProc) Done() <-chan struct{} { return p.done }
func (p *dmProc) ExitErr() error        { return errors.New("signal: killed") }
func (p *dmProc) Terminate() error      { p.exit(); return nil }
func (p *dmProc) Kill() error           { p.exit(); return nil }
func (p *dmProc) exit()                 { p.once.Do(func() { close(p.done) }) }

type dmLauncher struct {
	mu       sync.Mutex
	procs    []*dmProc
	healthy  atomic.Bool
	launches atomic.Int32
}

func (l *dmLauncher) Launch() (vmmgr.Proc, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.launches.Add(1)
	p := &dmProc{done: make(chan struct{})}
	l.procs = append(l.procs, p)
	return p, nil
}

func (l *dmLauncher) last() *dmProc {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.procs[len(l.procs)-1]
}

func (l *dmLauncher) health(context.Context) error {
	if !l.healthy.Load() {
		return errors.New("health 不通")
	}
	select {
	case <-l.last().done:
		return errors.New("ctl.sock 不存在")
	default:
		return nil
	}
}

type countKiller struct {
	inner state.Killer
	n     atomic.Int32
}

func (k *countKiller) KillClaude(ctx context.Context) error {
	k.n.Add(1)
	return k.inner.KillClaude(ctx)
}

var dmTiming = vmmgr.Timing{
	StartTimeout: 2 * time.Second, ProbeEvery: 10 * time.Millisecond, ProbeTimeout: 200 * time.Millisecond,
	PatrolFailures: 2, UnhealthyKill: time.Second, StopWait: 500 * time.Millisecond, KillWait: 200 * time.Millisecond,
	StableFor: time.Minute, BackoffMin: 20 * time.Millisecond, BackoffMax: 100 * time.Millisecond, MaxFailures: 3,
	StartWait: 5 * time.Second,
}

func waitCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等不到：%s", what)
}

func hasReason(s Status, r gatesm.Reason) bool {
	for _, x := range s.Gate.Reasons {
		if x == string(r) {
			return true
		}
	}
	return false
}

// a、c、e：启用时由管理器拉起虚拟机；运行中虚拟机崩溃 → 闸门带 vm 原因关、走代理的已有连接断开、新连接被拒、
// 期间没有任何连接走本机网络；重新拉起并健康后闸门不直接开（复查不通过时仍关），复查通过才开；全程没有结束 Claude。
func TestVMManagerInDaemon(t *testing.T) {
	l := &dmLauncher{}
	l.healthy.Store(true)
	mgr := &vmmgr.Manager{Launcher: l, Health: l.health, Timing: dmTiming}
	fake := &m3Direct{pinnedSet: map[string]bool{}}
	var killer *countKiller
	e := newM3DaemonCore(t, m3Rules, fake, func(_ *m3Env, o *Options) {
		o.Sys.VM = mgr
		killer = &countKiller{inner: o.Sys.Killer}
		o.Sys.Killer = killer
	})
	e.direct = fake
	if s := waitGateOpen(t, e.d); !s.GateOpen {
		t.Fatalf("启用后应打开：%+v", s.Gate)
	}
	st := status(t, e.d)
	if st.VMManager == nil || !st.VMManager.Managed || st.VMManager.Phase != vmmgr.PhaseRunning || l.launches.Load() != 1 {
		t.Fatalf("管理器状态 %+v，拉起 %d 次", st.VMManager, l.launches.Load())
	}
	_, raw := call(t, e.d, "GET", "/v1/engines", nil)
	if !strings.Contains(string(raw), `"vm_manager":{"managed":true,"phase":"running"`) {
		t.Fatalf("engines 应带管理器状态：%s", raw)
	}
	plain := netip.AddrPortFrom(e.lookup(t, "plain.example.org"), 443)
	held, err := e.dial(20004, plain)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := echoConn(held); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	dialsBefore := len(fake.dials)
	fake.mu.Unlock()

	// 复查先一律不通过：证明虚拟机恢复后闸门不会直接开
	var pass atomic.Bool
	e.d.re.Check = func(context.Context) (gatesm.Result, string) {
		if pass.Load() {
			return gatesm.Pass, ""
		}
		return gatesm.Fail, "测试注入"
	}
	// 虚拟机崩溃，且暂时起不来
	l.healthy.Store(false)
	l.last().exit()
	waitCond(t, "闸门带 vm 原因关", func() bool { s := status(t, e.d); return !s.GateOpen && hasReason(s, gatesm.ReasonVM) })
	if s := status(t, e.d); !s.Protected || s.State != "ENABLED" {
		t.Fatalf("虚拟机崩溃期间断网规则应保持、状态不变：protected=%v state=%s", s.Protected, s.State)
	}
	mustDropped(t, held, "虚拟机崩溃")
	if err := e.echo(t, 20003, plain); err == nil {
		t.Fatal("虚拟机不在时走代理的新连接应被拒")
	}
	if !hasEvent(e.d, "vm", "failed") {
		t.Fatal("应有虚拟机失败的事件")
	}
	// 重新拉起并健康：vm 原因清掉，但复查不通过，闸门仍关
	l.healthy.Store(true)
	waitCond(t, "重新健康", func() bool {
		s := status(t, e.d)
		return s.VMManager.Phase == vmmgr.PhaseRunning && !hasReason(s, gatesm.ReasonVM) && l.launches.Load() >= 2
	})
	waitCond(t, "复查跑过一轮", func() bool { return hasReason(status(t, e.d), gatesm.ReasonExitCheck) })
	if s := status(t, e.d); s.GateOpen {
		t.Fatalf("虚拟机恢复后闸门不能直接开（复查没通过）：%+v", s.Gate)
	}
	// 复查通过才开
	pass.Store(true)
	e.d.re.Kick()
	if s := waitGateOpen(t, e.d); !s.GateOpen {
		t.Fatalf("复查通过后应打开：%+v", s.Gate)
	}
	if err := e.echo(t, 20003, plain); err != nil {
		t.Fatalf("恢复后应能走代理：%v", err)
	}
	// 期间没有任何走代理的目标被直连
	fake.mu.Lock()
	for _, d := range fake.dials[dialsBefore:] {
		if strings.Contains(d, plain.Addr().String()) {
			t.Errorf("虚拟机不在时走代理的目标被直连了：%s", d)
		}
	}
	fake.mu.Unlock()
	if n := killer.n.Load(); n != 0 {
		t.Fatalf("虚拟机重启不应结束 Claude（调用 %d 次）", n)
	}
}

// d：abroadd 正常退出时关闭自己拉起的虚拟机；状态文件是回国（全新安装）时不拉起。
func TestVMManagerShutdownAndRestored(t *testing.T) {
	l := &dmLauncher{}
	l.healthy.Store(true)
	mgr := &vmmgr.Manager{Launcher: l, Health: l.health, Timing: dmTiming}
	de := newDataEnv(t, &m3Opener{missing: map[string]bool{}}, nil)
	o := de.opts()
	o.Sys.VM = mgr
	d := de.start(o)
	time.Sleep(100 * time.Millisecond)
	if l.launches.Load() != 0 || status(t, d).VMManager.Phase != vmmgr.PhaseIdle {
		t.Fatalf("回国状态不应拉起虚拟机（拉起 %d 次）", l.launches.Load())
	}
	post(t, d, "/v1/enable", 200)
	waitCond(t, "running", func() bool { return status(t, d).VMManager.Phase == vmmgr.PhaseRunning })
	p := l.last()
	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.Done():
	default:
		t.Fatal("abroadd 退出时应关闭虚拟机")
	}
}

// gateEvents 返回目前全部的闸门事件（“kind 详情”）。事件时间只精确到秒，所以按条数切分（本测试事件远少于环形缓冲的容量）。
func gateEvents(d *Daemon) (out []string) {
	for _, r := range d.events.List() {
		if r.Source == "gate" {
			out = append(out, r.Kind+" "+r.Detail)
		}
	}
	return out
}

// 把启用、停用、回国、运行中崩溃时闸门事件的实际顺序固定下来（应用的通知测试用同样的序列）。
func TestVMManagerGateEventOrder(t *testing.T) {
	l := &dmLauncher{}
	l.healthy.Store(true)
	mgr := &vmmgr.Manager{Launcher: l, Health: l.health, Timing: dmTiming}
	de := newDataEnv(t, &m3Opener{missing: map[string]bool{}}, nil)
	o := de.opts()
	o.Sys.VM = mgr
	d := de.start(o)
	step := func(name string, do func(), want []string) {
		t.Helper()
		before := len(gateEvents(d))
		do()
		time.Sleep(200 * time.Millisecond) // 让管理器的阶段变化落定
		got := gateEvents(d)[before:]
		t.Logf("%s：%q", name, got)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("%s 的闸门事件：%q，应为 %q", name, got, want)
		}
	}
	step("启用", func() {
		post(t, d, "/v1/enable", 200)
		waitGateOpen(t, d)
	}, []string{"gate_closed vm：虚拟机启动中", "gate_opened "})
	step("停用", func() { post(t, d, "/v1/disable", 200) }, []string{"gate_closed state：停用"})
	step("再启用", func() {
		post(t, d, "/v1/enable", 200)
		waitGateOpen(t, d)
	}, []string{"gate_opened "})
	step("运行中崩溃", func() {
		l.healthy.Store(false)
		l.last().exit()
		waitCond(t, "关", func() bool { return !status(t, d).GateOpen })
		l.healthy.Store(true)
		waitGateOpen(t, d)
	}, []string{"gate_closed vm：虚拟机没有运行", "gate_opened "})
	step("回国", func() {
		post(t, d, "/v1/restore", 200)
		waitCond(t, "虚拟机关掉", func() bool { return status(t, d).VMManager.Phase == vmmgr.PhaseIdle })
	}, []string{"gate_closed state：恢复本机网络", "gate_closed vm：虚拟机正在关闭"})
	step("从回国再启用", func() {
		post(t, d, "/v1/enable", 200)
		waitGateOpen(t, d)
	}, []string{"gate_closed vm：虚拟机启动中", "gate_opened "})
}
