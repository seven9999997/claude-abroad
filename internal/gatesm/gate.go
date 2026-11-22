// Package gatesm 是闸门状态机（design 8，m4-plan M4-3 v2.2）：带关闭原因和放行期的闸门、按出口的可用状态、
// 出口复查循环。只取 Cage 的思路（epoch、租约），不搬它的出站和检查项。时钟全部可注入。
//
// 闸门打开 = 没有任何关闭原因 且 放行期有效。
//   - 关闭原因只有四种（Reason*）；各方只能清自己加的（AddReason/ClearReason 按原因名）。
//   - 加任何原因都会让当前的放行作废（epoch 加一、放行期清零）：之后要重新检查并放行，过期的检查结果不能放行。
//   - 放行期到期不是原因：到期时由闸门自己的定时器走关闭路径，记一条 lease_expired 事件；不依赖复查协程。
//   - 每一次由开变关，恰好调用一次 OnClose（断开走代理的连接）。
package gatesm

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Reason 是关闭原因。
type Reason string

const (
	ReasonTakeover  Reason = "takeover"   // 监护发现本机接管出了问题（6.1）
	ReasonExitCheck Reason = "exit_check" // 主代理的出口检查明确不通过，或“无法判断”超过 10 分钟
	ReasonEnvCheck  Reason = "env_check"  // 环境检测的硬性项明确不通过（8.1，M4-6）
	ReasonState     Reason = "state"      // 状态不是启用（未启用、停用、恢复、回滚）
	// ReasonMainSwitch 是正在更换主代理（M5-4，m5-plan v2.2 决定 2）：从关闸门开始，到新主代理的出口检查通过、
	// 或失败后回到旧配置为止；之后由复查决定是否放行。
	ReasonMainSwitch Reason = "main_proxy_switch"
)

// 事件种类。
const (
	EvLeaseExpired = "lease_expired" // 放行期到期没人续，闸门自己关了
	EvClosed       = "gate_closed"   // 加原因关了闸门（Detail 是原因）
	EvOpened       = "gate_opened"
)

// Clock 是可注入的时钟。
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Stopper
}

// Stopper 是定时器。
type Stopper interface{ Stop() bool }

type realClock struct{}

func (realClock) Now() time.Time                              { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) Stopper { return time.AfterFunc(d, f) }

// RealClock 是真实时钟。
var RealClock Clock = realClock{}

// Gate 是闸门。零值不可用，用 New。
type Gate struct {
	clock    Clock
	onClose  func()                    // 由开变关时调用（断开走代理的连接），在锁外
	onEvent  func(kind, detail string) // 可为 nil
	onChange func(open bool)           // 可为 nil（测试检查不变量）

	mu         sync.Mutex
	reasons    map[Reason]string
	leaseUntil time.Time
	open       bool
	epoch      uint64
	timer      Stopper
	probeUntil time.Time // 探测窗口到何时（零值表示关着）
}

// Options 是闸门的回调。
type Options struct {
	Clock    Clock
	OnClose  func()
	OnEvent  func(kind, detail string)
	OnChange func(open bool)
}

// New 返回一个关着的闸门，初始原因是 state（进程刚启动：状态机确认是 ENABLED 后才清）。
func New(o Options) *Gate {
	if o.Clock == nil {
		o.Clock = RealClock
	}
	return &Gate{clock: o.Clock, onClose: o.OnClose, onEvent: o.OnEvent, onChange: o.OnChange,
		reasons: map[Reason]string{ReasonState: "进程刚启动"}}
}

// transition 在锁外执行由开变关、由关变开的回调。
type transition struct {
	closed, opened bool
	kind, detail   string
}

func (g *Gate) fire(t transition) {
	if t.closed {
		if g.onClose != nil {
			g.onClose()
		}
		if g.onChange != nil {
			g.onChange(false)
		}
	}
	if t.opened && g.onChange != nil {
		g.onChange(true)
	}
	if t.kind != "" && g.onEvent != nil {
		g.onEvent(t.kind, t.detail)
	}
}

// AddReason 加一个关闭原因：闸门立即关闭，当前的放行作废。
func (g *Gate) AddReason(r Reason, detail string) {
	g.mu.Lock()
	_, had := g.reasons[r]
	g.reasons[r] = detail
	g.epoch++
	if r == ReasonTakeover || r == ReasonExitCheck {
		g.probeUntil = time.Time{} // 接管故障、出口检查不通过：探测窗口立即失效
	}
	g.leaseUntil = time.Time{}
	g.stopTimer()
	t := transition{}
	if g.open {
		g.open = false
		t = transition{closed: true, kind: EvClosed, detail: string(r) + "：" + detail}
	} else if !had && g.onEvent != nil {
		t = transition{kind: EvClosed, detail: string(r) + "：" + detail}
	}
	g.mu.Unlock()
	g.fire(t)
}

