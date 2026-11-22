package daemon

import (
	"context"
	"sort"
	"sync"
	"time"

	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/exitconf"
	"claude-abroad/internal/gatesm"
	"claude-abroad/internal/state"
	"claude-abroad/internal/usererror"
)

// 分流出口的下发和按出口的检查（M4-8，m4-plan v2.11 决定 3、5，v2.12 决定 3、4）：
//   - 下发时机：启用时、出口集合变化时、发现虚拟机重启过时（health 的启动标识变了）。先把端点集合交给 abroad-vm
//     （宿主的出站白名单），再把出口交给 agent；失败只让出口不可用（发事件），不碰主代理和闸门。
//   - 下发和出口的增删用 exitPushMu 串行；不占状态机的忙标记、不阻塞主代理的复查。
//   - 每轮（复查的间隔）：看启动标识 → 需要时下发，否则刷新 agent 报告的引擎状态 → 逐个检查正式的出口
//     （并发有上限，每个出口单独限时，卡住的出口不拖累别的，也碰不到主代理的续期）。

// ExitEndpoint 是交给 abroad-vm 的一个服务器端点（主机、协议、端口；主机是域名时由 agent 经网关代解析）。
type ExitEndpoint struct {
	Host  string `json:"host"`
	Proto string `json:"proto"`
	Port  int    `json:"port"`
}

// AgentExit 是 agent 报告的一个出口的引擎状态（不含端点）。
type AgentExit struct {
	Tag    string `json:"tag"`
	Up     bool   `json:"up"`
	Reason string `json:"reason,omitempty"`
}

// ExitVM 是 daemon 和虚拟机之间关于分流出口的操作（ExternalVM 实现；测试用假的）。
type ExitVM interface {
	// BootID 是 agent health 里的启动标识（虚拟机原地重启后会变）。
	BootID(ctx context.Context) (string, error)
	// PushEndpoints 整体替换 abroad-vm 出站白名单里分流出口的那一组。
	PushEndpoints(ctx context.Context, eps []ExitEndpoint) error
	// PushExits 整体替换 agent 的分流出口，返回各出口的引擎状态。
	PushExits(ctx context.Context, exits []StoredExit) ([]AgentExit, error)
	// AgentExits 读 agent 报告的各出口的引擎状态。
	AgentExits(ctx context.Context) ([]AgentExit, error)
}

// 时长。
const (
	ExitVMTimeout     = 10 * time.Second // 读启动标识、刷新状态
	ExitPushTimeout   = 90 * time.Second // 下发（agent 要建命名空间、起引擎）
	ExitCheckTimeout  = exitcheck.TotalTimeout + 5*time.Second
	ExitCheckParallel = 4
)

// exitLoop 是下发和按出口检查的循环。
type exitLoop struct {
	d    *Daemon
	kick chan struct{}

	// 以下由 d.exitPushMu 保护。
	lastBoot string
	pending  bool

	// 按出口的检查：正在跑的出口、并发上限、退出时等它们。
	checkMu  sync.Mutex
	inflight map[string]bool
	sem      chan struct{}
	checkWG  sync.WaitGroup
}

