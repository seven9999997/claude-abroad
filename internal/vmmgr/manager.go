// Package vmmgr 是 abroadd 管理 abroad-vm 进程的生命周期（M6-2a，m6-plan v2.2，design 3.1）：
// 拉起、健康检查、崩溃后退避重启、退出时关闭。虚拟机内核本身的看护（看门狗、原地重启）仍由 abroad-vm 里的
// vmhost.Supervisor 负责；这里管的是 abroad-vm 这个进程。
//
// 不变量：
//   - 虚拟机不在、还没健康、健康检查连续不通过时，闸门带着 ReasonVM 关着（加原因会让放行作废）；
//   - 重新健康时只清掉 ReasonVM 并请复查补一轮，闸门要等出口检查通过才开，不直接开；
//   - 同一时刻最多一个由自己拉起的 abroad-vm；结束进程只经 Launch 返回的进程句柄（不按进程号找）；
//     强停后仍不退出时报告卡住，不再拉起第二个；
//   - 连续失败到 MaxFailures 次后停止重启并报告，保持断网，不自动回国；健康运行满 StableFor 或用户重新启用后计数清零；
//   - 管理器不结束任何别的进程（包括 Claude）。
package vmmgr

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"claude-abroad/internal/gatesm"
	"claude-abroad/internal/redact"
	"claude-abroad/internal/usererror"
)

// Proc 是一个由 Launcher 拉起的 abroad-vm 进程。
type Proc interface {
	Done() <-chan struct{} // 进程已退出（已回收）时关闭
	ExitErr() error        // Done 之后可读：退出的原因（正常退出为 nil）
	Terminate() error      // 请它优雅退出（SIGTERM）
	Kill() error           // 强制结束（SIGKILL）
}

// Launcher 拉起一个 abroad-vm 进程。以哪个用户、用什么命令拉起由实现决定（R1 的结论回来只换实现）。
type Launcher interface {
	Launch() (Proc, error)
}

// Clock 是可注入的时钟。
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// RealClock 是真实时钟。
var RealClock Clock = realClock{}

// Gate 是闸门里管理器要用的部分（gatesm.Gate、daemon.Gate 满足）。管理器只加、清 ReasonVM。
type Gate interface {
	AddReason(r gatesm.Reason, detail string)
	ClearReason(r gatesm.Reason)
}

// Kicker 请出口复查立即补一轮（gatesm.Rechecker 满足）。
type Kicker interface{ Kick() }

// Timing 是各项时长。测试里换成很短的值。
type Timing struct {
	StartTimeout   time.Duration // 拉起后这么久仍没健康：结束它，算一次失败
	ProbeEvery     time.Duration // 健康检查的间隔
	ProbeTimeout   time.Duration // 单次健康检查
	PatrolFailures int           // 健康之后连续这么多次不通过：闸门关（ReasonVM）
	UnhealthyKill  time.Duration // 不健康持续这么久：结束它重启，算一次失败
	StopWait       time.Duration // 请它退出后等这么久，超时强制结束
	KillWait       time.Duration // 强制结束后再等这么久，仍不退出就报告卡住
	StableFor      time.Duration // 健康运行满这么久，连续失败计数清零
	BackoffMin     time.Duration
	BackoffMax     time.Duration
	MaxFailures    int           // 连续失败这么多次后停止重启
	StartWait      time.Duration // Start（启用、地区同步后）最多等这么久
}

// DefaultTiming 是正式的取值。abroad-vm 自己的看门狗是 30 秒、优雅停止最多约 13 秒，这里的时长都比它长。
var DefaultTiming = Timing{
	StartTimeout:   90 * time.Second,
	ProbeEvery:     2 * time.Second,
	ProbeTimeout:   5 * time.Second,
	PatrolFailures: 3,
	UnhealthyKill:  90 * time.Second,
	StopWait:       20 * time.Second,
	KillWait:       5 * time.Second,
	StableFor:      2 * time.Minute,
	BackoffMin:     time.Second,
	BackoffMax:     60 * time.Second,
	MaxFailures:    6,
	StartWait:      3 * time.Minute,
}

// 阶段。
const (
	PhaseIdle      = "idle"      // 不需要虚拟机（回国、停用时关了虚拟机）
	PhaseStarting  = "starting"  // 已拉起，等健康
	PhaseRunning   = "running"   // 健康
	PhaseUnhealthy = "unhealthy" // 进程在，健康检查连续不通过（闸门关）
	PhaseBackoff   = "backoff"   // 失败后等下一次拉起
	PhaseStopping  = "stopping"  // 正在结束进程
	PhaseFailed    = "failed"    // 连续失败到上限，停止重启（保持断网）
	PhaseStuck     = "stuck"     // 强制结束后仍不退出，不再拉起第二个
)

