package main

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"claude-abroad/internal/vmhost"
)

// innerMachine 是被包住的假虚拟机。
type innerMachine struct {
	startErr    error
	stopOnForce bool
	done        chan struct{}
	once        sync.Once
	forceStops  atomic.Int32
}

func newInner() *innerMachine { return &innerMachine{done: make(chan struct{}), stopOnForce: true} }

func (m *innerMachine) Start() error          { return m.startErr }
func (m *innerMachine) stop()                 { m.once.Do(func() { close(m.done) }) }
func (m *innerMachine) Done() <-chan struct{} { return m.done }
func (m *innerMachine) Err() error            { return errors.New("stopped") }
func (m *innerMachine) ForceStop() error {
	m.forceStops.Add(1)
	if m.stopOnForce {
		m.stop()
	}
	return nil
}
func (m *innerMachine) DialVsock(context.Context, uint32) (net.Conn, error) {
	return nil, errors.New("no vsock")
}

// pairs 记录每次建的 socketpair，便于断言都关了。
type pairs struct {
	mu   sync.Mutex
	vm   []*os.File
	host []net.Conn
}

func (p *pairs) install(t *testing.T) {
	old := newSocketpair
	newSocketpair = func() (*os.File, net.Conn, error) {
		v, h, err := socketpair()
		if err == nil {
			p.mu.Lock()
			p.vm, p.host = append(p.vm, v), append(p.host, h)
			p.mu.Unlock()
		}
		return v, h, err
	}
	t.Cleanup(func() { newSocketpair = old })
}

func fileClosed(f *os.File) bool { _, err := f.Stat(); return errors.Is(err, os.ErrClosed) }
func connClosed(c net.Conn) bool { _, err := c.Write([]byte{0}); return errors.Is(err, net.ErrClosed) }

// blockingAccept 像 AcceptVfkit：一直读 conn，直到 conn 被关；返回时记一次。
func blockingAccept(returned *atomic.Int32) func(context.Context, net.Conn) error {
	return func(_ context.Context, c net.Conn) error {
		defer returned.Add(1)
		buf := make([]byte, 2048)
		for {
			if _, err := c.Read(buf); err != nil {
				return err
			}
		}
	}
}

func waitClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("等待超时")
	}
}

func TestNetFactoryFailureClosesBoth(t *testing.T) {
	var p pairs
	p.install(t)
	u := &userNet{accept: blockingAccept(new(atomic.Int32))}
	f := u.factory(func(vmhost.Spec) (vmhost.Machine, error) { return nil, errors.New("缺授权") })
	if _, err := f(vmhost.Spec{}); err == nil {
		t.Fatal("应返回内层错误")
	}
	if !fileClosed(p.vm[0]) || !connClosed(p.host[0]) {
		t.Fatal("Factory 失败时两端都应关闭")
	}
}

func TestNetStartFailureCleansUp(t *testing.T) {
	var p pairs
	p.install(t)
	var accepted, returned atomic.Int32
	u := &userNet{accept: func(ctx context.Context, c net.Conn) error { accepted.Add(1); return blockingAccept(&returned)(ctx, c) }}
	in := newInner()
	in.startErr = errors.New("启动失败")
	m, err := u.factory(func(s vmhost.Spec) (vmhost.Machine, error) {
		if s.NetFD == nil {
			t.Error("Spec.NetFD 应已设置")
		}
		return in, nil
	})(vmhost.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(); err == nil {
		t.Fatal("应返回启动错误")
	}
	waitClosed(t, m.Done())
	if accepted.Load() != 0 || !fileClosed(p.vm[0]) || !connClosed(p.host[0]) {
		t.Fatalf("Start 失败：不应交给网络（%d），两端都应关闭", accepted.Load())
	}
}

func TestNetNormalStop(t *testing.T) {
	var p pairs
	p.install(t)
	var returned atomic.Int32
	u := &userNet{accept: blockingAccept(&returned)}
	in := newInner()
	m, _ := u.factory(func(vmhost.Spec) (vmhost.Machine, error) { return in, nil })(vmhost.Spec{})
	if err := m.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.Done():
		t.Fatal("内层没停时 Done 不应关闭")
	case <-time.After(50 * time.Millisecond):
	}
	in.stop()
	waitClosed(t, m.Done())
	if returned.Load() != 1 || !fileClosed(p.vm[0]) || !connClosed(p.host[0]) || in.forceStops.Load() != 0 {
		t.Fatalf("正常停止：Accept 返回 %d 次，强停 %d 次", returned.Load(), in.forceStops.Load())
	}
	if m.Err() == nil || errors.Is(m.Err(), errNetDown) {
		t.Fatalf("正常停止不应报网络中断：%v", m.Err())
	}
}

