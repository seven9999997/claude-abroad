package vmhost

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// scripted 按机器编号给出 health 序列：第 i 次探测返回 seq[i]（越界后重复最后一个）。
// "" 表示失败，其他值是 boot_id。
func scripted(seqs map[int][]string) func(context.Context, Machine) (string, error) {
	var mu sync.Mutex
	calls := map[int]int{}
	return func(_ context.Context, m Machine) (string, error) {
		fm := m.(*fakeMachine)
		mu.Lock()
		seq := seqs[fm.id]
		i := calls[fm.id]
		calls[fm.id]++
		mu.Unlock()
		if len(seq) == 0 {
			return "", errors.New("down")
		}
		if i >= len(seq) {
			i = len(seq) - 1
		}
		if seq[i] == "" {
			return "", errors.New("down")
		}
		return seq[i], nil
	}
}

func (f *fakeFleet) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.machines)
}

// a. 连续失败满 N 次才强停；中间成功一次就清零。
func TestPatrolFailuresThenRestart(t *testing.T) {
	f := newFleet(nil)
	s := &Supervisor{Factory: f.factory, Timing: testTiming,
		Health: scripted(map[int][]string{
			0: {"A", "", "", "A", "", "", "A", "", "", ""}, // 两次“失败两回又成功”，最后连续 3 次失败
			1: {"B"},
		})}
	cancel, _ := runSup(t, s)
	defer cancel()
	m0 := next(t, f)
	m1 := next(t, f)
	if m0.forceStops.Load() != 1 || m1.id != 1 {
		t.Fatalf("应在连续 3 次失败后强停并重启：强停 %d", m0.forceStops.Load())
	}
}

func TestPatrolSuccessResetsCounter(t *testing.T) {
	f := newFleet(nil)
	// 每 3 次里失败 2 次，永远凑不满连续 3 次。
	seq := []string{"A"}
	for i := 0; i < 100; i++ {
		seq = append(seq, "", "", "A")
	}
	s := &Supervisor{Factory: f.factory, Timing: testTiming, Health: scripted(map[int][]string{0: seq})}
	cancel, _ := runSup(t, s)
	defer cancel()
	m0 := next(t, f)
	time.Sleep(40 * testTiming.PatrolEvery)
	if m0.forceStops.Load() != 0 || f.count() != 1 {
		t.Fatal("中间有成功时不应强停")
	}
}

// b. 墙上时钟跳变（睡眠唤醒）时清零、这一轮不计失败。
func TestPatrolWakeDoesNotCount(t *testing.T) {
	f := newFleet(nil)
	var offset atomic.Int64
	s := &Supervisor{Factory: f.factory, Timing: testTiming,
		Health: scripted(map[int][]string{0: {"A", ""}}), // 通过一次后一直不通
		Now: func() time.Time {
			// 每次读时间都“睡过了一小时”：每一轮都被当作刚唤醒。
			return time.Now().Add(time.Duration(offset.Add(int64(time.Hour))))
		}}
	cancel, _ := runSup(t, s)
	defer cancel()
	m0 := next(t, f)
	time.Sleep(30 * testTiming.PatrolEvery)
	if m0.forceStops.Load() != 0 || f.count() != 1 {
		t.Fatalf("唤醒后的失败不应计数：强停 %d", m0.forceStops.Load())
	}
}

type readyCall struct {
	id  string
	ctx context.Context
}

// c. boot_id 变化：不强停、OnReady 再来一次、旧 ctx 被取消。
func TestBootIDChange(t *testing.T) {
	f := newFleet(nil)
	calls := make(chan readyCall, 8)
	s := &Supervisor{Factory: f.factory, Timing: testTiming,
		Health:  scripted(map[int][]string{0: {"A", "A", "A", "B"}}),
		OnReady: func(ctx context.Context, id string) { calls <- readyCall{id, ctx} }}
	cancel, _ := runSup(t, s)
	defer cancel()
	m0 := next(t, f)
	first := <-calls
	second := <-calls
	if first.id != "A" || second.id != "B" {
		t.Fatalf("OnReady：%s、%s", first.id, second.id)
	}
	select {
	case <-first.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("boot_id 变化后旧的 ctx 应被取消")
	}
	if second.ctx.Err() != nil {
		t.Fatal("新的 ctx 不应被取消")
	}
	time.Sleep(10 * testTiming.PatrolEvery)
	if m0.forceStops.Load() != 0 || f.count() != 1 {
		t.Fatal("boot_id 变化不应强停")
	}
	select {
	case c := <-calls:
		t.Fatalf("boot_id 不再变化时不应再调用 OnReady：%s", c.id)
	default:
	}
	m0.stop(errors.New("crash"))
	select {
	case <-second.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("虚拟机停止后 ctx 应被取消")
	}
}

