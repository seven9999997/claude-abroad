package vmmgr

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 借用（docs/vm-borrow-plan.md v2 第 1 节第 2 步）。

type borrowResult struct {
	release  func(context.Context) error
	borrowed bool
	err      error
}

func (r *rig) borrowAsync() <-chan borrowResult {
	ch := make(chan borrowResult, 1)
	go func() {
		rel, b, err := r.m.Borrow(context.Background())
		ch <- borrowResult{rel, b, err}
	}()
	return ch
}

// waitBorrow 一边推进假时钟一边等 Borrow 返回。
func (r *rig) waitBorrow(ch <-chan borrowResult) borrowResult {
	r.t.Helper()
	var res borrowResult
	got := false
	r.pump(func() bool {
		select {
		case res = <-ch:
			got = true
		default:
		}
		return got
	}, "Borrow 返回")
	return res
}

// releaseAsync 在后台 release，推进时钟等它返回。
func (r *rig) releaseWait(rel func(context.Context) error) error {
	r.t.Helper()
	ch := make(chan error, 1)
	go func() { ch <- rel(context.Background()) }()
	var err error
	got := false
	r.pump(func() bool {
		select {
		case err = <-ch:
			got = true
		default:
		}
		return got
	}, "release 返回")
	return err
}

func goroutinesSettle(t *testing.T, base int) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if runtime.NumGoroutine() <= base+2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("协程没有回到基线：%d（基线 %d）", runtime.NumGoroutine(), base)
}

// 借用：拉起一次、等到健康；release 后进程已退出、回到 idle、Borrowed 清掉、协程不残留。
func TestBorrowStartsAndStops(t *testing.T) {
	r := newRig(t)
	base := runtime.NumGoroutine()
	res := r.waitBorrow(r.borrowAsync())
	if res.err != nil || !res.borrowed {
		t.Fatalf("Borrow：%v borrowed=%v", res.err, res.borrowed)
	}
	if st := r.m.Status(); st.Phase != PhaseRunning || !st.Borrowed || r.l.count() != 1 {
		t.Fatalf("借用中的状态 %+v，拉起 %d 次", st, r.l.count())
	}
	p := r.l.last()
	if err := r.releaseWait(res.release); err != nil {
		t.Fatalf("release：%v", err)
	}
	select {
	case <-p.Done():
	default:
		t.Fatal("release 返回时进程应已退出")
	}
	if st := r.m.Status(); st.Phase != PhaseIdle || st.Borrowed || st.Failures != 0 {
		t.Fatalf("release 后的状态 %+v", st)
	}
	if err := res.release(context.Background()); err != nil {
		t.Fatalf("重复 release 应是空操作：%v", err)
	}
	if _, ok := r.gate.vmReason(); ok {
		t.Fatal("借用结束后不应残留虚拟机原因")
	}
	goroutinesSettle(t, base)
}

// 已经要求常驻：不借用、不再拉起；release 不停。
func TestBorrowWhenWantedNoLaunch(t *testing.T) {
	r := newRig(t)
	ch := r.startAsync()
	r.pump(r.phaseIs(PhaseRunning), "running")
	if err := <-ch; err != nil {
		t.Fatal(err)
	}
	res := r.waitBorrow(r.borrowAsync())
	if res.err != nil || res.borrowed {
		t.Fatalf("常驻时不应借用：%v borrowed=%v", res.err, res.borrowed)
	}
	if err := r.releaseWait(res.release); err != nil {
		t.Fatal(err)
	}
	if r.l.count() != 1 || r.m.Status().Phase != PhaseRunning {
		t.Fatalf("常驻的虚拟机不应被停：拉起 %d 次，状态 %+v", r.l.count(), r.m.Status())
	}
}

// 借用期间被要求常驻（防御：正常流程被互斥挡住）：release 交还、不停。
func TestBorrowHandoverWhenWanted(t *testing.T) {
	r := newRig(t)
	res := r.waitBorrow(r.borrowAsync())
	if res.err != nil {
		t.Fatal(res.err)
	}
	r.m.SetWanted(true)
	if err := r.releaseWait(res.release); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		r.clock.Advance(time.Second)
	}
	time.Sleep(5 * time.Millisecond)
	if st := r.m.Status(); st.Phase != PhaseRunning || st.Borrowed || r.l.count() != 1 {
		t.Fatalf("交还后应常驻：%+v，拉起 %d 次", st, r.l.count())
	}
}