// Status 是给状态接口的管理器状态。不含路径以外的敏感内容（LastError 经脱敏）。
type Status struct {
	Managed   bool   `json:"managed"`              // 虚拟机由 abroadd 管理（false：由外部启动）
	Phase     string `json:"phase"`                // 见 Phase*
	Restarts  int    `json:"restarts"`             // 这次 abroadd 运行以来重新拉起的次数
	Failures  int    `json:"failures"`             // 当前的连续失败次数
	LastError string `json:"last_error,omitempty"` // 最近一次失败的原因
}

// 错误。
var (
	ErrFailed  = errors.New("虚拟机连续启动失败，已停止重启")
	ErrStuck   = errors.New("虚拟机进程强制结束后仍没有退出")
	ErrTimeout = errors.New("虚拟机在时限内没有就绪")
	ErrClosed  = errors.New("虚拟机管理器已经停止")
)

// Manager 管理 abroad-vm 进程。零值不可用：填好字段后调用 Run（只调一次）。
type Manager struct {
	Launcher Launcher
	Health   func(ctx context.Context) error // nil 表示健康（含出站已收紧）
	Gate     Gate
	Kick     Kicker // 可为 nil
	Clock    Clock  // nil 用 RealClock
	Timing   Timing // 零值用 DefaultTiming
	Logf     func(format string, a ...any)
	// OnEvent 记事件（可为 nil）：ready、recovered、unhealthy、failed、given_up、stuck。detail 已脱敏。
	OnEvent func(kind, detail string)

	mu      sync.Mutex
	wanted  bool
	st      Status
	changed chan struct{} // 状态变化时关闭并换新（广播）
	ctl     chan struct{} // 想要的状态变了、或要求重试（容量 1，合并）
	retry   bool
	closed  bool
}

func (m *Manager) init() {
	if m.changed == nil {
		m.changed = make(chan struct{})
		m.ctl = make(chan struct{}, 1)
		m.st = Status{Managed: true, Phase: PhaseIdle}
	}
}

func (m *Manager) clock() Clock {
	if m.Clock == nil {
		return RealClock
	}
	return m.Clock
}

func (m *Manager) timing() Timing {
	if m.Timing == (Timing{}) {
		return DefaultTiming
	}
	return m.Timing
}

func (m *Manager) event(kind, detail string) {
	if m.OnEvent != nil {
		m.OnEvent(kind, detail)
	}
}

func (m *Manager) logf(format string, a ...any) {
	if m.Logf != nil {
		m.Logf(format, a...)
	}
}

// Status 返回当前状态。
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	return m.st
}

// update 在锁内改状态并广播。
func (m *Manager) update(f func(s *Status)) {
	m.mu.Lock()
	m.init()
	f(&m.st)
	close(m.changed)
	m.changed = make(chan struct{})
	m.mu.Unlock()
}

func (m *Manager) setPhase(p string) { m.update(func(s *Status) { s.Phase = p }) }

func (m *Manager) poke() {
	select {
	case m.ctl <- struct{}{}:
	default:
	}
}

// SetWanted 设置是否需要虚拟机，不等待。abroadd 启动后按状态文件调用一次（回国状态不需要）。
func (m *Manager) SetWanted(w bool) {
	m.mu.Lock()
	m.init()
	m.wanted = w
	if w && m.st.Phase == PhaseFailed {
		m.retry = true // 用户重新要虚拟机（启用）：计数清零重来
		// 同步离开 failed，免得紧接着的 Start 读到旧阶段就返回失败
		m.st.Phase = PhaseBackoff
		close(m.changed)
		m.changed = make(chan struct{})
	}
	m.mu.Unlock()
	m.poke()
}

func (m *Manager) isWanted() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.wanted
}

func (m *Manager) takeRetry() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.retry
	m.retry = false
	return r
}

// waitFor 等到 done 返回 true（在锁内判断），或 ctx 结束，或超过 limit。
func (m *Manager) waitFor(ctx context.Context, limit time.Duration, done func(s Status, closed bool) (bool, error)) error {
	var timeout <-chan time.Time
	if limit > 0 {
		timeout = m.clock().After(limit)
	}
	for {
		m.mu.Lock()
		m.init()
		ok, err := done(m.st, m.closed)
		ch := m.changed
		m.mu.Unlock()
		if ok {
			return err
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return ErrTimeout
		}
	}
}

// Ready 只读报告虚拟机当前是否健康（复查用；不改变“是否需要虚拟机”）。
func (m *Manager) Ready() error {
	if s := m.Status(); s.Phase != PhaseRunning {
		return fmt.Errorf("虚拟机当前阶段：%s", s.Phase)
	}
	return nil
}

