package state

import (
	"claude-abroad/internal/gatesm"
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"claude-abroad/internal/pf"
	"claude-abroad/internal/sysnet"
)

var bg = context.Background()

func mustOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// isUserError 按 usererror 的固定格式判断（类型未导出）。
func isUserError(err error) bool { return err != nil && strings.Contains(err.Error(), "失败：") }

// assertEnabled：ENABLED，闸门开，路由、DNS、utun、IPv6 都接管了，pf 生效。
func (b *bed) assertEnabled() {
	b.t.Helper()
	f := b.state()
	if f.State != Enabled || f.Origin != "" || f.Utun == "" || f.Utun != b.tun.Name() || f.PFEnablePending {
		b.t.Fatalf("状态文件：%+v", f)
	}
	if !b.gate.IsOpen() || !b.fpf.Active(pf.DefaultAnchor) || !b.vm.Running() {
		b.t.Fatal("闸门、pf 或虚拟机不对")
	}
	b.assertTakenOver(f.Utun)
}

func (b *bed) assertTakenOver(utun string) {
	b.t.Helper()
	st := b.net.Snapshot()
	for _, r := range TakeoverRoutes {
		if st.Routes[r] != utun {
			b.t.Fatalf("路由 %s：%q", r, st.Routes[r])
		}
	}
	if st.DNS != TakeoverDNS {
		b.t.Fatal("DNS 没设")
	}
	for _, svc := range offServices {
		if st.IPv6[svc].Mode != sysnet.IPv6Off {
			b.t.Fatalf("%s 的 IPv6 没关", svc)
		}
	}
}

// assertDisabled：DISABLED，闸门关，pf 生效，utun、路由、DNS 仍在。
func (b *bed) assertDisabled() {
	b.t.Helper()
	f := b.state()
	if f.State != Disabled || f.Origin != "" {
		b.t.Fatalf("状态文件：%+v", f)
	}
	if b.gate.IsOpen() || !b.fpf.Active(pf.DefaultAnchor) || b.tun.Name() == "" {
		b.t.Fatal("闸门、pf 或 utun 不对")
	}
	b.assertTakenOver(b.tun.Name())
}

// b. 正常路径。
func TestNormalCycle(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	b.assertEnabled()
	snap := b.state().IPv6Original
	if !reflect.DeepEqual(snap, b.init.IPv6) {
		t.Fatalf("快照不对：%+v", snap)
	}
	if b.gate.Opens() != 1 || len(b.state().PFTokens) != 1 {
		t.Fatal("闸门或 token")
	}

	mustOK(t, b.m.Disable(bg, false))
	b.assertDisabled()
	if b.td.n != 1 || !b.vm.Running() {
		t.Fatal("停用应断开连接、保留虚拟机")
	}

	mustOK(t, b.m.Enable(bg))
	b.assertEnabled()
	if got := b.state().IPv6Original; !reflect.DeepEqual(got, snap) {
		t.Fatal("再次启用覆盖了快照")
	}
	if len(b.state().PFTokens) != 1 {
		t.Fatal("pf 已启用时不应再取 token")
	}

	mustOK(t, b.m.Restore(bg))
	b.assertClean(b.init)
	if b.vm.Running() {
		t.Fatal("虚拟机没关")
	}
	b.noViolations()
}

// 停用时可选关虚拟机。
func TestDisableStopVM(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	mustOK(t, b.m.Disable(bg, true))
	b.assertDisabled()
	if b.vm.Running() {
		t.Fatal("虚拟机没关")
	}
	mustOK(t, b.m.Restore(bg))
	b.assertClean(b.init)
	b.noViolations()
}

func index(tl []string, pred func(string) bool) int { return slices.IndexFunc(tl, pred) }
func lastIndex(tl []string, pred func(string) bool) int {
	for i := len(tl) - 1; i >= 0; i-- {
		if pred(tl[i]) {
			return i
		}
	}
	return -1
}
func has(sub string) func(string) bool {
	return func(s string) bool { return strings.Contains(s, sub) }
}
func pre(p string) func(string) bool { return func(s string) bool { return strings.HasPrefix(s, p) } }