func TestNetFailureForceStops(t *testing.T) {
	var p pairs
	p.install(t)
	fail := make(chan struct{})
	u := &userNet{accept: func(ctx context.Context, c net.Conn) error {
		select {
		case <-fail:
			return errors.New("switch 出错")
		case <-ctx.Done():
			return nil
		}
	}}
	in := newInner()
	in.stopOnForce = false // 先看失败标记，再手动停
	m, _ := u.factory(func(vmhost.Spec) (vmhost.Machine, error) { return in, nil })(vmhost.Spec{})
	m.Start()
	close(fail)
	deadline := time.Now().Add(2 * time.Second)
	for in.forceStops.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if in.forceStops.Load() != 1 {
		t.Fatal("网络出错应强停内层")
	}
	if _, err := m.DialVsock(context.Background(), 1081); !errors.Is(err, errNetDown) {
		t.Fatalf("网络出错后 DialVsock 应报错：%v", err)
	}
	select {
	case <-m.Done():
		t.Fatal("内层还没停，Done 不应关闭")
	default:
	}
	in.stop()
	waitClosed(t, m.Done())
	if !errors.Is(m.Err(), errNetDown) || !fileClosed(p.vm[0]) || !connClosed(p.host[0]) {
		t.Fatalf("应报网络中断且两端都关：%v", m.Err())
	}
}

// Supervisor 重启时拿到新的 socketpair；网络出错且强停无效时返回 ErrStuck、不起第二台。
func TestNetWithSupervisor(t *testing.T) {
	var p pairs
	p.install(t)
	var made atomic.Int32
	var current atomic.Pointer[innerMachine]
	u := &userNet{accept: func(ctx context.Context, c net.Conn) error { <-ctx.Done(); return nil }}
	f := u.factory(func(vmhost.Spec) (vmhost.Machine, error) {
		made.Add(1)
		in := newInner()
		current.Store(in)
		return in, nil
	})
	tm := vmhost.DefaultTiming
	tm.Watchdog, tm.ProbeEvery, tm.BackoffMin, tm.BackoffMax, tm.StopWait = time.Second, 5*time.Millisecond, 10*time.Millisecond, 20*time.Millisecond, 100*time.Millisecond
	sup := &vmhost.Supervisor{Factory: f, Timing: tm,
		Health: func(context.Context, vmhost.Machine) (string, error) { return "boot", nil }}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()
	for made.Load() < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	current.Load().stop() // 意外停止 → 重启
	for made.Load() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	p.mu.Lock()
	ok := len(p.vm) >= 2 && p.vm[0] != p.vm[1] && fileClosed(p.vm[0])
	p.mu.Unlock()
	if !ok {
		t.Fatal("重启应建新的 socketpair，旧的已关")
	}

	// 网络出错 + 强停无效：Supervisor 返回 ErrStuck，只起了一台。
	var made2 atomic.Int32
	u2 := &userNet{accept: func(context.Context, net.Conn) error { return errors.New("down") }}
	f2 := u2.factory(func(vmhost.Spec) (vmhost.Machine, error) {
		made2.Add(1)
		in := newInner()
		in.stopOnForce = false
		return in, nil
	})
	tm.PatrolEvery, tm.PatrolFailures, tm.ProbeTimeout = 5*time.Millisecond, 2, 20*time.Millisecond
	sup2 := &vmhost.Supervisor{Factory: f2, Timing: tm} // 默认 HTTP health：DialVsock 报错 → 不健康
	err := sup2.Run(context.Background())
	if !errors.Is(err, vmhost.ErrStuck) || made2.Load() != 1 {
		t.Fatalf("应返回 ErrStuck 且只起一台：%v，%d 台", err, made2.Load())
	}
}
