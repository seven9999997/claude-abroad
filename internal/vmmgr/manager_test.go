package vmmgr

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// 正常：Start 拉起、等到健康；健康时清掉原因并请复查补一轮（不直接开闸门）；Stop 请它退出、回到 idle。
func TestStartStop(t *testing.T) {
	r := newRig(t)
	if st := r.m.Status(); !st.Managed || st.Phase != PhaseIdle {
		t.Fatalf("初始状态 %+v", st)
	}
	if r.l.count() != 0 {
		t.Fatal("不需要虚拟机时不应拉起")
	}
	ch := r.startAsync()
	r.pump(r.phaseIs(PhaseRunning), "running")
	if err := <-ch; err != nil {
		t.Fatalf("Start：%v", err)
	}
	if _, ok := r.gate.vmReason(); ok {
		t.Fatal("健康后应清掉虚拟机原因")
	}
	if r.gate.kickCount() != 1 {
		t.Fatalf("健康后应请复查补一轮，kick=%d", r.gate.kickCount())
	}
	// Start 再调一次：已经健康，直接返回，不再拉起
	if err := r.m.Start(context.Background()); err != nil || r.l.count() != 1 {
		t.Fatalf("重复 Start：%v，拉起 %d 次", err, r.l.count())
	}
	p := r.l.last()
	if err := r.m.Stop(context.Background()); err != nil {
		t.Fatalf("Stop：%v", err)
	}
	if !p.termed || p.killed {
		t.Fatalf("Stop 应先请它退出（termed=%v killed=%v）", p.termed, p.killed)
	}
	if _, ok := r.gate.vmReason(); ok {
		t.Fatal("不需要虚拟机时由状态原因管闸门，虚拟机原因应清掉")
	}
	if r.l.maxAlive != 1 {
		t.Fatalf("同时存活 %d 个", r.l.maxAlive)
	}
}

// a-1 启动失败（拉不起来）：每次都失败、闸门关着；退避递增有上限；到上限停止重启、Start 报错；重新启用时计数清零重来。
func TestLaunchFails(t *testing.T) {
	r := newRig(t)
	r.l.set(func(l *fakeLauncher) { l.failNext = 100 })
	ch := r.startAsync()
	r.pump(r.phaseIs(PhaseFailed), "failed")
	err := <-ch
	if !errors.Is(err, ErrFailed) {
		t.Fatalf("Start 应报连续失败：%v", err)
	}
	r.wantGateClosed("停止重启后")
	st := r.m.Status()
	if st.Failures != testTiming.MaxFailures || r.l.count() != testTiming.MaxFailures || st.LastError == "" {
		t.Fatalf("状态 %+v，拉起 %d 次", st, r.l.count())
	}
	// 退避：1s、2s、4s（上限 8s），看管理器算出的值
	if got := r.backoffs(); !slices.Equal(got, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}) {
		t.Fatalf("退避应递增：%v", got)
	}
	// 停止重启后不再拉起
	before := r.l.count()
	for i := 0; i < 200; i++ {
		r.clock.Advance(time.Second)
	}
	time.Sleep(5 * time.Millisecond)
	if r.l.count() != before {
		t.Fatal("停止重启后又拉起了")
	}
	// 重新启用：计数清零，这次拉起成功
	r.l.set(func(l *fakeLauncher) { l.failNext = 0 })
	ch = r.startAsync()
	r.pump(r.phaseIs(PhaseRunning), "重试后 running")
	if err := <-ch; err != nil {
		t.Fatalf("重试 Start：%v", err)
	}
	if st := r.m.Status(); st.Failures != 0 {
		t.Fatalf("重试后计数应清零：%+v", st)
	}
}

// 退避有上限：断言管理器自己算出的退避值（日志“X 后重新拉起虚拟机”），不量假时钟上两次拉起的间隔
// （负载高时推进时钟的协程和登记定时器的协程错开，间隔会多出几格；architect 判 M6-3b）。
func TestBackoffCap(t *testing.T) {
	r := newRig(t, func(tm *Timing) { tm.MaxFailures = 8 })
	r.l.set(func(l *fakeLauncher) { l.failNext = 100 })
	r.m.SetWanted(true)
	r.pump(r.phaseIs(PhaseFailed), "failed")
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second, 8 * time.Second}
	if got := r.backoffs(); !slices.Equal(got, want) {
		t.Fatalf("退避应翻倍到上限 %s：%v", testTiming.BackoffMax, got)
	}
}