// c. 顺序断言。
func TestOrdering(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	tl := b.tl()
	enabling := index(tl, pre("store ENABLING"))
	load := index(tl, has(" -f -"))
	firstNet := index(tl, pre("net "))
	tunOpen := index(tl, pre("tun open"))
	if enabling < 0 || load < 0 || !(enabling < load) {
		t.Fatalf("ENABLING 落盘应早于第一次 pf 加载：%v", tl)
	}
	if !(load < firstNet && load < tunOpen) {
		t.Fatalf("第一次 pf 加载应早于 sysnet 改动和建 utun：%v", tl)
	}
	nE := 0
	for i, s := range tl {
		if s != "pf -E" {
			continue
		}
		nE++
		j := lastIndex(tl[:i], pre("store "))
		if j < 0 || !strings.Contains(tl[j], "pending=true") {
			t.Fatalf("-E 之前最后一次落盘没有 pending：%v", tl)
		}
		if k := index(tl[i:], pre("store ")); k < 0 || !strings.Contains(tl[i+k], "tokens=1") || strings.Contains(tl[i+k], "pending=true") {
			t.Fatalf("-E 之后第一次落盘应带上 token 并清 pending：%v", tl)
		}
	}
	if nE != 1 {
		t.Fatalf("-E 次数 %d", nE)
	}
	if e, g := index(tl, pre("store ENABLED")), index(tl, pre("gate open")); !(e >= 0 && e < g) {
		t.Fatalf("ENABLED 落盘应早于开闸门：%v", tl)
	}

	b.resetTimeline()
	mustOK(t, b.m.Restore(bg))
	tl = b.tl()
	flush := index(tl, has("-F all"))
	if flush < 0 {
		t.Fatalf("没有清锚点：%v", tl)
	}
	if lastNet, tc := lastIndex(tl, pre("net ")), index(tl, pre("tun close")); !(lastNet < flush && tc < flush && tc >= 0) {
		t.Fatalf("pf 清除应晚于所有 sysnet 还原和关 utun：%v", tl)
	}
	if r := index(tl, pre("store RESTORED")); !(r > flush) {
		t.Fatalf("RESTORED 落盘应晚于 pf 清除：%v", tl)
	}
	if rel := index(tl, pre("pf -X")); !(rel > flush) || index(tl, pre("store RESTORED")) < rel {
		t.Fatalf("释放 token 应在清锚点之后、RESTORED 之前：%v", tl)
	}
	b.noViolations()
}

// 一个启用失败点。
type enableFault struct {
	name   string
	inject func(b *bed)
}

// 成功启用（从 RESTORED）时的状态文件写入次数。
func enableWrites(t *testing.T) int {
	b := newBed(t)
	b.failNthWrite(1000)
	mustOK(t, b.m.Enable(bg))
	return b.writes
}

func enableFaults(t *testing.T, fromDisabled bool) []enableFault {
	fs := []enableFault{
		{"pf 加载", func(b *bed) { b.pfw.failApply(1) }},
		{"IPv6 快照", func(b *bed) { b.net.FailNext(sysnet.OpIPv6Snapshot, errInjected) }},
		{"pf 重新应用", func(b *bed) { b.pfw.failApply(2) }},
		{"DNS", func(b *bed) { b.net.FailNext(sysnet.OpDNSSet, errInjected) }}, // 模拟器后端从 DISABLED 来时跳过，见下
		{"虚拟机", func(b *bed) { b.vm.failStart = errInjected }},
		{"出口检查", func(b *bed) { b.exit.fail = errInjected }},
	}
	if fromDisabled && simBackend {
		// 模拟器后端（真实实现）从 DISABLED 来时，DNS 和第 1 条路由早就在：MacDNS.Set 先 show、看到相同就返回
		// ErrExists，route add 报 File exists 后用 get 判断同接口，都不会调用 Fake 的 Set、Add，注入点不存在
		// （和真机行为一致）。
		fs = slices.DeleteFunc(fs, func(f enableFault) bool { return f.name == "DNS" })
	}
	if !fromDisabled {
		fs = append(fs,
			enableFault{"-E", func(b *bed) { b.fpf.FailNext("enable", errInjected) }},
			enableFault{"建 utun", func(b *bed) { b.tun.failOpen = errInjected }},
		)
	}
	for i := range offServices {
		k := i + 1
		fs = append(fs, enableFault{"关第 " + itoa(k) + " 个服务的 IPv6", func(b *bed) { b.failNthNet(sysnet.OpIPv6Off, k) }})
	}
	for i := range TakeoverRoutes {
		k := i + 1
		if fromDisabled && (k > 1 || simBackend) {
			// 从 DISABLED 来时路由已经在，Add 返回“已存在”（按成功处理、不算改动），按成功次数数的注入点到不了。
			continue
		}
		fs = append(fs, enableFault{"第 " + itoa(k) + " 条路由", func(b *bed) { b.failNthNet(sysnet.OpRoutesAdd, k) }})
	}
	// 从 DISABLED 来时没有 AddToken 那次写入，换成 ClearEnablePending，次数相同；多出的注入点在用例里跳过。
	for k := 1; k <= enableWrites(t); k++ {
		fs = append(fs, enableFault{"第 " + itoa(k) + " 次状态文件写入", func(b *bed) { b.failNthWrite(k) }})
	}
	return fs
}

