package vmhost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"claude-abroad/internal/agent"
)

func runSup(t *testing.T, s *Supervisor) (context.CancelFunc, chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(cancel)
	return cancel, done
}

func waitErr(t *testing.T, done chan error, within time.Duration) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(within):
		t.Fatalf("Run 没有在 %s 内返回", within)
		return nil
	}
}

func next(t *testing.T, f *fakeFleet) *fakeMachine {
	t.Helper()
	select {
	case m := <-f.created:
		return m
	case <-time.After(3 * time.Second):
		t.Fatal("没有创建新的虚拟机")
		return nil
	}
}

func TestGracefulStop(t *testing.T) {
	f := newFleet(nil)
	var shutdowns atomic.Int32
	s := &Supervisor{Factory: f.factory, Timing: testTiming, Health: healthOK,
		Shutdown: func(_ context.Context, m Machine) error {
			shutdowns.Add(1)
			go m.(*fakeMachine).stop(errors.New("guest poweroff"))
			return nil
		}}
	cancel, done := runSup(t, s)
	m := next(t, f)
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := waitErr(t, done, time.Second); err != nil {
		t.Fatal(err)
	}
	if shutdowns.Load() != 1 || m.forceStops.Load() != 0 {
		t.Fatalf("应只走关机请求：shutdown %d，强停 %d", shutdowns.Load(), m.forceStops.Load())
	}
}

func TestShutdownTimeoutForceStops(t *testing.T) {
	f := newFleet(nil)
	s := &Supervisor{Factory: f.factory, Timing: testTiming, Health: healthOK,
		Shutdown: func(context.Context, Machine) error { return nil }} // 应答了但不关机
	cancel, done := runSup(t, s)
	m := next(t, f)
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	cancel()
	waitErr(t, done, time.Second)
	if m.forceStops.Load() != 1 || time.Since(start) < testTiming.StopWait {
		t.Fatalf("应等 StopWait 后强停：强停 %d，用时 %s", m.forceStops.Load(), time.Since(start))
	}
}

func TestForceSignalDuringGracefulStop(t *testing.T) {
	f := newFleet(nil)
	force := make(chan struct{})
	tm := testTiming
	tm.StopWait = 5 * time.Second
	s := &Supervisor{Factory: f.factory, Timing: tm, Health: healthOK, Force: force,
		Shutdown: func(context.Context, Machine) error { return nil }}
	cancel, done := runSup(t, s)
	m := next(t, f)
	time.Sleep(20 * time.Millisecond)
	cancel()
	time.Sleep(20 * time.Millisecond)
	close(force)
	waitErr(t, done, time.Second)
	if m.forceStops.Load() != 1 {
		t.Fatal("第二次信号应立即强停")
	}
}

func TestWatchdogWithHangingDial(t *testing.T) {
	// 拨号永不返回（直到 ctx 结束）：看门狗仍按时触发，停止也按时完成。
	f := newFleet(func(m *fakeMachine) {
		m.dial = func(ctx context.Context, _ uint32) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() }
	})
	s := &Supervisor{Factory: f.factory, Timing: testTiming} // 默认的 HTTP health / shutdown
	cancel, done := runSup(t, s)
	m0 := next(t, f)
	start := time.Now()
	next(t, f) // 看门狗强停后重启
	if m0.forceStops.Load() != 1 {
		t.Fatal("看门狗应强停第一台")
	}
	if d := time.Since(start); d < testTiming.Watchdog || d > testTiming.Watchdog+time.Second {
		t.Fatalf("看门狗用时 %s", d)
	}
	cancel()
	waitErr(t, done, testTiming.ShutdownTimeout+2*testTiming.StopWait+time.Second)
}

// 断言监护器决定的退避值（日志“X 后重启虚拟机”），不按实际经过的毫秒数判（-race、负载高时会偶发失败）。
func TestBackoffDoublesAndCaps(t *testing.T) {
	// 每台启动后立刻退出（没通过 health）：退避 20、40、80、80 毫秒。
	f := newFleet(func(m *fakeMachine) { go m.stop(errors.New("crash")) })
	var mu sync.Mutex
	var backoffs []time.Duration
	s := &Supervisor{Factory: f.factory, Timing: testTiming, Health: healthNever, Log: func(format string, a ...any) {
		if d, ok := strings.CutSuffix(fmt.Sprintf(format, a...), " 后重启虚拟机"); ok {
			if v, err := time.ParseDuration(d); err == nil {
				mu.Lock()
				backoffs = append(backoffs, v)
				mu.Unlock()
			}
		}
	}}
	cancel, _ := runSup(t, s)
	for i := 0; i < 5; i++ {
		next(t, f)
	}
	cancel()
	mu.Lock()
	defer mu.Unlock()
	want := []time.Duration{20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond, 80 * time.Millisecond}
	if len(backoffs) < 4 || !slices.Equal(backoffs[:4], want) {
		t.Errorf("退避应为 %v（翻倍、到上限为止），得到 %v", want, backoffs)
	}
}