// a-2 启动后健康检查一直不通过：StartTimeout 后结束它、算一次失败、闸门一直关着，退避后重新拉起。
func TestNeverHealthy(t *testing.T) {
	r := newRig(t)
	r.l.set(func(l *fakeLauncher) { l.healthy = false })
	r.m.SetWanted(true)
	r.pump(func() bool { return r.l.count() >= 2 }, "第二次拉起")
	first := r.l.procs[0]
	if !first.termed {
		t.Fatal("没就绪的虚拟机应被结束")
	}
	r.wantGateClosed("一直没就绪")
	if st := r.m.Status(); st.Failures < 1 || st.LastError == "" || st.Restarts < 1 {
		t.Fatalf("状态 %+v", st)
	}
	if r.gate.kickCount() != 0 {
		t.Fatal("没健康过不应请复查放行")
	}
}

// a-3 运行中被杀：立即关闸门（带原因），退避后重新拉起，重新健康后清原因、请复查。
func TestCrashWhileRunning(t *testing.T) {
	r := newRig(t)
	r.m.SetWanted(true)
	r.pump(r.phaseIs(PhaseRunning), "running")
	r.l.last().exit(errors.New("signal: killed"))
	r.settle(func() bool { _, ok := r.gate.vmReason(); return ok }, "崩溃后闸门关")
	r.pump(func() bool { return r.l.count() == 2 && r.m.Status().Phase == PhaseRunning }, "重新拉起并健康")
	if _, ok := r.gate.vmReason(); ok {
		t.Fatal("重新健康后应清掉原因")
	}
	if r.gate.kickCount() != 2 {
		t.Fatalf("每次健康请复查一次，kick=%d", r.gate.kickCount())
	}
	if st := r.m.Status(); st.Restarts != 1 || st.Failures != 1 {
		t.Fatalf("状态 %+v", st)
	}
	// 稳定运行满 StableFor：连续失败计数清零
	r.pump(func() bool { return r.m.Status().Failures == 0 }, "稳定后清零")
}

// a-4 运行中健康检查超时：连续 PatrolFailures 次后关闸门；恢复时清原因并请复查；一直不恢复就在 UnhealthyKill 后结束重启。
func TestPatrolTimeout(t *testing.T) {
	r := newRig(t)
	r.m.SetWanted(true)
	r.pump(r.phaseIs(PhaseRunning), "running")
	kicks := r.gate.kickCount()
	r.l.setHealthy(false)
	r.pump(r.phaseIs(PhaseUnhealthy), "unhealthy")
	r.wantGateClosed("健康检查连续不通过")
	// 恢复
	r.l.setHealthy(true)
	r.pump(r.phaseIs(PhaseRunning), "恢复 running")
	if _, ok := r.gate.vmReason(); ok {
		t.Fatal("恢复后应清掉原因")
	}
	if r.gate.kickCount() != kicks+1 {
		t.Fatal("恢复后应请复查补一轮（闸门不直接开）")
	}
	if r.l.count() != 1 {
		t.Fatal("短暂不健康不应重启进程")
	}
	// 一直不恢复：结束并重新拉起
	r.l.setHealthy(false)
	r.pump(func() bool { return r.l.count() == 2 }, "不健康太久后重启")
	if !r.l.procs[0].termed {
		t.Fatal("应先请旧进程退出")
	}
	r.wantGateClosed("不健康重启期间")
}

// a-5 退出时卡住不退：请退出 → 超时强制结束 → 仍不退就报告卡住，不拉起第二个；它最终退出后才继续。
func TestStuckOnStop(t *testing.T) {
	r := newRig(t)
	r.l.set(func(l *fakeLauncher) { l.ignoreTerm, l.ignoreKill = true, true })
	r.m.SetWanted(true)
	r.pump(r.phaseIs(PhaseRunning), "running")
	p := r.l.last()
	errc := make(chan error, 1)
	go func() { errc <- r.m.Stop(context.Background()) }()
	r.pump(r.phaseIs(PhaseStuck), "stuck")
	if err := <-errc; !errors.Is(err, ErrStuck) {
		t.Fatalf("Stop 应报卡住：%v", err)
	}
	if !p.termed || !p.killed {
		t.Fatalf("应先请退出再强制结束（termed=%v killed=%v）", p.termed, p.killed)
	}
	r.wantGateClosed("卡住")
	// 卡住期间重新要虚拟机：不拉起第二个
	r.m.SetWanted(true)
	for i := 0; i < 100; i++ {
		r.clock.Advance(time.Second)
	}
	time.Sleep(5 * time.Millisecond)
	if r.l.count() != 1 {
		t.Fatal("卡住期间拉起了第二个虚拟机")
	}
	// 它终于退出：继续（仍需要 → 重新拉起）
	r.l.set(func(l *fakeLauncher) { l.ignoreTerm, l.ignoreKill = false, false })
	p.exit(nil)
	r.pump(func() bool { return r.l.count() == 2 && r.m.Status().Phase == PhaseRunning }, "卡住的退出后重新拉起")
	if r.l.maxAlive != 1 {
		t.Fatalf("同时存活 %d 个", r.l.maxAlive)
	}
}

