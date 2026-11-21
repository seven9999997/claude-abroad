package state

import (
	"context"
	"errors"

	"claude-abroad/internal/region"
	"claude-abroad/internal/sysnet"
	"claude-abroad/internal/usererror"
)

// 启用第 ⑥ 步：地区同步（m4-plan v2.4 决定 2、3）。每一步之前先落盘步骤号：
// 记目标 → 结束 Claude（地区会变时）→ 虚拟机 → Mac 时区 → 关自动时区 → 用户的语言和地区格式 → 系统级的语言
// → 等虚拟机恢复并重新通过一次出口检查。
const (
	StepRegionTarget  = 60
	StepRegionKill    = 61
	StepRegionVM      = 62
	StepRegionTZ      = 63
	StepRegionAuto    = 64
	StepRegionUser    = 65
	StepRegionSys     = 66
	StepRegionRecheck = 67
	// 恢复第 ⑤ 步（在 RESTORING 里）。
	StepRestoreRegion = 5
)

// regionOn 报告地区同步的依赖是否齐全。
func (m *Machine) regionOn() bool {
	return m.D.Locale != nil && m.D.VMRegion != nil && m.D.Targets != nil
}

// regionPlan 是启用第 ⓪ 步读到的：目标和 Mac 此刻的设置（读不出来就不启用：没有原值就没法保证回国时还原）。
type regionPlan struct {
	on      bool
	target  region.Target
	cur     sysnet.MacLocale
	pending *RegionPending // 上次回国没还原干净的项（只在从 RESTORED 启用时用）
}

func (m *Machine) planRegion(ctx context.Context) (regionPlan, error) {
	if !m.regionOn() {
		return regionPlan{}, nil
	}
	t, ok, err := m.D.Targets.Target(ctx)
	if err != nil {
		return regionPlan{}, usererror.Wrap("确定目标地区", err, "本机没有改动。请先应用主代理或在“切换地区”里手动指定。")
	}
	if !ok {
		return regionPlan{}, nil
	}
	cur, err := m.D.Locale.Read(ctx)
	if err != nil {
		return regionPlan{}, usererror.Wrap("读取 Mac 当前的时区和语言", err, "本机没有改动，也没有结束任何进程。没有原值就没法保证回国时还原，请处理后再试。")
	}
	plan := regionPlan{on: true, target: t, cur: cur}
	if f, err := m.Store.Load(); err == nil && f.State == Restored {
		p, ok, err := m.Store.LoadPending()
		switch {
		case err != nil:
			m.emit(EvRegionPendingCorrupt, "待还原文件读不出来，已忽略")
		case ok:
			plan.pending = &p
		}
	}
	return plan, nil
}

// writeMac 把 Mac 从 cur 写到 want（只写不同的项；用户级在没人登录、换了人时跳过并发事件）。
// step 为真时每一项之前先落盘步骤号（启用第 ⑥ 步）。
func (m *Machine) writeMac(ctx context.Context, cur, want sysnet.MacLocale, step bool) error {
	set := func(n int) error {
		if !step {
			return nil
		}
		return m.setStep(n)
	}
	if err := set(StepRegionTZ); err != nil {
		return err
	}
	if cur.Timezone != want.Timezone {
		if err := m.D.Locale.SetTimezone(ctx, want.Timezone); err != nil {
			return err
		}
	}
	if err := set(StepRegionAuto); err != nil {
		return err
	}
	if !cur.AutoTZ.Equal(want.AutoTZ) {
		if err := m.D.Locale.SetAutoTZ(ctx, want.AutoTZ); err != nil {
			return err
		}
	}
	if err := set(StepRegionUser); err != nil {
		return err
	}
	if want.User != "" && (cur.User != want.User || !cur.UserLangs.Equal(want.UserLangs) || !cur.UserLocale.Equal(want.UserLocale)) {
		err := m.D.Locale.SetUser(ctx, want.User, want.UserLangs, want.UserLocale)
		switch {
		case errors.Is(err, sysnet.ErrNoConsoleUser), errors.Is(err, sysnet.ErrConsoleChanged):
			m.emit(EvRegionUserSkipped, "没有人登录或登录的用户换了：跳过用户的语言和地区格式")
		case err != nil:
			return err
		}
	} else if want.User == "" {
		m.emit(EvRegionUserSkipped, "没有人登录：跳过用户的语言和地区格式")
	}
	if err := set(StepRegionSys); err != nil {
		return err
	}
	if !cur.SysLangs.Equal(want.SysLangs) {
		if err := m.D.Locale.SetSystem(ctx, want.SysLangs); err != nil {
			return err
		}
	}
	return nil
}

