package state

import (
	"claude-abroad/internal/gatesm"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync/atomic"
	"time"

	"claude-abroad/internal/pf"
	"claude-abroad/internal/sysnet"
	"claude-abroad/internal/usererror"
)

// 接管用的路由和 DNS（设计 4.1）。
var (
	TakeoverRoutes = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/1"), netip.MustParsePrefix("128.0.0.0/1")}
	TakeoverDNS    = netip.MustParseAddr("198.18.0.2")
)

// 步骤序号（写进状态文件的 step，执行前先写）。
const (
	StepEnabling = 1 // 写 ENABLING
	StepPFLoad   = 2 // 加载 pf（无 utun）
	StepTakeover = 3 // IPv6、utun、pf 重新应用、路由、DNS
	StepVM       = 4
	StepExit     = 5
	StepEnvCheck = 7 // 环境检测（M4-6a）；第 ⑥ 步的地区用 60–67
	StepEnabled  = 8

	StepRestoring = 1 // 写 RESTORING
	// StepPFClear 只由恢复的 ⑥ 写（启用的 ⑥⑦ 是空步骤，不写 6）。重入时看到它就直接从 ⑥ 接着做：
	// 锚点可能已经清了，不能再回头做 ②–⑤。
	StepPFClear = 6
)

// CleanupTimeout 是回滚、恢复（写入 RESTORING 之后）的独立时限：它们不跟着调用方的 ctx 一起取消，
// 否则调用方断开时会停在 ENABLING 或 RESTORING，用户一直断网。
const CleanupTimeout = 60 * time.Second

func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), CleanupTimeout)
}

// ErrBusy 是已经有一个操作在进行。
var ErrBusy = errors.New("已经有一个操作在进行")

// Machine 是设计第 10 节的状态机：启用、停用、恢复（M2-7b）。
//   - 每一步都可以重入：失败后再次调用同一操作，或调用 Restore；
//   - 同一时刻只允许一个操作，并发调用返回 ErrBusy；
//   - 敏感操作的本人授权属于 M5：调用方负责授权，本类型不做判断。
//
// 崩溃后的 Recover、开机的 Boot 在 M2-7c。
type Machine struct {
	Store   *Store
	D       Deps
	OnEvent func(Event) // 可为 nil
	// Now 是时钟（第 ⑤ 步的通过时刻和复查用同一个时钟；nil 用 time.Now）。
	Now func() time.Time

	busy atomic.Bool
}

func (m *Machine) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Machine) emit(kind EventKind, detail string) {
	if m.OnEvent != nil {
		m.OnEvent(Event{Kind: kind, Detail: detail})
	}
}

// notify 通知数据面的状态接收方（没有配置时什么都不做）。
func (m *Machine) notify(enabled bool) {
	if m.D.DataState != nil {
		m.D.DataState.SetEnabled(enabled)
	}
}

func (m *Machine) lock() error {
	if !m.busy.CompareAndSwap(false, true) {
		return usererror.New("执行操作", "已经有一个启用、停用或恢复在进行", "请等它完成后再试。", ErrBusy)
	}
	return nil
}

func (m *Machine) unlock() { m.busy.Store(false) }

// Exclusive 在状态机的忙标记下执行 fn（和启用、停用、恢复互斥）；忙时返回 ErrBusy（m3-plan v2.8 决定 3：改规则）。
func (m *Machine) Exclusive(fn func() error) error {
	if err := m.lock(); err != nil {
		return err
	}
	defer m.unlock()
	return fn()
}

