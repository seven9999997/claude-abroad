package claudeproc

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	"claude-abroad/internal/procinfo"
)

// 结束流程的参数（design 10.1，m4-plan v2.3 决定 3）。
const (
	DefaultGrace    = 3 * time.Second        // TERM 之后等多久再 KILL
	DefaultKillWait = time.Second            // KILL 之后等它们消失
	DefaultPoll     = 100 * time.Millisecond // 等待时多久看一次
	DefaultRounds   = 3                      // 最多几轮（每轮重新扫描）
)

var errNoSigner = errors.New("没有签名查询")

// Signaler 发信号。真实实现只有 NewRealSignaler（signal_unix.go）。
type Signaler interface {
	Signal(pid int, sig syscall.Signal) error
}

// Clock 是结束流程用的时钟（测试注入）。
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RealClock 是真实时钟。
var RealClock Clock = realClock{}

// LeftError 是结束不了：还有残留的进程。
type LeftError struct{ Left []Target }

func (e *LeftError) Error() string {
	return fmt.Sprintf("还有 %d 个 Claude 进程结束不了", len(e.Left))
}

// Killer 是结束流程：对清单里的全部进程发 TERM，等 Grace；还在的发 KILL；重新扫描，新出现的同样处理，最多 Rounds 轮；
// 之后仍有残留就返回残留清单和 *LeftError。每次发信号前用“进程号加启动时间”复核，不一致（已退出、进程号被复用）就不发。
// 任何情况下都不向 Self 和 0、1 号进程发信号。
type Killer struct {
	Scanner *Scanner
	Info    func(pid int) (procinfo.Proc, error)
	Sig     Signaler
	Clock   Clock
	Grace   time.Duration
	Rounds  int
	// OnSignal 在每次真的发出信号后调用（计数、测试核对顺序）；可为 nil。
	OnSignal func(pid int, sig syscall.Signal)
}

func (k *Killer) clock() Clock {
	if k.Clock != nil {
		return k.Clock
	}
	return RealClock
}

func dur(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

// same 报告进程号上还是清单里的那个进程。
func (k *Killer) same(t Target) bool {
	p, err := k.Info(t.PID)
	return err == nil && p.PID == t.PID && p.Start.Equal(t.Start)
}

func (k *Killer) send(t Target, sig syscall.Signal) {
	if t.PID <= 1 || t.PID == k.Scanner.Self || !k.same(t) {
		return
	}
	if err := k.Sig.Signal(t.PID, sig); err != nil {
		return // 已经退出、没有权限：下一轮扫描会再看到它
	}
	if k.OnSignal != nil {
		k.OnSignal(t.PID, sig)
	}
}

// alive 返回还活着的（还是同一个进程）。
func (k *Killer) alive(ts []Target) []Target {
	var out []Target
	for _, t := range ts {
		if k.same(t) {
			out = append(out, t)
		}
	}
	return out
}

// wait 等到 ts 全部退出或超过 d，返回还活着的。
func (k *Killer) wait(ctx context.Context, ts []Target, d time.Duration) ([]Target, error) {
	c := k.clock()
	end := c.Now().Add(d)
	for {
		left := k.alive(ts)
		if len(left) == 0 || !c.Now().Before(end) {
			return left, nil
		}
		if err := c.Sleep(ctx, min(DefaultPoll, end.Sub(c.Now()))); err != nil {
			return left, err
		}
	}
}

// Kill 执行结束流程。返回 nil 表示扫描不到任何要结束的进程了。
func (k *Killer) Kill(ctx context.Context) ([]Target, error) {
	rounds := k.Rounds
	if rounds <= 0 {
		rounds = DefaultRounds
	}
	for round := 0; round < rounds; round++ {
		rep, err := k.Scanner.Scan()
		if err != nil {
			return nil, fmt.Errorf("扫描进程失败：%w", err)
		}
		if len(rep.Targets) == 0 {
			return nil, nil
		}
		for _, t := range rep.Targets {
			k.send(t, syscall.SIGTERM)
		}
		left, err := k.wait(ctx, rep.Targets, dur(k.Grace, DefaultGrace))
		if err != nil {
			return left, err
		}
		for _, t := range left {
			k.send(t, syscall.SIGKILL)
		}
		if _, err := k.wait(ctx, left, DefaultKillWait); err != nil {
			return nil, err
		}
	}
	rep, err := k.Scanner.Scan()
	if err != nil {
		return nil, fmt.Errorf("扫描进程失败：%w", err)
	}
	if len(rep.Targets) > 0 {
		return rep.Targets, &LeftError{Left: rep.Targets}
	}
	return nil, nil
}