// enableRegion 是启用第 ⑥ 步。返回是否同步过（同步过的要重新检查出口）。
func (m *Machine) enableRegion(ctx context.Context, p regionPlan) error {
	if !p.on {
		return nil
	}
	t := p.target
	if err := m.Store.Update(func(f *File) error {
		f.Step = StepRegionTarget
		f.Region.Target = &t
		return nil
	}); err != nil {
		return err
	}
	f, err := m.Store.Load()
	if err != nil {
		return err
	}
	if f.Region.Original == nil {
		return errors.New("状态文件里没有地区原值")
	}
	if err := m.setStep(StepRegionKill); err != nil {
		return err
	}
	if region.Changes(p.cur, t) {
		if err := m.D.Killer.KillClaude(ctx); err != nil {
			return err
		}
	}
	if err := m.setStep(StepRegionVM); err != nil {
		return err
	}
	if err := m.D.VMRegion.SetRegion(ctx, t); err != nil {
		return err
	}
	if err := m.writeMac(ctx, p.cur, region.Desired(*f.Region.Original, t), true); err != nil {
		return err
	}
	m.emit(EvRegionSynced, t.String())
	// 虚拟机里改完地区，引擎会重启：等它恢复并重新明确通过一次出口检查（m4-plan v2.4 决定 3）。
	if err := m.setStep(StepRegionRecheck); err != nil {
		return err
	}
	if err := m.D.VM.Start(ctx); err != nil {
		return err
	}
	return m.D.Check.Check(ctx)
}

// restoreRegion 是恢复第 ⑤ 步：按原值逐项写回（顺序与写入相反：系统级语言 → 用户 → 自动时区 → 时区）。
// 只在本周期写过目标时做；读一次当前值，只写不同的项；可重入。
// 尽力而为（m4-plan v2.5 必须改 1）：读当前值失败时按“每一项都要写”处理；某一项写失败（含用户换了、没人登录）
// 就记进待还原文件，继续下一项，不返回错误——不能因为时区、语言把用户卡在断网状态。
func (m *Machine) restoreRegion(ctx context.Context) error {
	f, err := m.Store.Load()
	if err != nil {
		return err
	}
	if f.Region.Target == nil || f.Region.Original == nil || m.D.Locale == nil {
		return nil
	}
	if err := m.setStep(StepRestoreRegion); err != nil {
		return err
	}
	o := *f.Region.Original
	cur, rerr := m.D.Locale.Read(ctx)
	unknown := rerr != nil
	p := RegionPending{}
	if unknown || !cur.SysLangs.Equal(o.SysLangs) {
		if err := m.D.Locale.SetSystem(ctx, o.SysLangs); err != nil {
			sl := o.SysLangs
			p.Items, p.SysLangs = append(p.Items, ItemSystem), &sl
		}
	}
	if o.User != "" && (unknown || cur.User != o.User || !cur.UserLangs.Equal(o.UserLangs) || !cur.UserLocale.Equal(o.UserLocale)) {
		if err := m.D.Locale.SetUser(ctx, o.User, o.UserLangs, o.UserLocale); err != nil {
			ul, uc := o.UserLangs, o.UserLocale
			p.Items, p.User, p.UserLangs, p.UserLocale = append(p.Items, ItemUser), o.User, &ul, &uc
			if errors.Is(err, sysnet.ErrNoConsoleUser) || errors.Is(err, sysnet.ErrConsoleChanged) {
				m.emit(EvRegionUserSkipped, "没有人登录或登录的用户换了：用户的语言和地区格式等原来的用户登录后再写回")
			}
		}
	}
	if unknown || !cur.AutoTZ.Equal(o.AutoTZ) {
		if err := m.D.Locale.SetAutoTZ(ctx, o.AutoTZ); err != nil {
			a := o.AutoTZ
			p.Items, p.AutoTZ = append(p.Items, ItemAutoTZ), &a
		}
	}
	if unknown || cur.Timezone != o.Timezone {
		if err := m.D.Locale.SetTimezone(ctx, o.Timezone); err != nil {
			p.Items, p.Timezone = append(p.Items, ItemTimezone), o.Timezone
		}
	}
	if len(p.Items) == 0 {
		// 这一轮全部写回了：上一次中断前留下的待还原文件（或上一周期没来得及删的）已经没有意义。
		if _, ok, _ := m.Store.LoadPending(); ok {
			if err := m.Store.DeletePending(); err == nil {
				m.emit(EvRegionPendingCleared, "时区和语言已全部还原")
			}
		}
		m.emit(EvRegionRestored, "地区已写回原值")
		return nil
	}
	if err := m.Store.SavePending(p); err != nil {
		m.emit(EvRegionPending, "时区或语言有几项没能还原（"+p.Labels()+"），待还原的原值也没能保存：请手动检查")
		return nil
	}
	m.emit(EvRegionPending, "网络会照常恢复，但时区或语言有几项没能还原："+p.Labels()+"。会在后台重试")
	return nil
}