// Enable 启用（设计 10 的启用，7b 决定 3、4）。调用方负责授权。
func (m *Machine) Enable(ctx context.Context) error {
	if err := m.lock(); err != nil {
		return err
	}
	defer m.unlock()
	f, err := m.Store.Load()
	if err != nil {
		return err
	}
	var origin State
	switch f.State {
	case Enabled:
		return nil
	case Restoring:
		return usererror.New("启用", "上一次恢复本机网络还没有完成", "请先再执行一次“恢复本机网络”。", nil)
	case Enabling:
		origin = f.Origin
		if origin == "" {
			origin = Restored
		}
	default:
		origin = f.State
	}
	// ⓪ 冲突检查：不通过就什么都不改。
	if err := m.D.Preflight.Check(ctx); err != nil {
		if usererror.Is(err) {
			return err // 检查器已经列好了每个阻断项和处理办法
		}
		return usererror.Wrap("启用前的冲突检查", err, "请按提示处理后再启用。")
	}
	// ⓪ 出口基准：没有或读不出来时拒绝启用，什么都不改（m4-plan v2.2 决定 6）。
	if m.D.Baseline != nil {
		if err := m.D.Baseline.Ready(); err != nil {
			return usererror.Wrap("启用", err, "还没有出口基准：请先建立出口基准（应用主代理），再启用。本机网络没有改动。")
		}
	}
	// ⓪ 地区：确定目标、读 Mac 此刻的设置；读不出来就不启用，什么都不改（m4-plan v2.4 决定 1）。
	plan, err := m.planRegion(ctx)
	if err != nil {
		return err
	}
	if err := m.enableSteps(ctx, origin, plan); err != nil {
		// ① 写 ENABLING 本身失败（文件还是原来的状态）：什么都没改，不回滚。
		// 目录 fsync 失败时文件可能已经是 ENABLING，照常回滚。
		if cur, lerr := m.Store.Load(); lerr == nil && cur.State != Enabling {
			return usererror.Wrap("启用", err, "本机网络没有改动，请再试一次。")
		}
		m.emit(EvRollback, "启用失败，回到 "+string(origin))
		cctx, cancel := detached(ctx)
		defer cancel()
		if rerr := m.rollback(cctx, origin); rerr != nil {
			m.emit(EvRollbackFailed, "回滚失败，状态留在 ENABLING，断网保护保持")
			return usererror.New("启用", fmt.Sprintf("%s；回滚也失败了（%s）", usererror.Reason(err), usererror.Reason(rerr)),
				"断网保护仍然有效。请再试一次启用，或执行“恢复本机网络”。", errors.Join(err, rerr))
		}
		next := "已回到启用前的状态，本机网络已恢复。请处理后再试。"
		if origin == Disabled {
			next = "已回到停用状态（断网保护仍然有效）。请处理后再试。"
		}
		return usererror.Wrap("启用", err, next)
	}
	return nil
}

func (m *Machine) setStep(step int) error {
	return m.Store.Update(func(f *File) error { f.Step = step; return nil })
}

// applyPF 写步骤后按 utun 应用 pf。“将要启用”标记和 token 由 PF 的实现经 Controller 钩子落盘（M2-7c 决定 2）。
func (m *Machine) applyPF(ctx context.Context, step int, utun string) error {
	if err := m.setStep(step); err != nil {
		return err
	}
	return m.D.PF.Apply(ctx, utun)
}

func ignoreExists(err error) error {
	if errors.Is(err, sysnet.ErrExists) {
		return nil
	}
	return err
}

// rebuild 建 utun（已有就复用，名字写回文件）、按它应用 pf、补路由和 DNS（已存在按成功）。
func (m *Machine) rebuild(ctx context.Context, step int) (string, error) {
	name := m.D.Tun.Name()
	if name == "" || !m.D.Tun.Alive() {
		if name != "" {
			// 死掉的 utun 仍报旧名字：先关掉再新建。
			if err := m.D.Tun.Close(ctx); err != nil {
				return "", err
			}
		}
		var err error
		if name, err = m.D.Tun.Open(ctx); err != nil {
			return "", err
		}
	}
	if err := m.Store.Update(func(f *File) error { f.Utun = name; return nil }); err != nil {
		return "", err
	}
	if err := m.applyPF(ctx, step, name); err != nil {
		return "", err
	}
	for _, dst := range TakeoverRoutes {
		if err := ignoreExists(m.D.Routes.Add(ctx, dst, name)); err != nil {
			return "", err
		}
	}
	if err := ignoreExists(m.D.DNS.Set(ctx, TakeoverDNS)); err != nil {
		return "", err
	}
	return name, nil
}

