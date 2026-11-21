package vmhost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// fakeMachine 是测试用的虚拟机。
type fakeMachine struct {
	id       int
	startErr error
	dial     func(ctx context.Context, port uint32) (net.Conn, error)
	// stopOnForce 为 false 时 ForceStop 不让它停（模拟卡死）。
	stopOnForce bool

	once       sync.Once
	done       chan struct{}
	mu         sync.Mutex
	err        error
	forceStops atomic.Int32
	started    atomic.Bool
}

func newFakeMachine(id int) *fakeMachine {
	return &fakeMachine{id: id, done: make(chan struct{}), stopOnForce: true}
}

func (m *fakeMachine) Start() error {
	if m.startErr != nil {
		return m.startErr
	}
	m.started.Store(true)
	return nil
}

func (m *fakeMachine) stop(err error) {
	m.once.Do(func() {
		m.mu.Lock()
		m.err = err
		m.mu.Unlock()
		close(m.done)
	})
}

func (m *fakeMachine) ForceStop() error {
	m.forceStops.Add(1)
	if m.stopOnForce {
		m.stop(errors.New("forced"))
	}
	return nil
}

func (m *fakeMachine) Done() <-chan struct{} { return m.done }

func (m *fakeMachine) Err() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.err
}

func (m *fakeMachine) DialVsock(ctx context.Context, port uint32) (net.Conn, error) {
	if m.dial != nil {
		return m.dial(ctx, port)
	}
	return nil, errors.New("no dial")
}

// fakeFleet 依次产出 fakeMachine，记录每次创建的时间。
type fakeFleet struct {
	mu       sync.Mutex
	machines []*fakeMachine
	times    []time.Time
	setup    func(m *fakeMachine)
	err      error
	created  chan *fakeMachine
}

func newFleet(setup func(m *fakeMachine)) *fakeFleet {
	return &fakeFleet{setup: setup, created: make(chan *fakeMachine, 100)}
}

func (f *fakeFleet) factory(Spec) (Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	m := newFakeMachine(len(f.machines))
	if f.setup != nil {
		f.setup(m)
	}
	f.machines = append(f.machines, m)
	f.times = append(f.times, time.Now())
	f.created <- m
	return m, nil
}

func (f *fakeFleet) gaps() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []time.Duration
	for i := 1; i < len(f.times); i++ {
		out = append(out, f.times[i].Sub(f.times[i-1]))
	}
	return out
}

var testTiming = Timing{
	Watchdog:        150 * time.Millisecond,
	ProbeEvery:      5 * time.Millisecond,
	ProbeTimeout:    40 * time.Millisecond,
	ShutdownTimeout: 40 * time.Millisecond,
	StopWait:        100 * time.Millisecond,
	StableFor:       120 * time.Millisecond,
	BackoffMin:      20 * time.Millisecond,
	BackoffMax:      80 * time.Millisecond,
	PatrolEvery:     10 * time.Millisecond,
	PatrolFailures:  3,
}

func healthOK(_ context.Context, m Machine) (string, error) {
	return fmt.Sprintf("boot-%d", m.(*fakeMachine).id), nil
}

func healthNever(ctx context.Context, _ Machine) (string, error) { <-ctx.Done(); return "", ctx.Err() }

// pipeListener 是基于 net.Pipe 的监听器：Dial 一次产生一对连接。
type pipeListener struct {
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{ch: make(chan net.Conn), closed: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *pipeListener) Addr() net.Addr { return &net.UnixAddr{Name: "pipe", Net: "unix"} }

func (l *pipeListener) Dial(ctx context.Context, _ uint32) (net.Conn, error) {
	a, b := net.Pipe()
	select {
	case l.ch <- b:
		return a, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
