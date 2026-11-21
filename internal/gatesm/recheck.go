package gatesm

import (
	"context"
	"sort"
	"sync"
	"time"
)

// 复查的参数（design 8）。
const (
	DefaultInterval     = 60 * time.Second
	DefaultLease        = 90 * time.Second
	DefaultTotal        = 20 * time.Second // 一轮复查的总时限（小于放行期减间隔的余量）
	DefaultUnknownGrace = 10 * time.Minute
	EnableReuseWindow   = 30 * time.Second // 启用第 ⑤ 步的通过结果在这之内，第 ⑧ 步直接用它放行
)

// EvCheckPanic 是检查函数 panic 了：这一轮按“无法判断”处理，循环继续（不让整个 abroadd 退出）。
const EvCheckPanic = "recheck_panic"

// Result 是一轮出口检查的结论。
type Result int

const (
	Pass Result = iota + 1
	Fail
	Unknown
)

// String 给 status：pass、fail、unknown。
func (r Result) String() string {
	switch r {
	case Pass:
		return "pass"
	case Fail:
		return "fail"
	case Unknown:
		return "unknown"
	}
	return ""
}

// CheckFunc 做一轮主代理的出口检查（daemon 组装：确保虚拟机在、再跑 exitcheck）。detail 不含地址。
type CheckFunc func(ctx context.Context) (Result, string)

// Rechecker 是出口复查循环：只在状态是启用时运行，每 Interval 一轮；Kick 立即补一轮。
//   - 通过：清 exit_check，放行 Lease；
//   - 不通过：加 exit_check；
//   - 无法判断：最近一次明确通过在 UnknownGrace 之内就续期，否则加 exit_check。
//     “最近一次通过”只在内存里：进程重启后（新的实例）必须重新明确通过一次。
type Rechecker struct {
	Gate         *Gate
	Check        CheckFunc
	Enabled      func() bool // 状态是不是启用（读状态文件）
	Clock        Clock
	Interval     time.Duration
	Lease        time.Duration
	Total        time.Duration
	UnknownGrace time.Duration
	OnEvent      func(kind, detail string)

	kickOnce sync.Once
	kick     chan struct{}
	runMu    sync.Mutex // 一次只跑一轮
	mu       sync.Mutex
	lastPass time.Time
	rounds   int
}

func (r *Rechecker) clock() Clock {
	if r.Clock != nil {
		return r.Clock
	}
	return RealClock
}

func d(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

func (r *Rechecker) kickCh() chan struct{} {
	r.kickOnce.Do(func() { r.kick = make(chan struct{}, 1) })
	return r.kick
}

// Kick 让 Run 立即补一轮（不阻塞）。
func (r *Rechecker) Kick() {
	select {
	case r.kickCh() <- struct{}{}:
	default:
	}
}

// AfterEnable 是启用第 ⑧ 步：第 ⑤ 步的通过结果不超过 EnableReuseWindow 时直接用它放行；超过就补一轮（异步）。
func (r *Rechecker) AfterEnable(passAt time.Time) {
	now := r.clock().Now()
	if !passAt.IsZero() && now.Sub(passAt) <= EnableReuseWindow && !passAt.After(now) {
		r.mu.Lock()
		r.lastPass = passAt
		r.mu.Unlock()
		r.Gate.ClearReason(ReasonExitCheck)
		r.Gate.Grant(r.Gate.BeginCheck(), d(r.Lease, DefaultLease))
		return
	}
	r.Kick()
}

// Run 按 Interval 循环（或被 Kick），直到 ctx 结束。不是启用状态时不发任何探测。
func (r *Rechecker) Run(ctx context.Context) {
	iv := d(r.Interval, DefaultInterval)
	kick := r.kickCh()
	for {
		t := r.clock().AfterFunc(iv, r.Kick)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-kick:
		}
		t.Stop()
		if r.Enabled != nil && !r.Enabled() {
			continue
		}
		r.RunOnce(ctx)
	}
}