// 断言的是监护器决定的退避值（从“X 后重启虚拟机”的日志读出），不是实际经过的毫秒数：
// 带 -race、负载高时调度延迟会让实测间隔偏大，按毫秒阈值判会偶发失败（architect 判 M6-2b）。
// 存活时长离 StableFor 留足余量（短的远小于、长的远大于），“稳定与否”不受调度延迟影响。
func TestBackoffResetsOnlyAfterStable(t *testing.T) {
	run := func(lifetime time.Duration) []time.Duration {
		f := newFleet(func(m *fakeMachine) {
			go func() { time.Sleep(lifetime); m.stop(errors.New("crash")) }()
		})
		var mu sync.Mutex
		var backoffs []time.Duration
		s := &Supervisor{Factory: f.factory, Timing: testTiming, Health: healthOK, Log: func(format string, a ...any) {
			msg := fmt.Sprintf(format, a...)
			if d, ok := strings.CutSuffix(msg, " 后重启虚拟机"); ok {
				if v, err := time.ParseDuration(d); err == nil {
					mu.Lock()
					backoffs = append(backoffs, v)
					mu.Unlock()
				}
			}
		}}
		cancel, _ := runSup(t, s)
		for i := 0; i < 4; i++ {
			next(t, f)
		}
		cancel()
		mu.Lock()
		defer mu.Unlock()
		if len(backoffs) < 3 {
			t.Fatalf("退避日志只有 %d 条：%v", len(backoffs), backoffs)
		}
		return backoffs[:3]
	}
	// health 通后只活 30 毫秒（远小于 StableFor）：退避按 20、40、80 翻倍。
	short := run(30 * time.Millisecond)
	if want := []time.Duration{20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond}; !slices.Equal(short, want) {
		t.Errorf("不稳定时退避应翻倍：%v，应为 %v", short, want)
	}
	// 活过 StableFor（再多 150 毫秒余量）：每次都复位为 BackoffMin。
	long := run(testTiming.StableFor + 150*time.Millisecond)
	for i, b := range long {
		if b != testTiming.BackoffMin {
			t.Errorf("稳定运行后退避应复位：第 %d 次 %s（%v）", i+1, b, long)
		}
	}
}

func TestCancelDuringBackoff(t *testing.T) {
	tm := testTiming
	tm.BackoffMin = 10 * time.Second
	f := newFleet(func(m *fakeMachine) { go m.stop(errors.New("crash")) })
	s := &Supervisor{Factory: f.factory, Timing: tm, Health: healthNever}
	cancel, done := runSup(t, s)
	next(t, f)
	time.Sleep(30 * time.Millisecond)
	cancel()
	waitErr(t, done, 500*time.Millisecond)
}

func TestFirstStartFailureReturns(t *testing.T) {
	f := newFleet(func(m *fakeMachine) { m.startErr = errors.New("缺少授权") })
	s := &Supervisor{Factory: f.factory, Timing: testTiming}
	if err := s.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "缺少授权") {
		t.Fatalf("首次启动失败应直接返回：%v", err)
	}
	f = newFleet(nil)
	f.err = errors.New("配置无效")
	s = &Supervisor{Factory: f.factory, Timing: testTiming}
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("首次创建失败应直接返回")
	}
}

func TestLaterStartFailureBacksOff(t *testing.T) {
	n := 0
	f := newFleet(func(m *fakeMachine) {
		n++
		if n == 1 {
			go m.stop(errors.New("crash"))
		} else if n == 2 {
			m.startErr = errors.New("偶发失败")
		}
	})
	s := &Supervisor{Factory: f.factory, Timing: testTiming, Health: healthNever}
	cancel, done := runSup(t, s)
	next(t, f)
	next(t, f)
	next(t, f) // 第二台启动失败后仍会再试
	cancel()
	if err := waitErr(t, done, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestDialFollowsCurrentMachine(t *testing.T) {
	stopFirst := make(chan struct{})
	f := newFleet(func(m *fakeMachine) {
		id := m.id
		m.dial = func(context.Context, uint32) (net.Conn, error) {
			a, _ := net.Pipe()
			return &taggedConn{Conn: a, id: id}, nil
		}
		if id == 0 {
			go func() { <-stopFirst; m.stop(errors.New("crash")) }()
		}
	})
	tm := testTiming
	tm.BackoffMin = 200 * time.Millisecond
	s := &Supervisor{Factory: f.factory, Timing: tm, Health: healthOK}
	cancel, _ := runSup(t, s)
	defer cancel()
	next(t, f)
	c, err := s.Dial(context.Background(), 1081)
	if err != nil || c.(*taggedConn).id != 0 {
		t.Fatalf("应拨到第一台：%v", err)
	}
	close(stopFirst)
	time.Sleep(50 * time.Millisecond) // 退避中
	if _, err := s.Dial(context.Background(), 1081); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("退避期间应立即报 ErrNotRunning：%v", err)
	}
	next(t, f)
	time.Sleep(10 * time.Millisecond)
	c, err = s.Dial(context.Background(), 1081)
	if err != nil || c.(*taggedConn).id != 1 {
		t.Fatalf("重启后应拨到新的一台：%v", err)
	}
}

type taggedConn struct {
	net.Conn
	id int
}

// 默认的 HTTP 探测对接真实的 agent 控制面处理器。
func TestHTTPHealthAndShutdownAgainstAgentHandler(t *testing.T) {
	l := newPipeListener()
	stopped := make(chan struct{})
	srv := agent.NewServer(agent.Handler(agent.NewState("6.12.115-0-virt"), func() { close(stopped) }))
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()
	m := newFakeMachine(0)
	m.dial = l.Dial

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if id, err := httpHealth(ctx, m); err != nil || len(id) != 32 {
		t.Fatalf("health：%q %v", id, err)
	}
	if err := httpShutdown(ctx, m); err != nil {
		t.Fatalf("shutdown：%v", err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("agent 没收到关机请求")
	}
	code, _, err := vsockRoundTrip(ctx, m, http.MethodGet, "/v1/nope")
	if err != nil || code != 404 {
		t.Fatalf("未知路径：%d %v", code, err)
	}
}
