package state

import (
	"claude-abroad/internal/gatesm"
	"context"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 监护的事件种类（M2-7e）。
const (
	EvGateClosed      EventKind = "gate_closed"      // 某项核对不通过，关了闸门（Detail 是原因）
	EvUtunRebuilt     EventKind = "utun_rebuilt"     // utun 没了，已重建
	EvRouteRepaired   EventKind = "route_repaired"   // 路由不对，已重新添加
	EvDNSRepaired     EventKind = "dns_repaired"     // DNS 设置不在，已补回
	EvRepairFailed    EventKind = "repair_failed"    // 修复动作本身出错
	EvStillFailing    EventKind = "still_failing"    // 修复后仍不通过，闸门保持关闭
	EvTakeoverCleared EventKind = "takeover_cleared" // 4 项恢复通过，清掉 takeover 原因并立即补一轮复查（开闸门由复查放行）
)

// ProbeAddrs 是核对路由用的三个地址：两条 /1 各一个，加上 fake-IP DNS（别的软件的 TUN 若加了更具体的路由会抢走它）。
var ProbeAddrs = []netip.Addr{
	netip.MustParseAddr("1.1.1.1"),
	netip.MustParseAddr("129.0.0.1"),
	TakeoverDNS,
}

// DefaultSuperviseInterval 是监护的间隔。
const DefaultSuperviseInterval = 5 * time.Second

// PatrolStepper 是监护需要的 pf 巡检（*pf.Patrol 满足）。
type PatrolStepper interface {
	Step(ctx context.Context) error
	Protected() bool
}

// Supervisor 是运行期监护（M2-7e，设计 6.1“运行期监护”）：
//   - Tick 跑一轮；Run 按 Interval 循环；Kick 让下一轮立刻开始；
//   - 与启用、停用、恢复、Recover 互斥：拿不到状态机的忙标记就跳过这一轮，不排队；
//   - RESTORED 什么都不做；ENABLING、RESTORING 只跑 pf 巡检；DISABLED、ENABLED 跑巡检后核对 4 项；
//   - 任一项不通过：加 takeover 原因（闸门开着时关闸门并 Teardown 一次），状态文件不改；当轮修复后再核对；
//   - 4 项都通过且之前加过 takeover：清掉它，立即补一轮复查（m4-plan v2.2 决定 5）。监护只管 takeover：
//     不启动虚拟机、不做出口检查、不开闸门，这些由复查循环负责。
type Supervisor struct {
	M        *Machine
	Patrol   PatrolStepper
	Interval time.Duration    // 为零用 DefaultSuperviseInterval
	Now      func() time.Time // 为 nil 用 time.Now
	OnEvent  func(Event)      // 可为 nil

	kickOnce sync.Once
	kick     chan struct{}
	rounds   atomic.Int64 // Run 跑完的轮数（含跳过的轮）；测试用来等启动后的第一轮结束

	// 只在持有状态机忙标记时访问。
	takeoverSet bool // 加过 takeover 原因、还没清
	// 查询本身出错（route get、scutil 偶发失败）的连续轮数：连续 2 轮才算不通过，成功一轮清零。
	viaErrs, dnsErrs int
}

// QueryErrThreshold 是查询出错连续几轮才算不通过。
const QueryErrThreshold = 2

func (s *Supervisor) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Supervisor) emit(kind EventKind, detail string) {
	if s.OnEvent != nil {
		s.OnEvent(Event{Kind: kind, Detail: detail})
	}
}

func (s *Supervisor) kickCh() chan struct{} {
	s.kickOnce.Do(func() { s.kick = make(chan struct{}, 1) })
	return s.kick
}

// Kick 让 Run 立刻跑下一轮（不阻塞；已经有一个在排就忽略）。
func (s *Supervisor) Kick() {
	select {
	case s.kickCh() <- struct{}{}:
	default:
	}
}

// Run 立即跑一轮，之后每 Interval（或被 Kick）跑一轮，直到 ctx 结束。
func (s *Supervisor) Run(ctx context.Context) {
	iv := s.Interval
	if iv <= 0 {
		iv = DefaultSuperviseInterval
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	kick := s.kickCh()
	for {
		_ = s.Tick(ctx)
		s.rounds.Add(1)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-kick:
		}
	}
}

// Rounds 是 Run 跑完的轮数。
func (s *Supervisor) Rounds() int64 { return s.rounds.Load() }

