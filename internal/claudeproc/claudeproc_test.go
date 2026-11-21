package claudeproc

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"claude-abroad/internal/procinfo"
)

const (
	desktop    = "/Applications/Claude.app/Contents/MacOS/Claude"
	helper     = "/Applications/Claude.app/Contents/Frameworks/Claude Helper.app/Contents/MacOS/Claude Helper"
	native     = "/Users/alice/.local/share/claude/versions/2.1.319"
	brewClaude = "/opt/homebrew/bin/claude"
	fakeClaude = "/usr/local/bin/claude" // 只是文件名叫 claude
	node       = "/usr/local/bin/node"
	self       = 4000
)

func signer() *FakeSigner {
	return &FakeSigner{Teams: map[string]string{desktop: AnthropicTeamID, helper: AnthropicTeamID, brewClaude: AnthropicTeamID,
		native: ""}, Err: map[string]bool{}}
}

func p(pid, ppid int, path string, args ...string) procinfo.Proc {
	if len(args) == 0 {
		args = []string{path}
	}
	return procinfo.Proc{PID: pid, PPID: ppid, Path: path, Args: args}
}

// world 是 a 的进程表。
func scanWorld() *FakeWorld {
	w := NewFakeWorld()
	for _, x := range []procinfo.Proc{
		p(1, 0, "/sbin/launchd"),
		p(100, 1, desktop),
		p(101, 100, helper, helper, "--type=renderer"),
		p(102, 101, "/bin/zsh"), // 后代的后代
		p(200, 1, native, "claude", "--resume"),
		p(201, 200, "/usr/local/bin/mcp-server"),
		p(202, 201, "/bin/sh", "/bin/sh", "-c", "git status"),
		p(300, 1, brewClaude),
		p(400, 1, fakeClaude),
		p(500, 1, node, "node", "/usr/local/lib/node_modules/@anthropic-ai/claude-code/cli.js"),
		p(501, 1, node, "node", "/srv/app/server.js"),
		p(502, 1, "/opt/bun", "bun", "x", "@anthropic-ai/claude-code"),
		p(600, 1, "/Applications/Other.app/Contents/MacOS/Other"),
		p(700, 1, "/Applications/Evil/Claude.app/Contents/MacOS/Claude"), // Bundle ID 相符、签名不符
		p(800, 1, "/opt/homebrew/bin/claude-unsigned/claude"),            // 签名查询失败
		// 环：900 ↔ 901，900 是 Claude Code 的后代
		p(900, 200, "/bin/a"), p(901, 900, "/bin/b"),
		// abroadd 自己是 Claude 的后代；它的子进程也不动
		p(self, 200, "/usr/local/bin/abroadd"), p(self+1, self, "/usr/local/bin/abroad-vm"),
	} {
		w.Add(x, ExitOnTerm, 0)
	}
	// 让 901 的父进程是 900、900 的父进程也指回 901 形成环（另外再从 200 挂进来）。
	w.mu.Lock()
	q := w.procs[900]
	q.PPID = 901
	w.procs[900] = q
	w.mu.Unlock()
	w.Add(p(902, 200, "/bin/c"), ExitOnTerm, 0)
	w.Add(p(903, 902, "/bin/d"), ExitOnTerm, 0)
	return w
}

func pids(ts []Target) []int {
	out := make([]int, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.PID)
	}
	return out
}

