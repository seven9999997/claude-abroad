package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"claude-abroad/internal/claudecfg"
	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/exitpolicy"
	"claude-abroad/internal/gatesm"
	"claude-abroad/internal/pf"
	"claude-abroad/internal/preflight"
	"claude-abroad/internal/resolverlock"
	"claude-abroad/internal/state"
	"claude-abroad/internal/sysnet"
	"claude-abroad/internal/usernet"
	"claude-abroad/internal/vmmgr"
)

// ---- 借用虚拟机（docs/vm-borrow-plan.md v2）：跟虚拟机死活绑定的假实现 ----

// alive 报告最近一次拉起的假 abroad-vm 是否还在、且健康（数据通道、ctl.sock 能用）。
func (l *dmLauncher) alive() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.procs) == 0 {
		return false
	}
	select {
	case <-l.procs[len(l.procs)-1].done:
		return false
	default:
	}
	return l.healthy.Load()
}

// vmBoundProber：虚拟机不在时五项都是 channel_down（真机上 data.sock 拨不通时就是这样），在时按预设结果。
type vmBoundProber struct {
	alive func() bool
	mu    sync.Mutex
	rep   exitcheck.Report
	n     int
	hook  func() // 检查开始时调用（可为 nil）
}

func (p *vmBoundProber) Check(context.Context, string, exitcheck.Options) exitcheck.Report {
	p.mu.Lock()
	p.n++
	rep, hook := p.rep, p.hook
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	if !p.alive() {
		return channelDownReport()
	}
	return rep
}

func channelDownReport() exitcheck.Report {
	var items []exitcheck.Item
	for _, n := range []string{"https", "ip", "mainland", "rule", "doh"} {
		items = append(items, exitcheck.Item{Name: n, Result: exitcheck.Unknown, Cause: exitcheck.CauseChannelDown})
	}
	return exitcheck.Report{Exit: "main", Verdict: exitcheck.Unknown, Items: items}
}

// vmBoundMainVM：虚拟机不在时一切下发都报错；failUp 非 nil 时虚拟机在也让主代理配置的下发失败。
type vmBoundMainVM struct {
	*fakeMainVM
	alive  func() bool
	mu     sync.Mutex
	failUp error
	pushes int // 成功下发主代理配置的次数
}

var errCtlSock = errors.New("ctl.sock 不存在")

func (v *vmBoundMainVM) BootID(ctx context.Context) (string, error) {
	if !v.alive() {
		return "", errCtlSock
	}
	return v.fakeMainVM.BootID(ctx)
}
func (v *vmBoundMainVM) PushMainHops(ctx context.Context, m usernet.MainHops) error {
	if !v.alive() {
		return errCtlSock
	}
	return v.fakeMainVM.PushMainHops(ctx, m)
}
func (v *vmBoundMainVM) PushMainProxy(ctx context.Context, y []byte) error {
	if !v.alive() {
		return errCtlSock
	}
	v.mu.Lock()
	f := v.failUp
	v.mu.Unlock()
	if f != nil {
		return f
	}
	if err := v.fakeMainVM.PushMainProxy(ctx, y); err != nil {
		return err
	}
	v.mu.Lock()
	v.pushes++
	v.mu.Unlock()
	return nil
}
func (v *vmBoundMainVM) MainProxyDigest(ctx context.Context) (string, error) {
	if !v.alive() {
		return "", errCtlSock
	}
	return v.fakeMainVM.MainProxyDigest(ctx)
}
func (v *vmBoundMainVM) PushEndpoints(ctx context.Context, e []ExitEndpoint) error {
	if !v.alive() {
		return errCtlSock
	}
	return v.fakeMainVM.PushEndpoints(ctx, e)
}
func (v *vmBoundMainVM) PushExits(ctx context.Context, xs []StoredExit) ([]AgentExit, error) {
	if !v.alive() {
		return nil, errCtlSock
	}
	return v.fakeMainVM.PushExits(ctx, xs)
}
func (v *vmBoundMainVM) pushCount() int { v.mu.Lock(); defer v.mu.Unlock(); return v.pushes }

// flakyLauncher 包住 dmLauncher：fail 为真时 Launch 直接报错（拉不起来）。
type flakyLauncher struct {
	*dmLauncher
	fail atomic.Bool
}

func (f *flakyLauncher) Launch() (vmmgr.Proc, error) {
	if f.fail.Load() {
		f.launches.Add(1)
		return nil, errors.New("exec: 找不到 abroad-vm")
	}
	return f.dmLauncher.Launch()
}

// borrowEnv 是由管理器管理虚拟机（假拉起器）、主代理一侧跟虚拟机死活绑定的一台机器。
type borrowEnv struct {
	*env
	d   *Daemon
	l   *dmLauncher
	fl  *flakyLauncher
	mgr *vmmgr.Manager
	pr  *vmBoundProber
	vm  *vmBoundMainVM
	// 在 sysnet 的某个改动之后卡住（让启用停在 ENABLING、回国停在 RESTORING）
	blockOp atomic.Value // string
	unblock chan struct{}
}

func newBorrowEnv(t *testing.T, mut func(*borrowEnv, *Options)) *borrowEnv {
	t.Helper()
	b := &borrowEnv{env: newEnv(t), l: &dmLauncher{}, unblock: make(chan struct{})}
	b.blockOp.Store("")
	b.l.healthy.Store(true)
	b.fl = &flakyLauncher{dmLauncher: b.l}
	b.mgr = &vmmgr.Manager{Launcher: b.fl, Health: b.l.health, Timing: dmTiming}
	b.pr = &vmBoundProber{alive: b.l.alive, rep: passReport()}
	b.vm = &vmBoundMainVM{fakeMainVM: &fakeMainVM{boot: "boot-1"}, alive: b.l.alive}
	orig := b.env.sys.Net.OnChange
	b.env.sys.Net.OnChange = func(op string) {
		orig(op)
		if op == b.blockOp.Load().(string) {
			<-b.unblock
		}
	}
	o := b.env.opts()
	o.Sys.VM = b.mgr
	o.Sys.Baseline = NewBaselineService(b.pr, b.env.dir)
	o.Sys.MainVM, o.Sys.ExitVM = b.vm, b.vm
	if mut != nil {
		mut(b, &o)
	}
	b.d = b.env.start(o)
	return b
}

