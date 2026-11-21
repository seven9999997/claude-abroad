package claudeproc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"syscall"
	"time"

	"claude-abroad/internal/procinfo"
)

// FakeSigner 是假的签名查询：路径 → Team ID；Err 里的路径返回错误。
type FakeSigner struct {
	mu    sync.Mutex
	Teams map[string]string
	Err   map[string]bool
	Calls int
}

func (f *FakeSigner) TeamID(p string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	if f.Err[p] {
		return "", errors.New("假的签名查询失败")
	}
	return f.Teams[p], nil
}

// Behavior 是假进程收到信号后的行为。
type Behavior int

const (
	ExitOnTerm Behavior = iota // TERM 后 Delay 退出
	ExitOnKill                 // 不理 TERM，KILL 后退出
	Unkillable                 // 怎么都不退
)

// SignalRec 是一次信号的记录。
type SignalRec struct {
	PID int
	Sig syscall.Signal
}

func (r SignalRec) String() string {
	n := "TERM"
	if r.Sig == syscall.SIGKILL {
		n = "KILL"
	}
	return fmt.Sprintf("%s %d", n, r.PID)
}

// FakeWorld 是假的进程表、信号发送器和时钟（结束流程的测试、假系统都用它）。
// Sleep 推进假时间，并让到点的进程退出。
type FakeWorld struct {
	mu       sync.Mutex
	now      time.Time
	procs    map[int]procinfo.Proc
	behavior map[int]Behavior
	delay    map[int]time.Duration
	dieAt    map[int]time.Time
	Signals  []SignalRec
	// OnSignal 在记录信号之后、锁外调用（测试用来在结束途中“新启动”进程）。
	OnSignal func(r SignalRec)
}

// NewFakeWorld 返回空的假世界。
func NewFakeWorld() *FakeWorld {
	return &FakeWorld{now: time.Unix(1_900_000_000, 0), procs: map[int]procinfo.Proc{}, behavior: map[int]Behavior{},
		delay: map[int]time.Duration{}, dieAt: map[int]time.Time{}}
}

// Add 加一个进程（Start 为零时用当前假时间）。
func (w *FakeWorld) Add(p procinfo.Proc, b Behavior, delay time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if p.Start.IsZero() {
		p.Start = w.now
	}
	w.procs[p.PID] = p
	w.behavior[p.PID] = b
	w.delay[p.PID] = delay
	delete(w.dieAt, p.PID)
}

// Remove 让进程退出。
func (w *FakeWorld) Remove(pid int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.procs, pid)
}

// Alive 报告进程是否还在。
func (w *FakeWorld) Alive(pid int) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.procs[pid]
	return ok
}

// List 实现 procinfo.Lister。
func (w *FakeWorld) List() ([]procinfo.Proc, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]procinfo.Proc, 0, len(w.procs))
	for _, p := range w.procs {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, nil
}

// Info 读一个进程。
func (w *FakeWorld) Info(pid int) (procinfo.Proc, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	p, ok := w.procs[pid]
	if !ok {
		return procinfo.Proc{}, errors.New("进程不存在")
	}
	return p, nil
}

// Signal 实现 Signaler。
func (w *FakeWorld) Signal(pid int, sig syscall.Signal) error {
	w.mu.Lock()
	if _, ok := w.procs[pid]; !ok {
		w.mu.Unlock()
		return errors.New("进程不存在")
	}
	rec := SignalRec{pid, sig}
	w.Signals = append(w.Signals, rec)
	switch b := w.behavior[pid]; {
	case sig == syscall.SIGKILL && b != Unkillable:
		delete(w.procs, pid)
	case sig == syscall.SIGTERM && b == ExitOnTerm:
		if w.delay[pid] <= 0 {
			delete(w.procs, pid)
		} else if _, ok := w.dieAt[pid]; !ok {
			w.dieAt[pid] = w.now.Add(w.delay[pid])
		}
	}
	cb := w.OnSignal
	w.mu.Unlock()
	if cb != nil {
		cb(rec)
	}
	return nil
}

// Now 实现 Clock。
func (w *FakeWorld) Now() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.now
}

// Sleep 实现 Clock：推进假时间，到点的进程退出。
func (w *FakeWorld) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.now = w.now.Add(d)
	for pid, at := range w.dieAt {
		if !at.After(w.now) {
			delete(w.procs, pid)
			delete(w.dieAt, pid)
		}
	}
	return nil
}

// SignalLog 返回信号记录的拷贝。
func (w *FakeWorld) SignalLog() []SignalRec {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]SignalRec(nil), w.Signals...)
}

// NewFakeService 用假世界组装 Service（假系统和测试用）。skip 为真时是“不结束任何进程”的实现。
func NewFakeService(w *FakeWorld, s Signer, self int, skip bool, logf func(string, ...any)) *Service {
	sc := &Scanner{Procs: w, Signer: s, Bundle: fakeBundles, Resolve: func(p string) (string, error) { return p, nil }, Self: self}
	svc := &Service{Scanner: sc, Skip: skip, Logf: logf}
	if !skip {
		svc.Killer = &Killer{Scanner: sc, Info: w.Info, Sig: w, Clock: w}
	}
	return svc
}

// fakeBundles：路径里的 Claude.app 当作桌面版，其余 .app 用目录名当 Bundle ID。
func fakeBundles(app string) (string, error) {
	switch {
	case len(app) >= len("/Claude.app") && app[len(app)-len("/Claude.app"):] == "/Claude.app":
		return DesktopBundleID, nil
	}
	return "com.example.other", nil
}