// RetryRegionPending 重试待还原的项（只在 RESTORED 下做；daemon 启动后和每 5 分钟调用）。全部成功就删掉文件并发事件；
// 部分成功就把剩下的写回文件。没有文件、文件损坏（发事件）、状态不是 RESTORED、有操作在进行时什么都不做。
func (m *Machine) RetryRegionPending(ctx context.Context) error {
	if m.D.Locale == nil {
		return nil
	}
	if err := m.lock(); err != nil {
		return nil // 有操作在进行：下一轮再试
	}
	defer m.unlock()
	f, err := m.Store.Load()
	if err != nil || f.State != Restored {
		return nil
	}
	p, ok, err := m.Store.LoadPending()
	if err != nil {
		m.emit(EvRegionPendingCorrupt, "待还原文件读不出来，已忽略")
		return nil
	}
	if !ok {
		return nil
	}
	left := RegionPending{}
	if p.has(ItemSystem) {
		if m.D.Locale.SetSystem(ctx, *p.SysLangs) != nil {
			left.Items, left.SysLangs = append(left.Items, ItemSystem), p.SysLangs
		}
	}
	if p.has(ItemUser) {
		if m.D.Locale.SetUser(ctx, p.User, *p.UserLangs, *p.UserLocale) != nil {
			left.Items, left.User, left.UserLangs, left.UserLocale = append(left.Items, ItemUser), p.User, p.UserLangs, p.UserLocale
		}
	}
	if p.has(ItemAutoTZ) {
		if m.D.Locale.SetAutoTZ(ctx, *p.AutoTZ) != nil {
			left.Items, left.AutoTZ = append(left.Items, ItemAutoTZ), p.AutoTZ
		}
	}
	if p.has(ItemTimezone) {
		if m.D.Locale.SetTimezone(ctx, p.Timezone) != nil {
			left.Items, left.Timezone = append(left.Items, ItemTimezone), p.Timezone
		}
	}
	if len(left.Items) == 0 {
		if err := m.Store.DeletePending(); err != nil {
			return err
		}
		m.emit(EvRegionPendingCleared, "时区和语言已全部还原")
		return nil
	}
	if len(left.Items) < len(p.Items) {
		return m.Store.SavePending(left)
	}
	return nil
}

// PendingItems 返回待还原的项（status 用；只有项目名）。文件损坏时返回 nil。
func (m *Machine) PendingItems() []string {
	p, ok, err := m.Store.LoadPending()
	if err != nil || !ok {
		return nil
	}
	return p.Items
}

