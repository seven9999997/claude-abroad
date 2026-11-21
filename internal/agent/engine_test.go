package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeProc 是假的子进程：exit 关闭时退出；ignoreTerm 时只有 SIGKILL 能结束它。
type fakeProc struct {
	exit       chan error
	once       sync.Once
	ignoreTerm bool
	mu         sync.Mutex
	sigs       []syscall.Signal
}

func (p *fakeProc) quit(err error) { p.once.Do(func() { p.exit <- err; close(p.exit) }) }

func (p *fakeProc) Wait() error { return <-p.exit }

func (p *fakeProc) Signal(sig syscall.Signal) error {
	p.mu.Lock()
	p.sigs = append(p.sigs, sig)
	p.mu.Unlock()
	if sig == syscall.SIGKILL || !p.ignoreTerm {
		go p.quit(errors.New("signal: " + sig.String()))
	}
	return nil
}

func (p *fakeProc) signals() []syscall.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]syscall.Signal(nil), p.sigs...)
}

// fakeRunner 记录启动，校验结果可配置。
type fakeRunner struct {
	mu         sync.Mutex
	checkErr   error
	checkOut   string
	checks     int
	starts     []time.Time
	procs      chan *fakeProc
	env        [][]string
	argvs      [][]string
	ignoreTerm bool
	ready      atomic.Bool // 就绪端口是否可连
}

func newFakeRunner() *fakeRunner { return &fakeRunner{procs: make(chan *fakeProc, 32)} }

func (r *fakeRunner) Start(argv, env []string, _ string, out io.Writer) (Proc, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = append(r.starts, time.Now())
	r.env = append(r.env, env)
	r.argvs = append(r.argvs, argv)
	p := &fakeProc{exit: make(chan error, 1), ignoreTerm: r.ignoreTerm}
	_, _ = io.WriteString(out, "started "+strings.Join(argv, " ")+" peer 198.51.100.9\n")
	r.procs <- p
	return p, nil
}

func (r *fakeRunner) Run(_ context.Context, _, env []string, _ string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks++
	r.env = append(r.env, env)
	return []byte(r.checkOut), r.checkErr
}

func (r *fakeRunner) setCheck(out string, err error) {
	r.mu.Lock()
	r.checkOut, r.checkErr = out, err
	r.mu.Unlock()
}

func (r *fakeRunner) gaps() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	var g []time.Duration
	for i := 1; i < len(r.starts); i++ {
		g = append(g, r.starts[i].Sub(r.starts[i-1]))
	}
	return g
}

var testEngineTiming = EngineTiming{
	BackoffMin: 20 * time.Millisecond, BackoffMax: 80 * time.Millisecond, StableFor: 100 * time.Millisecond,
	KillAfter: 50 * time.Millisecond, CheckTimeout: time.Second, ReadyPoll: 2 * time.Millisecond,
}

func newTestEngine(r *fakeRunner) *Engine {
	return &Engine{Name: "sing-box", Check: []string{"sing-box", "check"}, Run: []string{"sing-box", "run"},
		ReadyAddr: "127.0.0.1:1090", Log: NewRingLog(RingLogBytes), Runner: r, Timing: testEngineTiming,
		DialReady: func(context.Context, string) error {
			if r.ready.Load() {
				return nil
			}
			return errors.New("not yet")
		}}
}

func nextProc(t *testing.T, r *fakeRunner) *fakeProc {
	t.Helper()
	select {
	case p := <-r.procs:
		return p
	case <-time.After(3 * time.Second):
		t.Fatal("引擎没有启动")
		return nil
	}
}

