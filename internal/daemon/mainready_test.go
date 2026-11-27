package daemon

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/state"
	"claude-abroad/internal/wire"
)

// ---- 下发之后主代理要过一会儿才可用（2026-11-27 真机：借用时下发后立即检查，出口检查全是“虚拟机拒绝了数据流：exit_down”）----

// slowMainVM：下发主代理后 readyAfter 才报告主代理引擎可用（never 为真时一直不可用）。没就绪时开流回 exit_down，
// health 里 mihomo 是 backoff。每次读 health 时记下闸门是不是开着。
type slowMainVM struct {
	*vmBoundMainVM
	gateOpen func() bool

	mu         sync.Mutex
	readyAfter time.Duration
	never      bool
	pushedAt   time.Time
	polls      int
	gateSeen   []bool
}

func (s *slowMainVM) PushMainProxy(ctx context.Context, y []byte) error {
	if err := s.vmBoundMainVM.PushMainProxy(ctx, y); err != nil {
		return err
	}
	s.mu.Lock()
	s.pushedAt = time.Now()
	s.mu.Unlock()
	return nil
}

func (s *slowMainVM) mainUp() bool {
	if !s.alive() {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.never && !s.pushedAt.IsZero() && time.Since(s.pushedAt) >= s.readyAfter
}

func (s *slowMainVM) Health(context.Context) (VMHealth, error) {
	if !s.alive() {
		return VMHealth{}, ErrVMNotReady
	}
	up := s.mainUp()
	open := s.gateOpen != nil && s.gateOpen()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.polls++
	s.gateSeen = append(s.gateSeen, open)
	m := EngineView{State: "running"}
	switch {
	case s.pushedAt.IsZero():
		m = EngineView{State: "stopped"}
	case !up:
		m = EngineView{State: "backoff", Restarts: 1, LastError: "进程退出"}
	}
	return VMHealth{BootID: s.boot, Engines: map[string]EngineView{"sing-box": {State: "running"}, "mihomo": m}}, nil
}

func (s *slowMainVM) set(after time.Duration, never bool) {
	s.mu.Lock()
	s.readyAfter, s.never = after, never
	s.mu.Unlock()
}

func (s *slowMainVM) sawGateOpen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.gateSeen {
		if o {
			return true
		}
	}
	return false
}

func (s *slowMainVM) pollCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.polls }

// exitDownReport 是主代理引擎没起来时的检查结果：五项都是“连不上”，原因是虚拟机拒绝了数据流（exit_down）。
func exitDownReport() exitcheck.Report {
	why := (&wire.ReplyError{Code: wire.CodeExitDown}).Error()
	var items []exitcheck.Item
	for _, n := range []string{"https", "ip", "mainland", "rule", "doh"} {
		items = append(items, exitcheck.Item{Name: n, Result: exitcheck.Unknown, Cause: exitcheck.CauseConnect, Detail: why})
	}
	return exitcheck.Report{Exit: "main", Verdict: exitcheck.Unknown, Items: items}
}

// mainBoundProber：主代理没就绪时回 exit_down 的报告。
type mainBoundProber struct {
	inner *vmBoundProber
	vm    *slowMainVM
	mu    sync.Mutex
	early int // 主代理没就绪时被调用的次数
}

func (p *mainBoundProber) Check(ctx context.Context, exit string, o exitcheck.Options) exitcheck.Report {
	if p.vm.alive() && !p.vm.mainUp() {
		p.mu.Lock()
		p.early++
		p.mu.Unlock()
		return exitDownReport()
	}
	return p.inner.Check(ctx, exit, o)
}

func (p *mainBoundProber) earlyCalls() int { p.mu.Lock(); defer p.mu.Unlock(); return p.early }

// mainBoundExit 是启用第 ⑤ 步的检查：主代理没就绪时失败（开流回 exit_down）。
type mainBoundExit struct {
	vm    *slowMainVM
	mu    sync.Mutex
	early int
}

func (e *mainBoundExit) Check(context.Context) error {
	if !e.vm.mainUp() {
		e.mu.Lock()
		e.early++
		e.mu.Unlock()
		return fmt.Errorf("出口检查没有通过：%w", &wire.ReplyError{Code: wire.CodeExitDown})
	}
	return nil
}

func (e *mainBoundExit) earlyCalls() int { e.mu.Lock(); defer e.mu.Unlock(); return e.early }

type slowEnv struct {
	*borrowEnv
	slow *slowMainVM
	pr   *mainBoundProber
	ex   *mainBoundExit
}