// Start 实现 state.VM：需要虚拟机，等到它健康（启用第 ④ 步、地区同步之后）。
func (m *Manager) Start(ctx context.Context) error {
	m.SetWanted(true)
	err := m.waitFor(ctx, m.timing().StartWait, func(s Status, closed bool) (bool, error) {
		switch {
		case closed:
			return true, ErrClosed
		case s.Phase == PhaseRunning:
			return true, nil
		case s.Phase == PhaseFailed:
			return true, fmt.Errorf("%w：%s", ErrFailed, s.LastError)
		case s.Phase == PhaseStuck:
			return true, ErrStuck
		}
		return false, nil
	})
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrTimeout) || errors.Is(err, ErrFailed) || errors.Is(err, ErrStuck) {
		return usererror.New("启动虚拟机", "虚拟机没有就绪："+m.Status().LastError, "请在“引擎与虚拟机”页面看虚拟机的状态和最近的错误，处理后再试。", err)
	}
	return err
}

// Stop 实现 state.VM：不再需要虚拟机，等它退出（恢复第 ② 步、停用时关闭虚拟机）。
func (m *Manager) Stop(ctx context.Context) error {
	m.SetWanted(false)
	err := m.waitFor(ctx, 0, func(s Status, closed bool) (bool, error) {
		switch {
		case closed:
			return true, nil
		case s.Phase == PhaseIdle:
			return true, nil
		case s.Phase == PhaseStuck:
			return true, ErrStuck
		}
		return false, nil
	})
	if errors.Is(err, ErrStuck) {
		return usererror.New("关闭虚拟机", "虚拟机进程强制结束后仍没有退出", "请稍后再试；仍不行时重启电脑。", err)
	}
	return err
}

// Run 运行管理循环，直到 ctx 结束；结束时关闭自己拉起的虚拟机（先请它退出，超时强制结束）。
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	m.init()
	m.mu.Unlock()
	defer m.update(func(*Status) { m.closed = true })
	t := m.timing()
	launched := 0
	for {
		if ctx.Err() != nil {
			return
		}
		if !m.isWanted() {
			m.Gate.ClearReason(gatesm.ReasonVM) // 不需要虚拟机时由状态原因管闸门
			m.setPhase(PhaseIdle)
			select {
			case <-ctx.Done():
				return
			case <-m.ctl:
			}
			continue
		}
		m.takeRetry()
		m.update(func(s *Status) {
			s.Phase = PhaseStarting
			if launched > 0 {
				s.Restarts++
			}
		})
		launched++
		m.Gate.AddReason(gatesm.ReasonVM, "虚拟机启动中")
		p, err := m.Launcher.Launch()
		var why error
		if err != nil {
			why = fmt.Errorf("拉起 abroad-vm 失败：%w", err)
		} else {
			why = m.supervise(ctx, p, t)
			if why == nil || ctx.Err() != nil { // 不再需要，或 abroadd 要退出：关掉它
				if !m.stopProc(p, t) {
					m.stuck(ctx, p)
				}
				if ctx.Err() != nil {
					return
				}
				continue
			}
			if !m.stopProc(p, t) {
				m.stuck(ctx, p) // 等它最终退出，期间不拉起第二个
			}
		}
		if !m.fail(ctx, why) {
			continue // 到上限：fail 里已等到重试、不再需要或 ctx 结束
		}
		if !m.backoff(ctx) {
			return
		}
	}
}

