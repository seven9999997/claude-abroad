package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// 引擎状态（/v1/health 的 engines.<名>.state）。
const (
	EngineStopped     = "stopped"
	EngineChecking    = "checking"
	EngineCheckFailed = "check_failed"
	EngineStarting    = "starting"
	EngineRunning     = "running"
	EngineBackoff     = "backoff"
)

// EngineStatus 是一个引擎的状态快照。LastError 已脱敏、截断。
type EngineStatus struct {
	State     string    `json:"state"`
	Restarts  int       `json:"restarts"`
	LastError string    `json:"last_error,omitempty"`
	Since     time.Time `json:"since"`
}

// Proc 是一个运行中的子进程。
type Proc interface {
	Wait() error
	Signal(sig syscall.Signal) error
}

// ProcRunner 启动引擎和校验命令。真实实现见 ExecRunner；测试用假的。
type ProcRunner interface {
	// Start 启动长期运行的进程，stdout、stderr 都写到 out。
	Start(argv, env []string, dir string, out io.Writer) (Proc, error)
	// Run 运行校验命令，返回合并后的输出。
	Run(ctx context.Context, argv, env []string, dir string) ([]byte, error)
}

// EngineTiming 是引擎 supervisor 的时长。
type EngineTiming struct {
	BackoffMin, BackoffMax time.Duration
	StableFor              time.Duration // 就绪后连续运行这么久，退避才复位
	KillAfter              time.Duration // SIGTERM 后这么久还没退出就 SIGKILL
	CheckTimeout           time.Duration
	ReadyPoll              time.Duration
}

// DefaultEngineTiming 是正式取值。
var DefaultEngineTiming = EngineTiming{
	BackoffMin:   time.Second,
	BackoffMax:   30 * time.Second,
	StableFor:    60 * time.Second,
	KillAfter:    3 * time.Second,
	CheckTimeout: 30 * time.Second,
	ReadyPoll:    100 * time.Millisecond,
}

// EngineEnv 是引擎子进程的环境变量：显式给最小集合，不继承任何东西（尤其是 *_PROXY）。
var EngineEnv = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/run/claude-abroad", "TZ=UTC"}

// MaxLastError 是状态里错误信息的上限。
const MaxLastError = 2 << 10

// Engine 管一个引擎进程（sing-box 或 mihomo）：启动前先跑校验命令，校验失败不启动；
// 就绪 = ReadyAddr 能连上；意外退出按 BackoffMin→BackoffMax 翻倍退避后重启，
// 就绪后连续运行满 StableFor 才复位；停止先 SIGTERM，KillAfter 后 SIGKILL。
type Engine struct {
	Name      string
	Check     []string // 校验命令（可为空）
	Run       []string // 运行命令
	Dir       string
	ReadyAddr string // 127.0.0.1:端口
	Log       *RingLog
	Runner    ProcRunner
	Timing    EngineTiming
	// DialReady 探测就绪端口；默认 TCP 连接。
	DialReady func(ctx context.Context, addr string) error
	Logf      func(format string, a ...any)

	mu        sync.Mutex
	st        EngineStatus
	gen       uint64 // Restart 的次数
	curGen    uint64 // 当前这一轮（校验 + 运行）对应的 gen：开始校验时取，此后的配置都已落盘
	readyGen  uint64 // 最近一次就绪的那一轮的 gen
	failedGen uint64 // 最近一次校验失败的那一轮的 gen
	changed   chan struct{}
	restart   chan struct{}
	started   bool
	done      chan struct{}
}

// ErrEngineNotRunning 是等就绪时引擎已停止（或校验失败）。
var ErrEngineNotRunning = errors.New("引擎没有在运行")

func (e *Engine) init() {
	if e.changed == nil {
		e.changed = make(chan struct{})
		e.restart = make(chan struct{}, 1)
		e.done = make(chan struct{})
		e.st = EngineStatus{State: EngineStopped, Since: time.Now()}
	}
}

func (e *Engine) logf(format string, a ...any) {
	if e.Logf != nil {
		e.Logf(format, a...)
	}
}

// Status 返回状态快照。
func (e *Engine) Status() EngineStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.init()
	return e.st
}