// selectSaved 添加一个本地来源、选 exit 节点（未启用时只保存，成为“待下发”的选择）。
func (b *borrowEnv) selectSaved(t *testing.T) {
	t.Helper()
	m := &mainEnv{env: b.env, d: b.d}
	m.addYAML(t, "本地", mainNodesYAML)
	if code, rb := m.sel(t, MainSelectRequest{Selected: m.nodeID(t, "exit")}); code != 200 || !strings.Contains(string(rb), `"saved":true`) {
		t.Fatalf("未启用时选择应只保存：%d %s", code, rb)
	}
}

func saveTestBaseline(t *testing.T, path string) {
	t.Helper()
	base, err := exitcheck.BaselineFromReport(passReport(), exitpolicy.RuleSameCountry)
	if err != nil {
		t.Fatal(err)
	}
	if err := exitcheck.SaveBaseline(path, base); err != nil {
		t.Fatal(err)
	}
}

func (b *borrowEnv) vmStopped() bool {
	return !b.l.alive() && b.mgr.Status().Phase == vmmgr.PhaseIdle && !b.mgr.Status().Borrowed
}

// rawCall 同 call，但不调用 t.Fatal（给后台协程用）。
func rawCall(d *Daemon, method, path string, body []byte) (int, []byte, error) {
	req, _ := http.NewRequest(method, "http://abroadd"+path, bytes.NewReader(body))
	resp, err := client(d.SocketPath()).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, rb, nil
}

// rawConfirm 同 confirmCall，不调用 t.Fatal。
func rawConfirm(d *Daemon, path string) (int, []byte, error) {
	code, b, err := rawCall(d, "POST", path, nil)
	if err != nil || code != http.StatusConflict {
		return code, b, err
	}
	var cn ConfirmNeeded
	if json.Unmarshal(b, &cn) != nil || cn.Confirm == "" {
		return code, b, nil
	}
	body, _ := json.Marshal(ConfirmRequest{Confirm: cn.Confirm})
	return rawCall(d, "POST", path, body)
}

type asyncResp struct {
	code int
	body []byte
	err  error
}

func goCall(d *Daemon, method, path string, body []byte) <-chan asyncResp {
	ch := make(chan asyncResp, 1)
	go func() {
		c, b, err := rawCall(d, method, path, body)
		ch <- asyncResp{c, b, err}
	}()
	return ch
}

// gateSampler 每 5 毫秒读一次状态：闸门一旦开了、或原因里没有 state，就记下来。
type gateSampler struct {
	stop, done chan struct{}
	mu         sync.Mutex
	bad        []string
	n          int
}

func sampleGate(d *Daemon) *gateSampler {
	g := &gateSampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(g.done)
		for {
			st := d.Status()
			g.mu.Lock()
			g.n++
			if st.GateOpen || !hasReason(st, gatesm.ReasonState) {
				g.bad = append(g.bad, fmt.Sprintf("open=%v reasons=%v", st.GateOpen, st.Gate.Reasons))
			}
			g.mu.Unlock()
			select {
			case <-g.stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	return g
}

func (g *gateSampler) finish(t *testing.T) {
	t.Helper()
	close(g.stop)
	<-g.done
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.bad) > 0 {
		t.Fatalf("采样到闸门开着或没有 state 原因（%d 次采样）：%v", g.n, g.bad[:min(len(g.bad), 5)])
	}
	if g.n < 1 {
		t.Fatalf("没有采样：%d", g.n)
	}
}

func borrowKinds(d *Daemon) []string {
	var out []string
	for _, r := range d.events.List() {
		if r.Source == "vm_borrow" {
			out = append(out, r.Kind)
		}
	}
	return out
}

func errBodyOf(b []byte) (code, text string) {
	var eb struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(b, &eb)
	return eb.Code, eb.Error
}

// internalWords 是不能出现在面向用户文字里的内部说法：全部原因码的原值，以及“通道不通”一类。
func internalWords() []string {
	out := []string{"通道不通", "数据通道不可用"}
	for _, c := range exitcheck.Causes() {
		if c != exitcheck.CauseOK {
			out = append(out, string(c))
		}
	}
	return out
}

func assertNoInternalWords(t *testing.T, where, text string) {
	t.Helper()
	for _, w := range internalWords() {
		if strings.Contains(text, w) {
			t.Errorf("%s：面向用户的文字里出现了内部说法 %q：%s", where, w, text)
		}
	}
}

func launchAllowed(t *testing.T, d *Daemon) bool {
	t.Helper()
	_, b := call(t, d, "GET", "/v1/launchguard", nil)
	var v LaunchGuardView
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%s", b)
	}
	return v.Allowed
}