// 退出时请退出不理、强制结束有效：Stop 成功。
func TestStopNeedsKill(t *testing.T) {
	r := newRig(t)
	r.l.set(func(l *fakeLauncher) { l.ignoreTerm = true })
	r.m.SetWanted(true)
	r.pump(r.phaseIs(PhaseRunning), "running")
	p := r.l.last()
	errc := make(chan error, 1)
	go func() { errc <- r.m.Stop(context.Background()) }()
	r.pump(r.phaseIs(PhaseIdle), "idle")
	if err := <-errc; err != nil || !p.killed {
		t.Fatalf("Stop：%v killed=%v", err, p.killed)
	}
}

// a-6 重启风暴：进程拉起后马上崩，连续失败；退避递增；到上限停止重启并保持断网。
func TestCrashStorm(t *testing.T) {
	r := newRig(t)
	r.l.set(func(l *fakeLauncher) { l.healthy = false })
	r.m.SetWanted(true)
	for i := 0; i < testTiming.MaxFailures; i++ {
		r.pump(func() bool { return r.l.count() == i+1 }, "拉起")
		r.l.last().exit(errors.New("exit status 1"))
	}
	r.pump(r.phaseIs(PhaseFailed), "failed")
	r.wantGateClosed("风暴后")
	if got := r.backoffs(); !slices.Equal(got, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}) {
		t.Fatalf("退避应递增：%v", got)
	}
	if r.l.maxAlive != 1 {
		t.Fatalf("同时存活 %d 个", r.l.maxAlive)
	}
}

// d abroadd 退出（ctx 结束）：请自己拉起的虚拟机退出，Run 返回；之后 Start 报管理器已停止。
func TestShutdownStopsVM(t *testing.T) {
	r := newRig(t)
	r.m.SetWanted(true)
	r.pump(r.phaseIs(PhaseRunning), "running")
	p := r.l.last()
	r.cancel()
	r.pump(func() bool {
		select {
		case <-r.done:
			return true
		default:
			return false
		}
	}, "Run 退出")
	if !p.termed {
		t.Fatal("abroadd 退出时应关闭虚拟机")
	}
	select {
	case <-p.Done():
	default:
		t.Fatal("虚拟机没有退出")
	}
	if err := r.m.Start(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("停止后 Start：%v", err)
	}
}

// 退避期间不再需要虚拟机：回到 idle，不再拉起。
func TestUnwantDuringBackoff(t *testing.T) {
	r := newRig(t)
	r.l.set(func(l *fakeLauncher) { l.failNext = 100 })
	r.m.SetWanted(true)
	r.pump(r.phaseIs(PhaseBackoff), "backoff")
	if err := r.m.Stop(context.Background()); err != nil {
		t.Fatalf("Stop：%v", err)
	}
	n := r.l.count()
	for i := 0; i < 100; i++ {
		r.clock.Advance(time.Second)
	}
	time.Sleep(5 * time.Millisecond)
	if r.l.count() != n || r.m.Status().Phase != PhaseIdle {
		t.Fatalf("不需要后又拉起了（%d→%d，%s）", n, r.l.count(), r.m.Status().Phase)
	}
}

// Start 的时限：一直起不来又没到失败上限时，StartWait 到点返回面向用户的错误。
func TestStartWaitTimeout(t *testing.T) {
	r := newRig(t, func(tm *Timing) { tm.StartWait = 5 * time.Second })
	r.l.set(func(l *fakeLauncher) { l.healthy = false })
	ch := r.startAsync()
	var err error
	r.pump(func() bool {
		select {
		case err = <-ch:
			return true
		default:
			return false
		}
	}, "Start 返回")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("Start：%v", err)
	}
}
