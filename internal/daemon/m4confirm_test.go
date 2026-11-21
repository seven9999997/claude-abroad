package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"claude-abroad/internal/claudeproc"
	"claude-abroad/internal/gatesm"
	"claude-abroad/internal/pf"
	"claude-abroad/internal/procinfo"
)

// ---- M4-4：两步确认和恢复的端到端（m4-plan v2.3 决定 4、5、6、7）----

const (
	desktopExe = "/Applications/Claude.app/Contents/MacOS/Claude"
	nativeExe  = "/Users/alice/.local/share/claude/versions/2.1.319"
)

// secrets 是测试用的命令行参数和路径：日志、事件、status 里都不应出现。
var secrets = []string{"--secret-flag-abc", "secret-project", "sekrit-session", "/Users/alice", desktopExe}

// addClaude 在假进程表里放桌面版（及一个不理 TERM 的子进程）和一个原生安装的 Claude Code。
func addClaude(e *env, b claudeproc.Behavior) {
	e.sys.Signer.Teams[desktopExe] = claudeproc.AnthropicTeamID
	w := e.sys.Procs
	w.Add(procinfo.Proc{PID: 7100, PPID: 1, Path: desktopExe, Args: []string{desktopExe, "--secret-flag-abc"}}, claudeproc.ExitOnTerm, 0)
	w.Add(procinfo.Proc{PID: 7101, PPID: 7100, Path: "/bin/zsh", Args: []string{"zsh", "-c", "/Users/alice/secret-project/build.sh"}}, claudeproc.ExitOnKill, 0)
	w.Add(procinfo.Proc{PID: 7200, PPID: 1, Path: nativeExe, Args: []string{"claude", "--resume", "sekrit-session"}}, b, 0)
	w.Add(procinfo.Proc{PID: 7300, PPID: 1, Path: "/usr/bin/vim", Args: []string{"vim"}}, claudeproc.ExitOnTerm, 0) // 无关
}

func confirmEnv(t *testing.T, mods ...func(*Options)) (*env, *syncBuf, Options) {
	t.Helper()
	e := newEnv(t)
	buf := &syncBuf{}
	o := e.opts()
	o.Log = log.New(buf, "", 0)
	for _, m := range mods {
		m(&o)
	}
	return e, buf, o
}

// askConfirm 不带确认码调一次恢复：应是 409，带预览和确认码。
func askConfirm(t *testing.T, d *Daemon) ConfirmNeeded {
	t.Helper()
	code, b := call(t, d, "POST", "/v1/restore", nil)
	var cn ConfirmNeeded
	if code != http.StatusConflict || json.Unmarshal(b, &cn) != nil || len(cn.Confirm) != 32 || cn.ExpiresInS != 60 {
		t.Fatalf("不带确认码应回 409、带预览和 128 位确认码：%d %s", code, b)
	}
	return cn
}

func withConfirm(t *testing.T, d *Daemon, c string) (int, []byte) {
	t.Helper()
	body, _ := json.Marshal(ConfirmRequest{Confirm: c})
	return call(t, d, "POST", "/v1/restore", body)
}

func (e *env) stillEnabled(t *testing.T, d *Daemon, what string) {
	t.Helper()
	if s := status(t, d); s.State != "ENABLED" || e.sys.Net.Snapshot().Equal(e.init) || !e.sys.PF.Active(pf.DefaultAnchor) {
		t.Fatalf("%s：系统不应有改动（状态 %s）", what, s.State)
	}
}

func previewPIDs(pv claudeproc.Preview) []int {
	var out []int
	for _, p := range pv.Processes {
		out = append(out, p.PID)
	}
	return out
}

