package envcheck

import (
	"context"
	"errors"
	"sync"
	"time"

	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/gatesm"
	"claude-abroad/internal/usererror"
)

// 调度参数（design 8.1）。
const (
	DefaultInterval = 10 * time.Minute
	DefaultGrace    = 10 * time.Minute // 无法判断沿用上一次明确通过的结论最多这么久
)

// GateCtl 是环境检测对闸门的需要（gatesm.Gate 满足）：只动 env_check 这个原因，和探测窗口。
type GateCtl interface {
	AddReason(r gatesm.Reason, detail string)
	ClearReason(r gatesm.Reason)
	OpenProbe(d time.Duration) bool
	CloseProbe()
}

// Monitor 是环境检测的调度和与闸门的联动（m4-plan v2.6 决定 3、4）：
//   - 启用第 ⑦ 步（AtEnable）：打开探测窗口 → 跑全部硬性项（无法判断按 5、15、45 秒重试）→ 关闭窗口；
//   - 运行中：每 Interval 一轮，Kick 立即补一轮；只在状态是启用时跑。每轮都开探测窗口（闸门因环境检测关着时，
//     下一轮的探测要靠它才能发出去）。硬性项明确不通过 → 加 env_check；全部通过 → 清掉并补一轮出口复查（AfterPass）；
//     无法判断 → 最近一次明确通过在 Grace 之内就沿用，否则加 env_check。最近一次通过只在内存里（重启后要重新明确通过）。
//   - 不结束正在运行的 Claude（design 8.1）。
type Monitor struct {
	// BeforeEnable 在启用第 ⑦ 步开始时调用（测本机真实公网 IP，M4-6b）；可为 nil。
	BeforeEnable func(ctx context.Context)
	Runner       *Runner
	Gate         GateCtl
	Enabled      func() bool
	AfterPass    func()
	Clock        gatesm.Clock
	Interval     time.Duration
	Grace        time.Duration
	OnEvent      func(kind, detail string)

	kickOnce sync.Once
	kick     chan struct{}
	runMu    sync.Mutex
	mu       sync.Mutex
	lastPass time.Time
	last     *Report
	rounds   int
}

func (m *Monitor) clock() gatesm.Clock {
	if m.Clock != nil {
		return m.Clock
	}
	return gatesm.RealClock
}

func (m *Monitor) event(kind, detail string) {
	if m.OnEvent != nil {
		m.OnEvent(kind, detail)
	}
}

func (m *Monitor) kickCh() chan struct{} {
	m.kickOnce.Do(func() { m.kick = make(chan struct{}, 1) })
	return m.kick
}

// Kick 立即补一轮（不阻塞）。
func (m *Monitor) Kick() {
	select {
	case m.kickCh() <- struct{}{}:
	default:
	}
}

// Last 返回最近一轮的结果。
func (m *Monitor) Last() (Report, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last == nil {
		return Report{}, false
	}
	return *m.last, true
}

// Rounds 返回运行中跑过的轮数（测试用）。
func (m *Monitor) Rounds() int { m.mu.Lock(); defer m.mu.Unlock(); return m.rounds }

func (m *Monitor) record(rep Report, passed bool) {
	m.mu.Lock()
	m.last = &rep
	if passed {
		m.lastPass = m.clock().Now()
	}
	m.mu.Unlock()
}

// ErrEnvCheck 是启用第 ⑦ 步没有通过。
var ErrEnvCheck = errors.New("环境检测没有通过")

// AtEnable 是启用第 ⑦ 步（实现 state.EnvCheck）：全部硬性项明确通过返回 nil。
func (m *Monitor) AtEnable(ctx context.Context) error {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	if !m.Gate.OpenProbe(0) {
		return usererror.New("环境检测", "探测窗口打不开（接管或出口检查有问题）", "请稍后再试启用。", ErrEnvCheck)
	}
	defer m.Gate.CloseProbe()
	if m.BeforeEnable != nil {
		m.BeforeEnable(ctx)
	}
	rep := m.Runner.RunWithRetry(ctx)
	m.record(rep, rep.Hard == exitcheck.Pass)
	switch rep.Hard {
	case exitcheck.Pass:
		m.event("envcheck_pass", "启用时环境检测通过")
		return nil
	case exitcheck.Fail:
		f := rep.Failed()
		m.event("envcheck_fail", "启用时环境检测不通过："+Summary(f))
		return usererror.New("环境检测", "这些项不通过："+Summary(f), "请按下面处理后再启用："+Fixes(f), ErrEnvCheck)
	}
	u := rep.Unknown()
	m.event("envcheck_unknown", "启用时环境检测无法判断："+Summary(u))
	return usererror.New("环境检测", "重试后这些项仍无法判断："+Summary(u), "请稍后再试启用；"+Fixes(u), ErrEnvCheck)
}

// Run 按 Interval 循环（或被 Kick），直到 ctx 结束。不是启用状态时不跑。
func (m *Monitor) Run(ctx context.Context) {
	iv := m.Interval
	if iv <= 0 {
		iv = DefaultInterval
	}
	kick := m.kickCh()
	for {
		t := m.clock().AfterFunc(iv, m.Kick)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-kick:
		}
		t.Stop()
		if m.Enabled != nil && !m.Enabled() {
			continue
		}
		m.RunOnce(ctx)
	}
}

// RunOnce 跑一轮运行中的检测并联动闸门。
func (m *Monitor) RunOnce(ctx context.Context) Report {
	m.runMu.Lock()
	defer m.runMu.Unlock()
	opened := m.Gate.OpenProbe(0)
	rep := m.Runner.Round(ctx)
	if opened {
		m.Gate.CloseProbe()
	}
	now := m.clock().Now()
	m.mu.Lock()
	m.rounds++
	last := m.lastPass
	m.mu.Unlock()
	grace := m.Grace
	if grace <= 0 {
		grace = DefaultGrace
	}
	switch rep.Hard {
	case exitcheck.Pass:
		m.record(rep, true)
		m.Gate.ClearReason(gatesm.ReasonEnvCheck)
		if m.AfterPass != nil {
			m.AfterPass()
		}
	case exitcheck.Fail:
		m.record(rep, false)
		f := rep.Failed()
		m.Gate.AddReason(gatesm.ReasonEnvCheck, "环境检测不通过："+Summary(f))
		m.event("envcheck_fail", "环境检测不通过："+Summary(f))
	default:
		m.record(rep, false)
		if !last.IsZero() && now.Sub(last) < grace {
			break // 沿用上一次的结论
		}
		u := rep.Unknown()
		m.Gate.AddReason(gatesm.ReasonEnvCheck, "环境检测无法判断超过 10 分钟："+Summary(u))
		m.event("envcheck_unknown", "环境检测无法判断超过 10 分钟："+Summary(u))
	}
	return rep
}
