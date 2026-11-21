package vmhost

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// clockServer 是假的 /v1/clock：按顺序决定每次请求的应答码，并记录请求时间。
type clockServer struct {
	l     *pipeListener
	mu    sync.Mutex
	times []time.Time
	codes []int // 第 i 次请求的应答码（越界后用最后一个）
}

func newClockServer(t *testing.T, codes ...int) *clockServer {
	cs := &clockServer{l: newPipeListener(), codes: codes}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]int64
		_ = json.NewDecoder(r.Body).Decode(&req)
		cs.mu.Lock()
		i := len(cs.times)
		cs.times = append(cs.times, time.Now())
		code := cs.codes[min(i, len(cs.codes)-1)]
		cs.mu.Unlock()
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"ok":true,"offset_ms":3,"stepped":false}`))
	})}
	go func() { _ = srv.Serve(cs.l) }()
	t.Cleanup(func() { srv.Close() })
	return cs
}

func (cs *clockServer) dial(ctx context.Context) (net.Conn, error) { return cs.l.Dial(ctx, 1081) }

func (cs *clockServer) count() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return len(cs.times)
}

func (cs *clockServer) gaps() []time.Duration {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	var g []time.Duration
	for i := 1; i < len(cs.times); i++ {
		g = append(g, cs.times[i].Sub(cs.times[i-1]))
	}
	return g
}

func waitCount(t *testing.T, cs *clockServer, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for cs.count() < n && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if cs.count() < n {
		t.Fatalf("应有 %d 次对时，实际 %d", n, cs.count())
	}
}

func TestClockKeeperBackoffThenInterval(t *testing.T) {
	cs := newClockServer(t, 500, 500, 200)
	k := &ClockKeeper{Dial: cs.dial, Interval: 150 * time.Millisecond, RetryMin: 15 * time.Millisecond, RetryMax: 100 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	done := make(chan struct{})
	go func() { k.Run(ctx); close(done) }()
	waitCount(t, cs, 1)
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("启动应立即对时")
	}
	waitCount(t, cs, 4)
	g := cs.gaps()
	if g[0] < 15*time.Millisecond || g[1] < 30*time.Millisecond || g[1] > 100*time.Millisecond {
		t.Errorf("失败应按 15、30 毫秒退避：%v", g)
	}
	if g[2] < 140*time.Millisecond {
		t.Errorf("成功后应回到 150 毫秒的周期：%v", g)
	}
	cancel()
	<-done
	n := cs.count()
	time.Sleep(50 * time.Millisecond)
	if cs.count() != n {
		t.Fatal("ctx 结束后不应再发请求")
	}
}

func TestClockKeeperWake(t *testing.T) {
	cs := newClockServer(t, 200)
	wake := make(chan struct{}, 1)
	k := &ClockKeeper{Dial: cs.dial, Interval: time.Hour, Wake: wake}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go k.Run(ctx)
	waitCount(t, cs, 1)
	wake <- struct{}{}
	waitCount(t, cs, 2)
}

func TestSyncClockRTTTooLong(t *testing.T) {
	cs := newClockServer(t, 200)
	var calls atomic.Int32
	base := time.Now()
	now := func() time.Time { // 第二次读时间比第一次晚 2 秒
		if calls.Add(1) == 1 {
			return base
		}
		return base.Add(2 * time.Second)
	}
	if _, err := SyncClock(context.Background(), cs.dial, now); err == nil {
		t.Fatal("往返超过 ClockMaxRTT 应算失败")
	}
	r, err := SyncClock(context.Background(), cs.dial, nil)
	if err != nil || r.OffsetMs != 3 {
		t.Fatalf("正常对时：%+v %v", r, err)
	}
}

// 巡检判定唤醒时 OnWake 恰好调用一次；对时一直失败也不影响虚拟机。
func TestOnWakeOnceAndClockFailureNoStop(t *testing.T) {
	f := newFleet(nil)
	var jumped atomic.Bool
	var reads atomic.Int32
	var wakes atomic.Int32
	base := time.Unix(1_900_000_000, 0)
	s := &Supervisor{Factory: f.factory, Timing: testTiming, Health: healthOK,
		// 完全受控的时钟（m4-plan v2.7 第 0 步）：时间不跟真实时间走，每读一次只前进一个巡检间隔，
		// 第 5 次读开始整体“睡过一小时”。机器负载高、两轮巡检的真实间隔被拉长也不会多判唤醒。
		Now: func() time.Time {
			n := reads.Add(1)
			t := base.Add(time.Duration(n) * testTiming.PatrolEvery)
			if n >= 5 {
				jumped.Store(true)
				return t.Add(time.Hour)
			}
			return t
		},
		OnWake: func() { wakes.Add(1) },
		OnReady: func(ctx context.Context, _ string) {
			k := &ClockKeeper{Dial: func(context.Context) (net.Conn, error) { return nil, errors.New("down") },
				RetryMin: time.Millisecond, RetryMax: 5 * time.Millisecond}
			k.Run(ctx)
		}}
	cancel, _ := runSup(t, s)
	defer cancel()
	m0 := next(t, f)
	time.Sleep(30 * testTiming.PatrolEvery)
	if !jumped.Load() || wakes.Load() != 1 {
		t.Fatalf("OnWake 应恰好调用一次：%d", wakes.Load())
	}
	if m0.forceStops.Load() != 0 || f.count() != 1 {
		t.Fatal("对时失败不应强停虚拟机")
	}
}

// 往返中途睡了一觉：单调时钟只走了几毫秒，墙上时间走了 2 秒，应判失败。
func TestSyncClockRTTUsesWallClock(t *testing.T) {
	cs := newClockServer(t, 200)
	var calls atomic.Int32
	base := time.Now() // 带单调读数
	now := func() time.Time {
		if calls.Add(1) == 1 {
			return base
		}
		// 不带单调读数、墙上时间晚 2 秒（模拟往返中途睡了一觉）。
		return base.Round(0).Add(2 * time.Second)
	}
	if _, err := SyncClock(context.Background(), cs.dial, now); err == nil {
		t.Fatal("墙上时间的往返超过 ClockMaxRTT 应算失败")
	}
}