func (m *Machine) enableSteps(ctx context.Context, origin State, plan regionPlan) error {
	// ① 写 ENABLING（带来源）并落盘。全新启用（文件是 RESTORED）时，同时记下启用前的本机 DNS（只读，
	// 给直连拨号模块解析用，m3-plan v2.8 决定 2）；从 DISABLED 再启用、或从中断的 ENABLING 接着做时不重读
	// （那时系统 DNS 可能已经是我们设的 198.18.0.2）。读不出来不拦着启用：记为空、发事件（直连按域名会失败）。
	var origDNS []netip.Addr
	fresh := false
	if cur, err := m.Store.Load(); err == nil && cur.State == Restored {
		fresh = true
		if m.D.DNS != nil {
			d, err := m.D.DNS.System(ctx)
			if err != nil {
				m.emit(EvOrigDNSUnknown, "读不出启用前的本机 DNS，本机网络的域名连接会失败")
			}
			origDNS = d
		}
	}
	if err := m.Store.Update(func(f *File) error {
		f.State, f.Origin, f.Step = Enabling, origin, StepEnabling
		if fresh {
			f.OrigDNS = origDNS
		}
		// 地区原值：只在从 RESTORED 启用时记（以此刻为准：回国期间用户自己改过的以新的为准）；从 DISABLED 再启用时不重拍。
		if plan.on && (fresh || f.Region.Original == nil) {
			o := plan.cur
			if plan.pending != nil { // 上次回国没还原干净的项：原值以待还原文件为准（m4-plan v2.5）
				o = plan.pending.overlay(o)
			}
			f.Region.Original = &o
		}
		return nil
	}); err != nil {
		return err
	}
	// ② 加载 pf（此时开始断网）。从 DISABLED 来时 utun 还在，用它的名字。
	if err := m.applyPF(ctx, StepPFLoad, m.D.Tun.Name()); err != nil {
		return err
	}
	// ③ IPv6 快照（只拍一次）、逐个关 IPv6。
	if err := m.setStep(StepTakeover); err != nil {
		return err
	}
	cur, err := m.D.IPv6.Snapshot(ctx)
	if err != nil {
		return err
	}
	if _, err := m.Store.TakeIPv6Snapshot(cur); err != nil {
		return err
	}
	f, err := m.Store.Load()
	if err != nil {
		return err
	}
	for _, svc := range sortedKeys(f.IPv6Original) {
		switch f.IPv6Original[svc].Mode {
		case sysnet.IPv6Off:
			continue
		case sysnet.IPv6Unknown:
			m.emit(EvIPv6Unknown, svc+"：IPv6 设置没法原样还原，不关它（IPv6 出站仍被 pf 拦截）")
			continue
		}
		if err := m.D.IPv6.Off(ctx, svc); err != nil {
			if errors.Is(err, sysnet.ErrNotFound) {
				m.emit(EvServiceGone, svc)
				continue
			}
			return err
		}
	}
	// utun、按带 utun 的配置重新应用 pf、路由、DNS。
	if _, err := m.rebuild(ctx, StepTakeover); err != nil {
		return err
	}
	// ④ 虚拟机。
	if err := m.setStep(StepVM); err != nil {
		return err
	}
	if err := m.D.VM.Start(ctx); err != nil {
		return err
	}
	// ⑤ 出口检查：必须明确通过（无法判断按失败处理并回滚）。
	if err := m.setStep(StepExit); err != nil {
		return err
	}
	if err := m.D.Check.Check(ctx); err != nil {
		return err
	}
	// ⑥ 地区（M4-5）：同步后重新明确通过一次出口检查，“第 ⑤ 步的通过时间”以这一次为准。⑦ 在 M4-6 补上。
	if err := m.enableRegion(ctx, plan); err != nil {
		return err
	}
	// ⑦ 环境检测（M4-6a）：探测窗口里跑全部硬性项，明确通过才继续；不通过或重试后仍无法判断就回滚。
	if m.D.Env != nil {
		if err := m.setStep(StepEnvCheck); err != nil {
			return err
		}
		if err := m.D.Env.AtEnable(ctx); err != nil {
			return err
		}
	}
	passAt := m.now()
	// ⑧ 先写 ENABLED，再清 state 原因、由复查放行（I1：闸门打开时状态必须已经是 ENABLED）。
	if err := m.Store.Update(func(f *File) error {
		f.State, f.Origin, f.Step = Enabled, "", StepEnabled
		return nil
	}); err != nil {
		return err
	}
	if plan.pending != nil { // 待还原的原值已进状态文件：删掉待还原文件
		if err := m.Store.DeletePending(); err != nil {
			m.emit(EvRegionPendingCorrupt, "删除待还原文件失败")
		}
	}
	m.notify(true) // ENABLED 落盘之后、开闸门之前
	m.D.Gate.ClearReason(gatesm.ReasonState)
	m.D.Recheck.AfterEnable(passAt) // 第 ⑤ 步的结果 30 秒内直接放行，超过就补一轮（没通过也不回滚）
	return nil
}