// e、f、g. 不带确认码：409、预览、没有改动、没有信号；用错的码被拒；带正确的码执行：进程被结束、系统还原干净。
// 日志、事件、status 里没有确认码，也没有命令行参数和路径。
func TestRestoreTwoStep(t *testing.T) {
	e, buf, o := confirmEnv(t)
	d := e.start(o)
	post(t, d, "/v1/enable", 200)
	addClaude(e, claudeproc.ExitOnTerm)
	cn := askConfirm(t, d)
	if got := previewPIDs(cn.Preview); !slices.Equal(got, []int{7100, 7101, 7200}) || !cn.Preview.WillKill {
		t.Fatalf("预览：%v %+v", got, cn.Preview)
	}
	var zsh claudeproc.Target
	for _, p := range cn.Preview.Processes {
		if p.PID == 7101 {
			zsh = p
		}
	}
	if zsh.Cmd != "zsh -c ~/secret-project/build.sh" || zsh.App != "Claude 桌面版 的子进程" {
		t.Fatalf("预览的命令行应把主目录换成 ~：%+v", zsh)
	}
	e.stillEnabled(t, d, "只要预览")
	if n := len(e.sys.Procs.SignalLog()); n != 0 {
		t.Fatalf("预览不应发信号：%d", n)
	}
	issued := []string{cn.Confirm}
	// 用错、格式不对：被拒，没有改动。
	for _, bad := range []string{strings.Repeat("0", 32), "not-a-code"} {
		if code, b := withConfirm(t, d, bad); code != http.StatusForbidden {
			t.Fatalf("用错的确认码应被拒：%d %s", code, b)
		}
	}
	if code, _ := call(t, d, "POST", "/v1/restore", []byte(`{"confirm":"x","force":true}`)); code != http.StatusBadRequest {
		t.Fatal("不认识的字段应回 400")
	}
	e.stillEnabled(t, d, "确认码不对")
	// 正确的确认码：执行。
	code, b := withConfirm(t, d, cn.Confirm)
	if code != 200 {
		t.Fatalf("带正确的确认码应执行：%d %s", code, b)
	}
	if s := status(t, d); s.State != "RESTORED" || !e.sys.Net.Snapshot().Equal(e.init) {
		t.Fatalf("恢复后应还原干净：%s", s.State)
	}
	for _, pid := range []int{7100, 7101, 7200} {
		if e.sys.Procs.Alive(pid) {
			t.Fatalf("%d 应已结束", pid)
		}
	}
	if !e.sys.Procs.Alive(7300) {
		t.Fatal("无关进程不应受影响")
	}
	var sigs []string
	for _, r := range e.sys.Procs.SignalLog() {
		sigs = append(sigs, r.String())
	}
	if want := []string{"TERM 7100", "TERM 7101", "TERM 7200", "KILL 7101"}; !slices.Equal(sigs, want) {
		t.Fatalf("信号：%v", sigs)
	}
	// 已经是 RESTORED：不需要确认，直接返回。
	if code, _ := call(t, d, "POST", "/v1/restore", nil); code != 200 {
		t.Fatalf("RESTORED 时不需要确认：%d", code)
	}
	// 再走一遍：启用 → 预览 → 结束失败（注入）→ 同一个确认码不能再用。
	post(t, d, "/v1/enable", 200)
	cn2 := askConfirm(t, d)
	issued = append(issued, cn2.Confirm)
	e.sys.Kill.SetFail(ErrFakeKill)
	if code, _ := withConfirm(t, d, cn2.Confirm); code != http.StatusInternalServerError {
		t.Fatalf("结束失败应中止：%d", code)
	}
	e.sys.Kill.SetFail(nil)
	if code, _ := withConfirm(t, d, cn2.Confirm); code != http.StatusForbidden {
		t.Fatalf("确认码只能用一次：%d", code)
	}
	e.stillEnabled(t, d, "重复使用")
	// 日志、事件、status 里没有确认码，也没有命令行参数和路径。
	_, sb := call(t, d, "GET", "/v1/status", nil)
	var evs strings.Builder
	for _, r := range d.events.List() {
		evs.WriteString(r.Kind + " " + r.Detail + "\n")
	}
	all := buf.String() + evs.String() + string(sb)
	for _, x := range append(issued, secrets...) {
		if strings.Contains(all, x) {
			t.Fatalf("日志、事件或 status 里出现了 %q", x)
		}
	}
	if !strings.Contains(buf.String(), "要结束的 Claude 进程 3 个") {
		t.Fatalf("日志应只记数量：%s", buf.String())
	}
}