// a. 扫描。
func TestScan(t *testing.T) {
	w := scanWorld()
	s := signer()
	s.Err["/opt/homebrew/bin/claude-unsigned/claude"] = true
	s.Teams["/Applications/Evil/Claude.app/Contents/MacOS/Claude"] = "ABCDE12345"
	svc := NewFakeService(w, s, self, false, nil)
	rep, err := svc.Scanner.Scan()
	if err != nil {
		t.Fatal(err)
	}
	want := []int{100, 101, 102, 200, 201, 202, 300, 500, 502, 902, 903}
	if got := pids(rep.Targets); !slices.Equal(got, want) {
		t.Fatalf("清单：%v，应为 %v", got, want)
	}
	notes := map[int]string{}
	for _, u := range rep.Unverified {
		notes[u.PID] = u.Note
	}
	if notes[700] != noteWrongSign || notes[800] != noteUnsigned || len(notes) != 2 {
		t.Fatalf("没能确认的应标出：%v", notes)
	}
	for _, x := range []int{1, 400, 501, 600, self, self + 1} {
		if slices.Contains(pids(rep.Targets), x) {
			t.Fatalf("%d 不应在清单里", x)
		}
	}
	// 环：只从环外挂进来时，环里的进程不在清单里（它们的祖先不是 Claude），而且扫描不会死循环。
	if slices.Contains(pids(rep.Targets), 900) {
		t.Fatal("900、901 成环且不挂在 Claude 下")
	}
	apps := map[int]string{}
	for _, x := range rep.Targets {
		apps[x.PID] = x.App
	}
	if apps[100] != appDesktop || apps[101] != appDesktop || apps[102] != appDesktop+" 的子进程" || apps[200] != appCode {
		t.Fatalf("应用名：%v", apps)
	}
}

