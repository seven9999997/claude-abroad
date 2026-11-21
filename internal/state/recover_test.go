package state

import (
	"claude-abroad/internal/gatesm"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"claude-abroad/internal/pf"
	"claude-abroad/internal/sysnet"
)

// b. pf 的应用归到一处：KillStates 只在首次加载时出现；巡检自己重新启用时标记和 token 也落盘。
func TestSinglePatrol(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	if b.fpf.Kills() != 1 {
		t.Fatalf("启用后 KillStates %d 次", b.fpf.Kills())
	}
	// 换网段：巡检一轮只 Load 不 Kill。
	b.net.SetNet(sysnet.NetInfo{Direct: []netip.Prefix{netip.MustParsePrefix("10.20.0.0/16")}})
	b.resetTimeline()
	mustOK(t, b.ppf.P.Step(bg))
	if tl := b.tl(); index(tl, has(" -f -")) < 0 || b.fpf.Kills() != 1 {
		t.Fatalf("换网段应只 Load：%v kills=%d", tl, b.fpf.Kills())
	}
	if ok, _ := b.fpf.Eval(pf.DefaultAnchor, pf.Packet{If: "en0", User: "alice", Proto: "tcp", DstPort: 80, Dst: netip.MustParseAddr("10.20.1.1")}); !ok {
		t.Fatal("新网段应放行")
	}
	// 停用再启用：不产生 KillStates。
	mustOK(t, b.m.Disable(bg, false))
	mustOK(t, b.m.Enable(bg))
	if b.fpf.Kills() != 1 {
		t.Fatalf("停用再启用后 KillStates %d 次", b.fpf.Kills())
	}
	// 巡检发现 pf 被关，自己重新启用：标记落盘早于 -E，新 token 进状态文件。
	b.mu.Lock()
	b.noCheck = true
	b.mu.Unlock()
	b.fpf.Disable()
	b.mu.Lock()
	b.noCheck = false
	b.mu.Unlock()
	b.resetTimeline()
	mustOK(t, b.ppf.P.Step(bg))
	tl := b.tl()
	e := index(tl, func(s string) bool { return s == "pf -E" })
	if e < 0 {
		t.Fatalf("巡检没有重新启用：%v", tl)
	}
	if j := lastIndex(tl[:e], pre("store ")); j < 0 || !strings.Contains(tl[j], "pending=true") {
		t.Fatalf("-E 之前没有落盘标记：%v", tl)
	}
	f := b.state()
	if len(f.PFTokens) != 2 || f.PFEnablePending {
		t.Fatalf("巡检取得的 token 没进状态文件：%+v", f)
	}
	mustOK(t, b.m.Restore(bg))
	want := initialNet()
	want.Net = sysnet.NetInfo{Direct: []netip.Prefix{netip.MustParsePrefix("10.20.0.0/16")}} // 换过网段
	b.assertCleanTokens(want, 0)
	if b.fpf.Kills() != 1 {
		t.Fatalf("整个周期 KillStates %d 次", b.fpf.Kills())
	}
	b.noViolations()
}

// assertCleanTokens 同 assertClean，但允许 FakePF 里留 leak 个未释放的 token。
func (b *bed) assertCleanTokens(want sysnet.FakeState, leak int) {
	b.t.Helper()
	f := b.state()
	if f.State != Restored || len(f.PFTokens) != 0 || f.PFEnablePending || f.IPv6SnapshotTaken || f.Utun != "" {
		b.t.Fatalf("状态文件不干净：%+v", f)
	}
	if got := b.net.Snapshot(); !got.Equal(want) {
		b.t.Fatalf("假系统与初始值不等：\n%+v\n%+v", got, want)
	}
	if b.fpf.Active(pf.DefaultAnchor) {
		b.t.Fatal("锚点还在生效")
	}
	if n := len(b.fpf.Tokens()); n != leak {
		b.t.Fatalf("未释放的 token %d 个，期望 %d", n, leak)
	}
	if b.gate.IsOpen() || b.tun.Name() != "" {
		b.t.Fatal("闸门没关或 utun 没关")
	}
}

