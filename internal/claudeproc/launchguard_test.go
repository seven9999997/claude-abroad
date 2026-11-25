package claudeproc

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"claude-abroad/internal/procinfo"
)

type guardRig struct {
	w      *FakeWorld
	s      *fakeProcSigner
	g      *LaunchGuard
	mu     sync.Mutex
	pol    GuardPolicy
	blocks []GuardRecord
}

func newGuardRig(t *testing.T) *guardRig {
	t.Helper()
	r := &guardRig{w: NewFakeWorld(), s: newFakeProcSigner()}
	r.g = &LaunchGuard{Procs: r.w, Info: r.w.Info, Sig: r.w, Clock: r.w, Self: 999,
		Judge:  &AutoJudge{Signer: r.s, Bundle: fakeBundles},
		Policy: func() GuardPolicy { r.mu.Lock(); defer r.mu.Unlock(); return r.pol },
		OnBlock: func(rec GuardRecord) {
			r.mu.Lock()
			r.blocks = append(r.blocks, rec)
			r.mu.Unlock()
		}}
	return r
}

func (r *guardRig) setPolicy(p GuardPolicy) { r.mu.Lock(); r.pol = p; r.mu.Unlock() }

// addClaude 加一个签名是 Anthropic 的原生 Claude Code 进程（启动时间是当前假时间）。
func (r *guardRig) addClaude(pid int, b Behavior) {
	r.s.mu.Lock()
	r.s.teams[pid] = AnthropicTeamID
	r.s.mu.Unlock()
	r.w.Add(procinfo.Proc{PID: pid, PPID: 1, Path: nativeCode, Name: "2.1.319"}, b, 0)
}

func (r *guardRig) advance(d time.Duration) { _ = r.w.Sleep(context.Background(), d) }

func sigLog(w *FakeWorld) string {
	var out []string
	for _, s := range w.SignalLog() {
		out = append(out, s.String())
	}
	return strings.Join(out, ",")
}

// 允许启动时：不结束任何进程。
func TestGuardAllowed(t *testing.T) {
	r := newGuardRig(t)
	r.addClaude(100, ExitOnTerm)
	r.setPolicy(GuardPolicy{Allowed: true})
	if got := r.g.Tick(); len(got) != 0 || len(r.w.SignalLog()) != 0 {
		t.Fatalf("允许时不应结束：%v %s", got, sigLog(r.w))
	}
}

// 口径 B、C：只结束“不允许”开始之后启动的进程；之前已经在运行的不结束。
func TestGuardOnlyAfterSince(t *testing.T) {
	r := newGuardRig(t)
	r.addClaude(100, ExitOnTerm) // 早就在运行
	r.advance(time.Second)
	since := r.w.Now()
	r.setPolicy(GuardPolicy{Since: since})
	r.advance(time.Second)
	r.addClaude(101, ExitOnTerm) // 之后才启动
	got := r.g.Tick()
	if len(got) != 1 || got[0].PID != 101 || !r.w.Alive(100) || r.w.Alive(101) {
		t.Fatalf("只应结束之后启动的：%v，信号 %s", got, sigLog(r.w))
	}
	if got[0].Kind != KindNative || got[0].Name == "" {
		t.Fatalf("记录 %+v", got[0])
	}
}

// 口径 D：回国状态，不看启动时间，所有确认过的都结束；不能确认的（npm）不碰。
func TestGuardCoverAll(t *testing.T) {
	r := newGuardRig(t)
	r.addClaude(100, ExitOnTerm)
	r.w.Add(procinfo.Proc{PID: 102, PPID: 1, Path: nodeBin, Args: []string{nodeBin, "/x/node_modules/@anthropic-ai/claude-code/cli.js"}, Name: "node"}, ExitOnTerm, 0)
	r.w.Add(procinfo.Proc{PID: 103, PPID: 1, Path: "/usr/local/bin/claude", Name: "claude"}, ExitOnTerm, 0) // 没签名
	r.advance(time.Second)
	r.setPolicy(GuardPolicy{Since: r.w.Now(), CoverAll: true})
	r.g.Tick()
	if r.w.Alive(100) || !r.w.Alive(102) || !r.w.Alive(103) {
		t.Fatalf("应只结束确认过的：信号 %s", sigLog(r.w))
	}
}

// 不理 TERM 的：宽限期之后下一轮补 KILL；扫描不被阻塞；同一个进程只记一次拦截；自己和 1 号进程永不碰。
func TestGuardTermThenKill(t *testing.T) {
	r := newGuardRig(t)
	r.setPolicy(GuardPolicy{CoverAll: true})
	r.addClaude(200, ExitOnKill)
	r.addClaude(999, ExitOnTerm) // abroadd 自己（假设签名也对）
	r.g.Tick()
	if sigLog(r.w) != "TERM 200" {
		t.Fatalf("第一轮只发 TERM：%s", sigLog(r.w))
	}
	r.advance(500 * time.Millisecond)
	r.g.Tick()
	if sigLog(r.w) != "TERM 200" {
		t.Fatalf("宽限期内不重复发：%s", sigLog(r.w))
	}
	r.advance(600 * time.Millisecond)
	r.g.Tick()
	if sigLog(r.w) != "TERM 200,KILL 200" || r.w.Alive(200) {
		t.Fatalf("宽限期后应 KILL：%s", sigLog(r.w))
	}
	if len(r.g.Records()) != 1 || len(r.blocks) != 1 {
		t.Fatalf("同一个进程只记一次：%v", r.g.Records())
	}
}

// 进程号被复用：KILL 前复核启动时间，不打到新进程上。
func TestGuardPidReuse(t *testing.T) {
	r := newGuardRig(t)
	r.setPolicy(GuardPolicy{CoverAll: true})
	r.addClaude(300, ExitOnKill)
	r.g.Tick() // TERM
	r.w.Remove(300)
	r.advance(2 * time.Second)
	r.w.Add(procinfo.Proc{PID: 300, PPID: 1, Path: "/bin/zsh", Name: "zsh"}, ExitOnTerm, 0) // 进程号被别的进程复用
	r.g.Tick()
	if strings.Contains(sigLog(r.w), "KILL 300") || !r.w.Alive(300) {
		t.Fatalf("不应打到复用了进程号的新进程：%s", sigLog(r.w))
	}
}

// 扫描失败：这一轮不结束任何进程。
func TestGuardScanFails(t *testing.T) {
	r := newGuardRig(t)
	r.setPolicy(GuardPolicy{CoverAll: true})
	r.g.Procs = failLister{}
	if got := r.g.Tick(); len(got) != 0 {
		t.Fatal("扫描失败时不应结束")
	}
}

type failLister struct{}

func (failLister) List() ([]procinfo.Proc, error) { return nil, context.DeadlineExceeded }

// Run：按间隔扫描，ctx 结束时返回。
func TestGuardRun(t *testing.T) {
	r := newGuardRig(t)
	r.setPolicy(GuardPolicy{CoverAll: true})
	r.addClaude(400, ExitOnTerm)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.g.Run(ctx); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for r.w.Alive(400) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if r.w.Alive(400) {
		t.Fatal("Run 应结束它")
	}
}