func (e *Engine) setState(state, lastErr string, restarted bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.st.State, e.st.Since = state, time.Now()
	if lastErr != "" {
		e.st.LastError = lastErr
	}
	if restarted {
		e.st.Restarts++
	}
	switch state {
	case EngineRunning:
		e.readyGen = e.curGen
	case EngineCheckFailed:
		e.failedGen = e.curGen
	}
	close(e.changed)
	e.changed = make(chan struct{})
}

// Start 在后台启动管理循环（只启动一次），ctx 结束时停掉进程。
func (e *Engine) Start(ctx context.Context) {
	e.mu.Lock()
	e.init()
	if e.started {
		e.mu.Unlock()
		return
	}
	e.started = true
	e.mu.Unlock()
	go e.loop(ctx)
}

// Restart 让引擎重新校验并重启（换配置后调用）。没启动过时启动它。返回本次请求的编号，
// 供 WaitRunning 等待“这次之后”的就绪。
func (e *Engine) Restart(ctx context.Context) uint64 {
	e.mu.Lock()
	e.init()
	e.gen++
	gen := e.gen
	started := e.started
	if started {
		// gen++ 和发信号在同一把锁里，与 loop 顶部的“清通道 + 记 curGen”互斥：
		// 要么这次请求被这一轮吸收（curGen 已含它、信号被清掉），要么信号留给下一轮。
		select {
		case e.restart <- struct{}{}:
		default: // 已有一个待处理的重启请求
		}
	}
	e.mu.Unlock()
	if !started {
		e.Start(ctx)
	}
	return gen
}

