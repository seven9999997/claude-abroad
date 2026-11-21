package state

import (
	"claude-abroad/internal/sysnet"
	"slices"
	"strings"
	"testing"
)

// tlIndex 返回时间线里第一个以 prefix 开头的位置（没有时 -1），从 from 开始找。
func tlIndex(tl []string, prefix string, from int) int {
	for i := from; i < len(tl); i++ {
		if strings.HasPrefix(tl[i], prefix) {
			return i
		}
	}
	return -1
}

// g. 状态机的通知点（m3-plan v2.7 决定 3）。
func TestDataStateTimeline(t *testing.T) {
	b := newBed(t)
	// 启用：通知“启用”晚于 ENABLED 落盘、早于开闸门。
	b.resetTimeline()
	mustOK(t, b.m.Enable(bg))
	tl := b.tl()
	st, de, go_ := tlIndex(tl, "store ENABLED", 0), tlIndex(tl, "data enabled", 0), tlIndex(tl, "gate open", 0)
	if st < 0 || de < 0 || go_ < 0 || !(st < de && de < go_) {
		t.Fatalf("启用的顺序应是 ENABLED 落盘 → 通知启用 → 开闸门：%v", tl)
	}
	if tlIndex(tl, "data disabled", 0) >= 0 {
		t.Fatalf("启用成功时不应通知停用：%v", tl)
	}
	// 停用：通知“停用”早于关闸门。
	b.resetTimeline()
	mustOK(t, b.m.Disable(bg, false))
	tl = b.tl()
	dd, gc := tlIndex(tl, "data disabled", 0), tlIndex(tl, "gate close", 0)
	if dd < 0 || gc < 0 || dd > gc {
		t.Fatalf("停用的顺序应是 通知停用 → 关闸门：%v", tl)
	}
	// 再启用、再恢复：恢复一开始就通知停用（早于关闸门和写 RESTORING）。
	mustOK(t, b.m.Enable(bg))
	b.resetTimeline()
	mustOK(t, b.m.Restore(bg))
	tl = b.tl()
	dd, gc, sr := tlIndex(tl, "data disabled", 0), tlIndex(tl, "gate close", 0), tlIndex(tl, "store RESTORING", 0)
	if dd < 0 || dd > gc || dd > sr {
		t.Fatalf("恢复应一开始就通知停用：%v", tl)
	}
	if b.ds.get() {
		t.Fatal("恢复之后接收方应是停用")
	}
}

// g. 启用失败回滚到 DISABLED：最终通知与文件状态一致（停用）。
func TestDataStateRollback(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	mustOK(t, b.m.Disable(bg, false))
	b.exit.setAlways(errInjected)
	b.resetTimeline()
	if err := b.m.Enable(bg); err == nil {
		t.Fatal("出口检查失败时启用应失败")
	}
	if f := b.state(); f.State != Disabled || b.ds.get() {
		t.Fatalf("回滚到 DISABLED 后接收方应是停用：文件 %s，启用=%v", f.State, b.ds.get())
	}
	if !slices.Contains(b.tl(), "data disabled") {
		t.Fatalf("回滚应通知停用：%v", b.tl())
	}
	b.noViolations()
}

// g. 没有配置接收方时，行为与现在相同。
func TestNoDataStateReceiver(t *testing.T) {
	b := newBed(t)
	b.m.D.DataState = nil
	ds := b.ds
	b.ds = nil
	mustOK(t, b.m.Enable(bg))
	mustOK(t, b.m.Disable(bg, false))
	mustOK(t, b.m.Enable(bg))
	mustOK(t, b.m.Restore(bg))
	b.assertCleanTokens(b.init, 0)
	if ds.n != 0 {
		t.Fatal("没有配置时不应通知")
	}
	b.noViolations()
}

// c. 启用前的本机 DNS：从 RESTORED 启用时记下；从 DISABLED 再启用时不覆盖；恢复后清掉。
func TestOrigDNSRecorded(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	if f := b.state(); len(f.OrigDNS) != 1 || f.OrigDNS[0].String() != "192.168.1.1" {
		t.Fatalf("从 RESTORED 启用应记下系统 DNS：%v", f.OrigDNS)
	}
	mustOK(t, b.m.Disable(bg, false))
	b.net.SetSysDNSForTest("198.18.0.2") // 启用期间系统 DNS 是我们自己的
	mustOK(t, b.m.Enable(bg))
	if f := b.state(); len(f.OrigDNS) != 1 || f.OrigDNS[0].String() != "192.168.1.1" {
		t.Fatalf("从 DISABLED 再启用不应覆盖：%v", f.OrigDNS)
	}
	mustOK(t, b.m.Restore(bg))
	if f := b.state(); len(f.OrigDNS) != 0 {
		t.Fatalf("恢复后应清掉：%v", f.OrigDNS)
	}
	b.noViolations()
}

// 读不出本机 DNS 时不拦着启用，发事件。
func TestOrigDNSUnknown(t *testing.T) {
	if simBackend {
		t.Skip("命令模拟器直接读状态生成 scutil --dns，注入不了失败；这条测的是假实现一层的失败路径")
	}
	b := newBed(t)
	b.failNthNet(sysnet.OpDNSSystem, 1)
	mustOK(t, b.m.Enable(bg))
	if f := b.state(); f.State != Enabled || len(f.OrigDNS) != 0 {
		t.Fatalf("应照常启用、OrigDNS 为空：%+v", f)
	}
	if b.countEvents(EvOrigDNSUnknown) != 1 {
		t.Fatal("应发“读不出本机 DNS”事件")
	}
}