func TestEngineCheckFailedDoesNotStart(t *testing.T) {
	r := newFakeRunner()
	r.setCheck("FATAL decode config: password=hunter2 at 203.0.113.5", errors.New("exit status 1"))
	e := newTestEngine(r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gen := e.Restart(ctx)
	err := e.WaitRunning(ctx, gen)
	if !errors.Is(err, ErrEngineNotRunning) {
		t.Fatalf("校验失败时 WaitRunning 应报错：%v", err)
	}
	st := e.Status()
	if st.State != EngineCheckFailed || strings.Contains(st.LastError, "hunter2") || strings.Contains(st.LastError, "203.0.113.5") {
		t.Fatalf("状态应为 check_failed 且原因已脱敏：%+v", st)
	}
	if len(r.starts) != 0 {
		t.Fatal("校验失败不应启动进程")
	}
	// 换成好配置后重启成功。
	r.setCheck("", nil)
	r.ready.Store(true)
	gen = e.Restart(ctx)
	if err := e.WaitRunning(ctx, gen); err != nil {
		t.Fatal(err)
	}
}

func TestEngineBackoffAndStableReset(t *testing.T) {
	r := newFakeRunner()
	r.ready.Store(true)
	e := newTestEngine(r)
	// 断言引擎“要求”的退避时长（日志里的值），墙钟只核对下限：上限受调度抖动影响，
	// 负载高时会偶发超出（2026-11-18 -count=50 复现过一次 195ms）。
	var mu sync.Mutex
	var asked []string
	e.Logf = func(format string, a ...any) {
		if strings.Contains(format, "后重启") && len(a) == 3 {
			mu.Lock()
			asked = append(asked, fmt.Sprint(a[2]))
			mu.Unlock()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.Start(ctx)
	for i := 0; i < 4; i++ { // 前 4 个就绪后立即崩溃：退避 20、40、80、80（上限）
		p := nextProc(t, r)
		time.Sleep(5 * time.Millisecond)
		p.quit(errors.New("crash"))
	}
	p := nextProc(t, r)
	time.Sleep(testEngineTiming.StableFor + 20*time.Millisecond) // 稳定运行
	p.quit(errors.New("crash"))
	nextProc(t, r)
	mu.Lock()
	got := strings.Join(asked, " ")
	mu.Unlock()
	if got != "20ms 40ms 80ms 80ms 20ms" {
		t.Errorf("退避序列 %q，应为翻倍到上限、稳定运行后复位为 20ms", got)
	}
	g := r.gaps()
	for i, w := range []time.Duration{20, 40, 80, 80} {
		if g[i] < w*time.Millisecond {
			t.Errorf("第 %d 次实际间隔 %s，短于退避 %dms", i+1, g[i], w)
		}
	}
	if e.Status().Restarts != 5 {
		t.Errorf("重启计数 %d", e.Status().Restarts)
	}
}

func TestEngineRestartAndStopTermThenKill(t *testing.T) {
	r := newFakeRunner()
	r.ready.Store(true)
	r.ignoreTerm = true
	e := newTestEngine(r)
	ctx, cancel := context.WithCancel(context.Background())
	e.Start(ctx)
	p1 := nextProc(t, r)
	if err := e.WaitRunning(ctx, 0); err != nil {
		t.Fatal(err)
	}
	gen := e.Restart(ctx) // 换配置：停旧的、重新校验、起新的
	p2 := nextProc(t, r)
	if err := e.WaitRunning(ctx, gen); err != nil {
		t.Fatal(err)
	}
	if s := p1.signals(); len(s) != 2 || s[0] != syscall.SIGTERM || s[1] != syscall.SIGKILL {
		t.Fatalf("不理会 TERM 的进程应先 TERM 再 KILL：%v", s)
	}
	cancel()
	select {
	case <-e.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 结束后引擎应停止")
	}
	if s := p2.signals(); len(s) != 2 || e.Status().State != EngineStopped {
		t.Fatalf("停止：%v，%s", s, e.Status().State)
	}
	r.mu.Lock()
	checks := r.checks
	r.mu.Unlock()
	if checks != 2 {
		t.Errorf("每次启动前都应校验：%d 次", checks)
	}
}

func TestEngineEnvHasNoProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://leak:1")
	t.Setenv("all_proxy", "socks5://leak:2")
	r := newFakeRunner()
	r.ready.Store(true)
	e := newTestEngine(r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.Start(ctx)
	nextProc(t, r)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, env := range r.env {
		for _, kv := range env {
			if strings.Contains(strings.ToUpper(kv), "_PROXY") {
				t.Fatalf("子进程环境里不应有代理变量：%s", kv)
			}
		}
	}
}

func TestEngineOutputGoesToRingLogRedacted(t *testing.T) {
	r := newFakeRunner()
	r.ready.Store(true)
	e := newTestEngine(r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.Start(ctx)
	nextProc(t, r)
	lines, _ := e.Log.Tail(0)
	if len(lines) != 1 || strings.Contains(lines[0], "198.51.100.9") || !strings.Contains(lines[0], "started sing-box run") {
		t.Fatalf("输出应进环形缓冲并脱敏：%v", lines)
	}
}

// Restart 与 loop 的竞态：并发多次 Restart 后，启动次数不超过 Restart 次数 + 1，
// 且 WaitRunning 返回之后不会再无故重启一次。
func TestEngineRestartRace(t *testing.T) {
	for round := 0; round < 20; round++ {
		r := newFakeRunner()
		r.ready.Store(true)
		e := newTestEngine(r)
		ctx, cancel := context.WithCancel(context.Background())
		e.Start(ctx)
		if err := e.WaitRunning(ctx, 0); err != nil {
			t.Fatal(err)
		}
		const n = 5
		var wg sync.WaitGroup
		gens := make(chan uint64, n)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); gens <- e.Restart(ctx) }()
		}
		wg.Wait()
		close(gens)
		var last uint64
		for g := range gens {
			last = max(last, g)
		}
		if err := e.WaitRunning(ctx, last); err != nil {
			t.Fatal(err)
		}
		r.mu.Lock()
		before := len(r.starts)
		r.mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		r.mu.Lock()
		after := len(r.starts)
		r.mu.Unlock()
		cancel()
		<-e.Done()
		if before > n+1 {
			t.Fatalf("启动 %d 次，超过 Restart 次数 + 1", before)
		}
		if after != before {
			t.Fatalf("WaitRunning 返回后又启动了 %d 次", after-before)
		}
	}
}

// 退避中收到重启请求：提前醒来，退避复位。
func TestEngineRestartDuringBackoffResets(t *testing.T) {
	r := newFakeRunner()
	r.ready.Store(true)
	e := newTestEngine(r)
	e.Timing.BackoffMin = 40 * time.Millisecond
	e.Timing.BackoffMax = 2 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.Start(ctx)
	for i := 0; i < 3; i++ { // 退避升到 160ms
		p := nextProc(t, r)
		p.quit(errors.New("crash"))
	}
	p := nextProc(t, r)
	p.quit(errors.New("crash")) // 现在要退避 320ms
	time.Sleep(10 * time.Millisecond)
	e.Restart(ctx) // 打断退避
	p = nextProc(t, r)
	start := time.Now()
	p.quit(errors.New("crash"))
	nextProc(t, r)
	if d := time.Since(start); d > 120*time.Millisecond {
		t.Fatalf("重启请求后退避应复位为 40ms，实际 %s", d)
	}
}

func TestEngineCheckFailureNotLogged(t *testing.T) {
	r := newFakeRunner()
	r.setCheck("yaml: password is "+sentinel, errors.New("exit status 1"))
	e := newTestEngine(r)
	var logs syncBuffer
	e.Logf = func(format string, a ...any) { _, _ = logs.Write([]byte(fmtSprintf(format, a...) + "\n")) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = e.WaitRunning(ctx, e.Restart(ctx))
	if strings.Contains(logs.String(), sentinel) || !strings.Contains(logs.String(), "/v1/health") {
		t.Fatalf("校验失败的原因不应进日志：%q", logs.String())
	}
}

func TestMainProxyTimeoutsFitWriteTimeout(t *testing.T) {
	if MainProxyCheckTimeout+MainProxyReadyTimeout >= NewServer(nil).WriteTimeout {
		t.Fatal("下发主代理的最长耗时必须小于控制面的写超时")
	}
}
