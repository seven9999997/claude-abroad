package state

import (
	"slices"
	"testing"

	"claude-abroad/internal/pf"
)

// M6-11a：开关打开时，回国把锚点经一次加载换成回国拦截那一组（中间没有清空锚点），留一个 token；再启用时同样一次
// 加载换回接管规则；全程不变量成立（闸门开时生效的是接管规则、有接管痕迹时 pf 拦着）。
func TestReturnBlockNoGap(t *testing.T) {
	b := newBed(t)
	b.rb = true
	for round := 0; round < 2; round++ {
		mustOK(t, b.m.Enable(bg))
		b.fpf.ResetCalls()
		mustOK(t, b.m.Restore(bg))
		if c := b.fpf.Calls(); slices.Contains(c, "flush") {
			t.Fatalf("第 %d 轮回国清空过锚点（有空窗）：%v", round, c)
		}
		b.assertRestoredRB(b.init, 0)
		b.fpf.ResetCalls()
		mustOK(t, b.m.Enable(bg))
		if c := b.fpf.Calls(); slices.Contains(c, "flush") {
			t.Fatalf("第 %d 轮再启用清空过锚点（有空窗）：%v", round, c)
		}
		if b.isReturnBlock() || !b.blocked() {
			t.Fatal("启用后应是接管规则")
		}
	}
	mustOK(t, b.m.Restore(bg))
	b.assertRestoredRB(b.init, 0)
	b.noViolations()
}

// 开关关着：回国照旧清空锚点、释放全部 token（原断言）。
func TestReturnBlockOffUnchanged(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.Enable(bg))
	b.fpf.ResetCalls()
	mustOK(t, b.m.Restore(bg))
	if !slices.Contains(b.fpf.Calls(), "flush") {
		t.Fatalf("开关关着时应清空锚点：%v", b.fpf.Calls())
	}
	b.assertClean(b.init)
	if b.ppf.P.RestoredMode() {
		t.Fatal("开关关着时不应进入回国拦截模式")
	}
	// 监护在 RESTORED 里什么都不做
	s, _ := b.newSup()
	b.fpf.ResetCalls()
	mustOK(t, s.Tick(bg))
	if c := b.fpf.Calls(); len(c) != 0 {
		t.Fatalf("RESTORED 里不应碰 pf：%v", c)
	}
	b.noViolations()
}

// 已回国时巡检：锚点被清掉、pf 被关，监护的下一轮重新加载这一组并启用；新 token 取代旧的（pf 关过，旧的已失效）。
func TestReturnBlockPatrolReloads(t *testing.T) {
	b := newBed(t)
	b.rb = true
	mustOK(t, b.m.Enable(bg))
	mustOK(t, b.m.Restore(bg))
	s, _ := b.newSup()
	b.fpf.ClearAnchor()
	mustOK(t, s.Tick(bg))
	b.assertRestoredRB(b.init, 0)
	b.fpf.Disable() // 别的软件关了 pf：我们的 token 失效
	mustOK(t, s.Tick(bg))
	b.assertRestoredRB(b.init, 0)
	b.noViolations()
}

// 重启之后（pf 复位）Recover 把这一组加载回来，留下新拿到的 token；开关关掉之后（例如卸载过）不加载。
func TestReturnBlockAfterReboot(t *testing.T) {
	b := newBed(t)
	b.rb = true
	mustOK(t, b.m.Enable(bg))
	mustOK(t, b.m.Restore(bg))
	if _, err := b.reboot(); err != nil {
		t.Fatal(err)
	}
	if b.fpf.Raw(pf.DefaultAnchor) != nil {
		t.Fatal("重启后 RESTORED 时 guard 不应加载（M6-11b 再改）")
	}
	mustOK(t, b.m.Recover(bg))
	b.assertRestoredRB(b.init, 0)
	// 开关关掉：重启后不加载
	b.rb = false
	if _, err := b.reboot(); err != nil {
		t.Fatal(err)
	}
	mustOK(t, b.m.Recover(bg))
	if b.fpf.Raw(pf.DefaultAnchor) != nil || b.ppf.P.RestoredMode() {
		t.Fatal("开关关着时 Recover 不应加载回国拦截")
	}
	b.noViolations()
}