// crashOp 是崩溃注入的一个操作：setup 不注入，op 是被注入的操作。
type crashOp struct {
	name  string
	setup func(b *bed)
	op    func(b *bed)
}

var crashOps = []crashOp{
	{"从 RESTORED 启用", func(b *bed) {}, func(b *bed) { _ = b.m.Enable(bg) }},
	{"从 DISABLED 启用", func(b *bed) { mustOK(b.t, b.m.Enable(bg)); mustOK(b.t, b.m.Disable(bg, false)) }, func(b *bed) { _ = b.m.Enable(bg) }},
	{"停用", func(b *bed) { mustOK(b.t, b.m.Enable(bg)) }, func(b *bed) { _ = b.m.Disable(bg, true) }},
	{"恢复", func(b *bed) { mustOK(b.t, b.m.Enable(bg)) }, func(b *bed) { _ = b.m.Restore(bg) }},
	// M4-5：ENABLED 下的地区同步（应用主代理、切换地区）。
	{"运行中切换地区", func(b *bed) { mustOK(b.t, b.m.Enable(bg)) }, func(b *bed) { _ = b.m.SyncRegion(bg, otherTarget) }},
}

// opEvents 空跑一遍，数出改动事件。
func opEvents(t *testing.T, op crashOp) []string {
	b := newBed(t)
	op.setup(b)
	_, names := b.countChanges(func() { op.op(b) })
	b.noViolations()
	return names
}

var publicPkts = []pf.Packet{
	{If: "en0", User: "alice", Proto: "tcp", DstPort: 443, Dst: netip.MustParseAddr("160.79.104.10")},
	{If: "en0", User: "alice", Proto: "udp", DstPort: 443, Dst: netip.MustParseAddr("160.79.104.10")},
}

func (b *bed) blocked() bool {
	for _, p := range publicPkts {
		if ok, err := b.fpf.Eval(pf.DefaultAnchor, p); err != nil || ok {
			return false
		}
	}
	return true
}

// runCrashCase：第 k 个改动事件之后崩溃（reboot 为 true 时改为重启，先 Boot），新进程 Recover，核对终态，再 Restore。
func runCrashCase(t *testing.T, op crashOp, k int, ev string, reboot bool) {
	b := newBed(t)
	op.setup(b)
	if !b.runToCrash(k, func() { op.op(b) }) {
		t.Fatalf("第 %d 个事件没有触发崩溃", k)
	}
	atCrash := b.state()
	if reboot {
		calls, err := b.reboot()
		if err != nil {
			t.Fatalf("Boot：%v", err)
		}
		if atCrash.State == Restored {
			if len(calls) != 0 {
				t.Fatalf("RESTORED 时 Boot 调用了 pfctl：%v", calls)
			}
		} else {
			if !b.blocked() {
				t.Fatal("Boot 之后公网没被拦")
			}
			if !slices.Contains(calls, "pf -F states") {
				t.Fatalf("Boot 首次加载没有清状态：%v", calls)
			}
		}
	} else {
		b.crash()
	}
	if err := b.m.Recover(bg); err != nil {
		t.Fatalf("Recover：%v", err)
	}
	f := b.state()
	if b.ds.get() != (f.State == Enabled) { // m3-plan v2.7 验收 g：接收方的状态与文件状态一致
		t.Fatalf("Recover 之后数据面的状态接收方是启用=%v，但文件状态是 %s", b.ds.get(), f.State)
	}
	if f.State != Restored && !b.fpf.Active(pf.DefaultAnchor) {
		t.Fatalf("I3：状态 %s，pf 没生效", f.State)
	}
	leak := 0
	if !reboot && len(b.fpf.Tokens()) > len(f.PFTokens) {
		leak = len(b.fpf.Tokens()) - len(f.PFTokens)
	}
	switch {
	case atCrash.State == Enabling && atCrash.Origin != Disabled, atCrash.State == Restoring, atCrash.State == Restored:
		b.assertCleanTokens(b.init, leak)
	case atCrash.State == Enabling, atCrash.State == Disabled:
		b.assertDisabled()
	case atCrash.State == Enabled:
		b.assertEnabled()
	}
	mustOK(t, b.m.Restore(bg))
	b.assertCleanTokens(b.init, leak)
	if leak > 1 {
		t.Fatalf("未释放的 token %d 个", leak)
	}
	if leak == 1 {
		if ev != "pf -E" {
			t.Fatalf("在 %q 之后崩溃却漏了 token", ev)
		}
		if b.countEvents(EvTokenUnknown) == 0 {
			t.Fatal("漏了 token 却没有“token 未知”事件")
		}
	}
	b.noViolations()
}