var itoa = strconv.Itoa

// e. 启用的每一步失败：来源 RESTORED。
func TestEnableFailFromRestored(t *testing.T) {
	t.Parallel() // 独立的测试台；耗时主要是等待（补齐四第 0 步）
	for _, fault := range enableFaults(t, false) {
		t.Run(fault.name, func(t *testing.T) {
			t.Parallel() // 每例一个独立的测试台
			b := newBed(t)
			fault.inject(b)
			err := b.m.Enable(bg)
			if err == nil || !isUserError(err) {
				t.Fatalf("应返回 usererror：%v", err)
			}
			if !strings.HasPrefix(err.Error(), "启用失败：") {
				t.Fatalf("消息：%v", err)
			}
			b.assertClean(b.init)
			if fault.name == "出口检查" && b.gate.Opens() != 0 {
				t.Fatal("出口检查失败时闸门打开过")
			}
			wantRollback := 1
			if fault.name == "第 1 次状态文件写入" {
				wantRollback = 0                                        // ENABLING 没写进去，什么都没改
				if tl := b.tl(); len(tl) != 1 || tl[0] != "preflight" { // 只有只读的冲突检查
					t.Fatalf("ENABLING 写入失败后有改动：%v", tl)
				}
			}
			if b.countEvents(EvRollback) != wantRollback {
				t.Fatal("回滚事件次数不对")
			}
			b.noViolations()
			// 之后还能正常启用。
			mustOK(t, b.m.Enable(bg))
			b.assertEnabled()
			b.noViolations()
		})
	}
}

// e. 冲突检查失败：什么都不改，状态文件未被写入。
func TestEnablePreflightFail(t *testing.T) {
	for _, from := range []State{Restored, Disabled} {
		t.Run(string(from), func(t *testing.T) {
			b := newBed(t)
			if from == Disabled {
				mustOK(t, b.m.Enable(bg))
				mustOK(t, b.m.Disable(bg, false))
			}
			before := b.state()
			netBefore := b.net.Snapshot()
			b.resetTimeline()
			b.pre.fail = errInjected
			err := b.m.Enable(bg)
			if !errors.Is(err, errInjected) || !isUserError(err) {
				t.Fatalf("%v", err)
			}
			if tl := b.tl(); len(tl) != 0 {
				t.Fatalf("冲突检查失败后有改动：%v", tl)
			}
			if got := b.state(); !reflect.DeepEqual(got, before) {
				t.Fatalf("状态文件变了：%+v", got)
			}
			if !b.net.Snapshot().Equal(netBefore) {
				t.Fatal("假系统变了")
			}
			if from == Restored {
				if _, err := b.view.Load(); err != nil {
					t.Fatal(err)
				}
				if b.writes != 0 {
					t.Fatal("写过状态文件")
				}
			}
			b.noViolations()
		})
	}
}

