package state

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"claude-abroad/internal/region"
	"claude-abroad/internal/sysnet"
)

var otherTarget = region.Target{Timezone: "America/Los_Angeles", Lang: "en-US", Locale: "en_US", Country: "US", Source: region.SourceExit}

func (b *bed) locale() sysnet.MacLocale { return b.net.Snapshot().Locale }

func want(b *bed, t region.Target) sysnet.MacLocale {
	base := b.init.Locale
	base.User = "alice"
	d := region.Desired(base, t)
	d.User = ""
	return d
}

func hasEvent(b *bed, k EventKind) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range b.events {
		if e.Kind == k {
			return true
		}
	}
	return false
}

// c. 注入点数增加：同样的启用，地区打开时改动事件更多，且包含地区的每一步。
func TestRegionMoreCrashPoints(t *testing.T) {
	off := newBed(t)
	off.tgts.ok = false
	nOff, _ := off.countChanges(func() { mustOK(t, off.m.Enable(bg)) })
	on := newBed(t)
	nOn, names := on.countChanges(func() { mustOK(t, on.m.Enable(bg)) })
	if nOn <= nOff {
		t.Fatalf("地区打开后注入点应增加：%d → %d", nOff, nOn)
	}
	joined := strings.Join(names, "\n")
	for _, s := range []string{"step=60", "step=61", "step=62", "vm region Asia/Tokyo", "step=63", "net " + sysnet.OpLocaleTZ,
		"step=64", "net " + sysnet.OpLocaleAuto, "step=65", "net " + sysnet.OpLocaleUser, "step=66", "net " + sysnet.OpLocaleSys, "step=67"} {
		if !strings.Contains(joined, s) {
			t.Fatalf("改动事件里没有 %q：\n%s", s, joined)
		}
	}
	// 顺序：结束 Claude 在任何地区写入之前，虚拟机在 Mac 之前，系统级最后。
	idx := func(s string) int {
		return slices.IndexFunc(names, func(n string) bool { return strings.Contains(n, s) })
	}
	if !(idx("step=61") < idx("vm region") && idx("vm region") < idx(sysnet.OpLocaleTZ) && idx(sysnet.OpLocaleTZ) < idx(sysnet.OpLocaleAuto) &&
		idx(sysnet.OpLocaleAuto) < idx(sysnet.OpLocaleUser) && idx(sysnet.OpLocaleUser) < idx(sysnet.OpLocaleSys)) {
		t.Fatalf("顺序不对：\n%s", joined)
	}
}

// c. 启用后地区是目标；恢复后逐项等于初始值（系统级原来没有的键被删掉）；再次从 RESTORED 启用、恢复也一样。
func TestRegionEnableRestore(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	if got := b.locale(); !got.Equal(want(b, testTarget)) {
		t.Fatalf("启用后：%+v", got)
	}
	if f := b.state(); f.Region.Target == nil || !f.Region.Target.Same(testTarget) || f.Region.Original == nil {
		t.Fatalf("%+v", f.Region)
	}
	if b.kill.callsN() != 1 {
		t.Fatalf("地区会变：应结束一次 Claude：%d", b.kill.callsN())
	}
	if b.exit.callsN() != 2 || b.vm.starts() != 2 {
		t.Fatalf("地区同步后应等虚拟机并再检查一次出口：exit=%d vm=%d", b.exit.callsN(), b.vm.starts())
	}
	mustOK(t, b.m.Restore(bg))
	b.assertClean(b.init)
	if b.state().Region.Original != nil {
		t.Fatal("回国后状态文件不应留地区")
	}
}