// Tick 跑一轮。有操作在跑时直接返回 nil（这一轮跳过）。
func (s *Supervisor) Tick(ctx context.Context) error {
	m := s.M
	if !m.busy.CompareAndSwap(false, true) {
		return nil
	}
	defer m.unlock()
	f, err := m.Store.Load()
	if err != nil {
		// 状态文件读不出来：安全的一侧是关闸门（pf 由巡检保持）。
		s.closeGate("状态文件读不出来")
		_ = s.Patrol.Step(ctx)
		return err
	}
	switch f.State {
	case Restored:
		return nil
	case Enabling, Restoring:
		// 上次操作没做完：只保证 pf 在（巡检已暂停时 Step 什么都不做）。
		return s.Patrol.Step(ctx)
	}
	_ = s.Patrol.Step(ctx)
	if bad := s.verify(ctx); len(bad) > 0 {
		s.closeGate(strings.Join(bad, "、"))
		s.repair(ctx, f, bad)
		if bad = s.verify(ctx); len(bad) > 0 {
			s.emit(EvStillFailing, strings.Join(bad, "、"))
			return nil
		}
	}
	if s.takeoverSet {
		s.takeoverSet = false
		m.D.Gate.ClearReason(gatesm.ReasonTakeover)
		s.emit(EvTakeoverCleared, "")
		if f.State == Enabled {
			m.D.Recheck.Kick() // 不等 60 秒：故障消除后立即补一轮复查
			if m.D.Env != nil {
				m.D.Env.Kick() // 环境检测也立即补一轮（m4-plan v2.6 决定 4）
			}
		}
	}
	return nil
}

// closeGate 加 takeover 原因；闸门开着时断开走代理的连接一次。
func (s *Supervisor) closeGate(reason string) {
	m := s.M
	was := m.D.Gate.IsOpen()
	m.D.Gate.AddReason(gatesm.ReasonTakeover, reason)
	s.takeoverSet = true
	if was {
		m.D.Teardown.Teardown(context.Background())
		s.emit(EvGateClosed, reason)
	}
}

// 核对项的名字（事件里的原因）。
const (
	badPF    = "pf 保护失效"
	badUtun  = "utun 没了"
	badRoute = "路由不经我们的 utun"
	badDNS   = "DNS 设置不在"
)

// verify 核对 4 项，返回不通过的项。
func (s *Supervisor) verify(ctx context.Context) []string {
	m := s.M
	var bad []string
	if !s.Patrol.Protected() {
		bad = append(bad, badPF)
	}
	name := m.D.Tun.Name()
	if !m.D.Tun.Alive() || name == "" {
		bad = append(bad, badUtun)
	} else {
		wrong, qerr := false, false
		// 整张路由表：别的虚拟网卡占着公网或 fake-IP 段的路由（M2-9a 决定 1）。修不了：我们不删不改别人的路由。
		conf, err := m.D.Routes.Conflicts(ctx, name)
		if err != nil {
			qerr = true
		} else if len(conf) > 0 {
			wrong = true
		}
		for _, a := range ProbeAddrs {
			via, err := m.D.Routes.Via(ctx, a)
			if err != nil {
				qerr = true
				continue
			}
			if via != name {
				wrong = true
			}
		}
		if s.countQuery(&s.viaErrs, qerr) || wrong {
			bad = append(bad, badRoute)
		}
	}
	ok, err := m.D.DNS.Present(ctx)
	if s.countQuery(&s.dnsErrs, err != nil) || (err == nil && !ok) {
		bad = append(bad, badDNS)
	}
	return bad
}

// countQuery 记查询出错的连续轮数，返回是否已到门限（查到是坏的不经这里，当轮处理）。
func (s *Supervisor) countQuery(n *int, failed bool) bool {
	if !failed {
		*n = 0
		return false
	}
	*n++
	return *n >= QueryErrThreshold
}

func hasItem(bad []string, item string) bool {
	for _, b := range bad {
		if b == item {
			return true
		}
	}
	return false
}

// repair 当轮修复。pf 保护失效没有可修的动作（等巡检恢复）。
func (s *Supervisor) repair(ctx context.Context, f File, bad []string) {
	m := s.M
	if hasItem(bad, badUtun) {
		// rebuild 建新 utun、写回名字、按它应用 pf、补路由和 DNS。
		if _, err := m.rebuild(ctx, f.Step); err != nil {
			s.emit(EvRepairFailed, "重建 utun 失败")
			return
		}
		s.emit(EvUtunRebuilt, "")
		return
	}
	if hasItem(bad, badRoute) {
		name := m.D.Tun.Name()
		ok := true
		for _, dst := range TakeoverRoutes {
			if err := ignoreExists(m.D.Routes.Add(ctx, dst, name)); err != nil {
				ok = false
			}
		}
		if ok {
			s.emit(EvRouteRepaired, "")
		} else {
			s.emit(EvRepairFailed, "重新添加路由失败")
		}
	}
	if hasItem(bad, badDNS) {
		if err := ignoreExists(m.D.DNS.Set(ctx, TakeoverDNS)); err != nil {
			s.emit(EvRepairFailed, "补回 DNS 设置失败")
		} else {
			s.emit(EvDNSRepaired, "")
		}
	}
}