func newSlowEnv(t *testing.T, after time.Duration, never bool) *slowEnv {
	t.Helper()
	s := &slowEnv{}
	s.borrowEnv = newBorrowEnv(t, func(b *borrowEnv, o *Options) {
		s.slow = &slowMainVM{vmBoundMainVM: b.vm, readyAfter: after, never: never}
		s.pr = &mainBoundProber{inner: b.pr, vm: s.slow}
		s.ex = &mainBoundExit{vm: s.slow}
		o.Sys.MainVM, o.Sys.ExitVM = s.slow, s.slow
		o.Sys.Baseline = NewBaselineService(s.pr, b.env.dir)
		o.Sys.Exit = s.ex
	})
	s.slow.gateOpen = s.d.gate.IsOpen
	return s
}

// shortReadyTimeout 把等主代理就绪的时限临时改短（一直不就绪的用例不必等满 30 秒）。
func shortReadyTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := mainReadyTimeout
	mainReadyTimeout = d
	t.Cleanup(func() { mainReadyTimeout = old })
}

const slowAfter = 1200 * time.Millisecond

// 路 1：借用（应用主代理、检查主代理）：下发后主代理 1.2 秒才可用——先等就绪再检查，成功；检查时主代理已经可用。
func TestMainReadyBorrowWaits(t *testing.T) {
	for _, c := range []struct{ name, path string }{{"apply", "/v1/main-proxy/apply"}, {"check", "/v1/main-proxy/check"}} {
		t.Run(c.name, func(t *testing.T) {
			s := newSlowEnv(t, slowAfter, false)
			s.selectSaved(t)
			g := sampleGate(s.d)
			code, body := call(t, s.d, "POST", c.path, nil)
			g.finish(t)
			if code != 200 {
				t.Fatalf("下发后等主代理就绪再检查，应成功：%d %s", code, body)
			}
			if n := s.pr.earlyCalls(); n != 0 {
				t.Fatalf("主代理没就绪时不应检查出口：提前检查了 %d 次", n)
			}
			if s.slow.pollCount() < 2 {
				t.Fatalf("应轮询虚拟机 health 等主代理就绪：%d 次", s.slow.pollCount())
			}
			if s.slow.sawGateOpen() {
				t.Fatal("等待期间闸门应一直关着")
			}
			waitCond(t, "虚拟机停掉", s.vmStopped)
		})
	}
}

// 路 1：一直不就绪：按时限给出“主代理没有就绪”和 agent 报告的原因（main_unreachable），不检查出口，虚拟机停掉，闸门全程关。
func TestMainReadyBorrowTimeout(t *testing.T) {
	for _, c := range []struct {
		name, path string
		status     int
	}{{"apply", "/v1/main-proxy/apply", 422}, {"check", "/v1/main-proxy/check", 502}} {
		t.Run(c.name, func(t *testing.T) {
			shortReadyTimeout(t, 1500*time.Millisecond)
			s := newSlowEnv(t, 0, true)
			s.selectSaved(t)
			g := sampleGate(s.d)
			start := time.Now()
			code, body := call(t, s.d, "POST", c.path, nil)
			g.finish(t)
			ec, text := errBodyOf(body)
			if code != c.status || ec != "main_unreachable" || !strings.Contains(text, "主代理在虚拟机里 1.5 秒内没有就绪") || !strings.Contains(text, "进程退出") {
				t.Fatalf("应答：%d %s", code, body)
			}
			if el := time.Since(start); el < 1500*time.Millisecond {
				t.Fatalf("应等满时限：%s", el)
			}
			assertNoInternalWords(t, c.name, text)
			if strings.Contains(text, "exit_down") || strings.Contains(text, "backoff") {
				t.Fatalf("不应出现内部代码：%s", text)
			}
			if n := s.pr.earlyCalls(); n != 0 {
				t.Fatalf("没就绪不应检查出口：%d 次", n)
			}
			waitCond(t, "虚拟机停掉", s.vmStopped)
			if _, err := os.Stat(s.d.opts.Sys.Baseline.Path); err == nil {
				t.Fatal("失败时不应保存基准")
			}
			if s.slow.sawGateOpen() {
				t.Fatal("闸门应全程关着")
			}
			if got := borrowKinds(s.d); got[len(got)-1] != BorrowFailed {
				t.Fatalf("最后一条进度应是失败：%v", got)
			}
		})
	}
}