// rollback 是启用失败后的回滚（7b 决定 4）。失败时状态留在 ENABLING，pf 保持加载。
func (m *Machine) rollback(ctx context.Context, origin State) error {
	m.notify(false)
	m.D.Gate.AddReason(gatesm.ReasonState, "启用失败，回滚")
	if err := m.D.PF.SaveUnsaved(); err != nil {
		return err
	}
	if origin == Disabled {
		return m.Store.Update(func(f *File) error {
			f.State, f.Origin, f.Step = Disabled, "", 0
			return nil
		})
	}
	return m.restoreSteps(ctx)
}

// Disable 停用（设计 10 的停用）：关闸门 → 断开走代理的连接 → 可选关虚拟机 → 写 DISABLED。调用方负责授权。
func (m *Machine) Disable(ctx context.Context, stopVM bool) error {
	if err := m.lock(); err != nil {
		return err
	}
	defer m.unlock()
	f, err := m.Store.Load()
	if err != nil {
		return err
	}
	switch f.State {
	case Disabled:
		return nil
	case Enabled:
	default:
		return usererror.New("停用", "当前不是启用状态", "请先启用，或执行“恢复本机网络”。", nil)
	}
	m.notify(false) // 关闸门之前：数据面立刻按停用判定，断开不在 offline_allow 里的直连
	m.D.Gate.AddReason(gatesm.ReasonState, "停用")
	m.D.Teardown.Teardown(ctx)
	if stopVM {
		if err := m.D.VM.Stop(ctx); err != nil {
			return usererror.Wrap("停用时关闭虚拟机", err, "闸门已经关闭，可以再试一次停用。")
		}
	}
	if err := m.Store.Update(func(f *File) error {
		f.State, f.Origin, f.Step = Disabled, "", 0
		return nil
	}); err != nil {
		return usererror.Wrap("停用", err, "闸门已经关闭，可以再试一次停用。")
	}
	return nil
}

// Restore 恢复本机网络（设计 10 的恢复，7b 决定 6、7）。调用方负责授权和二次确认。
func (m *Machine) Restore(ctx context.Context) error {
	if err := m.lock(); err != nil {
		return err
	}
	defer m.unlock()
	f, err := m.Store.Load()
	if err != nil {
		return err
	}
	if f.State == Restored {
		return nil
	}
	// ⓪ 结束 Claude 并确认已退出：失败就中止，什么都不改。
	if err := m.D.Killer.KillClaude(ctx); err != nil {
		if usererror.Is(err) { // 已经写好了原因和下一步（例如残留的数量）
			return err
		}
		return usererror.Wrap("结束 Claude 进程", err, "本机网络没有改动。请手动退出 Claude 后再试。")
	}
	// I1：闸门开时状态必须是 ENABLED，所以写 RESTORING 之前先关闸门。
	m.notify(false)
	m.D.Gate.AddReason(gatesm.ReasonState, "恢复本机网络")
	ctx, cancel := detached(ctx)
	defer cancel()
	// ① 写 RESTORING（已经是 RESTORING 时不重写，保留步骤序号，从断点接着做）。
	if f.State != Restoring {
		if err := m.Store.Update(func(f *File) error {
			f.State, f.Step = Restoring, StepRestoring
			return nil
		}); err != nil {
			return usererror.Wrap("恢复本机网络", err, "断网保护仍然有效，请再试一次。")
		}
	}
	if err := m.restoreSteps(ctx); err != nil {
		return usererror.Wrap("恢复本机网络", err, "断网保护仍然有效，请再试一次。")
	}
	return nil
}

func ignoreNotFound(err error) error {
	if errors.Is(err, sysnet.ErrNotFound) {
		return nil
	}
	return err
}

