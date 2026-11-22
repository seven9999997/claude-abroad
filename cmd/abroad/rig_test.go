package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"claude-abroad/internal/agent"
	"claude-abroad/internal/apiclient"
	"claude-abroad/internal/claudeproc"
	"claude-abroad/internal/daemon"
	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/exitpolicy"
	"claude-abroad/internal/procinfo"
	"claude-abroad/internal/usernet"
)

// 调用方：属主（完全信任，相当于 sudo）和控制台用户。
const consoleUID = 7001

// rig 是进程内的假系统 abroadd，加上按调用方注入的对端 uid。
type rig struct {
	t    *testing.T
	sys  *daemon.FakeSystem
	d    *daemon.Daemon
	dir  string
	peer atomic.Int64
	vm   *fakeVM
	pr   *fakeProber
	sub  []byte // 订阅地址抓到的内容
	mu   sync.Mutex
	urls []string // 抓过的订阅地址
}

type rigOpt struct {
	main   bool // 主代理一侧：基准、假虚拟机、订阅抓取
	claude bool // 进程表里有一个 Claude
	reject bool // abroadd 认为对端是“其他用户”（连接被断开）
}

func newRig(t *testing.T, o rigOpt) *rig {
	t.Helper()
	short, err := os.MkdirTemp("", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	r := &rig{t: t, sys: daemon.NewFakeSystem(), dir: filepath.Join(short, "st")}
	r.peer.Store(int64(os.Getuid()))
	if o.claude {
		const exe = "/Applications/Claude.app/Contents/MacOS/Claude"
		r.sys.Signer.Teams[exe] = claudeproc.AnthropicTeamID
		r.sys.Procs.Add(procinfo.Proc{PID: 7100, PPID: 1, Path: exe, Args: []string{exe}}, claudeproc.ExitOnTerm, 0)
	}
	sys := r.sys.System()
	if o.main {
		r.vm, r.pr = &fakeVM{boot: "boot-1"}, &fakeProber{}
		sys.Baseline = daemon.NewBaselineService(r.pr, r.dir)
		sys.MainVM, sys.ExitVM = r.vm, r.vm
		sys.CheckExit = func(ctx context.Context, tag string) exitcheck.Report {
			rep := r.pr.Check(ctx, tag, exitcheck.Options{})
			rep.Exit = tag
			return rep
		}
		sys.FetchSource = func(_ context.Context, u string, _ bool) ([]byte, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.urls = append(r.urls, u)
			return r.sub, nil
		}
	}
	opts := daemon.Options{StateDir: r.dir, Sys: sys, SuperviseInterval: time.Hour, LockTimeout: 300 * time.Millisecond, LockRetry: 20 * time.Millisecond,
		PeerUID: func(*net.UnixConn) (int, error) {
			if o.reject {
				return 7002, nil
			}
			return int(r.peer.Load()), nil
		},
		ConsoleUID: func() (int, error) { return consoleUID, nil },
	}
	d, err := daemon.Start(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	r.d = d
	t.Cleanup(func() { d.Shutdown() })
	return r
}

func (r *rig) asConsole() { r.peer.Store(consoleUID) }
func (r *rig) asOwner()   { r.peer.Store(int64(os.Getuid())) }

// res 是一次命令行运行的结果。
type res struct {
	code     int
	out, err string
	reqs     int
}

func (x res) all() string { return x.out + x.err }

// in 是一次运行的输入：标准输入、是不是终端、是否绕过本地的 sudo 检查。
type in struct {
	stdin string
	tty   bool
	skip  bool
	ctx   context.Context
	out   *syncBuf
}

func (r *rig) run(args ...string) res { return r.runWith(in{}, args...) }

func (r *rig) runWith(i in, args ...string) res {
	r.t.Helper()
	var out, errb bytes.Buffer
	cl := &apiclient.Client{Socket: r.d.SocketPath(), Timeout: 30 * time.Second}
	a := &app{stdin: strings.NewReader(i.stdin), stdout: &out, stderr: &errb, client: cl, ctx: i.ctx, skipLocalCheck: i.skip,
		isAdmin:    func() bool { return r.peer.Load() != consoleUID },
		isTerminal: func() bool { return i.tty }}
	if i.out != nil {
		a.stdout = i.out
	}
	code := a.run(args)
	return res{code: code, out: out.String(), err: errb.String(), reqs: cl.Requests}
}

func (r *rig) status() daemon.Status {
	r.t.Helper()
	x := r.run("--json", "status")
	var st daemon.Status
	if x.code != 0 || json.Unmarshal([]byte(x.out), &st) != nil {
		r.t.Fatalf("status：%+v", x)
	}
	return st
}

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// ---- 假的探测器和虚拟机（主代理一侧） ----

type fakeProber struct{}

func (fakeProber) Check(context.Context, string, exitcheck.Options) exitcheck.Report {
	g := &exitpolicy.Geo{IP: netip.MustParseAddr("203.0.113.7"), Country: "US", Timezone: "America/Los_Angeles"}
	return exitcheck.Report{Exit: "main", At: time.Now(), Verdict: exitcheck.Unknown, IP: g.IP, Country: "US", Geo: g, Timezone: g.Timezone,
		Items: []exitcheck.Item{{Name: "https", Result: exitcheck.Pass}, {Name: "ip", Result: exitcheck.Pass}, {Name: "mainland", Result: exitcheck.Pass},
			{Name: "geo", Result: exitcheck.Pass}, {Name: "rule", Result: exitcheck.Unknown, Cause: exitcheck.CauseNoBaseline}, {Name: "doh", Result: exitcheck.Pass}}}
}

type fakeVM struct {
	mu     sync.Mutex
	boot   string
	digest string
	pushes int
}

func (f *fakeVM) BootID(context.Context) (string, error)                     { return f.boot, nil }
func (f *fakeVM) PushMainHops(context.Context, usernet.MainHops) error       { return nil }
func (f *fakeVM) HopPrivate(context.Context) (int64, error)                  { return 0, nil }
func (f *fakeVM) PushEndpoints(context.Context, []daemon.ExitEndpoint) error { return nil }
func (f *fakeVM) AgentExits(context.Context) ([]daemon.AgentExit, error)     { return nil, nil }
func (f *fakeVM) PushMainProxy(_ context.Context, y []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.digest, f.pushes = agent.ConfigDigest(y), f.pushes+1
	return nil
}
func (f *fakeVM) MainProxyDigest(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.digest, nil
}
func (f *fakeVM) PushExits(_ context.Context, xs []daemon.StoredExit) ([]daemon.AgentExit, error) {
	var out []daemon.AgentExit
	for _, x := range xs {
		out = append(out, daemon.AgentExit{Tag: x.Tag, Up: true})
	}
	return out, nil
}
func (f *fakeVM) Health(context.Context) (daemon.VMHealth, error) {
	return daemon.VMHealth{BootID: f.boot, Version: "agent-1.0", Engines: map[string]daemon.EngineView{"mihomo": {State: "running"}}}, nil
}
