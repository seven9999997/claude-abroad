package agent

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

var errBoom = errors.New("boom")

func runInit(t *testing.T, f *fakeSys, sigs chan os.Signal) chan error {
	done := make(chan error, 1)
	env := f.env()
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	env.Sleep = func(time.Duration) { <-block } // 停机超时不触发
	go func() { done <- Init(env, sigs) }()
	return done
}

func waitDone(t *testing.T, done chan error) error {
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Init 没有返回")
		return nil
	}
}

func TestInitReturnsWhenServeExits(t *testing.T) {
	for _, status := range []string{"exit status 0", "exit status 1", "signal: killed"} {
		t.Run(status, func(t *testing.T) {
			f := newFakeSys(t)
			f.waits <- WaitResult{Pid: 55, Status: "exit status 0"} // 孤儿，收割后继续
			f.waits <- WaitResult{Pid: 56, Status: "signal: killed"}
			f.waits <- WaitResult{Pid: 100, Status: status} // serve
			if err := waitDone(t, runInit(t, f, make(chan os.Signal))); err != nil {
				t.Fatalf("serve 退出时 Init 应返回 nil（由调用方关机）：%v", err)
			}
			calls := strings.Join(f.Calls(), "\n")
			for _, want := range []string{
				"mount tmpfs /run tmpfs 0x6 mode=0755,size=16m",
				"cad_off",
				"spawn /sbin/abroad-agent serve",
				"reaped 55", "reaped 56", "reaped 100 " + status,
			} {
				if !strings.Contains(calls, want) {
					t.Errorf("缺少 %q：\n%s", want, calls)
				}
			}
		})
	}
}

func TestInitForwardsShutdownSignal(t *testing.T) {
	f := newFakeSys(t)
	sigs := make(chan os.Signal, 2)
	done := runInit(t, f, sigs)
	sigs <- syscall.SIGTERM
	sigs <- syscall.SIGINT // 第二个信号不再重复通知
	select {
	case pid := <-f.killed:
		if pid != 100 {
			t.Fatalf("应通知 serve（pid 100），实际 %d", pid)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("没有通知 serve")
	}
	f.waits <- WaitResult{Pid: 100, Status: "exit status 0"}
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
	calls := strings.Join(f.Calls(), "\n")
	if n := strings.Count(calls, "kill 100"); n != 1 {
		t.Errorf("应只通知一次，实际 %d 次", n)
	}
	if strings.Contains(calls, "poweroff") {
		t.Error("serve 按时退出时 Init 自己不关机（由调用方关）")
	}
}

func TestInitStopTimeoutPowersOff(t *testing.T) {
	f := newFakeSys(t)
	env := f.env()
	slept := make(chan time.Duration, 1)
	env.Sleep = func(d time.Duration) { slept <- d }
	sigs := make(chan os.Signal, 1)
	go func() { _ = Init(env, sigs) }()
	sigs <- syscall.SIGTERM
	if d := <-slept; d != StopTimeout {
		t.Fatalf("应等 %s，实际 %s", StopTimeout, d)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		n, s := f.poweroffs, f.syncs
		f.mu.Unlock()
		if n == 1 && s == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve 不退出时应 sync + 关机：poweroff %d，sync %d", n, s)
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.waits <- WaitResult{Pid: 100, Status: "exit status 0"} // 让 Init 结束
}

func TestInitErrors(t *testing.T) {
	f := newFakeSys(t)
	f.spawnErr = errBoom
	if err := Init(f.env(), make(chan os.Signal)); err == nil || !strings.Contains(err.Error(), "serve") {
		t.Fatalf("serve 起不来应报错：%v", err)
	}
	f = newFakeSys(t)
	f.waitErr = syscall.ECHILD
	if err := Init(f.env(), make(chan os.Signal)); err == nil {
		t.Fatal("wait4 出错应报错")
	}
}

type panicKillSys struct{ *fakeSys }

func (p panicKillSys) Kill(int, syscall.Signal) error { panic("kill 出错") }

func TestInitGoroutinePanicPowersOff(t *testing.T) {
	f := newFakeSys(t)
	env := f.env()
	env.Sys = panicKillSys{f}
	sigs := make(chan os.Signal, 1)
	go func() { _ = Init(env, sigs) }()
	sigs <- syscall.SIGTERM
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		n := f.poweroffs
		f.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("信号转发的 goroutine panic 时应关机")
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.waits <- WaitResult{Pid: 100, Status: "exit status 0"}
}