// e. 启用的每一步失败：来源 DISABLED。
func TestEnableFailFromDisabled(t *testing.T) {
	t.Parallel() // 独立的测试台；耗时主要是等待（补齐四第 0 步）
	for _, fault := range enableFaults(t, true) {
		t.Run(fault.name, func(t *testing.T) {
			t.Parallel() // 每例一个独立的测试台
			b := newBed(t)
			mustOK(t, b.m.Enable(bg))
			mustOK(t, b.m.Disable(bg, false))
			opens := b.gate.Opens()
			fault.inject(b)
			err := b.m.Enable(bg)
			if strings.Contains(fault.name, "状态文件写入") && err == nil {
				// 写入次数比来源 RESTORED 时少，超出的注入点不会触发。
				t.Skip("这个写入点在 DISABLED 来源下不存在")
			}
			if err == nil || !isUserError(err) {
				t.Fatalf("应返回 usererror：%v", err)
			}
			b.assertDisabled()
			if fault.name == "出口检查" && b.gate.Opens() != opens {
				t.Fatal("出口检查失败时闸门打开过")
			}
			b.noViolations()
			mustOK(t, b.m.Enable(bg))
			b.assertEnabled()
			mustOK(t, b.m.Restore(bg))
			b.assertClean(b.init)
			b.noViolations()
		})
	}
}

// f. 启用失败、回滚中再失败：状态留在 ENABLING，pf 生效；之后 Restore 或 Enable 都能走完。
func TestRollbackFailure(t *testing.T) {
	t.Parallel() // 独立的测试台；耗时主要是等待（补齐四第 0 步）
	rollbackFaults := []enableFault{
		{"关虚拟机", func(b *bed) { b.vm.failStop = errInjected }},
		{"删 DNS", func(b *bed) { b.net.FailNext(sysnet.OpDNSRemove, errInjected) }},
		{"删路由", func(b *bed) { b.net.FailNext(sysnet.OpRoutesDelete, errInjected) }},
		{"关 utun", func(b *bed) { b.tun.failClose = errInjected }},
		{"还原 IPv6", func(b *bed) { b.net.FailNext(sysnet.OpIPv6Restore, errInjected) }},
		{"清锚点", func(b *bed) { b.pfw.failF = errInjected }},
	}
	for _, rf := range rollbackFaults {
		for _, then := range []string{"Restore", "Enable"} {
			t.Run(rf.name+"/"+then, func(t *testing.T) {
				t.Parallel() // 每例一个独立的测试台
				b := newBed(t)
				b.exit.fail = errInjected // 启用在出口检查失败
				rf.inject(b)
				err := b.m.Enable(bg)
				if err == nil || !isUserError(err) || !strings.Contains(err.Error(), "回滚也失败") {
					t.Fatalf("%v", err)
				}
				if f := b.state(); f.State != Enabling || f.Origin != Restored {
					t.Fatalf("应留在 ENABLING：%+v", f)
				}
				if !b.fpf.Active(pf.DefaultAnchor) {
					t.Fatal("回滚失败后 pf 应仍生效")
				}
				if b.countEvents(EvRollbackFailed) != 1 {
					t.Fatal("没有回滚失败事件")
				}
				if then == "Restore" {
					mustOK(t, b.m.Restore(bg))
					b.assertClean(b.init)
				} else {
					mustOK(t, b.m.Enable(bg))
					b.assertEnabled()
					if !reflect.DeepEqual(b.state().IPv6Original, b.init.IPv6) {
						t.Fatal("快照被覆盖")
					}
					mustOK(t, b.m.Restore(bg))
					b.assertClean(b.init)
				}
				b.noViolations()
			})
		}
	}
}

// 恢复的写入次数（从 ENABLED，单 token）。
func restoreWrites(t *testing.T) int {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	b.failNthWrite(1000)
	mustOK(t, b.m.Restore(bg))
	return b.writes
}