// c. 从 DISABLED 再启用时不重拍原值（那时 Mac 已经是目标地区）；中途崩溃也不覆盖。
func TestRegionDisabledKeepsOriginal(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	orig := *b.state().Region.Original
	mustOK(t, b.m.Disable(bg, false))
	b.tgts.t = otherTarget
	if !b.runToCrash(8, func() { _ = b.m.Enable(bg) }) {
		t.Fatal("没崩溃")
	}
	b.crash()
	mustOK(t, b.m.Recover(bg))
	if o := b.state().Region.Original; o == nil || !o.Equal(orig) {
		t.Fatalf("原值被覆盖：%+v", o)
	}
	mustOK(t, b.m.Enable(bg))
	if o := b.state().Region.Original; !o.Equal(orig) {
		t.Fatal("从 DISABLED 再启用时不应重拍原值")
	}
	if got := b.locale(); !got.Equal(want(b, otherTarget)) {
		t.Fatalf("%+v", got)
	}
	mustOK(t, b.m.Restore(bg))
	b.assertClean(b.init)
}

// c. 用户在回国期间自己改过设置：下一次从 RESTORED 启用以新的为准，恢复后等于新的。
func TestRegionUserChangedWhileHome(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	mustOK(t, b.m.Restore(bg))
	changed := b.init.Locale
	changed.Timezone, changed.AutoTZ = "Europe/Paris", sysnet.OptStr{}
	changed.UserLangs = sysnet.SomeList("fr-FR")
	b.net.SetLocaleForTest(changed)
	mustOK(t, b.m.Enable(bg))
	mustOK(t, b.m.Restore(bg))
	init := b.init
	init.Locale = changed
	b.assertClean(init)
}

// d. 读不出当前值：启用失败，系统没有任何改动，状态文件没有地区，没有结束任何进程。
func TestRegionReadFails(t *testing.T) {
	b := newBed(t)
	b.net.FailNext(sysnet.OpLocaleRead, errors.New("读不出"))
	err := b.m.Enable(bg)
	if err == nil || !strings.Contains(err.Error(), "读取 Mac 当前的时区和语言") {
		t.Fatalf("%v", err)
	}
	f := b.state()
	if f.State != Restored || f.Region.Original != nil || f.Region.Target != nil || b.writes != 0 {
		t.Fatalf("状态文件不应写入：%+v writes=%d", f, b.writes)
	}
	if b.kill.callsN() != 0 || b.fpf.Active("claude-abroad") || !b.net.Snapshot().Equal(b.init) {
		t.Fatal("不应有任何改动")
	}
}

// e. Claude 先被结束、再改设置；结束不了时中止（回滚），地区和网络都不变；地区不变时不结束。
func TestRegionKillOrder(t *testing.T) {
	b := newBed(t)
	var atKill sysnet.MacLocale
	b.kill.hook = func() { atKill = b.locale() }
	mustOK(t, b.m.Enable(bg))
	if !atKill.Equal(b.init.Locale) {
		t.Fatal("结束 Claude 时 Mac 的地区应还没改")
	}
	mustOK(t, b.m.Restore(bg))

	b2 := newBed(t)
	b2.kill.setAlways(errors.New("还有 1 个 Claude 进程结束不了"))
	if err := b2.m.Enable(bg); err == nil {
		t.Fatal("结束不了应失败")
	}
	b2.assertClean(b2.init)
	if b2.vmr.n != 0 {
		t.Fatal("结束不了时不应下发虚拟机的地区")
	}

	// 地区不变（Mac 已经是目标）：不结束任何进程。
	b3 := newBed(t)
	same := want(b3, testTarget)
	b3.net.SetLocaleForTest(same)
	mustOK(t, b3.m.Enable(bg))
	if b3.kill.callsN() != 0 {
		t.Fatalf("地区不变时不应结束 Claude：%d", b3.kill.callsN())
	}
}

// f. 地区同步后的再次检查不通过：启用失败并回滚，地区还原为原值。
func TestRegionRecheckFails(t *testing.T) {
	b := newBed(t)
	n := 0
	b.exit.hook = func() {
		n++
		if n == 2 {
			b.exit.mu.Lock()
			b.exit.fail = errors.New("出口检查不通过")
			b.exit.mu.Unlock()
		}
	}
	if err := b.m.Enable(bg); err == nil {
		t.Fatal("应失败")
	}
	b.assertClean(b.init)
	if b.vmr.n != 1 {
		t.Fatal("应同步过一次")
	}
}