// a. 环挂在 Claude 下：深度不限，有环也不死循环。
func TestScanCycleAndDepth(t *testing.T) {
	w := NewFakeWorld()
	w.Add(p(10, 1, native), ExitOnTerm, 0)
	prev := 10
	for i := 11; i < 211; i++ { // 200 层
		w.Add(p(i, prev, "/bin/x"), ExitOnTerm, 0)
		prev = i
	}
	w.Add(p(300, 210, "/bin/y"), ExitOnTerm, 0)
	w.Add(p(301, 300, "/bin/z"), ExitOnTerm, 0)
	w.mu.Lock()
	q := w.procs[300]
	q.PPID = 301 // 300 ↔ 301 成环；210 的子进程里没有 300 了
	w.procs[300] = q
	w.mu.Unlock()
	w.Add(p(302, 210, "/bin/w"), ExitOnTerm, 0)
	w.mu.Lock()
	q = w.procs[301]
	q.PPID = 302
	w.procs[301] = q
	w.mu.Unlock()
	svc := NewFakeService(w, signer(), self, false, nil)
	done := make(chan Report, 1)
	go func() { r, _ := svc.Scanner.Scan(); done <- r }()
	select {
	case r := <-done:
		got := pids(r.Targets)
		if len(got) != 204 || !slices.Contains(got, 300) || !slices.Contains(got, 301) {
			t.Fatalf("200 层加环：%d 个", len(got))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("扫描死循环")
	}
}

// node 经 bin 链接启动：参数是 /usr/local/bin/claude，链接指向包目录时算；不指向时不算。
func TestScanNodeSymlink(t *testing.T) {
	w := NewFakeWorld()
	w.Add(p(10, 1, node, "node", "/usr/local/bin/claude"), ExitOnTerm, 0)
	w.Add(p(11, 1, node, "node", "/opt/bin/claude"), ExitOnTerm, 0)
	sc := &Scanner{Procs: w, Signer: signer(), Resolve: func(s string) (string, error) {
		if s == "/usr/local/bin/claude" {
			return "/usr/local/lib/node_modules/@anthropic-ai/claude-code/cli.js", nil
		}
		return s, nil
	}}
	r, _ := sc.Scan()
	if got := pids(r.Targets); !slices.Equal(got, []int{10}) {
		t.Fatalf("%v", got)
	}
}

// g. 预览内容：截断到 200 字符、主目录换成 ~、没有环境变量（procinfo 只给参数）。
func TestDisplayCmd(t *testing.T) {
	long := "/Users/alice/proj/" + strings.Repeat("x", 300)
	c := DisplayCmd(procinfo.Proc{Path: native, Args: []string{"/Users/alice/.local/bin/claude", "--add-dir", long}})
	if strings.Contains(c, "/Users/alice") || !strings.HasPrefix(c, "~/.local/bin/claude --add-dir ~/proj/") {
		t.Fatalf("主目录没换：%q", c)
	}
	if n := len([]rune(c)); n != MaxCmd || !strings.HasSuffix(c, "…") {
		t.Fatalf("应截断到 %d：%d", MaxCmd, n)
	}
	if c := DisplayCmd(procinfo.Proc{Path: "/bin/sh", Args: []string{"sh", "-c", "a\nb\x1b"}}); strings.ContainsAny(c, "\n\x1b") {
		t.Fatalf("控制字符应替换：%q", c)
	}
}

// ---- b. 结束流程 ----

func killWorld(t *testing.T, w *FakeWorld) (*Service, *[]string) {
	t.Helper()
	var logs []string
	svc := NewFakeService(w, signer(), self, false, func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
	return svc, &logs
}

func sigs(w *FakeWorld) []string {
	var out []string
	for _, r := range w.SignalLog() {
		out = append(out, r.String())
	}
	return out
}

func TestKillTermThenKill(t *testing.T) {
	w := NewFakeWorld()
	w.Add(p(10, 1, native), ExitOnTerm, 2*time.Second) // 2 秒内退出：不发 KILL
	w.Add(p(11, 10, "/bin/sh"), ExitOnKill, 0)         // 不理 TERM
	w.Add(p(12, 1, brewClaude), ExitOnTerm, 0)
	w.Add(p(1, 0, "/sbin/launchd"), ExitOnTerm, 0)
	svc, _ := killWorld(t, w)
	start := w.Now()
	if err := svc.KillClaude(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := sigs(w), []string{"TERM 10", "TERM 11", "TERM 12", "KILL 11"}; !slices.Equal(got, want) {
		t.Fatalf("信号：%v，应为 %v", got, want)
	}
	if el := w.Now().Sub(start); el < DefaultGrace || el > DefaultGrace+2*time.Second {
		t.Fatalf("应等满 3 秒再 KILL：%v", el)
	}
	if !w.Alive(1) {
		t.Fatal("1 号进程不应受影响")
	}
}

// 结束途中新启动的 Claude：下一轮扫描到，同样处理；最多 3 轮。
func TestKillNewProcessesRounds(t *testing.T) {
	w := NewFakeWorld()
	w.Add(p(10, 1, native), ExitOnTerm, 0)
	next := 20
	w.OnSignal = func(r SignalRec) {
		if r.Sig == syscall.SIGTERM && next < 22 { // 前两轮各冒出一个新的
			w.Add(p(next, 1, native), ExitOnTerm, 0)
			next++
		}
	}
	svc, _ := killWorld(t, w)
	if err := svc.KillClaude(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := sigs(w), []string{"TERM 10", "TERM 20", "TERM 21"}; !slices.Equal(got, want) {
		t.Fatalf("%v", got)
	}
	// 每轮都冒新的：3 轮后仍有残留 → 返回残留和错误。
	w2 := NewFakeWorld()
	w2.Add(p(10, 1, native), ExitOnTerm, 0)
	n2 := 100
	w2.OnSignal = func(r SignalRec) {
		if r.Sig == syscall.SIGTERM {
			w2.Add(p(n2, 1, native), ExitOnTerm, 0)
			n2++
		}
	}
	svc2, logs := killWorld(t, w2)
	err := svc2.KillClaude(context.Background())
	var le *LeftError
	if !errors.As(err, &le) || len(le.Left) != 1 || !strings.Contains(err.Error(), "1 个") {
		t.Fatalf("应返回残留：%v", err)
	}
	if n := strings.Count(strings.Join(sigs(w2), ","), "TERM"); n != 3 {
		t.Fatalf("最多 3 轮：%d", n)
	}
	for _, l := range *logs {
		if strings.Contains(l, native) || strings.Contains(l, "alice") {
			t.Fatalf("日志不应有命令行：%s", l)
		}
	}
}

func TestKillUnkillableLeft(t *testing.T) {
	w := NewFakeWorld()
	w.Add(p(10, 1, native), Unkillable, 0)
	svc, _ := killWorld(t, w)
	err := svc.KillClaude(context.Background())
	var le *LeftError
	if !errors.As(err, &le) || len(le.Left) != 1 || le.Left[0].PID != 10 {
		t.Fatalf("%v", err)
	}
	if got, want := sigs(w), []string{"TERM 10", "KILL 10", "TERM 10", "KILL 10", "TERM 10", "KILL 10"}; !slices.Equal(got, want) {
		t.Fatalf("%v", got)
	}
}

// 发信号前复核：进程号被复用（启动时间不同）时不发。
func TestKillPIDReused(t *testing.T) {
	w := NewFakeWorld()
	w.Add(p(10, 1, native), ExitOnTerm, 0)
	svc, _ := killWorld(t, w)
	sc := svc.Scanner
	rep, _ := sc.Scan()
	// 扫描之后 10 退出，进程号被一个无关进程复用。
	w.Remove(10)
	w.Sleep(context.Background(), time.Second)
	w.Add(p(10, 1, "/usr/bin/vim"), ExitOnTerm, 0)
	k := svc.Killer
	k.send(rep.Targets[0], syscall.SIGTERM)
	if len(w.SignalLog()) != 0 || !w.Alive(10) {
		t.Fatal("进程号被复用时不应发信号")
	}
}

// 任何情况下都不向自己和 1 号发信号：即使扫描结果里有（Self 不对时也有 1 号的保护）。
func TestKillNeverSelfOrInit(t *testing.T) {
	w := NewFakeWorld()
	w.Add(p(1, 0, native), ExitOnTerm, 0) // 极端：1 号看起来像 Claude
	w.Add(p(self, 1, native), ExitOnTerm, 0)
	w.Add(p(self+1, self, native), ExitOnTerm, 0)
	svc, _ := killWorld(t, w)
	_ = svc.KillClaude(context.Background())
	for _, r := range w.SignalLog() {
		if r.PID == 1 || r.PID == self || r.PID == self+1 {
			t.Fatalf("向 %d 发了信号", r.PID)
		}
	}
	// 直接调用 send 也不行。
	svc.Killer.send(Target{PID: self}, syscall.SIGKILL)
	svc.Killer.send(Target{PID: 1}, syscall.SIGKILL)
	if len(w.SignalLog()) != 0 {
		t.Fatal("不应有任何信号")
	}
}

// 不结束任何进程的实现：预览照常、标明不会结束；结束时没有信号。
func TestSkipService(t *testing.T) {
	w := scanWorld()
	var logs []string
	svc := NewFakeService(w, signer(), self, true, func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) })
	pv, err := svc.Preview(context.Background())
	if err != nil || len(pv.Processes) == 0 || pv.WillKill || pv.Notice != SkipNotice {
		t.Fatalf("%+v %v", pv, err)
	}
	if err := svc.KillClaude(context.Background()); err != nil || len(w.SignalLog()) != 0 {
		t.Fatalf("不应发信号：%v %v", err, w.SignalLog())
	}
	if !strings.Contains(strings.Join(logs, "\n"), "!!!!!!!!") {
		t.Fatal("应有醒目日志")
	}
}

func TestParseCodesign(t *testing.T) {
	for out, want := range map[string]string{
		"Executable=/x\nIdentifier=com.anthropic.claude-code\nTeamIdentifier=Q6L2SF6YDW\n": "Q6L2SF6YDW",
		"Identifier=com.apple.ls\nTeamIdentifier=not set\n":                                "",
		"/tmp/x: code object is not signed at all\n":                                       "",
	} {
		got, err := ParseCodesign(out)
		if err != nil || got != want {
			t.Fatalf("%q → %q %v", out, got, err)
		}
	}
	for _, out := range []string{"", "TeamIdentifier=lowercase1\n", "error: something\n", "TeamIdentifier=TOOLONG12345\n"} {
		if _, err := ParseCodesign(out); err == nil {
			t.Fatalf("不认识的输出应返回错误：%q", out)
		}
	}
}
