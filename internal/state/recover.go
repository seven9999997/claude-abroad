package state

import (
	"claude-abroad/internal/gatesm"
	"context"

	"claude-abroad/internal/usererror"
)

// Recover 是 abroadd 启动时按状态文件接着做（M2-7c 决定 4）。调用方必须持有 Store 的跨进程锁。
//   - 状态不是 RESTORED 时，第一件事是确保 pf 已加载；之后任何一步失败都不影响这一点（I3）；
//   - ENABLING：来源 RESTORED 走恢复的 ②–⑦（不结束 Claude）；来源 DISABLED 重建后写回 DISABLED；
//   - DISABLED：重建 utun、pf、路由、DNS，闸门保持关闭；
//   - ENABLED：同样重建；运行中的地区同步被中断时接着写完；清 state 原因，立即补一轮复查；检查和开闸门交给复查循环（m4-plan v2.2 决定 4）；
//   - RESTORING：先再结束一次 Claude（失败就停在 RESTORING），再从断点接着做；
//   - RESTORED：保留回国拦截时（M6-11a）加载那一组，否则什么都不做；
//   - 状态文件损坏：确保 pf 已加载，发事件，返回 ErrCorrupt（应急程序 abroad-rescue 处理，M6-5b）。
func (m *Machine) Recover(ctx context.Context) error {
	if !m.Store.HoldsLock() {
		return notLocked("恢复上次的状态")
	}
	if err := m.lock(); err != nil {
		return err
	}
	defer m.unlock()
	f, err := m.Store.Load()
	if err != nil {
		m.notify(false)
		m.emit(EvCorrupt, "状态文件损坏，按“未恢复”加载断网保护")
		if aerr := m.D.PF.Apply(ctx, m.D.Tun.Name()); aerr != nil {
			return usererror.Wrap("加载断网保护", aerr, "状态文件也已损坏。"+RescueHint+"。")
		}
		return err
	}
	if f.State == Restored {
		// 保留回国拦截时（M6-11a）：把那一组加载回来（重启后锚点是空的）。失败只记事件：状态已经是 RESTORED，
		// 巡检之后每轮再试。
		if m.returnBlock() {
			if err := m.D.PF.ApplyRestored(ctx); err != nil {
				m.emit(EvReturnBlockFailed, "回国拦截没能加载，稍后再试")
			}
		}
		return nil
	}
	// I3：先确保 pf 已加载（闸门本来就是关的：进程刚启动）。
	// 数据面先按“停用”判定；文件状态是 ENABLED 时下面再通知“启用”（与文件状态一致）。
	m.notify(false)
	m.D.Gate.AddReason(gatesm.ReasonState, "恢复上次的状态")
	if err := m.D.PF.Apply(ctx, m.D.Tun.Name()); err != nil {
		return usererror.Wrap("恢复时加载断网保护", err, "请重启 Claude 出国；仍不行请执行“恢复本机网络”。")
	}
	wrap := func(err error) error {
		return usererror.Wrap("恢复上次的状态", err, "断网保护仍然有效。请再试一次，或执行“恢复本机网络”。")
	}
	switch f.State {
	case Enabling:
		if f.Origin == Disabled {
			if _, err := m.rebuild(ctx, f.Step); err != nil {
				return wrap(err)
			}
			if err := m.Store.Update(func(f *File) error {
				f.State, f.Origin, f.Step = Disabled, "", 0
				return nil
			}); err != nil {
				return wrap(err)
			}
			return nil
		}
		cctx, cancel := detached(ctx)
		defer cancel()
		if err := m.restoreSteps(cctx); err != nil {
			return wrap(err)
		}
	case Disabled:
		if _, err := m.rebuild(ctx, f.Step); err != nil {
			return wrap(err)
		}
	case Enabled:
		m.notify(true) // 文件状态是 ENABLED：即使下面重建失败、闸门关着，也按启用判定（只断走代理的，直连照常）
		if _, err := m.rebuild(ctx, f.Step); err != nil {
			return wrap(err)
		}
		if f.Region.Syncing { // 运行中的地区同步被中断：接着写完（不再结束 Claude）
			if err := m.resumeSync(ctx); err != nil {
				return wrap(err)
			}
		}
		if m.D.Env != nil { // 进程重启后环境检测必须重新明确通过（m4-plan v2.6 决定 4）
			m.D.Gate.AddReason(gatesm.ReasonEnvCheck, "进程重启后等环境检测重新通过")
			m.D.Env.Kick()
		}
		m.D.Gate.ClearReason(gatesm.ReasonState)
		m.D.Recheck.Kick() // 检查（含确认虚拟机在）和开闸门都由复查循环做
	case Restoring:
		if err := m.D.Killer.KillClaude(ctx); err != nil {
			return usererror.Wrap("结束 Claude 进程", err, "断网保护仍然有效。请手动退出 Claude 后执行“恢复本机网络”。")
		}
		cctx, cancel := detached(ctx)
		defer cancel()
		if err := m.restoreSteps(cctx); err != nil {
			return wrap(err)
		}
	}
	return nil
}