// endpointsOf 从出口的配置里取端点（去重、排序）。
func endpointsOf(exits []StoredExit) []ExitEndpoint {
	seen := map[ExitEndpoint]bool{}
	var out []ExitEndpoint
	for _, x := range exits {
		var eps []exitconf.Endpoint
		switch x.Kind {
		case exitconf.WireGuard:
			if w, err := exitconf.ParseWG([]byte(x.Conf)); err == nil {
				eps = w.Endpoints()
			}
		case exitconf.OpenVPN:
			if o, err := exitconf.ParseOVPN([]byte(x.Conf)); err == nil {
				eps = o.Endpoints()
			}
		case exitconf.Clash:
			if c, err := exitconf.ParseClash([]byte(x.Conf)); err == nil {
				eps = c.Endpoints()
			}
		}
		for _, e := range eps {
			ep := ExitEndpoint{Host: e.Host, Proto: e.Proto, Port: e.Port}
			if !seen[ep] {
				seen[ep] = true
				out = append(out, ep)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		if a.Proto != b.Proto {
			return a.Proto < b.Proto
		}
		return a.Port < b.Port
	})
	return out
}

// exitsChangedLocked 在出口集合变了之后调用（调用方持有 exitPushMu）：登记新的集合（新出口从“不可用”开始），
// 标记要下发，并叫醒循环。
func (d *Daemon) exitsChangedLocked() {
	tags := []string{}
	for _, x := range d.exits.All() {
		tags = append(tags, x.Tag)
	}
	d.exitAvail.Configure(tags)
	if d.exitLoop != nil {
		d.exitLoop.pending = true
		d.exitLoop.wake()
	}
}

// exitsOnEnable 在状态机进入“启用”时调用：下发一次。
func (d *Daemon) exitsOnEnable() {
	if d.exitLoop == nil {
		return
	}
	d.exitPushMu.Lock()
	d.exitLoop.pending = true
	d.exitPushMu.Unlock()
	d.exitLoop.wake()
}

func (l *exitLoop) wake() {
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

func (l *exitLoop) enabled() bool {
	f, err := l.d.store.Load()
	return err == nil && f.State == state.Enabled
}

// run 每个复查间隔或被叫醒时跑一轮，直到 ctx 结束。
func (l *exitLoop) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	defer l.checkWG.Wait() // 退出前等正在跑的检查（ctx 已取消，它们很快返回）
	iv := l.d.opts.RecheckInterval
	if iv <= 0 {
		iv = gatesm.DefaultInterval
	}
	for {
		timer := make(chan struct{}, 1)
		stop := l.d.clock().AfterFunc(iv, func() {
			select {
			case timer <- struct{}{}:
			default:
			}
		})
		select {
		case <-ctx.Done():
			stop.Stop()
			return
		case <-timer:
		case <-l.kick:
			stop.Stop()
		}
		if l.enabled() {
			l.round(ctx)
		}
	}
}

// round 跑一轮：下发或刷新，然后检查。
func (l *exitLoop) round(ctx context.Context) {
	d := l.d
	d.maintainMain(ctx) // 先主代理（重启、配置被改），再分流出口（M5-4 决定 3）
	vm := d.opts.Sys.ExitVM
	if vm == nil || len(d.exits.All()) == 0 && !l.pendingNow() {
		return
	}
	d.exitPushMu.Lock()
	exits := d.exits.All()
	bctx, cancel := context.WithTimeout(ctx, ExitVMTimeout)
	boot, err := vm.BootID(bctx)
	cancel()
	pushed := false
	if err != nil {
		for _, x := range exits {
			d.exitAvail.SetPushed(x.Tag, false, "虚拟机没有就绪")
		}
		l.pending = true // 虚拟机好了再下发
	} else if boot != l.lastBoot || l.pending {
		if l.lastBoot != "" && boot != l.lastBoot {
			d.events.Add("exits", "vm_rebooted", "虚拟机重启过，重新下发分流出口")
		}
		if l.push(ctx, vm, exits) {
			l.lastBoot, l.pending, pushed = boot, false, true
		}
	} else {
		rctx, cancel := context.WithTimeout(ctx, ExitVMTimeout)
		st, err := vm.AgentExits(rctx)
		cancel()
		if err == nil {
			l.applyAgent(exits, st)
		}
	}
	d.exitPushMu.Unlock()
	_ = pushed
	l.checkAll(ctx, exits)
}

func (l *exitLoop) pendingNow() bool {
	l.d.exitPushMu.Lock()
	defer l.d.exitPushMu.Unlock()
	return l.pending
}

// push 先换出站白名单的端点集合，再换 agent 的出口。调用方持有 exitPushMu。
func (l *exitLoop) push(ctx context.Context, vm ExitVM, exits []StoredExit) bool {
	d := l.d
	fail := func(why string) bool {
		for _, x := range exits {
			d.exitAvail.SetPushed(x.Tag, false, why)
		}
		d.events.Add("exits", "exit_push_failed", "分流出口没有下发到虚拟机："+why+"（这些出口暂时不可用，命中它们的连接走主代理）")
		return false
	}
	pctx, cancel := context.WithTimeout(ctx, ExitPushTimeout)
	defer cancel()
	if err := vm.PushEndpoints(pctx, endpointsOf(exits)); err != nil {
		return fail("出站白名单没有更新：" + usererror.Reason(err))
	}
	st, err := vm.PushExits(pctx, exits)
	if err != nil && st == nil {
		return fail(usererror.Reason(err))
	}
	for _, x := range exits {
		d.exitAvail.SetPushed(x.Tag, true, "")
	}
	l.applyAgent(exits, st)
	if err != nil { // 部分出口没建起来：它们在 applyAgent 里已经是“引擎不可用”
		d.events.Add("exits", "exit_push_partial", "有分流出口没有建起来："+usererror.Reason(err))
	}
	return true
}

// applyAgent 把 agent 报告的引擎状态记进可用状态；agent 没报的出口按不可用。
func (l *exitLoop) applyAgent(exits []StoredExit, st []AgentExit) {
	got := map[string]AgentExit{}
	for _, a := range st {
		got[a.Tag] = a
	}
	for _, x := range exits {
		a, ok := got[x.Tag]
		switch {
		case !ok:
			l.d.exitAvail.SetAgent(x.Tag, false, "虚拟机里没有这个出口")
		default:
			l.d.exitAvail.SetAgent(x.Tag, a.Up, a.Reason)
		}
	}
}

// checkAll 发起这一轮的按出口检查，不等它们做完：每个出口同一时间只有一个检查在跑（上一轮的还没完就跳过），
// 全部出口共用并发上限 ExitCheckParallel，每个单独限时。一个出口卡住只影响它自己，不耽误这一轮的下发、
// 别的出口的检查，也碰不到主代理的续期。不碰闸门、不改地区。
func (l *exitLoop) checkAll(ctx context.Context, exits []StoredExit) {
	check := l.d.opts.Sys.CheckExit
	if check == nil {
		return
	}
	for _, x := range exits {
		v := l.d.exitAvail.View(x.Tag)
		if !v.Pushed || !v.AgentUp {
			continue // 还没下发或引擎不可用：检查也开不了流
		}
		l.checkMu.Lock()
		if l.inflight == nil {
			l.inflight = map[string]bool{}
		}
		if l.inflight[x.Tag] {
			l.checkMu.Unlock()
			continue
		}
		l.inflight[x.Tag] = true
		l.checkMu.Unlock()
		l.checkWG.Add(1)
		go func(tag string) {
			defer l.checkWG.Done()
			defer func() {
				l.checkMu.Lock()
				delete(l.inflight, tag)
				l.checkMu.Unlock()
			}()
			select {
			case l.sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-l.sem }()
			cctx, cancel := context.WithTimeout(ctx, ExitCheckTimeout)
			defer cancel()
			rep := check(cctx, tag)
			if ctx.Err() != nil {
				return
			}
			items := map[string]string{}
			for _, it := range rep.Items {
				items[it.Name] = string(it.Result)
			}
			before := l.d.exitAvail.View(tag).Up
			l.d.exitAvail.Update(tag, toGate(rep.SplitVerdict()), toGate(rep.UDPResult()), items)
			if after := l.d.exitAvail.View(tag); after.Up != before {
				if after.Up {
					l.d.events.Add("exits", "exit_up", "分流出口 "+tag+" 可用")
				} else {
					l.d.events.Add("exits", "exit_down", "分流出口 "+tag+" 不可用（"+after.Reason+"），命中它的连接走主代理")
				}
			}
		}(x.Tag)
	}
}

func toGate(r exitcheck.Result) gatesm.Result {
	switch r {
	case exitcheck.Pass:
		return gatesm.Pass
	case exitcheck.Fail:
		return gatesm.Fail
	}
	return gatesm.Unknown
}

// clock 是 daemon 的时钟（Options.Clock，默认真实时钟）。
func (d *Daemon) clock() gatesm.Clock {
	if d.opts.Clock != nil {
		return d.opts.Clock
	}
	return gatesm.RealClock
}

// enableHook 包住状态机的“状态接收方”：进入启用时下发一次分流出口（m4-plan v2.11 决定 3）。
type enableHook struct {
	inner state.DataState // 可为 nil（假系统没有数据面）
	d     *Daemon
}

func (h enableHook) SetEnabled(enabled bool) {
	if h.inner != nil {
		h.inner.SetEnabled(enabled)
	}
	if enabled {
		h.d.exitsOnEnable()
	}
}
