package state

import (
	"errors"
	"testing"
)

// 借用虚拟机的互斥入口（docs/vm-borrow-plan.md v2 必须改 1）。

// 持有 ExclusiveHold 时清空回国拦截锚点：一轮 Tick 内重新加载；fn 返回后 held 为假。
func TestExclusiveHoldPatrolsReturnBlock(t *testing.T) {
	b := newBed(t)
	b.rb = true
	mustOK(t, b.m.Enable(bg))
	mustOK(t, b.m.Restore(bg))
	s, _ := b.newSup()
	err := b.m.ExclusiveHold(func() error {
		if !b.m.held.Load() {
			t.Fatal("持有期间 held 应为真")
		}
		b.fpf.ClearAnchor()
		if b.isReturnBlock() {
			t.Fatal("锚点应已被清掉")
		}
		mustOK(t, s.Tick(bg))
		if !b.isReturnBlock() {
			t.Fatal("借用期间巡检应重新加载回国拦截")
		}
		return nil
	})
	mustOK(t, err)
	if b.m.held.Load() {
		t.Fatal("结束后 held 应为假")
	}
	b.assertRestoredRB(b.init, 0)
	b.noViolations()
}

// 持有普通的 Exclusive 时：Tick 照旧整轮跳过，不重载。
func TestExclusiveSkipsTick(t *testing.T) {
	b := newBed(t)
	b.rb = true
	mustOK(t, b.m.Enable(bg))
	mustOK(t, b.m.Restore(bg))
	s, _ := b.newSup()
	mustOK(t, b.m.Exclusive(func() error {
		b.fpf.ClearAnchor()
		b.fpf.ResetCalls()
		mustOK(t, s.Tick(bg))
		if c := b.fpf.Calls(); len(c) != 0 || b.isReturnBlock() {
			t.Fatalf("普通持锁者期间 Tick 应跳过：%v", c)
		}
		return nil
	}))
	mustOK(t, s.Tick(bg)) // 放锁后照常补上
	b.assertRestoredRB(b.init, 0)
}

// 持有 ExclusiveHold、但状态不是 RESTORED（停用）或不在回国拦截模式：Tick 什么都不做。
func TestExclusiveHoldOtherStatesNoop(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		b := newBed(t)
		b.rb = true
		mustOK(t, b.m.Enable(bg))
		mustOK(t, b.m.Disable(bg, false))
		s, _ := b.newSup()
		mustOK(t, b.m.ExclusiveHold(func() error {
			b.fpf.ResetCalls()
			mustOK(t, s.Tick(bg))
			if c := b.fpf.Calls(); len(c) != 0 {
				t.Fatalf("停用状态下借用期间 Tick 不应碰 pf：%v", c)
			}
			return nil
		}))
	})
	t.Run("restored_no_return_block", func(t *testing.T) {
		b := newBed(t)
		s, _ := b.newSup()
		mustOK(t, b.m.ExclusiveHold(func() error {
			b.fpf.ResetCalls()
			mustOK(t, s.Tick(bg))
			if c := b.fpf.Calls(); len(c) != 0 {
				t.Fatalf("不在回国拦截模式时 Tick 不应碰 pf：%v", c)
			}
			return nil
		}))
	})
}

// 持有期间启用、恢复、Exclusive 都得到忙；Admit 拒绝时 ExclusiveHold 不执行 fn、不置 held。
func TestExclusiveHoldBusyAndAdmit(t *testing.T) {
	b := newBed(t)
	mustOK(t, b.m.ExclusiveHold(func() error {
		for name, err := range map[string]error{
			"Enable":    b.m.Enable(bg),
			"Restore":   b.m.Restore(bg),
			"Exclusive": b.m.Exclusive(func() error { return nil }),
			"Disable":   b.m.Disable(bg, false),
		} {
			if !errors.Is(err, ErrBusy) {
				t.Errorf("%s 应得到忙：%v", name, err)
			}
		}
		return nil
	}))
	deny := errors.New("已卸载")
	b.m.Admit = func() error { return deny }
	ran := false
	if err := b.m.ExclusiveHold(func() error { ran = true; return nil }); !errors.Is(err, deny) || ran || b.m.held.Load() {
		t.Fatalf("Admit 拒绝时不应执行：err=%v ran=%v held=%v", err, ran, b.m.held.Load())
	}
}