func testCrashes(t *testing.T, reboot bool) {
	for _, op := range crashOps {
		names := opEvents(t, op)
		if len(names) == 0 {
			t.Fatalf("%s 没有改动事件", op.name)
		}
		t.Run(op.name, func(t *testing.T) {
			for k := 1; k <= len(names); k++ {
				t.Run(fmt.Sprintf("%02d %s", k, names[k-1]), func(t *testing.T) {
					runCrashCase(t, op, k, names[k-1], reboot)
				})
			}
		})
	}
}

// d. 崩溃注入，穷举。
func TestCrashExhaustive(t *testing.T) { testCrashes(t, false) }

// e. 重启注入，穷举。
func TestRebootExhaustive(t *testing.T) { testCrashes(t, true) }

// d. 单独点名的两例：第一个网络服务关掉 IPv6 之后崩溃；标记落盘之后、token 落盘之前崩溃。
func TestCrashNamedCases(t *testing.T) {
	op := crashOps[0]
	names := opEvents(t, op)
	firstOff := slices.Index(names, "net "+sysnet.OpIPv6Off) + 1
	enable := slices.Index(names, "pf -E") + 1
	if firstOff == 0 || enable < 2 || !strings.Contains(names[enable-2], "pending=true") {
		t.Fatalf("找不到点名的事件：%v", names)
	}
	for _, reboot := range []bool{false, true} {
		t.Run(fmt.Sprintf("第一个服务关掉 IPv6 之后/重启=%v", reboot), func(t *testing.T) {
			runCrashCase(t, op, firstOff, names[firstOff-1], reboot)
		})
		t.Run(fmt.Sprintf("标记落盘之后/重启=%v", reboot), func(t *testing.T) {
			runCrashCase(t, op, enable-1, names[enable-2], reboot)
		})
		t.Run(fmt.Sprintf("-E 之后、token 落盘之前/重启=%v", reboot), func(t *testing.T) {
			runCrashCase(t, op, enable, names[enable-1], reboot)
		})
	}
	// -E 之后崩溃（不重启）确实漏了一个 token，并有事件。
	b := newBed(t)
	b.runToCrash(enable, func() { _ = b.m.Enable(bg) })
	b.crash()
	mustOK(t, b.m.Recover(bg))
	mustOK(t, b.m.Restore(bg))
	if len(b.fpf.Tokens()) != 1 || b.countEvents(EvTokenUnknown) == 0 {
		t.Fatalf("tokens=%v", b.fpf.Tokens())
	}
}