// WaitRunning 等引擎在编号 gen 的请求之后就绪。校验失败或 ctx 结束时返回错误。
func (e *Engine) WaitRunning(ctx context.Context, gen uint64) error {
	for {
		e.mu.Lock()
		e.init()
		ok := e.readyGen >= gen && e.st.State == EngineRunning
		failed := e.failedGen >= gen && e.st.State == EngineCheckFailed
		st, ch := e.st, e.changed
		e.mu.Unlock()
		if ok {
			return nil
		}
		if failed {
			return fmt.Errorf("%w：校验失败：%s", ErrEngineNotRunning, st.LastError)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}

// Done 在管理循环退出（进程已停）后关闭；从没启动过时直接返回已关闭的 channel。
func (e *Engine) Done() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.init()
	if !e.started {
		c := make(chan struct{})
		close(c)
		return c
	}
	return e.done
}

func (e *Engine) timing() EngineTiming {
	t, d := e.Timing, DefaultEngineTiming
	for _, f := range []struct{ v, def *time.Duration }{
		{&t.BackoffMin, &d.BackoffMin}, {&t.BackoffMax, &d.BackoffMax}, {&t.StableFor, &d.StableFor},
		{&t.KillAfter, &d.KillAfter}, {&t.CheckTimeout, &d.CheckTimeout}, {&t.ReadyPoll, &d.ReadyPoll},
	} {
		if *f.v <= 0 {
			*f.v = *f.def
		}
	}
	return t
}

func (e *Engine) dialReady(ctx context.Context) error {
	if e.DialReady != nil {
		return e.DialReady(ctx, e.ReadyAddr)
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", e.ReadyAddr)
	if err != nil {
		return err
	}
	return c.Close()
}

func shortErr(b []byte, err error) string {
	s := strings.TrimSpace(string(b))
	if s == "" && err != nil {
		s = err.Error()
	}
	if len(s) > MaxLastError {
		s = s[:MaxLastError] + "…"
	}
	return Redact(s)
}

func (e *Engine) loop(ctx context.Context) {
	defer close(e.done)
	t := e.timing()
	backoff := t.BackoffMin
	for {
		e.mu.Lock()
		select { // 这一轮会用最新的配置，之前积压的重启请求已包含在内
		case <-e.restart:
		default:
		}
		e.curGen = e.gen
		e.mu.Unlock()
		// 1. 校验。
		if len(e.Check) > 0 {
			e.setState(EngineChecking, "", false)
			cctx, cancel := context.WithTimeout(ctx, t.CheckTimeout)
			out, err := e.Runner.Run(cctx, e.Check, EngineEnv, e.Dir)
			cancel()
			if ctx.Err() != nil {
				e.setState(EngineStopped, "", false)
				return
			}
			if err != nil {
				msg := shortErr(out, err)
				// 原因只进 LastError（/v1/health），不进控制台日志：报错可能原样带出配置里的值。
				e.logf("%s 配置校验失败，不启动（原因见 /v1/health）", e.Name)
				e.setState(EngineCheckFailed, msg, false)
				select { // 等新配置
				case <-ctx.Done():
					e.setState(EngineStopped, "", false)
					return
				case <-e.restart:
					backoff = t.BackoffMin
					continue
				}
			}
		}

		// 2. 启动。
		e.setState(EngineStarting, "", false)
		p, err := e.Runner.Start(e.Run, EngineEnv, e.Dir, e.Log)
		if err != nil {
			e.setState(EngineBackoff, shortErr(nil, err), true)
			switch e.sleep(ctx, backoff) {
			case sleepStopped:
				e.setState(EngineStopped, "", false)
				return
			case sleepRestarted:
				backoff = t.BackoffMin
			default:
				backoff = min(backoff*2, t.BackoffMax)
			}
			continue
		}
		exited := make(chan error, 1)
		go func() { exited <- p.Wait() }()

		// 3. 等就绪，再等退出、重启请求或停止。
		rctx, rcancel := context.WithCancel(ctx)
		readyCh := make(chan struct{})
		go func() {
			for rctx.Err() == nil {
				if e.dialReady(rctx) == nil {
					close(readyCh)
					return
				}
				select {
				case <-rctx.Done():
				case <-time.After(t.ReadyPoll):
				}
			}
		}()
		var readyAt time.Time
		stop := func() {
			rcancel()
			_ = p.Signal(syscall.SIGTERM)
			select {
			case <-exited:
			case <-time.After(t.KillAfter):
				_ = p.Signal(syscall.SIGKILL)
				<-exited
			}
		}
	running:
		for {
			select {
			case <-readyCh:
				readyCh = nil
				readyAt = time.Now()
				e.setState(EngineRunning, "", false)
			case err := <-exited:
				rcancel()
				msg := "进程退出"
				if err != nil {
					msg = shortErr(nil, err)
				}
				if !readyAt.IsZero() && time.Since(readyAt) >= t.StableFor {
					backoff = t.BackoffMin
				}
				e.logf("%s 意外退出（%s），%s 后重启", e.Name, msg, backoff)
				e.setState(EngineBackoff, msg, true)
				switch e.sleep(ctx, backoff) {
				case sleepStopped:
					e.setState(EngineStopped, "", false)
					return
				case sleepRestarted:
					backoff = t.BackoffMin
				default:
					backoff = min(backoff*2, t.BackoffMax)
				}
				break running
			case <-e.restart:
				stop()
				backoff = t.BackoffMin
				break running
			case <-ctx.Done():
				stop()
				e.setState(EngineStopped, "", false)
				return
			}
		}
	}
}

const (
	sleepElapsed = iota
	sleepRestarted
	sleepStopped
)

// sleep 等 d；期间收到重启请求提前结束（新配置不必等完退避，退避也复位）。
func (e *Engine) sleep(ctx context.Context, d time.Duration) int {
	select {
	case <-ctx.Done():
		return sleepStopped
	case <-time.After(d):
		return sleepElapsed
	case <-e.restart:
		return sleepRestarted
	}
}

// ExecRunner 用 os/exec 运行真实进程。
type ExecRunner struct{}

type execProc struct{ cmd *exec.Cmd }

func (p execProc) Wait() error                     { return p.cmd.Wait() }
func (p execProc) Signal(sig syscall.Signal) error { return p.cmd.Process.Signal(sig) }

func (ExecRunner) Start(argv, env []string, dir string, out io.Writer) (Proc, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env, cmd.Dir, cmd.Stdout, cmd.Stderr = env, dir, out, out
	// 引擎若留下持有输出管道的孙进程，Wait 最多再等 5 秒，不让停止流程卡死。
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return execProc{cmd}, nil
}

func (ExecRunner) Run(ctx context.Context, argv, env []string, dir string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env, cmd.Dir = env, dir
	return cmd.CombinedOutput()
}