// ① 由管理器管理虚拟机、状态是回国：应用主代理成功、基准建立、安装引导可以完成；拉起一次，结束后进程已退出、管理器空闲；
// 全程闸门关着，假 pf 没有调用，假的地区实现没有写入，状态文件不变，启动管控不受影响。
func TestBorrowApplyMainRestored(t *testing.T) {
	b := newBorrowEnv(t, nil)
	b.selectSaved(t)
	stateBefore, _ := os.ReadFile(filepath.Join(b.dir, "state.json"))
	netBefore := b.sys.Net.Snapshot()
	regionBefore := b.sys.VMRegion.Count()
	allowedBefore := launchAllowed(t, b.d)
	b.sys.PF.ResetCalls()
	g := sampleGate(b.d)
	code, body := call(t, b.d, "POST", "/v1/main-proxy/apply", nil)
	if code != 200 {
		t.Fatalf("回国状态应用主代理应成功：%d %s", code, body)
	}
	g.finish(t)
	var res MainProxyResult
	if json.Unmarshal(body, &res) != nil || !res.Borrowed || res.Baseline.Country != "US" || !strings.Contains(res.Note, "临时启动") {
		t.Fatalf("应答：%s", body)
	}
	if code, body := call(t, b.d, "POST", "/v1/setup/complete", nil); code != 200 {
		t.Fatalf("安装引导应能完成：%d %s", code, body)
	}
	if n := b.l.launches.Load(); n != 1 {
		t.Fatalf("应拉起一次，实际 %d 次", n)
	}
	if !b.vmStopped() {
		t.Fatalf("应答时虚拟机应已停掉：%+v", b.mgr.Status())
	}
	if c := b.sys.PF.Calls(); len(c) != 0 {
		t.Fatalf("借用期间不应碰 pf：%v", c)
	}
	if !b.sys.Net.Snapshot().Equal(netBefore) || b.sys.VMRegion.Count() != regionBefore {
		t.Fatal("借用期间不应改本机网络、地区")
	}
	if after, _ := os.ReadFile(filepath.Join(b.dir, "state.json")); !bytes.Equal(after, stateBefore) {
		t.Fatalf("状态文件不应变：\n%s\n%s", stateBefore, after)
	}
	if launchAllowed(t, b.d) != allowedBefore {
		t.Fatal("启动管控的判断不应变")
	}
	// 借来的虚拟机里的下发不写“已下发”记录，待下发的选择还在（启用时照常下发）
	if a, _ := b.d.main.applied(); a != nil {
		t.Fatal("借用时不应写“已下发”记录")
	}
	if _, err := os.Stat(b.d.main.file("pending.json")); err != nil {
		t.Fatal("待下发的选择应还在")
	}
	if got := borrowKinds(b.d); !slices.Equal(got, []string{BorrowStarting, BorrowPushing, BorrowChecking, BorrowStopping, BorrowDone}) {
		t.Fatalf("进度事件：%v", got)
	}
	if st := status(t, b.d); st.Borrow != nil || st.State != "RESTORED" {
		t.Fatalf("结束后状态：%+v %s", st.Borrow, st.State)
	}
	b.noViolations()

	// 必须改 1：打开回国拦截；借用卡在等健康时清掉锚点，借用结束前就被重新加载。
	t.Run("return_block_reload", func(t *testing.T) {
		want, _ := pf.Render(pf.ReturnBlockConfig())
		b := newBorrowEnv(t, func(b *borrowEnv, o *Options) {
			base := filepath.Dir(b.env.dir)
			_ = os.MkdirAll(b.env.dir, 0o700)
			o.Sys.ResolverLock = &resolverlock.Lock{Dir: filepath.Join(base, "resolver"), RecordDir: b.env.dir}
			o.Sys.ClaudeSettings = &claudecfg.Settings{Dir: filepath.Join(base, "ClaudeCode", "managed-settings.d"), RecordDir: b.env.dir}
			o.Sys.FirstRun = true
			o.Sys.Preflight = PreflightHints{C: &preflight.Checker{R: cleanRunner{t}}}
			o.SuperviseInterval = 20 * time.Millisecond
			slow := dmTiming
			slow.StartTimeout, slow.BorrowWait = 30*time.Second, 40*time.Second
			b.mgr.Timing = slow
		})
		if string(b.sys.PF.Raw(pf.DefaultAnchor)) != string(want) {
			t.Fatalf("回国拦截应已加载：%s", b.sys.PF.Raw(pf.DefaultAnchor))
		}
		b.selectSaved(t)
		b.l.healthy.Store(false)
		ch := goCall(b.d, "POST", "/v1/main-proxy/apply", nil)
		waitCond(t, "借用停在等健康", func() bool { return b.mgr.Status().Phase == vmmgr.PhaseStarting && b.d.borrowing.Load() })
		b.sys.PF.ClearAnchor()
		waitCond(t, "借用期间巡检重新加载回国拦截", func() bool { return string(b.sys.PF.Raw(pf.DefaultAnchor)) == string(want) })
		if !b.d.borrowing.Load() {
			t.Fatal("重新加载应发生在借用结束之前")
		}
		b.l.healthy.Store(true)
		r := <-ch
		if r.err != nil || r.code != 200 {
			t.Fatalf("借用应照常完成：%d %s %v", r.code, r.body, r.err)
		}
		if !b.vmStopped() {
			t.Fatalf("结束后应停掉：%+v", b.mgr.Status())
		}
	})
}