// ErrNotEnabled 是运行中的地区同步只在 ENABLED 下做。
var ErrNotEnabled = errors.New("状态不是启用")

// SyncRegion 是 ENABLED 下的地区同步（应用主代理、切换地区；m4-plan v2.4 决定 4、5）：
// 记目标并标记同步中 → 地区会变时结束 Claude（结束不了就中止：目标、地区、网络都不变）→ 虚拟机 → Mac → 清标记 → 补一轮复查。
// 状态不是 ENABLED 时返回 ErrNotEnabled（调用方只保存目标，等启用时再同步）。调用方负责两步确认。
func (m *Machine) SyncRegion(ctx context.Context, t region.Target) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if !m.regionOn() {
		return errors.New("没有地区同步的依赖")
	}
	if err := m.lock(); err != nil {
		return err
	}
	defer m.unlock()
	f, err := m.Store.Load()
	if err != nil {
		return err
	}
	if f.State != Enabled {
		return ErrNotEnabled
	}
	cur, err := m.D.Locale.Read(ctx)
	if err != nil {
		return usererror.Wrap("读取 Mac 当前的时区和语言", err, "地区没有改动。")
	}
	if region.Changes(cur, t) {
		if err := m.D.Killer.KillClaude(ctx); err != nil {
			if usererror.Is(err) {
				return err
			}
			return usererror.Wrap("结束 Claude 进程", err, "地区没有改动。请手动退出 Claude 后再试。")
		}
	}
	if err := m.Store.Update(func(f *File) error {
		f.Region.Target, f.Region.Syncing = &t, true
		if f.Region.Original == nil {
			o := cur
			f.Region.Original = &o // 没有原值（不应出现）：以此刻为准
		}
		return nil
	}); err != nil {
		return err
	}
	if err := m.finishSync(ctx, cur); err != nil {
		return usererror.Wrap("同步地区", err, "断网保护仍然有效。请再试一次；回国时会按原值还原。")
	}
	m.D.Recheck.Kick()
	return nil
}

// finishSync 按文件里的目标写虚拟机和 Mac，然后清掉同步中标记（Recover 遇到同步中断时也走这里，不再结束 Claude）。
func (m *Machine) finishSync(ctx context.Context, cur sysnet.MacLocale) error {
	f, err := m.Store.Load()
	if err != nil {
		return err
	}
	t := *f.Region.Target
	if err := m.D.VMRegion.SetRegion(ctx, t); err != nil {
		return err
	}
	if err := m.writeMac(ctx, cur, region.Desired(*f.Region.Original, t), false); err != nil {
		return err
	}
	m.emit(EvRegionSynced, t.String())
	return m.Store.Update(func(f *File) error { f.Region.Syncing = false; return nil })
}

// resumeSync 是 Recover 在 ENABLED 下发现同步中断时接着写完（不结束 Claude：结束在中断之前已经做过）。
func (m *Machine) resumeSync(ctx context.Context) error {
	if !m.regionOn() {
		return nil
	}
	cur, err := m.D.Locale.Read(ctx)
	if err != nil {
		return err
	}
	return m.finishSync(ctx, cur)
}

// ReapplyRegion 是环境检测 04、05 的“自动改回”（m4-plan v2.7 决定 3）：只在 ENABLED 下，按状态文件里的目标和原值
// 写和当前不同的项；不结束 Claude、不改目标。拿不到操作锁时返回 ErrBusy（这一轮记无法判断）。
func (m *Machine) ReapplyRegion(ctx context.Context) error {
	if !m.regionOn() {
		return nil
	}
	if err := m.lock(); err != nil {
		return err
	}
	defer m.unlock()
	f, err := m.Store.Load()
	if err != nil {
		return err
	}
	if f.State != Enabled || f.Region.Target == nil || f.Region.Original == nil {
		return ErrNotEnabled
	}
	cur, err := m.D.Locale.Read(ctx)
	if err != nil {
		return err
	}
	return m.writeMac(ctx, cur, region.Desired(*f.Region.Original, *f.Region.Target), false)
}