// e. 过期被拒；进程重启后旧确认码失效。
func TestRestoreConfirmExpiryAndRestart(t *testing.T) {
	clk := &stepClock{now: time.Now()}
	e, _, o := confirmEnv(t, func(o *Options) { o.Clock = clk })
	d := e.start(o)
	post(t, d, "/v1/enable", 200)
	cn := askConfirm(t, d)
	clk.Advance(ConfirmTTL + time.Second)
	if code, _ := withConfirm(t, d, cn.Confirm); code != http.StatusForbidden {
		t.Fatalf("过期的确认码应被拒：%d", code)
	}
	e.stillEnabled(t, d, "过期")
	// 59 秒内有效。
	cn = askConfirm(t, d)
	clk.Advance(ConfirmTTL - time.Second)
	cnOld := askConfirm(t, d) // 重启前再发一个
	if code, _ := withConfirm(t, d, cn.Confirm); code != 200 {
		t.Fatalf("有效期内应执行：%d", code)
	}
	post(t, d, "/v1/enable", 200)
	d.abandon()
	e.d = nil
	e.cur.Store(nil)
	d = e.start(o)
	if code, _ := withConfirm(t, d, cnOld.Confirm); code != http.StatusForbidden {
		t.Fatalf("重启后旧确认码应失效：%d", code)
	}
	e.stillEnabled(t, d, "重启后的旧确认码")
	post(t, d, "/v1/restore", 200) // 重新两步：可以
}

// e. 确认码绑定操作类型和参数摘要；只要码存在，不论是否通过都作废。
func TestConfirmsUnit(t *testing.T) {
	now := time.Unix(1_900_000_000, 0)
	var mu sync.Mutex
	c := newConfirms(func() time.Time { mu.Lock(); defer mu.Unlock(); return now })
	dg := digestOf(OpRestore)
	a := c.issue(OpRestore, dg)
	if _, err := c.take(a, "enable", dg); !errors.Is(err, ErrConfirm) {
		t.Fatal("用于别的操作应被拒")
	}
	if _, err := c.take(a, OpRestore, dg); !errors.Is(err, ErrConfirm) {
		t.Fatal("被拒过一次的码也作废")
	}
	b := c.issue(OpRestore, dg)
	if _, err := c.take(b, OpRestore, digestOf(OpRestore, "force")); !errors.Is(err, ErrConfirm) {
		t.Fatal("参数摘要不符应被拒")
	}
	x := c.issue(OpRestore, dg)
	y := c.issue(OpRestore, dg)
	if x == y || len(x) != 32 {
		t.Fatal("确认码应随机、128 位")
	}
	mu.Lock()
	now = now.Add(ConfirmTTL)
	mu.Unlock()
	if _, err := c.take(x, OpRestore, dg); !errors.Is(err, ErrConfirm) {
		t.Fatal("刚好 60 秒应过期")
	}
	z := c.issue(OpRestore, dg)
	if _, err := c.take(z, OpRestore, dg); err != nil {
		t.Fatal(err)
	}
	if _, err := c.take(z, OpRestore, dg); !errors.Is(err, ErrConfirm) {
		t.Fatal("一次性")
	}
	if _, e := c.take("", OpRestore, dg); e == nil {
		t.Fatal("空码")
	}
}

// f. 结束不了：恢复中止，系统没有改动，状态不变，错误里有残留的数量。
func TestRestoreLeftovers(t *testing.T) {
	e, _, o := confirmEnv(t)
	d := e.start(o)
	post(t, d, "/v1/enable", 200)
	addClaude(e, claudeproc.Unkillable)
	cn := askConfirm(t, d)
	code, b := withConfirm(t, d, cn.Confirm)
	if code != http.StatusInternalServerError || !strings.Contains(string(b), "还有 1 个 Claude 进程结束不了") {
		t.Fatalf("应中止并报残留数量：%d %s", code, b)
	}
	e.stillEnabled(t, d, "结束不了")
	if !e.sys.Procs.Alive(7200) || e.sys.Procs.Alive(7100) {
		t.Fatal("能结束的已结束，结束不了的还在")
	}
}

// f. 不结束任何进程的实现（--dev-skip-claude-kill）：预览照常、标明不会结束；恢复完成；没有任何信号。
func TestRestoreSkipImpl(t *testing.T) {
	e, buf, o := confirmEnv(t)
	sys := e.sys.System()
	svc := claudeproc.NewFakeService(e.sys.Procs, e.sys.Signer, os.Getpid(), true, func(f string, a ...any) { o.Log.Printf(f, a...) })
	sys.Killer, sys.Claude = svc, svc
	o.Sys = sys
	d := e.start(o)
	post(t, d, "/v1/enable", 200)
	addClaude(e, claudeproc.ExitOnTerm)
	cn := askConfirm(t, d)
	if cn.Preview.WillKill || cn.Preview.Notice != claudeproc.SkipNotice || len(cn.Preview.Processes) != 3 {
		t.Fatalf("预览应如实列出并标明不会结束：%+v", cn.Preview)
	}
	if code, b := withConfirm(t, d, cn.Confirm); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	if s := status(t, d); s.State != "RESTORED" || !e.sys.Net.Snapshot().Equal(e.init) {
		t.Fatal("恢复应完成")
	}
	if n := len(e.sys.Procs.SignalLog()); n != 0 || !e.sys.Procs.Alive(7100) {
		t.Fatalf("不应发任何信号：%d", n)
	}
	if !strings.Contains(buf.String(), "!!!!!!!!") {
		t.Fatal("应有醒目日志")
	}
}