// 拉不起来：第一次失败就返回（不重试），类别是 launch，回到 idle。
func TestBorrowLaunchFailsNoRetry(t *testing.T) {
	r := newRig(t)
	r.l.set(func(l *fakeLauncher) { l.failNext = 100 })
	res := r.waitBorrow(r.borrowAsync())
	var be *BorrowError
	if !errors.As(res.err, &be) || be.Kind != KindLaunch || !strings.Contains(be.Error(), "拉起虚拟机失败") {
		t.Fatalf("应报拉不起来：%v", res.err)
	}
	r.pump(r.phaseIs(PhaseIdle), "idle")
	for i := 0; i < 100; i++ {
		r.clock.Advance(time.Second)
	}
	time.Sleep(5 * time.Millisecond)
	if r.l.count() != 1 {
		t.Fatalf("借用失败不应重试：拉起 %d 次", r.l.count())
	}
	if st := r.m.Status(); st.Failures != 0 || st.Borrowed {
		t.Fatalf("状态 %+v", st)
	}
}

// 拉起后一直不健康：到时限算没有就绪，进程被停掉。
func TestBorrowNotReadyTimeout(t *testing.T) {
	r := newRig(t)
	r.l.set(func(l *fakeLauncher) { l.healthy = false })
	res := r.waitBorrow(r.borrowAsync())
	var be *BorrowError
	if !errors.As(res.err, &be) || be.Kind != KindNotReady {
		t.Fatalf("应报没有就绪：%v", res.err)
	}
	r.pump(r.phaseIs(PhaseIdle), "idle")
	select {
	case <-r.l.last().Done():
	default:
		t.Fatal("没就绪的虚拟机应被停掉")
	}
	if r.l.count() != 1 {
		t.Fatalf("不应重试：拉起 %d 次", r.l.count())
	}
}

// release 时进程卡住：报告卡住，不拉起第二个。
func TestBorrowStuckOnRelease(t *testing.T) {
	r := newRig(t)
	r.l.set(func(l *fakeLauncher) { l.ignoreTerm, l.ignoreKill = true, true })
	res := r.waitBorrow(r.borrowAsync())
	if res.err != nil {
		t.Fatal(res.err)
	}
	if err := r.releaseWait(res.release); !errors.Is(err, ErrStuck) {
		t.Fatalf("release 应报卡住：%v", err)
	}
	// 卡住期间再借用：立即报卡住，不拉起第二个
	res2 := r.waitBorrow(r.borrowAsync())
	var be *BorrowError
	if !errors.As(res2.err, &be) || be.Kind != KindStuck {
		t.Fatalf("卡住时再借用应报卡住：%v", res2.err)
	}
	if r.l.count() != 1 || r.l.maxAlive != 1 {
		t.Fatalf("卡住时拉起了第二个：%d", r.l.count())
	}
	r.l.last().exit(nil)
	r.pump(r.phaseIs(PhaseIdle), "卡住的退出后回到 idle")
}

// 管理循环已经结束：借用报 ErrClosed。
func TestBorrowClosed(t *testing.T) {
	r := newRig(t)
	r.cancel()
	r.pump(func() bool {
		select {
		case <-r.done:
			return true
		default:
			return false
		}
	}, "Run 退出")
	if _, b, err := r.m.Borrow(context.Background()); !errors.Is(err, ErrClosed) || b {
		t.Fatalf("应报已停止：%v borrowed=%v", err, b)
	}
}

// 借用的失败不计入常驻时的连续失败：借用失败后再 Start，计数从 0 起、能成功。
func TestBorrowFailuresNotCounted(t *testing.T) {
	r := newRig(t)
	r.l.set(func(l *fakeLauncher) { l.failNext = 1 })
	if res := r.waitBorrow(r.borrowAsync()); res.err == nil {
		t.Fatal("应失败一次")
	}
	r.pump(r.phaseIs(PhaseIdle), "idle")
	if f := r.m.Status().Failures; f != 0 {
		t.Fatalf("借用失败后计数应清零：%d", f)
	}
	ch := r.startAsync()
	r.pump(r.phaseIs(PhaseRunning), "running")
	if err := <-ch; err != nil {
		t.Fatal(err)
	}
	if f := r.m.Status().Failures; f != 0 {
		t.Fatalf("Start 后计数：%d", f)
	}
}

// 借用期间闸门只出现虚拟机原因的加清：健康时清掉并请复查一轮，结束后没有残留。
func TestBorrowGateOnlyVMReason(t *testing.T) {
	r := newRig(t)
	res := r.waitBorrow(r.borrowAsync())
	if res.err != nil {
		t.Fatal(res.err)
	}
	if _, ok := r.gate.vmReason(); ok {
		t.Fatal("健康后虚拟机原因应清掉")
	}
	if err := r.releaseWait(res.release); err != nil {
		t.Fatal(err)
	}
	r.gate.mu.Lock()
	n := len(r.gate.reasons)
	r.gate.mu.Unlock()
	if n != 0 {
		t.Fatalf("借用结束后闸门原因应为空：%v", r.gate.reasons)
	}
}