// ClearReason 清掉一个原因。不会因此打开：打开要等下一次放行（Grant）。
func (g *Gate) ClearReason(r Reason) {
	g.mu.Lock()
	delete(g.reasons, r)
	g.mu.Unlock()
}

// BeginCheck 在开始一轮检查前取序号；Grant 时带上它。期间加过原因（闸门被关过）就作废。
func (g *Gate) BeginCheck() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.epoch
}

// Grant 放行（或续期）lease：序号没过期、没有任何原因时才生效，返回是否生效。
func (g *Gate) Grant(epoch uint64, lease time.Duration) bool {
	g.mu.Lock()
	if epoch != g.epoch || len(g.reasons) > 0 || lease <= 0 {
		g.mu.Unlock()
		return false
	}
	now := g.clock.Now()
	g.leaseUntil = now.Add(lease)
	g.stopTimer()
	ep := epoch
	g.timer = g.clock.AfterFunc(lease, func() { g.expire(ep) })
	t := transition{}
	if !g.open {
		g.open = true
		t = transition{opened: true, kind: EvOpened}
	}
	g.mu.Unlock()
	g.fire(t)
	return true
}

func (g *Gate) stopTimer() {
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
}

// expire 是放行期定时器：到期且没被续期（期间没有新的放行）就走关闭路径。
func (g *Gate) expire(epoch uint64) {
	g.mu.Lock()
	if epoch != g.epoch || !g.open || g.clock.Now().Before(g.leaseUntil) {
		g.mu.Unlock()
		return
	}
	g.open = false
	g.epoch++
	g.leaseUntil = time.Time{}
	g.timer = nil
	g.mu.Unlock()
	g.fire(transition{closed: true, kind: EvLeaseExpired, detail: "放行期到期没有续期"})
}

// IsOpen 报告闸门是否开着：没有任何原因，且放行期没过。
func (g *Gate) IsOpen() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open && len(g.reasons) == 0 && g.clock.Now().Before(g.leaseUntil)
}

// OnlyEnvCheck 报告当前是否只剩 env_check 这一个原因（给 M4-6 的探测放行用）。
func (g *Gate) OnlyEnvCheck() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.reasons[ReasonEnvCheck]
	return ok && len(g.reasons) == 1
}

// Status 是闸门的状态（status 用）。
type Status struct {
	Open        bool     `json:"open"`
	Reasons     []string `json:"reasons"`
	LeaseLeftMs int64    `json:"lease_left_ms"`
}

// Status 返回当前状态（原因按名字排序）。
func (g *Gate) Status() Status {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clock.Now()
	st := Status{Open: g.open && len(g.reasons) == 0 && now.Before(g.leaseUntil), Reasons: []string{}}
	for r := range g.reasons {
		st.Reasons = append(st.Reasons, string(r))
	}
	sort.Strings(st.Reasons)
	if st.Open {
		st.LeaseLeftMs = g.leaseUntil.Sub(now).Milliseconds()
	}
	return st
}

// String 给日志用。
func (s Status) String() string {
	if s.Open {
		return "开"
	}
	return "关（" + strings.Join(s.Reasons, "、") + "）"
}

// ---- 探测窗口（m4-plan v2.6 决定 2）----

// DefaultProbeWindow 是探测窗口的默认时限。
const DefaultProbeWindow = 90 * time.Second

func (g *Gate) probeBlocked() bool {
	_, t := g.reasons[ReasonTakeover]
	_, x := g.reasons[ReasonExitCheck]
	return t || x
}

// OpenProbe 打开探测窗口（时限 d，0 用默认 90 秒）：窗口期间网络栈放行 abroadd 自己的探测（固定走主代理）。
// 有 takeover 或 exit_check 原因时打不开，返回 false。窗口不改变闸门的开关（IsOpen 照旧）。重复打开会延长到新的时限。
func (g *Gate) OpenProbe(d time.Duration) bool {
	if d <= 0 {
		d = DefaultProbeWindow
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.probeBlocked() {
		return false
	}
	g.probeUntil = g.clock.Now().Add(d)
	return true
}

// CloseProbe 关闭探测窗口（重复关闭是安全的）。
func (g *Gate) CloseProbe() {
	g.mu.Lock()
	g.probeUntil = time.Time{}
	g.mu.Unlock()
}

// ProbeOpen 报告探测窗口是否有效：打开了、没到时限、没有 takeover 和 exit_check 原因。
func (g *Gate) ProbeOpen() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.probeUntil.IsZero() && !g.probeBlocked() && g.clock.Now().Before(g.probeUntil)
}