// RunOnce 跑一轮复查（状态是否启用由调用方判断）。
func (r *Rechecker) RunOnce(ctx context.Context) Result {
	r.runMu.Lock()
	defer r.runMu.Unlock()
	epoch := r.Gate.BeginCheck()
	cctx, cancel := context.WithTimeout(ctx, d(r.Total, DefaultTotal))
	res, detail := r.safeCheck(cctx)
	timedOut := cctx.Err() != nil && ctx.Err() == nil
	cancel()
	if timedOut && res == Pass {
		res = Unknown // 超过总时限的“通过”不算数
	}
	now := r.clock().Now()
	r.mu.Lock()
	r.rounds++
	last := r.lastPass
	if res == Pass {
		r.lastPass = now
	}
	r.mu.Unlock()
	lease := d(r.Lease, DefaultLease)
	switch res {
	case Pass:
		r.Gate.ClearReason(ReasonExitCheck)
		r.Gate.Grant(epoch, lease)
	case Fail:
		r.Gate.AddReason(ReasonExitCheck, "出口检查不通过："+detail)
	default:
		if !last.IsZero() && now.Sub(last) < d(r.UnknownGrace, DefaultUnknownGrace) {
			r.Gate.Grant(epoch, lease) // 沿用上一次的结论续期
		} else {
			r.Gate.AddReason(ReasonExitCheck, "出口检查无法判断超过 10 分钟："+detail)
		}
	}
	return res
}

// safeCheck 调检查函数；panic 时记事件、按“无法判断”处理。事件内容是固定文案，不带 panic 的值（可能含地址）。
func (r *Rechecker) safeCheck(ctx context.Context) (res Result, detail string) {
	defer func() {
		if p := recover(); p != nil {
			res, detail = Unknown, "检查函数异常退出"
			if r.OnEvent != nil {
				r.OnEvent(EvCheckPanic, "出口检查异常退出，这一轮按无法判断处理")
			}
		}
	}()
	return r.Check(ctx)
}

// Rounds 返回已跑的轮数（测试用）。
func (r *Rechecker) Rounds() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rounds
}

// ---- 按出口的可用状态 ----

// ExitAvail 是分流出口的可用状态（主代理不在这里：它不通过时整个闸门关）。M4-8，m4-plan v2.12 决定 4：
//   - 没登记过的出口不可用（默认值是“不可用”）；
//   - 正式的出口：可用 = 已登记、下发成功、agent 报告引擎可用、最近的检查结论是通过（或在“无法判断”的
//     沿用期内且此前有过明确通过），并且不在“开流明确失败后的 DownFor”里——第一次明确通过之前不可用；
//   - 开发用的多出口（只能经 SetDev 登记，正式的出口接口打不上）：沿用 M3 的“除非开流明确失败，否则可用”。
//
// UDP：正式的出口只有最近一次 UDP 探测明确通过时才让 UDP 走它，否则只把 UDP 回退主代理（TCP 照走它）。
type ExitAvail struct {
	Clock        Clock
	DownFor      time.Duration // 默认 30 秒
	UnknownGrace time.Duration // 默认 10 分钟

	mu   sync.Mutex
	down map[string]time.Time // MarkDown：不可用到何时
	ex   map[string]*exitState
}

type exitState struct {
	dev      bool
	pushed   bool
	pushErr  string
	agentUp  bool
	agentWhy string
	checked  bool
	last     Result
	lastAt   time.Time
	hadPass  bool
	lastPass time.Time
	udp      Result
	items    map[string]string // 最近一次检查的各项结论（不含出口 IP）
}

// DefaultExitDownFor 是开流回出口不存在或不可用后按不可用处理的时长。
const DefaultExitDownFor = 30 * time.Second

func (a *ExitAvail) now() time.Time {
	if a.Clock != nil {
		return a.Clock.Now()
	}
	return time.Now()
}

func (a *ExitAvail) st(exit string) *exitState {
	if a.ex == nil {
		a.ex = map[string]*exitState{}
	}
	s := a.ex[exit]
	if s == nil {
		s = &exitState{}
		a.ex[exit] = s
	}
	return s
}

// Configure 登记正式的出口集合：不在集合里的正式出口连同状态一起去掉（开发用的不动）；新出口从“不可用”开始。
func (a *ExitAvail) Configure(tags []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	keep := map[string]bool{}
	for _, t := range tags {
		keep[t] = true
		a.st(t)
	}
	for t, s := range a.ex {
		if !s.dev && !keep[t] {
			delete(a.ex, t)
			delete(a.down, t)
		}
	}
}

// SetDev 登记开发用的多出口（只给开发接口用）。
func (a *ExitAvail) SetDev(exit string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.st(exit)
	*s = exitState{dev: true}
}

// SetPushed 记下下发给虚拟机是否成功（失败原因不含配置内容）。
func (a *ExitAvail) SetPushed(exit string, ok bool, why string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s, found := a.ex[exit]; found && !s.dev {
		s.pushed, s.pushErr = ok, why
		if !ok {
			s.agentUp, s.agentWhy = false, ""
		}
	}
}