var reIPv4 = regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}\b`)
var reIPv6 = regexp.MustCompile(`(?i)\b[0-9a-f]{1,4}(:[0-9a-f]{0,4}){2,7}\b`)

// 裁定③：vm_borrow 事件详情、status.borrow 里不出现 IP、节点服务器和端口；成功的详情只有国家和时区。
func TestBorrowEventsNoAddress(t *testing.T) {
	b := newBorrowEnv(t, nil)
	b.selectSaved(t)
	post(t, b.d, "/v1/main-proxy/apply", 200)
	b.fl.fail.Store(true)
	_, _ = call(t, b.d, "POST", "/v1/main-proxy/apply", nil)
	b.fl.fail.Store(false)
	b.vm.mu.Lock()
	b.vm.failUp = errors.New("agent 回 422：节点 srvmark.example.com:54321 不合法")
	b.vm.mu.Unlock()
	_, _ = call(t, b.d, "POST", "/v1/main-proxy/apply", nil)
	b.vm.mu.Lock()
	b.vm.failUp = nil
	b.vm.mu.Unlock()
	b.pr.mu.Lock()
	r := passReport()
	r.Items[0] = exitcheck.Item{Name: "https", Result: exitcheck.Unknown, Cause: exitcheck.CauseTimeout}
	b.pr.rep = r
	b.pr.mu.Unlock()
	_, _ = call(t, b.d, "POST", "/v1/main-proxy/check", nil)
	n := 0
	for _, ev := range b.d.events.List() {
		if ev.Source != "vm_borrow" {
			continue
		}
		n++
		if reIPv4.MatchString(ev.Detail) || reIPv6.MatchString(ev.Detail) {
			t.Errorf("事件详情里有地址：%+v", ev)
		}
		for _, m := range mainMarkers {
			if strings.Contains(ev.Detail, m) {
				t.Errorf("事件详情里有节点信息 %q：%+v", m, ev)
			}
		}
		assertNoInternalWords(t, "事件", ev.Detail)
		if ev.Kind == BorrowDone && ev.Detail != "出口在 US（America/Los_Angeles）" {
			t.Errorf("成功的详情只写国家和时区：%q", ev.Detail)
		}
	}
	if n < 10 {
		t.Fatalf("事件太少：%d", n)
	}
}

// ② 只检查不保存的主代理检查：同样借用、结束时停掉；不写基准。
func TestBorrowMainCheckRestored(t *testing.T) {
	b := newBorrowEnv(t, nil)
	b.selectSaved(t)
	b.sys.PF.ResetCalls()
	g := sampleGate(b.d)
	code, body := call(t, b.d, "POST", "/v1/main-proxy/check", nil)
	g.finish(t)
	var sum BaselineSummary
	if code != 200 || json.Unmarshal(body, &sum) != nil || !sum.Borrowed || sum.Country != "US" {
		t.Fatalf("检查主代理：%d %s", code, body)
	}
	if b.l.launches.Load() != 1 || !b.vmStopped() {
		t.Fatalf("应拉起一次并停掉：%d %+v", b.l.launches.Load(), b.mgr.Status())
	}
	if _, err := os.Stat(b.d.opts.Sys.Baseline.Path); err == nil {
		t.Fatal("只检查不应保存基准")
	}
	if c := b.sys.PF.Calls(); len(c) != 0 || b.sys.VMRegion.Count() != 0 {
		t.Fatalf("不应碰 pf 和地区：%v", c)
	}
}

// 裁定④：未启用时不借用虚拟机检查分流出口。
func TestExitCheckNotEnabled(t *testing.T) {
	b := newBorrowEnv(t, func(_ *borrowEnv, o *Options) {
		o.Sys.CheckExit = func(context.Context, string) exitcheck.Report { return passReport() }
	})
	if err := b.d.exits.Put(StoredExit{Tag: "wg", Kind: "wireguard", Conf: tWG}); err != nil {
		t.Fatal(err)
	}
	check := func(where string) {
		code, body := call(t, b.d, "POST", "/v1/exits/wg/check", nil)
		c, text := errBodyOf(body)
		if code != 409 || c != "not_enabled" || text != ExitCheckNotEnabledText {
			t.Fatalf("%s：%d %s", where, code, body)
		}
	}
	check("回国")
	b.selectSaved(t)
	saveTestBaseline(t, b.d.opts.Sys.Baseline.Path)
	post(t, b.d, "/v1/enable", 200)
	post(t, b.d, "/v1/disable", 200)
	before := b.l.launches.Load()
	check("停用")
	if b.l.launches.Load() != before {
		t.Fatal("不应拉起")
	}
	b2 := newBorrowEnv(t, func(_ *borrowEnv, o *Options) {
		o.Sys.CheckExit = func(context.Context, string) exitcheck.Report { return passReport() }
	})
	_ = b2.d.exits.Put(StoredExit{Tag: "wg", Kind: "wireguard", Conf: tWG})
	if code, _ := call(t, b2.d, "POST", "/v1/exits/wg/check", nil); code != 409 || b2.l.launches.Load() != 0 {
		t.Fatalf("回国时不应拉起：%d %d", code, b2.l.launches.Load())
	}
}

// ③ 失败的四种情况：每条都停掉虚拟机，错误能区分是哪一种，闸门全程关。
func TestBorrowFailures(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(b *borrowEnv)
		code     int
		errCode  string
		contains string
	}{
		{"launch", func(b *borrowEnv) { b.fl.fail.Store(true) }, 503, "vm_unavailable", "拉起虚拟机失败"},
		{"not_ready", func(b *borrowEnv) { b.l.healthy.Store(false) }, 503, "vm_unavailable", "没有就绪"},
		{"push", func(b *borrowEnv) {
			b.vm.mu.Lock()
			b.vm.failUp = errors.New("虚拟机回 HTTP 422：配置不合法")
			b.vm.mu.Unlock()
		}, 502, "main_push_failed", "下发到虚拟机失败"},
		{"probe", func(b *borrowEnv) {
			r := passReport()
			r.Items[0] = exitcheck.Item{Name: "https", Result: exitcheck.Unknown, Cause: exitcheck.CauseConnect}
			r.Items[1] = exitcheck.Item{Name: "ip", Result: exitcheck.Unknown, Cause: exitcheck.CauseTimeout}
			b.pr.mu.Lock()
			b.pr.rep = r
			b.pr.mu.Unlock()
		}, 422, "main_unreachable", "主代理不通"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newBorrowEnv(t, nil)
			b.selectSaved(t)
			c.setup(b)
			g := sampleGate(b.d)
			code, body := call(t, b.d, "POST", "/v1/main-proxy/apply", nil)
			g.finish(t)
			ec, text := errBodyOf(body)
			if code != c.code || ec != c.errCode || !strings.Contains(text, c.contains) {
				t.Fatalf("应答：%d %s", code, body)
			}
			assertNoInternalWords(t, c.name, text)
			waitCond(t, "虚拟机停掉", b.vmStopped)
			if f := b.mgr.Status().Failures; f != 0 {
				t.Fatalf("借用失败不计入连续失败：%d", f)
			}
			if n := b.l.launches.Load(); n != 1 {
				t.Fatalf("不应重试：拉起 %d 次", n)
			}
			if _, err := os.Stat(b.d.opts.Sys.Baseline.Path); err == nil {
				t.Fatal("失败时不应保存基准")
			}
			if got := borrowKinds(b.d); got[len(got)-1] != BorrowFailed {
				t.Fatalf("最后一条进度应是失败：%v", got)
			}
		})
	}
}

// ③ 补：借来的虚拟机、没有保存的选择：明确原因，虚拟机已停。
func TestBorrowNoMainSelected(t *testing.T) {
	b := newBorrowEnv(t, nil)
	code, body := call(t, b.d, "POST", "/v1/main-proxy/apply", nil)
	if ec, _ := errBodyOf(body); code != 422 || ec != "no_main_selected" {
		t.Fatalf("%d %s", code, body)
	}
	if !b.vmStopped() || b.l.launches.Load() != 1 {
		t.Fatalf("虚拟机应已停：%+v", b.mgr.Status())
	}
}

// ③ 补（风险 9）：停用状态、管理器已停止重启：不借用、不重试，提示重新启用。
func TestBorrowDisabledGivenUpNoRetry(t *testing.T) {
	b := newBorrowEnv(t, nil)
	b.selectSaved(t)
	saveTestBaseline(t, b.d.opts.Sys.Baseline.Path)
	post(t, b.d, "/v1/enable", 200)
	post(t, b.d, "/v1/disable", 200)
	b.fl.fail.Store(true)
	b.l.last().exit()
	waitCond(t, "管理器停止重启", func() bool { return b.mgr.Status().Phase == vmmgr.PhaseFailed })
	n := b.l.launches.Load()
	for _, path := range []string{"/v1/main-proxy/apply", "/v1/main-proxy/check"} {
		code, body := call(t, b.d, "POST", path, nil)
		ec, text := errBodyOf(body)
		if code != 503 || ec != "vm_unavailable" || !strings.Contains(text, "已停止重启") || !strings.Contains(text, "重新启用") {
			t.Fatalf("%s：%d %s", path, code, body)
		}
	}
	if b.l.launches.Load() != n || b.mgr.Status().Phase != vmmgr.PhaseFailed {
		t.Fatalf("不应重试：%d → %d，%+v", n, b.l.launches.Load(), b.mgr.Status())
	}
}

// ④ 借用期间启用、回国、卸载、另一次借用都得到“忙”，确认码可以再用；借用结束后再启用：虚拟机常驻。
func TestBorrowBusyEnableRestoreUninstall(t *testing.T) {
	b := newBorrowEnv(t, func(b *borrowEnv, o *Options) {
		slow := dmTiming
		slow.StartTimeout, slow.BorrowWait = 30*time.Second, 40*time.Second
		b.mgr.Timing = slow
	})
	b.selectSaved(t)
	saveTestBaseline(t, b.d.opts.Sys.Baseline.Path) // 有基准，启用本身能做（被拒只能是因为忙）
	b.l.healthy.Store(false)
	ch := goCall(b.d, "POST", "/v1/main-proxy/apply", nil)
	waitCond(t, "借用停在等健康", func() bool { return b.d.borrowing.Load() })
	busy := func(what string, code int, body []byte) {
		t.Helper()
		if ec, _ := errBodyOf(body); code != 409 || ec != "busy" {
			t.Fatalf("%s 应得到忙：%d %s", what, code, body)
		}
	}
	// 启用：地区会变时先两步确认（确认框不碰虚拟机），带上确认码执行时得到忙
	confirmed := func(path string) (int, []byte) {
		t.Helper()
		code, body := call(t, b.d, "POST", path, nil)
		var cn ConfirmNeeded
		if code == 409 && json.Unmarshal(body, &cn) == nil && cn.Confirm != "" {
			cb, _ := json.Marshal(ConfirmRequest{Confirm: cn.Confirm})
			return call(t, b.d, "POST", path, cb)
		}
		return code, body
	}
	code, body := confirmed("/v1/enable")
	busy("同步启用", code, body)
	code, body = confirmed("/v1/enable?async=1")
	var jb Job
	if code != 202 || json.Unmarshal(body, &jb) != nil {
		t.Fatalf("异步启用：%d %s", code, body)
	}
	waitCond(t, "异步启用的任务结束", func() bool { j, _ := b.d.jobs.get(jb.ID); return j.State != JobRunning })
	if j, _ := b.d.jobs.get(jb.ID); j.State != JobFailed || !strings.Contains(j.Error, "忙") {
		t.Fatalf("异步启用的任务应因忙失败：%+v", j)
	}
	// 回国（已经是回国状态，平时是空操作）：借用期间回“忙”
	code, body = call(t, b.d, "POST", "/v1/restore", nil)
	busy("回国", code, body)
	// 卸载
	code, body = call(t, b.d, "POST", "/v1/actions/uninstall", nil)
	var ucn struct {
		Confirm string `json:"confirm"`
	}
	if code != 409 || json.Unmarshal(body, &ucn) != nil || ucn.Confirm == "" {
		t.Fatalf("卸载第一步：%d %s", code, body)
	}
	ub, _ := json.Marshal(map[string]string{"confirm": ucn.Confirm})
	code, body = call(t, b.d, "POST", "/v1/actions/uninstall", ub)
	busy("卸载", code, body)
	if b.d.uninstalled() || b.d.uninstalling.Load() {
		t.Fatal("被拒的卸载不应留下标记")
	}
	// 另一次借用、改规则
	code, body = call(t, b.d, "POST", "/v1/main-proxy/check", nil)
	busy("另一次借用", code, body)
	_, rules := call(t, b.d, "GET", "/v1/rules?format=json", nil)
	code, body = call(t, b.d, "PUT", "/v1/rules", rules)
	busy("改规则", code, body)
	// 放开：借用完成
	b.l.healthy.Store(true)
	if r := <-ch; r.err != nil || r.code != 200 {
		t.Fatalf("借用应完成：%d %s %v", r.code, r.body, r.err)
	}
	if !b.vmStopped() {
		t.Fatalf("借用结束后应停掉：%+v", b.mgr.Status())
	}
	if code, body := call(t, b.d, "POST", "/v1/restore", nil); code != 200 {
		t.Fatalf("借用结束后回国（空操作）应成功：%d %s", code, body)
	}
	// 卸载的确认码放回了：借用结束后还能用
	if code, body := call(t, b.d, "POST", "/v1/actions/uninstall", ub); code != 200 {
		t.Fatalf("放回的确认码应还能用：%d %s", code, body)
	}
}

// ④ 借用结束后再启用：虚拟机常驻，不会被“借用结束”那一步停掉。
func TestEnableAfterBorrowStaysResident(t *testing.T) {
	b := newBorrowEnv(t, nil)
	b.selectSaved(t)
	post(t, b.d, "/v1/main-proxy/apply", 200)
	if !b.vmStopped() {
		t.Fatal("借用结束应已停")
	}
	post(t, b.d, "/v1/enable", 200)
	time.Sleep(dmTiming.StopWait + dmTiming.KillWait + 300*time.Millisecond)
	if st := b.mgr.Status(); st.Phase != vmmgr.PhaseRunning || st.Borrowed || b.l.launches.Load() != 2 || !b.l.alive() {
		t.Fatalf("启用后应常驻：%+v，拉起 %d 次", st, b.l.launches.Load())
	}
}

// ④ 补：由管理器管理、回国、有待下发的选择：启用成功，主代理在虚拟机拉起并健康之后下发。
func TestEnableFromRestoredManagedPushesAfterStart(t *testing.T) {
	b := newBorrowEnv(t, nil)
	b.selectSaved(t)
	saveTestBaseline(t, b.d.opts.Sys.Baseline.Path)
	code, body := callOp(t, b.d, "/v1/enable")
	if code != 200 {
		t.Fatalf("回国状态启用应成功：%d %s", code, body)
	}
	if b.vm.pushCount() != 1 {
		t.Fatalf("应在虚拟机起来后下发一次主代理，实际 %d 次", b.vm.pushCount())
	}
	if a, _ := b.d.main.applied(); a == nil {
		t.Fatal("启用时的下发应写“已下发”记录")
	}
	if _, err := os.Stat(b.d.main.file("pending.json")); err == nil {
		t.Fatal("启用下发后待下发的选择应清掉")
	}
}

// 必须改 2：启用第 ④ 步虚拟机起来后下发失败：启用失败、回到 RESTORED，闸门全程关，没写“已下发”，虚拟机按回滚停掉。
func TestEnableManagedPushFailsAtStep4(t *testing.T) {
	b := newBorrowEnv(t, nil)
	b.selectSaved(t)
	saveTestBaseline(t, b.d.opts.Sys.Baseline.Path)
	b.vm.mu.Lock()
	b.vm.failUp = errors.New("虚拟机回 HTTP 422：配置不合法")
	b.vm.mu.Unlock()
	g := sampleGate(b.d)
	code, body := callOp(t, b.d, "/v1/enable")
	g.finish(t)
	ec, text := errBodyOf(body)
	if code == 200 || ec != "main_push_failed" || !strings.Contains(text, "下发到虚拟机失败") {
		t.Fatalf("应答：%d %s", code, body)
	}
	if f, _ := b.view.Load(); f.State != state.Restored {
		t.Fatalf("应回到 RESTORED：%s", f.State)
	}
	if a, _ := b.d.main.applied(); a != nil {
		t.Fatal("不应写“已下发”记录")
	}
	if _, err := os.Stat(b.d.main.file("pending.json")); err != nil {
		t.Fatal("待下发的选择应还在")
	}
	waitCond(t, "虚拟机按回滚停掉", b.vmStopped)
	if b.l.launches.Load() != 1 {
		t.Fatalf("拉起 %d 次", b.l.launches.Load())
	}
	b.noViolations()
}

// 必须改 2：重复调用 mainPushVM.Start（地区同步后的“等虚拟机恢复”）和启用前的下发：同一份配置按摘要跳过、不重复下发。
func TestMainPushVMStartSkipsSameDigest(t *testing.T) {
	b := newBorrowEnv(t, nil)
	b.selectSaved(t)
	saveTestBaseline(t, b.d.opts.Sys.Baseline.Path)
	post(t, b.d, "/v1/enable", 200)
	if b.vm.pushCount() != 1 {
		t.Fatalf("启用应下发一次：%d", b.vm.pushCount())
	}
	for i := 0; i < 3; i++ {
		if err := b.d.m.D.VM.Start(bg); err != nil {
			t.Fatal(err)
		}
		if err := b.d.ensureMainPushed(bg); err != nil {
			t.Fatal(err)
		}
	}
	tgt, ok, err := b.d.regions.Peek()
	if err != nil || !ok {
		t.Fatalf("目标地区：%v %v", ok, err)
	}
	if err := b.d.m.SyncRegion(bg, tgt); err != nil {
		t.Fatalf("地区同步：%v", err)
	}
	if b.vm.pushCount() != 1 {
		t.Fatalf("同一份配置不应重复下发：%d 次", b.vm.pushCount())
	}
}

// ⑤ 已启用时：不再借用、不重复拉起，结束后虚拟机仍在、闸门仍开。
func TestBorrowNotNeededWhenEnabled(t *testing.T) {
	b := newBorrowEnv(t, nil)
	b.selectSaved(t)
	saveTestBaseline(t, b.d.opts.Sys.Baseline.Path)
	post(t, b.d, "/v1/enable", 200)
	if !waitGateOpen(t, b.d).GateOpen {
		t.Fatal("启用后闸门应开")
	}
	n := b.l.launches.Load()
	code, body := call(t, b.d, "POST", "/v1/main-proxy/apply", nil)
	var res MainProxyResult
	if code != 200 || json.Unmarshal(body, &res) != nil || res.Borrowed {
		t.Fatalf("已启用时应用主代理：%d %s", code, body)
	}
	code, body = call(t, b.d, "POST", "/v1/main-proxy/check", nil)
	if code != 200 || strings.Contains(string(body), `"borrowed":true`) {
		t.Fatalf("已启用时检查：%d %s", code, body)
	}
	if b.l.launches.Load() != n || b.mgr.Status().Phase != vmmgr.PhaseRunning || !status(t, b.d).GateOpen {
		t.Fatalf("不应重复拉起或停掉：%d → %d，%+v", n, b.l.launches.Load(), b.mgr.Status())
	}
}

func routePath(p string) string {
	p = strings.ReplaceAll(p, "{tag}", "wg")
	p = strings.ReplaceAll(p, "{id}", "x")
	return p
}

// ⑥ 未启用时：除了可借用的两个接口和启用，按路由表逐条调用一次，都不拉起虚拟机。
func TestRoutesNeverLaunchWhenNotEnabled(t *testing.T) {
	b := newBorrowEnv(t, func(_ *borrowEnv, o *Options) {
		o.Sys.CheckExit = func(context.Context, string) exitcheck.Report { return passReport() }
	})
	_ = b.d.exits.Put(StoredExit{Tag: "wg", Kind: "wireguard", Conf: tWG})
	b.selectSaved(t)
	n := 0
	for _, rt := range RouteTable() {
		if BorrowRoutes[rt.ID] || ResidentRoutes[rt.ID] {
			continue
		}
		path := routePath(rt.Path)
		if rt.ID == "events" { // 事件推送是长连接：读一下就断开
			ctx, cancel := context.WithTimeout(bg, 200*time.Millisecond)
			req, _ := http.NewRequestWithContext(ctx, "GET", "http://abroadd"+path, nil)
			if resp, err := client(b.d.SocketPath()).Do(req); err == nil {
				resp.Body.Close()
			}
			cancel()
			n++
			continue
		}
		var body []byte
		if rt.Method != "GET" && rt.Method != "DELETE" {
			body = []byte("{}")
		}
		_, _ = call(t, b.d, rt.Method, path, body)
		n++
		if l := b.l.launches.Load(); l != 0 || b.mgr.Status().Phase != vmmgr.PhaseIdle {
			t.Fatalf("%s %s 拉起了虚拟机（%d 次，%+v）", rt.Method, path, l, b.mgr.Status())
		}
	}
	if n < 40 {
		t.Fatalf("只调用了 %d 个接口", n)
	}
}

// d. 路由分类：可借用的只有两个，常驻的只有启用；分类里的都在路由表里（其余由上一条测试逐条核对不拉起）。
func TestBorrowRouteClassification(t *testing.T) {
	ids := map[string]bool{}
	for _, rt := range RouteTable() {
		ids[rt.ID] = true
	}
	want := map[string]bool{"main-proxy-apply": true, "main-check": true}
	if len(BorrowRoutes) != len(want) {
		t.Fatalf("可借用的接口：%v", BorrowRoutes)
	}
	for id := range BorrowRoutes {
		if !want[id] || !ids[id] {
			t.Fatalf("可借用的接口不对：%s", id)
		}
	}
	if len(ResidentRoutes) != 1 || !ResidentRoutes["enable"] {
		t.Fatalf("常驻的接口：%v", ResidentRoutes)
	}
	if BorrowRoutes["exit-check"] {
		t.Fatal("分流出口未启用时不借用（architect 改判）")
	}
}

// ⑧ abroadd 借用期间崩溃：借用状态只在内存，状态目录没有变化；用同一个状态目录重启后是回国、不拉起。
func TestBorrowCrashLeavesNothing(t *testing.T) {
	b := newBorrowEnv(t, func(b *borrowEnv, o *Options) {
		slow := dmTiming
		slow.StartTimeout, slow.BorrowWait = 30*time.Second, 40*time.Second
		b.mgr.Timing = slow
	})
	b.selectSaved(t)
	snap := func() map[string]string {
		out := map[string]string{}
		_ = filepath.WalkDir(b.dir, func(p string, de os.DirEntry, err error) error {
			if err != nil || de.IsDir() || strings.HasSuffix(p, ".sock") || strings.Contains(p, "lock") {
				return nil
			}
			c, _ := os.ReadFile(p)
			out[p] = string(c)
			return nil
		})
		return out
	}
	before := snap()
	b.l.healthy.Store(false)
	ch := goCall(b.d, "POST", "/v1/main-proxy/apply", nil)
	waitCond(t, "借用停在等健康", func() bool { return b.d.borrowing.Load() && b.l.launches.Load() == 1 })
	during := snap()
	for p, c := range before {
		if during[p] != c {
			t.Errorf("借用期间文件变了：%s", p)
		}
	}
	for p := range during {
		if _, ok := before[p]; !ok {
			t.Errorf("借用期间多了文件：%s", p)
		}
	}
	p := b.l.last()
	b.d.abandon() // 模拟进程被杀：管理循环结束、关掉自己拉起的进程（真实环境里由 --parent-watch 让 abroad-vm 自己停）
	<-ch
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("abroadd 没了之后虚拟机应停")
	}
	l2 := &dmLauncher{}
	l2.healthy.Store(true)
	o := b.env.opts()
	o.Sys.VM = &vmmgr.Manager{Launcher: l2, Health: l2.health, Timing: dmTiming}
	o.Sys.Baseline = NewBaselineService(b.pr, b.env.dir)
	o.Sys.MainVM, o.Sys.ExitVM = b.vm, b.vm
	d2 := b.env.start(o)
	time.Sleep(100 * time.Millisecond)
	if st := status(t, d2); st.State != "RESTORED" || st.VMManager.Phase != vmmgr.PhaseIdle || l2.launches.Load() != 0 {
		t.Fatalf("重启后：%s %+v，拉起 %d 次", st.State, st.VMManager, l2.launches.Load())
	}
}

// c. 连续借用（成功和四种失败交替）后没有残留：进程都退出、管理器空闲、标记清掉、协程回到基线。
func TestBorrowNoLeftovers(t *testing.T) {
	b := newBorrowEnv(t, nil)
	b.selectSaved(t)
	post(t, b.d, "/v1/main-proxy/apply", 200) // 先跑一次，让一次性的后台协程都起来
	time.Sleep(100 * time.Millisecond)
	base := runtime.NumGoroutine()
	timeoutRep := passReport()
	timeoutRep.Items[0] = exitcheck.Item{Name: "https", Result: exitcheck.Unknown, Cause: exitcheck.CauseTimeout}
	for i := 0; i < 20; i++ {
		switch i % 5 {
		case 1:
			b.fl.fail.Store(true)
		case 2:
			b.vm.mu.Lock()
			b.vm.failUp = errors.New("下发失败")
			b.vm.mu.Unlock()
		case 3:
			b.pr.mu.Lock()
			b.pr.rep = timeoutRep
			b.pr.mu.Unlock()
		case 4:
			b.l.healthy.Store(false)
		}
		_, _ = call(t, b.d, "POST", "/v1/main-proxy/apply", nil)
		b.fl.fail.Store(false)
		b.l.healthy.Store(true)
		b.vm.mu.Lock()
		b.vm.failUp = nil
		b.vm.mu.Unlock()
		b.pr.mu.Lock()
		b.pr.rep = passReport()
		b.pr.mu.Unlock()
		waitCond(t, "这一轮的虚拟机停掉", b.vmStopped)
	}
	b.l.mu.Lock()
	for i, p := range b.l.procs {
		select {
		case <-p.done:
		default:
			t.Errorf("第 %d 个进程没退出", i)
		}
	}
	b.l.mu.Unlock()
	if b.d.borrowing.Load() || b.d.borrow.Load() != nil || b.mgr.Status().Borrowed {
		t.Fatal("借用的标记应清掉")
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base+2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if g := runtime.NumGoroutine(); g > base+2 {
		t.Fatalf("协程残留：%d（基线 %d）", g, base)
	}
}

// 面向用户的文字里没有原因码（failedItems 是检查结果进入错误、闸门原因、事件的唯一入口）。
func TestFailedItemsNoRawCause(t *testing.T) {
	for _, c := range exitcheck.Causes() {
		var items []exitcheck.Item
		for _, n := range exitcheck.ItemNames() {
			items = append(items, exitcheck.Item{Name: n, Result: exitcheck.ResultOf(c), Cause: c})
		}
		items = append(items, exitcheck.Item{Name: "https", Result: exitcheck.Unknown, Cause: c})
		got := failedItems(exitcheck.Report{Items: items})
		assertNoInternalWords(t, string(c), got)
		for _, n := range []string{"https", "mainland", "doh"} {
			if strings.Contains(got, n) {
				t.Errorf("原因 %s：文字里有英文项名 %s：%q", c, n, got)
			}
		}
	}
}

// ---- 矩阵（⑦）：三种虚拟机来源 × 五种状态 × 要经虚拟机的接口 ----

// extVM 模拟外部启动的虚拟机（走 borrowVM 里“实现 Probe”的分支，和 ExternalVM 一样）：up 为假时没在运行。
type extVM struct{ up atomic.Bool }

func (v *extVM) Start(context.Context) error {
	if !v.up.Load() {
		return fmt.Errorf("%w：ctl.sock 不存在", ErrVMNotRunning)
	}
	return nil
}
func (v *extVM) Stop(context.Context) error { return nil }
func (v *extVM) Probe(context.Context) error {
	if !v.up.Load() {
		return fmt.Errorf("%w：ctl.sock 不存在", ErrVMNotRunning)
	}
	return nil
}

var vmCodes = map[string]bool{"vm_unavailable": true, "main_push_failed": true, "main_unreachable": true, "no_main_selected": true,
	"state_incomplete": true, "not_enabled": true, "via_host_required": true, "unsupported": true, "busy": true}

func TestVMSourceStateMatrix(t *testing.T) {
	sources := []string{"managed", "external_up", "external_down", "fake"}
	states := []string{"RESTORED", "DISABLED", "ENABLED", "ENABLING", "RESTORING", "LEFTOVER"}
	type ep struct {
		name, method, path string
		body               []byte
		vm                 bool // 要经虚拟机的三个接口：代码必须在明确的集合里
	}
	sub, _ := json.Marshal(AddSourceRequest{Kind: "subscription", Name: "订阅", URL: "https://example.invalid/sub"})
	eps := []ep{
		{"apply", "POST", "/v1/main-proxy/apply", nil, true},
		{"check", "POST", "/v1/main-proxy/check", nil, true},
		{"exit-check", "POST", "/v1/exits/wg/check", nil, true},
		{"select", "PUT", "/v1/main-proxy", nil, false},
		{"source-add", "POST", "/v1/main-proxy/sources", sub, false},
		{"envcheck-run", "POST", "/v1/envcheck/run", nil, false},
	}
	for _, src := range sources {
		for _, st := range states {
			t.Run(src+"/"+st, func(t *testing.T) {
				ext := &extVM{}
				ext.up.Store(true)
				b := newBorrowEnv(t, func(b *borrowEnv, o *Options) {
					o.Sys.CheckExit = func(context.Context, string) exitcheck.Report { return passReport() }
					o.Sys.FetchSource = func(context.Context, string, bool) ([]byte, error) { return nil, errors.New("连不上") }
					switch src {
					case "external_up", "external_down":
						o.Sys.VM = ext
						b.pr.alive, b.vm.alive = ext.up.Load, ext.up.Load
					case "fake":
						o.Sys.VM = b.env.sys.VMs
						b.pr.alive, b.vm.alive = func() bool { return true }, func() bool { return true }
					}
				})
				_ = b.d.exits.Put(StoredExit{Tag: "wg", Kind: "wireguard", Conf: tWG})
				b.selectSaved(t)
				saveTestBaseline(t, b.d.opts.Sys.Baseline.Path)
				var pending <-chan asyncResp
				switch st {
				case "DISABLED":
					post(t, b.d, "/v1/enable", 200)
					post(t, b.d, "/v1/disable", 200)
				case "ENABLED":
					post(t, b.d, "/v1/enable", 200)
				case "ENABLING":
					b.blockOp.Store(sysnet.OpRoutesAdd)
					ch := make(chan asyncResp, 1)
					go func() { c, bd, err := rawConfirm(b.d, "/v1/enable"); ch <- asyncResp{c, bd, err} }()
					pending = ch
					waitCond(t, "停在 ENABLING", func() bool { f, _ := b.view.Load(); return f.State == state.Enabling })
				case "RESTORING":
					post(t, b.d, "/v1/enable", 200)
					b.blockOp.Store(sysnet.OpRoutesDelete)
					ch := make(chan asyncResp, 1)
					go func() { c, bd, err := rawConfirm(b.d, "/v1/restore"); ch <- asyncResp{c, bd, err} }()
					pending = ch
					waitCond(t, "停在 RESTORING", func() bool { f, _ := b.view.Load(); return f.State == state.Restoring })
				case "LEFTOVER": // 崩溃后 Recover 没走完留下的半截状态（没有操作在进行）
					if err := b.view.Update(func(f *state.File) error { f.State, f.Origin = state.Enabling, state.Restored; return nil }); err != nil {
						t.Fatal(err)
					}
				}
				if src == "external_down" {
					ext.up.Store(false)
				}
				phase0 := b.mgr.Status().Phase
				launches0 := b.l.launches.Load()
				selBody, _ := json.Marshal(MainSelectRequest{Selected: (&mainEnv{env: b.env, d: b.d}).nodeID(t, "exit")})
				got := map[string]int{}
				for _, e := range eps {
					body := e.body
					if e.name == "select" {
						body = selBody
					}
					code, rb := call(t, b.d, e.method, e.path, body)
					got[e.name] = code
					ec, text := errBodyOf(rb)
					where := src + "/" + st + "/" + e.name
					if code >= 300 {
						assertNoInternalWords(t, where, text)
						if e.vm && !vmCodes[ec] {
							t.Errorf("%s：代码 %q 不在明确的集合里：%d %s", where, ec, code, rb)
						}
					}
					// 写死的期望值
					want := 0
					switch {
					case e.name == "exit-check" && st != "ENABLED":
						want = 409
						if ec != "not_enabled" {
							t.Errorf("%s：应是 not_enabled：%s", where, rb)
						}
					case e.name == "exit-check" && st == "ENABLED":
						want = 200
					case e.vm && (st == "ENABLING" || st == "RESTORING"):
						want = 409
						if ec != "busy" {
							t.Errorf("%s：应是 busy：%s", where, rb)
						}
					case e.vm && st == "LEFTOVER":
						want = 409
						if ec != "state_incomplete" {
							t.Errorf("%s：应是 state_incomplete：%s", where, rb)
						}
					case e.vm && src == "external_down":
						want = 503
						if ec != "vm_unavailable" {
							t.Errorf("%s：应是 vm_unavailable：%s", where, rb)
						}
					case e.vm:
						want = 200
					}
					if want != 0 && code != want {
						t.Errorf("%s：%d（期望 %d）%s", where, code, want, rb)
					}
				}
				if pending != nil {
					close(b.unblock)
					if r := <-pending; r.err != nil {
						t.Fatal(r.err)
					}
				}
				if src == "managed" {
					switch st {
					case "RESTORED":
						waitCond(t, "回到 idle", b.vmStopped)
					case "DISABLED", "ENABLED":
						if b.l.launches.Load() != launches0 || b.mgr.Status().Phase != phase0 {
							t.Errorf("常驻时不应重新拉起：%d → %d，%s → %s", launches0, b.l.launches.Load(), phase0, b.mgr.Status().Phase)
						}
					}
				}
			})
		}
	}
	// 具体的 ExternalVM 类型（没在运行）×回国：明确原因。
	t.Run("ExternalVM/RESTORED", func(t *testing.T) {
		b := newBorrowEnv(t, func(b *borrowEnv, o *Options) {
			o.Sys.VM = ExternalVM{Dir: t.TempDir()}
			b.pr.alive, b.vm.alive = func() bool { return false }, func() bool { return false }
		})
		b.selectSaved(t)
		for _, path := range []string{"/v1/main-proxy/apply", "/v1/main-proxy/check"} {
			code, rb := call(t, b.d, "POST", path, nil)
			ec, text := errBodyOf(rb)
			if code != 503 || ec != "vm_unavailable" || !strings.Contains(text, "外部启动") {
				t.Fatalf("%s：%d %s", path, code, rb)
			}
			assertNoInternalWords(t, path, text)
		}
	})
}