// c（续）. boot_id 变化后稳定期从头算：这次退出不让退避复位。
func TestBootIDChangeRestartsStableClock(t *testing.T) {
	tm := testTiming
	tm.StableFor = 150 * time.Millisecond
	tm.BackoffMin = 30 * time.Millisecond
	tm.BackoffMax = 240 * time.Millisecond
	f := newFleet(func(m *fakeMachine) {
		switch m.id {
		case 0:
			go m.stop(errors.New("crash")) // 不健康就退出：退避升到 60
		case 1:
			// 健康后 200 毫秒 boot_id 变，再 50 毫秒退出：自首次健康已超过 StableFor，
			// 但自 boot_id 变化只有 50 毫秒，不能复位。
			go func() { time.Sleep(250 * time.Millisecond); m.stop(errors.New("crash")) }()
		}
	})
	var mu sync.Mutex
	start := map[int]time.Time{}
	s := &Supervisor{Factory: f.factory, Timing: tm,
		Health: func(ctx context.Context, m Machine) (string, error) {
			fm := m.(*fakeMachine)
			if fm.id != 1 {
				return healthNever(ctx, m)
			}
			mu.Lock()
			if start[1].IsZero() {
				start[1] = time.Now()
			}
			since := time.Since(start[1])
			mu.Unlock()
			if since > 200*time.Millisecond {
				return "B", nil
			}
			return "A", nil
		}}
	cancel, _ := runSup(t, s)
	defer cancel()
	next(t, f)
	m1 := next(t, f)
	<-m1.Done()
	stopped := time.Now()
	next(t, f)
	if gap := time.Since(stopped); gap < 55*time.Millisecond {
		t.Fatalf("boot_id 变化后不满稳定期，退避应为 60 毫秒，实际 %s", gap)
	}
}

// d. OnReady 每次开机只触发一次；阻塞的 OnReady 不影响停止。
func TestOnReadyOncePerBootAndNonBlocking(t *testing.T) {
	f := newFleet(nil)
	var n atomic.Int32
	s := &Supervisor{Factory: f.factory, Timing: testTiming, Health: healthOK,
		OnReady: func(context.Context, string) {
			n.Add(1)
			select {} // 永远阻塞，也不看 ctx
		},
		Shutdown: func(_ context.Context, m Machine) error {
			go m.(*fakeMachine).stop(errors.New("poweroff"))
			return nil
		}}
	cancel, done := runSup(t, s)
	next(t, f)
	time.Sleep(20 * testTiming.PatrolEvery)
	if n.Load() != 1 {
		t.Fatalf("OnReady 应只调用一次，实际 %d", n.Load())
	}
	cancel()
	if err := waitErr(t, done, time.Second); err != nil {
		t.Fatal(err)
	}
}

// e. 强停无效：看门狗、巡检、优雅停止三条路径都返回 ErrStuck，且不起第二台。
func TestStuckMachineNeverStartsSecond(t *testing.T) {
	stuck := func(m *fakeMachine) { m.stopOnForce = false }
	cases := map[string]struct {
		health   func(context.Context, Machine) (string, error)
		shutdown func(context.Context, Machine) error
		cancel   bool
	}{
		"看门狗":  {health: healthNever},
		"巡检":   {health: scripted(map[int][]string{0: {"A", ""}})},
		"优雅停止": {health: healthOK, shutdown: func(context.Context, Machine) error { return nil }, cancel: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFleet(stuck)
			s := &Supervisor{Factory: f.factory, Timing: testTiming, Health: c.health, Shutdown: c.shutdown}
			cancel, done := runSup(t, s)
			next(t, f)
			if c.cancel {
				time.Sleep(20 * time.Millisecond)
				cancel()
			}
			err := waitErr(t, done, 2*time.Second)
			if !errors.Is(err, ErrStuck) {
				t.Fatalf("应返回 ErrStuck：%v", err)
			}
			if f.count() != 1 {
				t.Fatalf("不应再起第二台：共 %d 台", f.count())
			}
		})
	}
}

// f. boot_id 为空、应答体超限都算 health 失败。
func TestHTTPHealthRejectsBadBody(t *testing.T) {
	for name, c := range map[string]struct {
		body string
		want string
	}{
		"没有 boot_id": {`{"ok":true}`, "boot_id"},
		"boot_id 为空": {`{"boot_id":""}`, "boot_id"},
		"不是 JSON":    {`nope`, "JSON"},
		"超过上限":       {`{"boot_id":"x","pad":"` + strings.Repeat("a", MaxHealthBody) + `"}`, "超过"},
	} {
		t.Run(name, func(t *testing.T) {
			l := newPipeListener()
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(c.body))
			})}
			go func() { _ = srv.Serve(l) }()
			defer srv.Close()
			m := newFakeMachine(0)
			m.dial = l.Dial
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := httpHealth(ctx, m); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("应报错（含“%s”）：%v", c.want, err)
			}
		})
	}
}

// 巡检探测是超时型失败（虚拟机卡死时的典型表现）时，探测耗时不能被误当成睡眠唤醒。
func TestPatrolTimeoutFailuresStillCount(t *testing.T) {
	f := newFleet(nil)
	var passed sync.Map
	s := &Supervisor{Factory: f.factory, Timing: testTiming,
		Health: func(ctx context.Context, m Machine) (string, error) {
			id := m.(*fakeMachine).id
			if _, ok := passed.LoadOrStore(id, true); !ok {
				return "A", nil // 每台第一次通过
			}
			return healthNever(ctx, m) // 之后每次都等到超时
		}}
	cancel, _ := runSup(t, s)
	defer cancel()
	m0 := next(t, f)
	next(t, f)
	if m0.forceStops.Load() != 1 {
		t.Fatal("超时型失败也应计数并强停")
	}
}

func TestTimingPerFieldDefaults(t *testing.T) {
	s := &Supervisor{Timing: Timing{Watchdog: time.Second}}
	tm := s.timing()
	if tm.Watchdog != time.Second || tm.PatrolEvery != DefaultTiming.PatrolEvery || tm.PatrolFailures != DefaultTiming.PatrolFailures {
		t.Fatalf("逐字段兜底不对：%+v", tm)
	}
}