// f. ENABLED：Recover 只重建并清 state 原因，不自己做出口检查、不启动虚拟机（m4-plan v2.2 决定 4）。
// 复查没通过时仍 ENABLED、闸门关、pf 生效；之后复查通过（Kick）闸门打开。
func TestRecoverEnabledLeavesCheckToRecheck(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	b.crash()
	b.re.setOK(false)
	vm0, ex0 := b.vm.starts(), b.exit.callsN()
	mustOK(t, b.m.Recover(bg))
	if b.state().State != Enabled || b.gate.IsOpen() || !b.fpf.Active(pf.DefaultAnchor) || b.gate.has(gatesm.ReasonState) {
		t.Fatal("复查没通过时：应 ENABLED、闸门关、pf 生效、state 原因已清")
	}
	if b.vm.starts() != vm0 || b.exit.callsN() != ex0 || b.re.n() == 0 {
		t.Fatalf("Recover 不应启动虚拟机或做出口检查，应补一轮复查：vm %d→%d check %d→%d kicks %d", vm0, b.vm.starts(), ex0, b.exit.callsN(), b.re.n())
	}
	b.re.setOK(true)
	b.re.Kick()
	b.assertEnabled()
	b.noViolations()
}

// f. RESTORING 且结束 Claude 失败：停在 RESTORING、pf 生效、没有网络改动；之后成功一次走到 RESTORED。
func TestRecoverRestoringKillFails(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	b.vm.failStop = errInjected
	if err := b.m.Restore(bg); err == nil {
		t.Fatal("应失败")
	}
	b.crash()
	b.kill.fail = errInjected
	b.resetTimeline()
	if err := b.m.Recover(bg); !errors.Is(err, errInjected) {
		t.Fatalf("%v", err)
	}
	if b.state().State != Restoring || !b.fpf.Active(pf.DefaultAnchor) {
		t.Fatal("应停在 RESTORING 且 pf 生效")
	}
	if i := index(b.tl(), pre("net ")); i >= 0 {
		t.Fatalf("有网络改动：%v", b.tl())
	}
	mustOK(t, b.m.Recover(bg))
	b.assertClean(b.init)
	b.noViolations()
}

// f. RESTORING 步骤 6（重启后）：Boot 加载 pf，Recover 只做 ⑥⑦。
func TestRecoverRestoringStep6AfterReboot(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	b.failNthWrite(7) // 恢复的第 7 次写入：⑥ 里从文件去掉 token（M4-5 起 ⑤ 多写一次步骤号）
	if err := b.m.Restore(bg); err == nil {
		t.Fatal("应失败")
	}
	if f := b.state(); f.State != Restoring || f.Step != StepPFClear {
		t.Fatalf("%+v", f)
	}
	calls, err := b.reboot()
	mustOK(t, err)
	if index(calls, has(" -f -")) < 0 {
		t.Fatalf("Boot 没加载 pf：%v", calls)
	}
	b.resetTimeline()
	mustOK(t, b.m.Recover(bg))
	for _, s := range b.tl() {
		if strings.HasPrefix(s, "net ") || strings.HasPrefix(s, "tun ") || strings.HasPrefix(s, "vm ") {
			t.Fatalf("步骤 6 的 Recover 不应碰网络：%v", b.tl())
		}
	}
	b.assertClean(b.init)
	b.noViolations()
}

// f. 重建时路由、DNS 已存在不报错。
func TestRecoverRouteDNSExist(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	mustOK(t, b.m.Disable(bg, false))
	// 进程退出但 utun、路由、DNS 都还在（例如 utun 被别的东西接住）：新进程有自己的 utun。
	b.release()
	b.spawn()
	mustOK(t, b.m.Recover(bg))
	if b.state().State != Disabled || !b.fpf.Active(pf.DefaultAnchor) || b.gate.IsOpen() {
		t.Fatal("应回到 DISABLED")
	}
	mustOK(t, b.m.Restore(bg))
	b.noViolations()
}

// f. RESTORED：没有任何调用。
func TestRecoverRestoredNoop(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Recover(bg))
	if tl := b.tl(); len(tl) != 0 {
		t.Fatalf("%v", tl)
	}
	mustOK(t, b.m.Enable(bg))
	mustOK(t, b.m.Restore(bg))
	b.crash()
	b.resetTimeline()
	mustOK(t, b.m.Recover(bg))
	if tl := b.tl(); len(tl) != 0 {
		t.Fatalf("%v", tl)
	}
}