// f. 恢复的每一步失败：状态 RESTORING（第一次写入失败时仍是 ENABLED，闸门已关），pf 生效（释放 token
// 失败时锚点已清空）；再次 Restore 走完。
func TestRestoreStepFailure(t *testing.T) {
	t.Parallel() // 独立的测试台；耗时主要是等待（补齐四第 0 步）
	faults := []enableFault{
		{"关虚拟机", func(b *bed) { b.vm.failStop = errInjected }},
		{"删 DNS", func(b *bed) { b.net.FailNext(sysnet.OpDNSRemove, errInjected) }},
		{"删第 1 条路由", func(b *bed) { b.failNthNet(sysnet.OpRoutesDelete, 1) }},
		{"删第 2 条路由", func(b *bed) { b.failNthNet(sysnet.OpRoutesDelete, 2) }},
		{"关 utun", func(b *bed) { b.tun.failClose = errInjected }},
		{"清锚点", func(b *bed) { b.pfw.failF = errInjected }},
	}
	for i := range offServices {
		k := i + 1
		faults = append(faults, enableFault{"还原第 " + itoa(k) + " 个服务", func(b *bed) { b.failNthNet(sysnet.OpIPv6Restore, k) }})
	}
	nWrites := restoreWrites(t) // 只量一次（原来每例里量两次，各要一整轮启用和恢复）
	for k := 1; k <= nWrites; k++ {
		faults = append(faults, enableFault{"第 " + itoa(k) + " 次状态文件写入", func(b *bed) { b.failNthWrite(k) }})
	}
	for _, fault := range faults {
		t.Run(fault.name, func(t *testing.T) {
			t.Parallel() // 每例一个独立的测试台
			b := newBed(t)
			mustOK(t, b.m.Enable(bg))
			fault.inject(b)
			err := b.m.Restore(bg)
			if err == nil || !isUserError(err) {
				t.Fatalf("应返回 usererror：%v", err)
			}
			f := b.state()
			wantState := Restoring
			if fault.name == "第 1 次状态文件写入" {
				wantState = Enabled
			}
			if f.State != wantState {
				t.Fatalf("状态 %s，期望 %s", f.State, wantState)
			}
			if b.gate.IsOpen() {
				t.Fatal("闸门没关")
			}
			flushed := fault.name == "第 "+itoa(nWrites-1)+" 次状态文件写入" ||
				fault.name == "第 "+itoa(nWrites)+" 次状态文件写入"
			if !flushed && !b.fpf.Active(pf.DefaultAnchor) {
				t.Fatal("pf 应仍生效")
			}
			mustOK(t, b.m.Restore(bg))
			b.assertClean(b.init)
			b.noViolations()
		})
	}
}

// f. 已在目标状态时再调用：不产生任何改动调用。
func TestIdempotentNoop(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Restore(bg)) // RESTORED 时 Restore
	if tl := b.tl(); len(tl) != 0 || b.kill.calls != 0 {
		t.Fatalf("RESTORED 时 Restore 有调用：%v", tl)
	}
	mustOK(t, b.m.Enable(bg))
	b.resetTimeline()
	mustOK(t, b.m.Enable(bg))
	if tl := b.tl(); len(tl) != 0 || b.pre.calls != 1 {
		t.Fatalf("ENABLED 时 Enable 有调用：%v", tl)
	}
	mustOK(t, b.m.Disable(bg, false))
	b.resetTimeline()
	mustOK(t, b.m.Disable(bg, true))
	if tl := b.tl(); len(tl) != 0 {
		t.Fatalf("DISABLED 时 Disable 有调用：%v", tl)
	}
	// RESTORED 时 Disable 报错，不改动。
	mustOK(t, b.m.Restore(bg))
	b.resetTimeline()
	if err := b.m.Disable(bg, false); err == nil || !isUserError(err) {
		t.Fatalf("%v", err)
	}
	if tl := b.tl(); len(tl) != 0 {
		t.Fatalf("%v", tl)
	}
	b.noViolations()
}

// g. 结束 Claude 失败时中止，什么都不变。
func TestRestoreKillFails(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	before, netBefore := b.state(), b.net.Snapshot()
	b.resetTimeline()
	b.kill.fail = errInjected
	err := b.m.Restore(bg)
	if !errors.Is(err, errInjected) || !isUserError(err) {
		t.Fatalf("%v", err)
	}
	if tl := b.tl(); len(tl) != 0 {
		t.Fatalf("有改动：%v", tl)
	}
	if !reflect.DeepEqual(b.state(), before) || !b.net.Snapshot().Equal(netBefore) || !b.gate.IsOpen() || !b.fpf.Active(pf.DefaultAnchor) {
		t.Fatal("状态变了")
	}
	b.noViolations()
}