// restoreSteps 是恢复的 ②–⑦（启用从 RESTORED 来的回滚也走这里）。pf 最后清除，RESTORED 最后写。
func (m *Machine) restoreSteps(ctx context.Context) error {
	if err := m.D.PF.SaveUnsaved(); err != nil {
		return err
	}
	f, err := m.Store.Load()
	if err != nil {
		return err
	}
	if f.Step != StepPFClear {
		if err := m.undoTakeover(ctx); err != nil {
			return err
		}
	}
	return m.clearPF(ctx)
}

// undoTakeover 是恢复的 ②–⑤。
func (m *Machine) undoTakeover(ctx context.Context) error {
	// ② 关虚拟机。
	if err := m.setStep(2); err != nil {
		return err
	}
	if err := m.D.VM.Stop(ctx); err != nil {
		return err
	}
	// ③ 删 DNS、路由、utun。
	if err := m.setStep(3); err != nil {
		return err
	}
	if err := ignoreNotFound(m.D.DNS.Remove(ctx)); err != nil {
		return err
	}
	for _, dst := range TakeoverRoutes {
		if err := ignoreNotFound(m.D.Routes.Delete(ctx, dst)); err != nil {
			return err
		}
	}
	if err := m.D.Tun.Close(ctx); err != nil {
		return err
	}
	if err := m.Store.Update(func(f *File) error { f.Utun = ""; f.Step = 4; return nil }); err != nil {
		return err
	}
	// ④ 按快照还原 IPv6。快照里已消失的网络服务跳过并发事件。
	f, err := m.Store.Load()
	if err != nil {
		return err
	}
	for _, svc := range sortedKeys(f.IPv6Original) {
		orig := f.IPv6Original[svc]
		if orig.Mode == sysnet.IPv6Off || orig.Mode == sysnet.IPv6Unknown {
			continue // 启用时没动过它
		}
		if err := m.D.IPv6.Restore(ctx, svc, orig); err != nil {
			if errors.Is(err, sysnet.ErrNotFound) {
				m.emit(EvServiceGone, svc)
				continue
			}
			return err
		}
	}
	// ⑤ 还原地区（M4-5）。
	return m.restoreRegion(ctx)
}

// clearPF 是恢复的 ⑥⑦。
func (m *Machine) clearPF(ctx context.Context) error {
	// ⑥ 清空锚点，释放全部 token（每释放一个就从文件里去掉，重入时不重复释放）。
	if err := m.setStep(StepPFClear); err != nil {
		return err
	}
	if err := m.D.PF.Flush(ctx); err != nil {
		return err
	}
	// 开机时 guard 取得的启用引用（M6-2b）：先收养进状态文件，下面一起释放（回国干净）。
	if ad, ok := m.D.PF.(interface{ AdoptExternal() error }); ok {
		_ = ad.AdoptExternal()
		_ = m.D.PF.SaveUnsaved()
	}
	f, err := m.Store.Load()
	if err != nil {
		return err
	}
	if f.PFEnablePending {
		// 可能漏了一个启用引用：不执行 pfctl -d（会关掉别的软件依赖的 pf）。
		m.emit(EvTokenUnknown, "可能漏了一个 pf 启用引用；pf 可能保持启用但我们的锚点为空，无害，重启后消失")
	}
	// 锚点已清空：释放是尽力而为（设计 6.2）。失败只记事件、从文件里去掉，不能卡在 RESTORING
	// （否则每次开机都按“非 RESTORED”加载 pf）。
	for _, tok := range f.PFTokens {
		if err := m.D.PF.Release(ctx, tok); err != nil {
			if errors.Is(err, pf.ErrTokenInvalid) {
				m.emit(EvTokenReleaseFailed, "启用引用已失效（可能别的软件关过 pf）")
			} else {
				m.emit(EvTokenReleaseFailed, "释放启用引用失败；pf 可能保持启用但我们的锚点为空，无害，重启后消失")
			}
		}
		if err := m.Store.Update(func(f *File) error {
			f.PFTokens = slices.DeleteFunc(f.PFTokens, func(t string) bool { return t == tok })
			return nil
		}); err != nil {
			return err
		}
	}
	// ⑦ 写 RESTORED（清掉快照，下一个周期可以重拍）。
	return m.Store.MarkRestored()
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
