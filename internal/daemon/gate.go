// Package daemon 是 abroadd 的组装层（M2-9a）：启动与退出、事件缓冲、闸门、本地接口。
// cmd/abroadd 只解析参数和处理信号；所有依赖经 System 注入，测试直接构造。
package daemon

import (
	"sync/atomic"
	"time"

	"claude-abroad/internal/gatesm"
)

// Gate 是闸门（m4-plan M4-3）：带关闭原因和放行期的 gatesm.Gate 外面包一层，满足 state.Gate；
// stack.Gate、fakedns.Gate 只要只读的 Open() bool，经 View 提供。
type Gate struct {
	g *gatesm.Gate
	// onChange 在开关真的变化后调用（可为 nil；测试用来检查不变量）。复查循环在后台改闸门，所以用原子指针。
	onChange atomic.Pointer[func(open bool)]
	// hook 是 daemon 内部的钩子（数据面：由开变关时断开连接，M2-9b-2 决定 3），在 OnChange 之前调用。
	hook    func(open bool)
	onEvent func(kind, detail string)
}

func newGate(clock gatesm.Clock) *Gate {
	d := &Gate{}
	d.g = gatesm.New(gatesm.Options{Clock: clock, OnChange: d.changed, OnEvent: func(k, detail string) {
		if d.onEvent != nil {
			d.onEvent(k, detail)
		}
	}})
	return d
}

func (g *Gate) changed(open bool) {
	if g.hook != nil {
		g.hook(open)
	}
	if f := g.onChange.Load(); f != nil {
		(*f)(open)
	}
}

// SetOnChange 设置开关变化的回调（测试用；可在运行中设置）。
func (g *Gate) SetOnChange(f func(open bool)) { g.onChange.Store(&f) }

// AddReason、ClearReason、IsOpen 实现 state.Gate。
func (g *Gate) AddReason(r gatesm.Reason, detail string) { g.g.AddReason(r, detail) }
func (g *Gate) ClearReason(r gatesm.Reason)              { g.g.ClearReason(r) }
func (g *Gate) IsOpen() bool                             { return g.g.IsOpen() }

// BeginCheck、Grant 给复查用。
func (g *Gate) BeginCheck() uint64                           { return g.g.BeginCheck() }
func (g *Gate) Grant(epoch uint64, lease time.Duration) bool { return g.g.Grant(epoch, lease) }

// Status 是 status 里的闸门状态。
func (g *Gate) Status() gatesm.Status { return g.g.Status() }

// OnlyEnvCheck 给 M4-6 的探测放行用。
func (g *Gate) OnlyEnvCheck() bool { return g.g.OnlyEnvCheck() }

// View 是只读视图，满足 stack.Gate 和 fakedns.Gate。
func (g *Gate) View() GateView { return GateView{g} }

// GateView 的 Open 报告闸门是否开着。
type GateView struct{ g *Gate }

// Open 报告闸门是否开着。
func (v GateView) Open() bool { return v.g.IsOpen() }

// OpenProbe、CloseProbe、ProbeOpen 是探测窗口（M4-6a）。
func (g *Gate) OpenProbe(d time.Duration) bool { return g.g.OpenProbe(d) }
func (g *Gate) CloseProbe()                    { g.g.CloseProbe() }
func (g *Gate) ProbeOpen() bool                { return g.g.ProbeOpen() }

// ProbeOpen 报告探测窗口是否有效（满足 stack.ProbeGate）。
func (v GateView) ProbeOpen() bool { return v.g.ProbeOpen() }