// ---- j. M4-3 补的一条：第 ⑤ 步通过后拖过 30 秒，第 ⑧ 步补查不通过 → ENABLED、闸门关；之后复查通过 → 打开。----

// jumpClock 在第 ⑤ 步通过之后（下一次读时间之后）把时间往前推一段，模拟 ⑤ 到 ⑧ 之间耗时。
type jumpClock struct {
	*stepClock
	jump atomic.Int64
}

func (c *jumpClock) Now() time.Time {
	v := c.stepClock.Now()
	if j := c.jump.Swap(0); j > 0 {
		c.stepClock.Advance(time.Duration(j))
	}
	return v
}

// seqCheck：启用第 ⑤ 步（Check）通过并安排时钟跳 31 秒；复查（CheckResult）按顺序给结果。
type seqCheck struct {
	clk *jumpClock
	mu  sync.Mutex
	seq []gatesm.Result
	n   int
}

func (s *seqCheck) Check(context.Context) error {
	s.clk.jump.Store(int64(31 * time.Second))
	return nil
}

func (s *seqCheck) CheckResult(context.Context) (gatesm.Result, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.seq[min(s.n, len(s.seq)-1)]
	s.n++
	return r, fmt.Sprint("第 ", s.n, " 轮")
}

func (s *seqCheck) rounds() int { s.mu.Lock(); defer s.mu.Unlock(); return s.n }

func TestEnableLateRecheckFails(t *testing.T) {
	clk := &jumpClock{stepClock: &stepClock{now: time.Now()}}
	e := newEnv(t)
	chk := &seqCheck{clk: clk, seq: []gatesm.Result{gatesm.Fail, gatesm.Pass}}
	sys := e.sys.System()
	sys.Exit = chk
	e.system = &sys
	o := e.opts()
	o.Clock = clk
	d := e.start(o)
	post(t, d, "/v1/enable", 200)
	for i := 0; i < 300 && chk.rounds() < 1; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	var s Status
	for i := 0; i < 300; i++ {
		if s = status(t, d); len(s.Gate.Reasons) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if chk.rounds() != 1 || s.State != "ENABLED" || s.GateOpen || !slices.Equal(s.Gate.Reasons, []string{string(gatesm.ReasonExitCheck)}) {
		t.Fatalf("超过 30 秒应补查一轮；没通过：ENABLED、闸门关、原因 exit_check（不回滚）：轮数 %d %+v", chk.rounds(), s)
	}
	d.re.Kick() // 下一轮复查（通过）
	if s = waitGateOpen(t, d); !s.GateOpen || s.State != "ENABLED" || chk.rounds() != 2 {
		t.Fatalf("之后复查通过应打开：%+v", s)
	}
}

// 执行时遇到“已经有一个操作在进行”：确认码放回去（保留原过期时间），用户不必重新预览；过期的不放回。
func TestConfirmsGiveBack(t *testing.T) {
	now := time.Unix(1_900_000_000, 0)
	var mu sync.Mutex
	c := newConfirms(func() time.Time { mu.Lock(); defer mu.Unlock(); return now })
	dg := digestOf(OpRestore)
	code := c.issue(OpRestore, dg)
	p, err := c.take(code, OpRestore, dg)
	if err != nil {
		t.Fatal(err)
	}
	c.giveBack(code, p)
	if _, err := c.take(code, OpRestore, dg); err != nil {
		t.Fatal("放回后可以再用一次")
	}
	if _, err := c.take(code, OpRestore, dg); err == nil {
		t.Fatal("仍是一次性")
	}
	code = c.issue(OpRestore, dg)
	p, _ = c.take(code, OpRestore, dg)
	mu.Lock()
	now = now.Add(ConfirmTTL)
	mu.Unlock()
	c.giveBack(code, p)
	if _, err := c.take(code, OpRestore, dg); err == nil {
		t.Fatal("过期的不放回")
	}
}