// g. 从四种状态出发都能恢复。
func TestRestoreFromEachState(t *testing.T) {
	setups := map[State]func(b *bed){
		Enabled:  func(b *bed) { mustOK(b.t, b.m.Enable(bg)) },
		Disabled: func(b *bed) { mustOK(b.t, b.m.Enable(bg)); mustOK(b.t, b.m.Disable(bg, false)) },
		Enabling: func(b *bed) {
			b.exit.fail = errInjected
			b.vm.failStop = errInjected // 回滚失败，留在 ENABLING
			_ = b.m.Enable(bg)
		},
		Restoring: func(b *bed) {
			mustOK(b.t, b.m.Enable(bg))
			b.net.FailNext(sysnet.OpIPv6Restore, errInjected)
			_ = b.m.Restore(bg)
		},
	}
	for st, setup := range setups {
		t.Run(string(st), func(t *testing.T) {
			b := newBed(t)
			setup(b)
			if got := b.state().State; got != st {
				t.Fatalf("准备阶段：%s", got)
			}
			mustOK(t, b.m.Restore(bg))
			b.assertClean(b.init)
			b.noViolations()
		})
	}
}

// h. IPv6：失败回到 DISABLED、再次启用时快照都不被覆盖；最终四种模式逐项还原。
func TestIPv6SnapshotKept(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	mustOK(t, b.m.Disable(bg, false))
	b.failNthNet(sysnet.OpIPv6Off, 2) // 关掉第一个服务后失败
	if err := b.m.Enable(bg); err == nil {
		t.Fatal("应失败")
	}
	b.assertDisabled()
	if !reflect.DeepEqual(b.state().IPv6Original, b.init.IPv6) {
		t.Fatal("失败后快照被覆盖")
	}
	mustOK(t, b.m.Enable(bg))
	if !reflect.DeepEqual(b.state().IPv6Original, b.init.IPv6) {
		t.Fatal("再次启用后快照被覆盖")
	}
	mustOK(t, b.m.Restore(bg))
	got := b.net.Snapshot().IPv6
	modes := map[sysnet.IPv6Mode]bool{}
	for svc, want := range b.init.IPv6 {
		if got[svc] != want {
			t.Fatalf("%s：%+v，期望 %+v", svc, got[svc], want)
		}
		modes[want.Mode] = true
	}
	if !modes[sysnet.IPv6Automatic] || !modes[sysnet.IPv6Manual] || !modes[sysnet.IPv6LinkLocal] || !modes[sysnet.IPv6Off] || !b.init.IPv6["Ethernet"].Router.IsValid() {
		t.Fatal("初始值应含四种模式、Manual 带路由器")
	}
	b.assertClean(b.init)
	b.noViolations()
}

// h. 快照里的服务在恢复前被删掉：恢复仍完成，有事件。
func TestIPv6ServiceGone(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	b.net.RemoveService("Thunder")
	mustOK(t, b.m.Restore(bg))
	want := initialNet()
	delete(want.IPv6, "Thunder")
	b.assertClean(want)
	if b.countEvents(EvServiceGone) != 1 {
		t.Fatal("没有服务消失的事件")
	}
	b.noViolations()
}

// i. token：中途 pf 被关后再次应用拿到第二个 token，恢复时两个都释放。
func TestTwoTokens(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	mustOK(t, b.m.Disable(bg, false))
	b.mu.Lock()
	b.noCheck = true // 别的软件 pfctl -d 的瞬间本来就不受保护
	b.mu.Unlock()
	b.fpf.Disable()
	b.mu.Lock()
	b.noCheck = false
	b.mu.Unlock()
	mustOK(t, b.m.Enable(bg))
	toks := b.state().PFTokens
	if len(toks) != 2 || toks[0] == toks[1] {
		t.Fatalf("应有两个 token：%v", toks)
	}
	b.resetTimeline()
	mustOK(t, b.m.Restore(bg))
	var released []string
	for _, s := range b.tl() {
		if r, ok := strings.CutPrefix(s, "pf -X "); ok {
			released = append(released, r)
		} else if r, ok := strings.CutPrefix(s, "pf-failed -X "); ok {
			released = append(released, r) // 第一个已经作废（pf 被关过），释放失败也算释放过
		}
	}
	if !slices.Equal(released, toks) {
		t.Fatalf("释放了 %v，期望 %v", released, toks)
	}
	b.assertClean(b.init)
	b.noViolations()
}