// 路 2：启用第 ④ 步下发之后，先等主代理就绪再做第 ⑤ 步的出口检查：启用成功。
func TestMainReadyEnableWaits(t *testing.T) {
	s := newSlowEnv(t, slowAfter, false)
	s.selectSaved(t)
	saveTestBaseline(t, s.d.opts.Sys.Baseline.Path)
	code, body := callOp(t, s.d, "/v1/enable")
	if code != 200 {
		t.Fatalf("启用应成功：%d %s", code, body)
	}
	if n := s.ex.earlyCalls(); n != 0 {
		t.Fatalf("主代理没就绪时不应做第 ⑤ 步检查：%d 次", n)
	}
	if s.slow.sawGateOpen() {
		t.Fatal("等主代理就绪期间闸门应关着")
	}
}

// 路 2：一直不就绪：启用失败（main_unreachable、明确原因），回到原来的状态，没写“已下发”，虚拟机按回滚停掉，闸门全程关。
func TestMainReadyEnableTimeout(t *testing.T) {
	shortReadyTimeout(t, 1500*time.Millisecond)
	s := newSlowEnv(t, 0, true)
	s.selectSaved(t)
	saveTestBaseline(t, s.d.opts.Sys.Baseline.Path)
	before, _ := s.view.Load()
	g := sampleGate(s.d)
	code, body := callOp(t, s.d, "/v1/enable")
	g.finish(t)
	ec, text := errBodyOf(body)
	if code == 200 || ec != "main_unreachable" || !strings.Contains(text, "主代理在虚拟机里 1.5 秒内没有就绪") || !strings.Contains(text, "进程退出") {
		t.Fatalf("应答：%d %s", code, body)
	}
	if n := s.ex.earlyCalls(); n != 0 {
		t.Fatalf("没就绪不应做第 ⑤ 步检查：%d 次", n)
	}
	if f, _ := s.view.Load(); f.State != before.State || f.State != state.Restored {
		t.Fatalf("应回到原来的状态 %s：%s", before.State, f.State)
	}
	if a, _ := s.d.main.applied(); a != nil {
		t.Fatal("没就绪不应留下“已下发”记录")
	}
	if _, err := os.Stat(s.d.main.file("pending.json")); err != nil {
		t.Fatal("待下发的选择应还在")
	}
	waitCond(t, "虚拟机按回滚停掉", s.vmStopped)
	s.noViolations()
}

// 路 3：已启用时换节点：下发新配置后先等主代理就绪再检查，成功。
func TestMainReadySwitchWaits(t *testing.T) {
	s := newSlowEnv(t, 0, false)
	s.selectSaved(t)
	saveTestBaseline(t, s.d.opts.Sys.Baseline.Path)
	post(t, s.d, "/v1/enable", 200)
	s.slow.set(slowAfter, false)
	m := &mainEnv{env: s.env, d: s.d}
	code, body := m.sel(t, MainSelectRequest{Selected: m.nodeID(t, "exit2")})
	if code != 200 {
		t.Fatalf("换节点应成功：%d %s", code, body)
	}
	if n := s.pr.earlyCalls(); n != 0 {
		t.Fatalf("新主代理没就绪时不应检查：%d 次", n)
	}
}

// 路 3：一直不就绪：换节点失败、回到原来的主代理（switch_rolled_back），原因写明“没有就绪”；等待期间闸门关着。
func TestMainReadySwitchTimeout(t *testing.T) {
	shortReadyTimeout(t, 1500*time.Millisecond)
	s := newSlowEnv(t, 0, false)
	s.selectSaved(t)
	saveTestBaseline(t, s.d.opts.Sys.Baseline.Path)
	post(t, s.d, "/v1/enable", 200)
	old, _ := s.d.main.applied()
	s.slow.mu.Lock()
	s.slow.gateSeen = nil
	s.slow.mu.Unlock()
	s.slow.set(0, true)
	m := &mainEnv{env: s.env, d: s.d}
	code, body := m.sel(t, MainSelectRequest{Selected: m.nodeID(t, "exit2")})
	ec, text := errBodyOf(body)
	if code == 200 || ec != "switch_rolled_back" || !strings.Contains(text, "主代理在虚拟机里 1.5 秒内没有就绪") || !strings.Contains(text, "进程退出") {
		t.Fatalf("应答：%d %s", code, body)
	}
	if n := s.pr.earlyCalls(); n != 0 {
		t.Fatalf("没就绪不应检查：%d 次", n)
	}
	if s.slow.sawGateOpen() {
		t.Fatal("等新主代理就绪期间闸门应关着")
	}
	if a, _ := s.d.main.applied(); a == nil || old == nil || a.Digest != old.Digest {
		t.Fatal("应回到原来的主代理")
	}
}