// supervise 看着一个已拉起的进程。返回 nil 表示不再需要（或 ctx 结束）；返回错误表示这次失败了。
func (m *Manager) supervise(ctx context.Context, p Proc, t Timing) error {
	c := m.clock()
	deadline := c.Now().Add(t.StartTimeout)
	healthy := false
	fails := 0
	var healthySince, badSince time.Time
	for {
		select {
		case <-p.Done():
			if err := p.ExitErr(); err != nil {
				return fmt.Errorf("abroad-vm 意外退出：%w", err)
			}
			return errors.New("abroad-vm 意外退出")
		case <-ctx.Done():
			return nil
		case <-m.ctl:
			m.takeRetry()
			if !m.isWanted() {
				return nil
			}
			continue
		case <-c.After(t.ProbeEvery):
		}
		pctx, cancel := context.WithTimeout(ctx, t.ProbeTimeout)
		err := m.Health(pctx)
		cancel()
		now := c.Now()
		if err == nil {
			fails = 0
			if !healthy || !badSince.IsZero() {
				if !healthy {
					healthy, healthySince = true, now
					m.logf("虚拟机已就绪")
					m.event("ready", "")
				} else {
					m.logf("虚拟机恢复健康")
					m.event("recovered", "")
				}
				badSince = time.Time{}
				m.Gate.ClearReason(gatesm.ReasonVM)
				if m.Kick != nil {
					m.Kick.Kick() // 闸门不直接开：等出口复查通过
				}
				m.update(func(s *Status) { s.Phase = PhaseRunning }) // 最后才公布：看到 running 时原因已清、复查已请
			}
			if now.Sub(healthySince) >= t.StableFor && m.Status().Failures != 0 {
				m.update(func(s *Status) { s.Failures = 0 })
			}
			continue
		}
		if !healthy {
			if !now.Before(deadline) {
				return fmt.Errorf("拉起后 %s 内没有就绪：%v", t.StartTimeout, err)
			}
			continue
		}
		fails++
		if fails >= t.PatrolFailures && badSince.IsZero() {
			badSince = now
			healthySince = now // 稳定期从头算
			detail := "虚拟机健康检查连续不通过"
			m.logf("%s：%v", detail, redact.String(err.Error()))
			m.update(func(s *Status) { s.Phase = PhaseUnhealthy; s.LastError = redact.String(err.Error()) })
			m.Gate.AddReason(gatesm.ReasonVM, detail)
			m.event("unhealthy", redact.String(err.Error()))
		}
		if !badSince.IsZero() && now.Sub(badSince) >= t.UnhealthyKill {
			return fmt.Errorf("健康检查不通过超过 %s：%v", t.UnhealthyKill, err)
		}
	}
}

// stopProc 结束进程：先请它退出，等 StopWait；再强制结束，等 KillWait。返回是否已退出。
func (m *Manager) stopProc(p Proc, t Timing) bool {
	select {
	case <-p.Done():
		return true
	default:
	}
	m.setPhase(PhaseStopping)
	m.Gate.AddReason(gatesm.ReasonVM, "虚拟机正在关闭")
	c := m.clock()
	_ = p.Terminate()
	select {
	case <-p.Done():
		return true
	case <-c.After(t.StopWait):
	}
	m.logf("虚拟机 %s 内没有退出，强制结束", t.StopWait)
	_ = p.Kill()
	select {
	case <-p.Done():
		return true
	case <-c.After(t.KillWait):
	}
	return false
}

// stuck：进程强制结束后仍没退出。不再拉起第二个；等它最终退出（或 abroadd 退出）。
func (m *Manager) stuck(ctx context.Context, p Proc) {
	m.logf("%v，不再拉起新的虚拟机", ErrStuck)
	m.update(func(s *Status) { s.Phase = PhaseStuck; s.LastError = ErrStuck.Error() })
	m.event("stuck", ErrStuck.Error())
	m.Gate.AddReason(gatesm.ReasonVM, "虚拟机进程卡住")
	select {
	case <-p.Done():
		m.logf("卡住的虚拟机进程已退出")
	case <-ctx.Done():
	}
}

// fail 记一次失败。没到上限返回 true（接着退避）；到上限时进入 failed，等到重试、不再需要或 ctx 结束后返回 false。
func (m *Manager) fail(ctx context.Context, why error) bool {
	msg := redact.String(why.Error())
	m.logf("虚拟机失败：%s", msg)
	m.event("failed", msg)
	m.Gate.AddReason(gatesm.ReasonVM, "虚拟机没有运行")
	n := 0
	m.update(func(s *Status) {
		s.Failures++
		s.LastError = msg
		n = s.Failures
	})
	if n < m.timing().MaxFailures {
		return true
	}
	m.logf("虚拟机连续失败 %d 次，停止重启（保持断网，不自动回国）", n)
	m.setPhase(PhaseFailed)
	m.event("given_up", fmt.Sprintf("连续失败 %d 次", n))
	for {
		select {
		case <-ctx.Done():
			return false
		case <-m.ctl:
		}
		if m.takeRetry() || !m.isWanted() {
			m.update(func(s *Status) { s.Failures = 0 })
			return false
		}
	}
}

// backoff 按连续失败次数翻倍等待。返回 false 表示 ctx 结束。
func (m *Manager) backoff(ctx context.Context) bool {
	t := m.timing()
	n := m.Status().Failures
	d := t.BackoffMin
	for i := 1; i < n && d < t.BackoffMax; i++ {
		d *= 2
	}
	if d > t.BackoffMax {
		d = t.BackoffMax
	}
	m.setPhase(PhaseBackoff)
	select {
	case <-ctx.Done():
		return false
	case <-m.clock().After(d):
		return true
	case <-m.ctl:
		// 想要的状态变了：回到循环开头重新判断（不再需要就停在 idle；仍需要就立即拉起）
		return true
	}
}