// i. pending 为 true 且文件里没有对应 token：恢复仍完成、有事件、没有 -d。
func TestPendingUnknownToken(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	// 模拟崩溃在 -E 成功与落盘之间：pf 多了一个文件里没有的启用引用。
	_, _, err := (&recRunner{R: b.fpf, b: b}).Run(bg, []string{"-E"}, nil)
	mustOK(t, err)
	mustOK(t, b.store.SetEnablePending())
	mustOK(t, b.m.Restore(bg))
	if b.countEvents(EvTokenUnknown) != 1 {
		t.Fatal("没有 token 未知的事件")
	}
	for _, s := range b.tl() {
		if s == "pf -d" {
			t.Fatal("执行了 pfctl -d")
		}
	}
	f := b.state()
	if f.State != Restored || f.PFEnablePending {
		t.Fatalf("%+v", f)
	}
	if b.fpf.Active(pf.DefaultAnchor) {
		t.Fatal("锚点还在生效")
	}
	// 漏掉的那个引用留着（pf 启用、锚点为空，无害）。
	if len(b.fpf.Tokens()) != 1 {
		t.Fatalf("%v", b.fpf.Tokens())
	}
	if !b.net.Snapshot().Equal(b.init) {
		t.Fatal("假系统没还原")
	}
	b.noViolations()
}

// j. 并发：Enable 进行中调用 Restore，恰有一个返回 ErrBusy。
func TestConcurrentBusyExactlyOne(t *testing.T) {
	b := newBed(t)
	b.pre.entered, b.pre.block = make(chan struct{}), make(chan struct{})
	errs := make(chan error, 2)
	go func() { errs <- b.m.Enable(bg) }()
	<-b.pre.entered
	go func() { errs <- b.m.Restore(bg) }()
	first := <-errs // Restore：Enable 还卡在冲突检查里
	close(b.pre.block)
	second := <-errs
	busy := 0
	for _, e := range []error{first, second} {
		if errors.Is(e, ErrBusy) {
			busy++
			if !isUserError(e) {
				t.Fatal("ErrBusy 应是 usererror")
			}
		} else if e != nil {
			t.Fatal(e)
		}
	}
	if busy != 1 {
		t.Fatalf("ErrBusy 次数 %d", busy)
	}
	b.assertEnabled()
	// 两个都不阻塞时随便并发也不出错（-race）。
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(2)
		go func() { defer wg.Done(); _ = b.m.Disable(bg, false) }()
		go func() { defer wg.Done(); _ = b.m.Enable(bg) }()
	}
	wg.Wait()
	b.noViolations()
}

// 释放失败是尽力而为：一次 Restore 就走到干净的 RESTORED，有事件，没有 -d。
func TestReleaseFailureBestEffort(t *testing.T) {
	cases := map[string]func(b *bed){
		// 不含 "invalid token" 的错误。
		"其他错误": func(b *bed) { b.pfw.failRelease(1) },
		// 别的软件 pfctl -d 让 token 作废：FakePF 报 "invalid token"。
		"token 已失效": func(b *bed) {
			b.mu.Lock()
			b.noCheck = true
			b.mu.Unlock()
			b.fpf.Disable()
			// 别的软件关 pf 之后马上又开了（否则锚点不生效、断言不成立）：pf 启用但没有我们的 token。
			_, _, err := b.fpf.Run(bg, []string{"-E"}, nil)
			mustOK(b.t, err)
			b.mu.Lock()
			b.noCheck = false
			b.mu.Unlock()
		},
	}
	for name, inject := range cases {
		t.Run(name, func(t *testing.T) {
			b := newBed(t)
			mustOK(t, b.m.Enable(bg))
			inject(b)
			b.resetTimeline()
			mustOK(t, b.m.Restore(bg))
			f := b.state()
			if f.State != Restored || len(f.PFTokens) != 0 {
				t.Fatalf("%+v", f)
			}
			if b.countEvents(EvTokenReleaseFailed) != 1 {
				t.Fatal("没有释放失败的事件")
			}
			for _, s := range b.tl() {
				if s == "pf -d" {
					t.Fatal("执行了 pfctl -d")
				}
			}
			if !b.net.Snapshot().Equal(b.init) || b.fpf.Active(pf.DefaultAnchor) || b.tun.Name() != "" {
				t.Fatal("没有回国干净")
			}
			b.noViolations()
		})
	}
}