// SetAgent 记下 agent 报告的引擎状态。
func (a *ExitAvail) SetAgent(exit string, up bool, why string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s, found := a.ex[exit]; found && !s.dev {
		s.agentUp, s.agentWhy = up, why
	}
}

// MarkDown 记下开流明确回了出口不存在或不可用。
func (a *ExitAvail) MarkDown(exit string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.down == nil {
		a.down = map[string]time.Time{}
	}
	a.down[exit] = a.now().Add(d(a.DownFor, DefaultExitDownFor))
}

// Update 记下一个正式出口的检查结果：r 是总判定（HTTPS、出口 IP、大陆段、DoH），udp 是 UDP 探测，items 是各项结论。
func (a *ExitAvail) Update(exit string, r, udp Result, items map[string]string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, found := a.ex[exit]
	if !found || s.dev {
		return
	}
	now := a.now()
	s.checked, s.last, s.lastAt, s.udp, s.items = true, r, now, udp, items
	if r == Pass {
		s.hadPass, s.lastPass = true, now
	}
}

func (a *ExitAvail) downNow(exit string, now time.Time) bool {
	if u, ok := a.down[exit]; ok {
		if now.Before(u) {
			return true
		}
		delete(a.down, exit)
	}
	return false
}

// why 返回不可用的原因（可用时空串）。调用方持锁。
func (a *ExitAvail) why(exit string, now time.Time) string {
	s, found := a.ex[exit]
	if !found {
		return "没有这个出口"
	}
	if a.downNow(exit, now) {
		return "最近一次开流失败"
	}
	if s.dev {
		return ""
	}
	switch {
	case !s.pushed:
		if s.pushErr != "" {
			return "还没下发到虚拟机：" + s.pushErr
		}
		return "还没下发到虚拟机"
	case !s.agentUp:
		if s.agentWhy != "" {
			return "引擎不可用：" + s.agentWhy
		}
		return "引擎不可用"
	case !s.checked || !s.hadPass:
		if s.checked && s.last == Fail {
			return "出口检查没有通过"
		}
		return "还没有通过出口检查"
	case s.last == Fail:
		return "出口检查没有通过"
	case s.last == Unknown && now.Sub(s.lastPass) >= d(a.UnknownGrace, DefaultUnknownGrace):
		return "出口检查无法判断超过 10 分钟"
	}
	return ""
}

// Up 报告出口是否可用（给网络栈：不可用时规则回退主代理）。
func (a *ExitAvail) Up(exit string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.why(exit, a.now()) == ""
}

// UDPOK 报告 UDP 能不能走这个出口：开发用的照旧可以；正式的只有最近一次 UDP 探测明确通过才行。
func (a *ExitAvail) UDPOK(exit string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, found := a.ex[exit]
	if !found {
		return false
	}
	return s.dev || s.udp == Pass
}

// ExitView 是一个出口的可用状态（status、接口用；没有出口 IP、端点地址）。
type ExitView struct {
	Up        bool              `json:"up"`
	Reason    string            `json:"reason,omitempty"`
	Dev       bool              `json:"dev,omitempty"`
	Pushed    bool              `json:"pushed"`
	AgentUp   bool              `json:"agent_up"`
	Check     string            `json:"check,omitempty"` // pass、fail、unknown；没检查过时为空
	CheckedAt time.Time         `json:"checked_at,omitempty"`
	UDP       string            `json:"udp,omitempty"`
	Items     map[string]string `json:"items,omitempty"`
}

// View 返回一个出口的可用状态。
func (a *ExitAvail) View(exit string) ExitView {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	v := ExitView{Reason: a.why(exit, now)}
	v.Up = v.Reason == ""
	if s, found := a.ex[exit]; found {
		v.Dev, v.Pushed, v.AgentUp = s.dev, s.pushed, s.agentUp
		if s.checked {
			v.Check, v.CheckedAt, v.UDP = s.last.String(), s.lastAt, s.udp.String()
			v.Items = make(map[string]string, len(s.items))
			for k, x := range s.items {
				v.Items[k] = x
			}
		}
	}
	return v
}

// Snapshot 返回已登记出口的可用状态（status 用）。
func (a *ExitAvail) Snapshot() map[string]bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	keys := make([]string, 0, len(a.ex))
	for x := range a.ex {
		keys = append(keys, x)
	}
	sort.Strings(keys)
	out := map[string]bool{}
	for _, x := range keys {
		out[x] = a.why(x, now) == ""
	}
	return out
}