// 没有人登录：跳过用户级并发事件，时区照常；恢复也跳过用户级。
func TestRegionNoConsoleUser(t *testing.T) {
	b := newBed(t)
	b.net.SetConsoleUser("")
	mustOK(t, b.m.Enable(bg))
	l := b.locale()
	if l.Timezone != testTarget.Timezone || !l.UserLangs.Equal(b.init.Locale.UserLangs) || !hasEvent(b, EvRegionUserSkipped) {
		t.Fatalf("%+v", l)
	}
	mustOK(t, b.m.Restore(bg))
	init := b.init
	init.Console = ""
	b.assertClean(init)
}

// 运行中切换地区：结束 Claude、改到新目标、补一轮复查；不是 ENABLED 时返回 ErrNotEnabled 且不改；结束不了时什么都不变。
func TestSyncRegion(t *testing.T) {
	b := newBed(t)
	if err := b.m.SyncRegion(bg, otherTarget); !errors.Is(err, ErrNotEnabled) || b.kill.callsN() != 0 {
		t.Fatalf("%v", err)
	}
	mustOK(t, b.m.Enable(bg))
	kicks := b.re.kicks
	b.kill.setAlways(errors.New("结束不了"))
	if err := b.m.SyncRegion(bg, otherTarget); err == nil {
		t.Fatal("应失败")
	}
	if !b.locale().Equal(want(b, testTarget)) || !b.state().Region.Target.Same(testTarget) {
		t.Fatal("结束不了时目标和地区都不变")
	}
	b.kill.setAlways(nil)
	mustOK(t, b.m.SyncRegion(bg, otherTarget))
	if !b.locale().Equal(want(b, otherTarget)) || b.vmr.last != otherTarget || b.state().Region.Syncing || b.re.kicks != kicks+1 {
		t.Fatalf("%+v", b.locale())
	}
	// 同一个目标再同步：地区不变，不结束 Claude。
	k := b.kill.callsN()
	mustOK(t, b.m.SyncRegion(bg, otherTarget))
	if b.kill.callsN() != k {
		t.Fatal("地区不变时不应结束 Claude")
	}
	if err := b.m.SyncRegion(bg, region.Target{Timezone: "Mars/Base", Lang: "en-US", Locale: "en_US", Source: region.SourceManual}); err == nil {
		t.Fatal("不合法的目标应拒绝")
	}
	mustOK(t, b.m.Restore(bg))
	b.assertClean(b.init)
}

var _ = context.Background

// j. 自动改回：只写不同的项，不结束 Claude；不是 ENABLED 时不改；拿不到锁时 ErrBusy。
func TestReapplyRegion(t *testing.T) {
	b := newBed(t)
	if err := b.m.ReapplyRegion(bg); !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("%v", err)
	}
	mustOK(t, b.m.Enable(bg))
	kills := b.kill.callsN()
	l := b.locale()
	l.Timezone, l.AutoTZ = "Europe/Paris", sysnet.Some("1") // 别的软件改了时区、打开了自动时区
	b.net.SetLocaleForTest(l)
	n := len(b.net.Calls())
	mustOK(t, b.m.ReapplyRegion(bg))
	if !b.locale().Equal(want(b, testTarget)) || b.kill.callsN() != kills {
		t.Fatalf("应改回、不结束 Claude：%+v", b.locale())
	}
	var writes []string
	for _, c := range b.net.Calls()[n:] {
		if c != sysnet.OpLocaleRead {
			writes = append(writes, c)
		}
	}
	if len(writes) != 2 || !strings.HasPrefix(writes[0], sysnet.OpLocaleTZ) || !strings.HasPrefix(writes[1], sysnet.OpLocaleAuto) {
		t.Fatalf("只写不同的项：%v", writes)
	}
	b.m.busy.Store(true)
	if err := b.m.ReapplyRegion(bg); !errors.Is(err, ErrBusy) {
		t.Fatalf("拿不到锁应返回 ErrBusy：%v", err)
	}
	b.m.busy.Store(false)
	mustOK(t, b.m.Restore(bg))
	b.assertClean(b.init)
}