// 从 RESTORED 来源的启用回滚里释放失败，同样回到 RESTORED。
func TestRollbackReleaseFailure(t *testing.T) {
	b := newBed(t)
	b.exit.fail = errInjected
	b.pfw.failRelease(1)
	err := b.m.Enable(bg)
	if err == nil || strings.Contains(err.Error(), "回滚也失败") {
		t.Fatalf("%v", err)
	}
	f := b.state()
	if f.State != Restored || len(f.PFTokens) != 0 || f.PFEnablePending {
		t.Fatalf("%+v", f)
	}
	if b.countEvents(EvTokenReleaseFailed) != 1 || !b.net.Snapshot().Equal(b.init) || b.fpf.Active(pf.DefaultAnchor) {
		t.Fatal("没有回国干净")
	}
	b.noViolations()
}

// 调用方的 ctx 在启用中途取消：回滚不跟着取消，回到干净的 RESTORED。
func TestEnableCancelledRollsBack(t *testing.T) {
	b := newBed(t)
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	b.exit.hook = cancel
	err := b.m.Enable(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	b.assertClean(b.init)
	b.noViolations()
}

// Restore 写完 RESTORING 后取消 ctx，仍走到 RESTORED。
func TestRestoreCancelledContinues(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	b.vm.onStop = cancel // ② 关虚拟机时（RESTORING 已落盘）取消
	mustOK(t, b.m.Restore(ctx))
	b.assertClean(b.init)
	b.noViolations()
}

// 目录 fsync 失败时 ENABLING 可能已经改名进去：照常回滚，回到干净的 RESTORED。
func TestEnablingDirSyncFailure(t *testing.T) {
	b := newBed(t)
	n := 0
	b.store.syncDir = func(dir string) error {
		n++
		if n == 1 {
			return errInjected
		}
		return fsyncDir(dir)
	}
	if err := b.m.Enable(bg); !errors.Is(err, errInjected) {
		t.Fatalf("%v", err)
	}
	if b.countEvents(EvRollback) != 1 {
		t.Fatal("应回滚")
	}
	b.assertClean(b.init)
	b.noViolations()
}

// M2-8 第 0 步：unknown 的服务启用和恢复全程不碰，有事件，恢复后 Equal。
func TestIPv6UnknownUntouched(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	mustOK(t, b.m.Disable(bg, false))
	mustOK(t, b.m.Enable(bg))
	mustOK(t, b.m.Restore(bg))
	for _, c := range b.net.Calls() {
		if strings.HasSuffix(c, " Odd VPN") || strings.Contains(c, "Odd VPN ") {
			t.Fatalf("碰了 unknown 的服务：%s", c)
		}
	}
	if b.countEvents(EvIPv6Unknown) != 2 {
		t.Fatalf("事件 %d 次", b.countEvents(EvIPv6Unknown))
	}
	if b.state().IPv6Original != nil {
		t.Fatal("恢复后快照应清掉")
	}
	b.assertClean(b.init)
	b.noViolations()
}

type fakeBaseline struct{ err error }

func (f fakeBaseline) Ready() error { return f.err }

// m4-plan v2.2 决定 4、6：没有基准时启用被拒、什么都不改；第 ⑤ 步没通过时回滚、闸门从未打开；
// 通过后第 ⑧ 步清 state 原因、交给复查放行（AfterEnable 恰好一次）。
func TestEnableBaselineAndExitCheck(t *testing.T) {
	b := newBed(t)
	b.m.D.Baseline = fakeBaseline{errInjected}
	if err := b.m.Enable(bg); err == nil || !isUserError(err) {
		t.Fatalf("没有基准时应拒绝：%v", err)
	}
	if c := b.changes(); len(c) != 0 || b.state().State != Restored {
		t.Fatalf("被拒时不应有任何改动：%v", c)
	}
	b.m.D.Baseline = fakeBaseline{}
	b.exit.fail = errInjected
	if err := b.m.Enable(bg); err == nil {
		t.Fatal("第 ⑤ 步没通过应失败")
	}
	if b.state().State != Restored || b.gate.Opens() != 0 || !b.gate.has(gatesm.ReasonState) {
		t.Fatal("回滚后应回到 RESTORED，闸门从未打开，state 原因还在")
	}
	mustOK(t, b.m.Enable(bg))
	if b.re.afterEnable != 1 || !b.gate.IsOpen() || b.gate.has(gatesm.ReasonState) {
		t.Fatal("通过后应清 state、AfterEnable 一次并放行")
	}
	b.assertEnabled()
	b.noViolations()
}
