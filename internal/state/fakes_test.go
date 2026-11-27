package state

import (
	"bytes"
	"claude-abroad/internal/gatesm"
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"claude-abroad/internal/pf"
	"claude-abroad/internal/region"
	"claude-abroad/internal/sysnet"
)

// bed 是状态机的测试台（7b 决定 10）：pf 用真实 Controller + FakePF，网络用 sysnet.Fake，其余是假实现。
// 所有改动都记进一条时间线，并在每次成功改动之后检查不变量（I1、I2、泄漏断言）。
type bed struct {
	t       *testing.T
	sim     *sysnet.SimRunner // 只在 simBackend 时非 nil
	dir     string
	release func() // 释放当前“进程”的跨进程锁
	ppf     *PatrolPF
	store   *Store
	view    *Store // 另一个实例，只读，给不变量检查用（不和状态机抢锁）
	net     *sysnet.Fake
	init    sysnet.FakeState
	fpf     *pf.FakePF
	pfw     *pfWrap
	tun     *fakeTun
	vm      *fakeVM
	exit    *fakeErr
	gate    *fakeGate
	re      *fakeRecheck
	ds      *fakeDataState // 数据面的状态接收方（每个“进程”一个，初始是停用）
	td      *fakeTeardown
	kill    *fakeErr
	pre     *fakeErr
	vmr     *fakeVMRegion
	tgts    *fakeTargets
	env     *fakeEnv
	m       *Machine
	rb      bool // 回国后保留回国拦截（M6-11a 的开关）
	noLAN   bool // “允许局域网”关（补齐三）

	mu         sync.Mutex
	timeline   []string
	events     []Event
	violations []string
	writes     int            // 成功的状态文件写入次数
	failWrite  int            // 第几次写入失败（0 表示不注入）
	armed      map[string]int // sysnet 操作 → 第几次失败
	succ       map[string]int // sysnet 操作 → 成功次数
	noCheck    bool
	nev        int // 改动事件计数（状态文件写入、假系统、pf、utun、闸门、虚拟机）
	crashAt    int // 第几个事件之后崩溃（0 表示不崩溃）
	evNames    []string
	tunSeq     int
}

// simBackend：STATE_BACKEND=sim 时，路由、DNS、IPv6 用“真实实现（MacRoutes、MacDNS、MacIPv6）+ 命令模拟器”，
// 底下仍是同一个 sysnet.Fake（M2-9b-1 b）。TestSimBackendSuite 用这个环境变量把整个包再跑一遍。
var simBackend = os.Getenv("STATE_BACKEND") == "sim"

// crashSignal 是测试里模拟崩溃的 panic 值。
type crashSignal struct{}

func initialNet() sysnet.FakeState {
	return sysnet.FakeState{
		IPv6: map[string]sysnet.IPv6Setting{
			"Ethernet": {Mode: sysnet.IPv6Manual, Address: netip.MustParseAddr("fd00::5"), PrefixLen: 64, Router: netip.MustParseAddr("fd00::1")},
			"Thunder":  {Mode: sysnet.IPv6LinkLocal},
			"USB":      {Mode: sysnet.IPv6Off},
			"Wi-Fi":    {Mode: sysnet.IPv6Automatic},
			"Odd VPN":  {Mode: sysnet.IPv6Unknown}, // 还原不了：启用时不关、回国时不还原（M2-8 第 0 步）
		},
		Default: "en0",
		Net:     sysnet.NetInfo{Direct: []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")}, VMIfs: []string{"bridge100"}},
		SysDNS:  []netip.Addr{netip.MustParseAddr("192.168.1.1")},
		// 地区（M4-5）：系统级的语言原来没有这个键（还原时要删除）。
		Locale: sysnet.MacLocale{Timezone: "Europe/Berlin", AutoTZ: sysnet.Some("1"),
			UserLangs: sysnet.SomeList("zh-Hans-CN", "en-US"), UserLocale: sysnet.Some("zh-Hans_CN")},
		Console: "alice",
	}
}

// testTarget 是测试台的目标地区。
var testTarget = region.Target{Timezone: "Asia/Tokyo", Lang: "ja-JP", Locale: "ja_JP", Country: "JP", Source: region.SourceExit}

// fakeVMRegion 记下虚拟机的地区下发（时间线上是改动事件，崩溃注入覆盖它）。
type fakeVMRegion struct {
	b    *bed
	mu   sync.Mutex
	last region.Target
	n    int
	fail error
}

func (v *fakeVMRegion) SetRegion(ctx context.Context, t region.Target) error {
	v.mu.Lock()
	if err := v.fail; err != nil {
		v.fail = nil
		v.mu.Unlock()
		return err
	}
	v.last, v.n = t, v.n+1
	v.mu.Unlock()
	v.b.rec("vm region " + t.Timezone)
	return nil
}

// fakeEnv 是环境检测（M4-6a）：AtEnable 按 fail 返回；Kick 时 ok 就清掉 env_check（模拟一轮明确通过）。
type fakeEnv struct {
	b      *bed
	mu     sync.Mutex
	fail   error
	ok     bool
	atEn   int
	kicks  int
	atHook func()
}

func (e *fakeEnv) AtEnable(ctx context.Context) error {
	e.mu.Lock()
	e.atEn++
	err, h := e.fail, e.atHook
	e.mu.Unlock()
	if h != nil {
		h()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return err
}

func (e *fakeEnv) Kick() {
	e.mu.Lock()
	e.kicks++
	ok := e.ok
	e.mu.Unlock()
	if ok {
		e.b.gate.ClearReason(gatesm.ReasonEnvCheck)
	}
}

// fakeTargets 给启用时的目标地区。
type fakeTargets struct {
	mu  sync.Mutex
	t   region.Target
	ok  bool
	err error
	n   int
}

func (f *fakeTargets) Target(context.Context) (region.Target, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return f.t, f.ok, f.err
}

// 会被关掉 IPv6 的服务（原值不是 Off 的，按名字排序）。
var offServices = []string{"Ethernet", "Thunder", "Wi-Fi"}

func newBed(t *testing.T) *bed {
	t.Helper()
	b := &bed{t: t, init: initialNet(), armed: map[string]int{}, succ: map[string]int{}}
	b.dir = filepath.Join(t.TempDir(), "st")
	b.view = NewStore(b.dir)
	b.net = sysnet.NewFake(b.init)
	b.net.OnChange = func(op string) {
		b.rec("net " + op)
		b.mu.Lock()
		b.succ[op]++
		if k, ok := b.armed[op]; ok && b.succ[op] == k-1 {
			delete(b.armed, op)
			b.net.FailNext(op, errInjected)
		}
		b.mu.Unlock()
		b.check("net " + op)
	}
	b.fpf = pf.NewFakePF()
	b.fpf.UIDs = map[string]string{"_abroad": "250"}
	b.vm = &fakeVM{b: b}
	b.exit = &fakeErr{b: b, name: "exit"}
	b.td = &fakeTeardown{b: b}
	b.kill = &fakeErr{b: b, name: "kill"}
	b.pre = &fakeErr{b: b, name: "preflight"}
	b.vmr = &fakeVMRegion{b: b}
	b.tgts = &fakeTargets{t: testTarget, ok: true}
	b.env = &fakeEnv{b: b, ok: true}
	b.spawn()
	t.Cleanup(func() {
		if b.release != nil {
			b.release()
		}
	})
	return b
}

func (b *bed) onEvent(e Event) { b.mu.Lock(); b.events = append(b.events, e); b.mu.Unlock() }

// newProcStore 建一个“进程”的 Store（带时间线和注入钩子），并拿到跨进程锁。
func (b *bed) newProcStore() (*Store, func()) {
	st := NewStore(b.dir)
	// 测试台不做目录 fsync：崩溃在这里是内存里模拟的（丢掉进程对象），不靠断电，目录落盘与否不影响任何断言；
	// 这个包的耗时主要花在 fsync 上（macOS 上是整盘的 F_FULLFSYNC，并行也分摊不开），去掉这一半能省四成。
	// 目录 fsync 本身及其失败的处理由 file_test.go 的 Save 测试和 TestEnablingDirSyncFailure（自己再换掉这个钩子）覆盖。
	st.syncDir = func(string) error { return nil }
	st.afterSave = func(f File) {
		b.mu.Lock()
		b.writes++
		b.mu.Unlock()
		b.rec(fmt.Sprintf("store %s step=%d pending=%v tokens=%d", f.State, f.Step, f.PFEnablePending, len(f.PFTokens)))
	}
	st.afterPending = func(what string) { b.rec("store pending " + what) }
	st.beforeRename = func() error {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.failWrite != 0 && b.writes+1 == b.failWrite {
			b.failWrite = 0
			return errInjected
		}
		return nil
	}
	rel, err := st.Lock()
	if err != nil {
		b.t.Fatal(err)
	}
	return st, rel
}

func (b *bed) newPF(st *Store) (*PatrolPF, *pfWrap) {
	ctl := &pf.Controller{R: &recRunner{R: b.fpf, b: b}, Matcher: pf.PrintMatcher{User: "_abroad", UID: "250"}}
	ppf := NewPatrolPF(ctl, st, sysnet.NetSource(b.net), "_abroad", func() bool { return !b.noLAN }, b.onEvent)
	return ppf, &pfWrap{PF: ppf, b: b, fail: map[int]error{}}
}

// spawn 起一个新的 abroadd “进程”：新的 Store 实例（持锁）、Patrol、utun（关着）、闸门（关着）、状态机。
func (b *bed) spawn() {
	b.store, b.release = b.newProcStore()
	b.ppf, b.pfw = b.newPF(b.store)
	b.tun = &fakeTun{b: b}
	b.gate = &fakeGate{b: b, reasons: map[gatesm.Reason]bool{gatesm.ReasonState: true}}
	b.re = &fakeRecheck{b: b, ok: true}
	b.ds = &fakeDataState{b: b}
	var routes sysnet.Routes = b.net.Routes()
	var dns sysnet.DNS = b.net.DNS()
	var v6 sysnet.IPv6 = b.net.IPv6()
	var loc sysnet.MacRegion = b.net.Locale()
	if simBackend {
		if b.sim == nil {
			b.sim = sysnet.NewSimRunner(b.net)
		}
		routes = sysnet.MacRoutes{R: b.sim, Iface: func() string { return b.tun.Name() }}
		dns = sysnet.MacDNS{R: b.sim, Server: TakeoverDNS}
		v6 = sysnet.MacIPv6{R: b.sim}
		loc = region.Mac{Run: b.sim, Console: func() (string, int, error) {
			if c := b.net.Snapshot().Console; c != "" {
				return c, 501, nil
			}
			return "", 0, sysnet.ErrNoConsoleUser
		}}
	}
	b.m = &Machine{Store: b.store, D: Deps{
		PF: b.pfw, Tun: b.tun, Routes: routes, DNS: dns, IPv6: v6,
		VM: b.vm, Check: b.exit, Gate: b.gate, Recheck: b.re, Teardown: b.td, Killer: b.kill, Preflight: b.pre, DataState: b.ds,
		Locale: loc, VMRegion: b.vmr, Targets: b.tgts, Env: b.env,
		ReturnBlock: func() bool { return b.rb },
	}, OnEvent: b.onEvent}
}

// crash 模拟 abroadd 崩溃（M2-7c 决定 6）：内存里的东西丢掉，utun 消失、经它的路由随之消失，闸门换新的；
// FakePF、DNS、IPv6、状态文件、虚拟机保留。然后起一个新进程（还没 Recover）。
func (b *bed) crash() {
	b.mu.Lock()
	b.crashAt = 0
	b.mu.Unlock()
	if name := b.tun.Name(); name != "" {
		b.net.RemoveRoutesVia(name)
	}
	b.release()
	b.spawn()
}

// reboot 模拟重启：在崩溃的基础上 pf 复位、路由和 DNS 清掉、虚拟机停掉，IPv6 保留。然后跑 Boot（guard 进程），
// 再起 abroadd 进程（还没 Recover）。返回 Boot 期间的 pfctl 调用。
func (b *bed) reboot() (bootCalls []string, bootErr error) {
	b.mu.Lock()
	b.crashAt = 0
	b.mu.Unlock()
	b.release()
	b.fpf.Reset()
	b.net.ClearRoutesAndDNS()
	b.vm.mu.Lock()
	b.vm.running = false
	b.vm.mu.Unlock()
	// guard 进程。
	st, rel := b.newProcStore()
	_, pw := b.newPF(st)
	b.resetTimeline()
	bootErr = Boot(bg, st, pw, b.onEvent)
	for _, s := range b.tl() {
		if strings.HasPrefix(s, "pf ") {
			bootCalls = append(bootCalls, s)
		}
	}
	rel()
	b.spawn()
	return bootCalls, bootErr
}

var errInjected = fmt.Errorf("注入的失败")

// isChange 报告时间线上的一条是不是“改动事件”（崩溃注入按它计数）。
func isChange(s string) bool {
	for _, p := range []string{"store ", "net ", "tun ", "gate ", "vm "} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	if a, ok := strings.CutPrefix(s, "pf "); ok {
		return strings.HasSuffix(a, " -f -") || strings.Contains(a, "-F ") || a == "-E" || strings.HasPrefix(a, "-X ")
	}
	return false
}

// rec 记进时间线；是改动事件且到了 crashAt 时 panic(crashSignal{}) 模拟崩溃（改动已经发生）。
func (b *bed) rec(s string) {
	b.mu.Lock()
	b.timeline = append(b.timeline, s)
	if isChange(s) {
		b.nev++
		b.evNames = append(b.evNames, s)
		if b.crashAt != 0 && b.nev == b.crashAt {
			b.crashAt = 0
			b.mu.Unlock()
			panic(crashSignal{})
		}
	}
	b.mu.Unlock()
}

// runToCrash 跑 fn，第 k 个改动事件之后崩溃；返回是否真的崩溃了。
func (b *bed) runToCrash(k int, fn func()) (crashed bool) {
	b.mu.Lock()
	b.nev, b.crashAt, b.evNames = 0, k, nil
	b.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(crashSignal); !ok {
				panic(r)
			}
			crashed = true
		}
		b.mu.Lock()
		b.crashAt = 0
		b.mu.Unlock()
	}()
	fn()
	return false
}

// countChanges 空跑 fn，返回改动事件数和名字。
func (b *bed) countChanges(fn func()) (int, []string) {
	b.mu.Lock()
	b.nev, b.crashAt, b.evNames = 0, 0, nil
	b.mu.Unlock()
	fn()
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.nev, append([]string(nil), b.evNames...)
}

func (b *bed) tl() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.timeline...)
}

func (b *bed) resetTimeline() { b.mu.Lock(); b.timeline = nil; b.mu.Unlock() }

// failNthNet 让 sysnet 的 op 第 k 次调用失败（k 从 1 起；按成功次数计）。
func (b *bed) failNthNet(op string, k int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.succ = map[string]int{}
	if k == 1 {
		b.net.FailNext(op, errInjected)
		return
	}
	b.armed[op] = k
}

// failNthWrite 让从现在起第 k 次状态文件写入失败。
func (b *bed) failNthWrite(k int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.writes, b.failWrite = 0, k
}

func (b *bed) countEvents(kind EventKind) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, e := range b.events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// traces 报告接管痕迹（路由、DNS、被我们关掉的 IPv6、utun）。
func (b *bed) traces() []string {
	var out []string
	st := b.net.Snapshot()
	if len(st.Routes) > 0 {
		out = append(out, "路由")
	}
	if st.DNS.IsValid() {
		out = append(out, "DNS")
	}
	for svc, s := range st.IPv6 {
		if s.Mode == sysnet.IPv6Off && b.init.IPv6[svc].Mode != sysnet.IPv6Off {
			out = append(out, "IPv6 "+svc)
		}
	}
	if b.tun.Name() != "" {
		out = append(out, "utun")
	}
	return out
}

// check 检查不变量。leak=false 时不做泄漏断言（pf 清除到写 RESTORED 之间）。
func (b *bed) checkWith(where string, leak bool) {
	b.mu.Lock()
	skip := b.noCheck
	b.mu.Unlock()
	if skip {
		return
	}
	f, err := b.view.Load()
	if err != nil {
		b.violate(where, "读状态文件："+err.Error())
		return
	}
	// 生效的是接管规则（回国拦截那一组不算断网保护，M6-11a）。
	active := b.fpf.Active(pf.DefaultAnchor) && !b.isReturnBlock()
	if b.gate.IsOpen() && (f.State != Enabled || !active) {
		b.violate(where, fmt.Sprintf("I1：闸门开，但状态 %s、pf 生效=%v", f.State, active))
	}
	if b.gate.IsOpen() && b.ds != nil && !b.ds.get() {
		b.violate(where, "闸门开着，但数据面的状态接收方不是“启用”（m3-plan v2.7 决定 3）")
	}
	tr := b.traces()
	if len(tr) > 0 && !active {
		b.violate(where, fmt.Sprintf("I2：有接管痕迹 %v，但 pf 没生效", tr))
	}
	if leak && (f.State != Restored || len(tr) > 0) {
		for _, proto := range []string{"tcp", "udp"} {
			for _, dst := range []string{"160.79.104.10", "1.1.1.1"} { // 后者不在回国拦截的地址段里
				ok, err := b.fpf.Eval(pf.DefaultAnchor, pf.Packet{If: "en0", User: "alice", Proto: proto, DstPort: 443, Dst: netip.MustParseAddr(dst)})
				if err != nil || ok {
					b.violate(where, fmt.Sprintf("泄漏：状态 %s、痕迹 %v，en0 上普通用户到 %s 的 %s 被放行（err=%v）", f.State, tr, dst, proto, err))
				}
			}
		}
	}
}

func (b *bed) check(where string) { b.checkWith(where, true) }

// isReturnBlock 报告锚点里是不是恰好是回国拦截那一组。
func (b *bed) isReturnBlock() bool {
	want, _ := pf.Render(pf.ReturnBlockConfig())
	return bytes.Equal(b.fpf.Raw(pf.DefaultAnchor), want)
}

// assertRestoredRB：开关打开时回国后的终态（M6-11a）——状态文件 RESTORED、恰好留一个 token 且它在 pf 里有效，锚点恰好
// 是回国拦截那一组且生效（Anthropic 地址段被拒、别处放行），假系统与初始值相等，闸门关，utun 关。leak 是允许多出来的
// 未落盘 token 个数。
func (b *bed) assertRestoredRB(want sysnet.FakeState, leak int) {
	b.t.Helper()
	f := b.state()
	// 恰好留一个；只有 -E 之后、token 落盘之前崩溃（漏了一个，有“token 未知”事件）时文件里可以一个都没有，
	// 这时 pf 由漏掉的那个保持启用。
	if f.State != Restored || len(f.PFTokens) > 1 || (len(f.PFTokens) == 0 && leak == 0) || f.PFEnablePending || f.IPv6SnapshotTaken || f.Utun != "" {
		b.t.Fatalf("状态文件：%+v（漏 %d 个）", f, leak)
	}
	live := b.fpf.Tokens()
	if (len(f.PFTokens) == 1 && !slices.Contains(live, f.PFTokens[0])) || len(live) != len(f.PFTokens)+leak {
		b.t.Fatalf("留下的 token %v，pf 里有效的 %v（允许多 %d 个）", f.PFTokens, live, leak)
	}
	if got := b.net.Snapshot(); !got.Equal(want) {
		b.t.Fatalf("假系统与初始值不等：\n%+v\n%+v", got, want)
	}
	if !b.isReturnBlock() || !b.fpf.Active(pf.DefaultAnchor) {
		b.t.Fatalf("锚点应恰好是回国拦截那一组且生效：\n%s", b.fpf.Raw(pf.DefaultAnchor))
	}
	for dst, wantPass := range map[string]bool{"160.79.104.10": false, "2607:6bc0::1": false, "1.1.1.1": true} {
		ok, err := b.fpf.Eval(pf.DefaultAnchor, pf.Packet{If: "en0", User: "alice", Proto: "tcp", DstPort: 443, Dst: netip.MustParseAddr(dst)})
		if err != nil || ok != wantPass {
			b.t.Fatalf("回国后到 %s：放行=%v（err=%v）", dst, ok, err)
		}
	}
	if b.gate.IsOpen() || b.tun.Name() != "" {
		b.t.Fatal("闸门没关或 utun 没关")
	}
}

// checkCleared 是 Flush、Release 之后的正面断言（I2、I3 的例外窗口）：状态是 RESTORING 或回滚中的
// ENABLING，步骤序号 6，没有任何接管痕迹，闸门关着。
func (b *bed) checkCleared(where string) {
	b.checkWith(where, false)
	f, err := b.view.Load()
	if err != nil {
		b.violate(where, "读状态文件："+err.Error())
		return
	}
	if (f.State != Restoring && f.State != Enabling) || f.Step != StepPFClear {
		b.violate(where, fmt.Sprintf("清锚点窗口：状态 %s、步骤 %d", f.State, f.Step))
	}
	if tr := b.traces(); len(tr) > 0 {
		b.violate(where, fmt.Sprintf("清锚点窗口：还有接管痕迹 %v", tr))
	}
	if b.gate.IsOpen() {
		b.violate(where, "清锚点窗口：闸门开着")
	}
}

func (b *bed) violate(where, what string) {
	b.mu.Lock()
	b.violations = append(b.violations, where+"："+what)
	b.mu.Unlock()
}

func (b *bed) noViolations() {
	b.t.Helper()
	if b.sim != nil {
		if u := b.sim.Unknown(); len(u) > 0 {
			b.t.Fatalf("模拟器收到计划外的命令：%v", u)
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.violations) > 0 {
		b.t.Fatalf("不变量被破坏：\n%s", strings.Join(b.violations, "\n"))
	}
}

func (b *bed) state() File {
	b.t.Helper()
	f, err := b.view.Load()
	if err != nil {
		b.t.Fatal(err)
	}
	return f
}

// assertClean：回到干净的 RESTORED，假系统与初始值相等，锚点为空，token 全部释放，闸门关，utun 关。
func (b *bed) assertClean(want sysnet.FakeState) {
	b.t.Helper()
	f := b.state()
	if f.State != Restored || len(f.PFTokens) != 0 || f.PFEnablePending || f.IPv6SnapshotTaken || f.Utun != "" {
		b.t.Fatalf("状态文件不干净：%+v", f)
	}
	if got := b.net.Snapshot(); !got.Equal(want) {
		b.t.Fatalf("假系统与初始值不等：\n%+v\n%+v", got, want)
	}
	if b.fpf.Active(pf.DefaultAnchor) {
		b.t.Fatal("锚点还在生效")
	}
	if ok, _ := b.fpf.Eval(pf.DefaultAnchor, pf.Packet{If: "en0", User: "alice", Proto: "tcp", DstPort: 443, Dst: netip.MustParseAddr("160.79.104.10")}); !ok {
		b.t.Fatal("回国后仍被拦")
	}
	if toks := b.fpf.Tokens(); len(toks) != 0 {
		b.t.Fatalf("还有没释放的 token：%v", toks)
	}
	if b.gate.IsOpen() || b.tun.Name() != "" {
		b.t.Fatal("闸门没关或 utun 没关")
	}
}

// recRunner 把 pfctl 调用记进时间线。
type recRunner struct {
	R pf.Runner
	b *bed
}

func (r *recRunner) Run(ctx context.Context, args []string, stdin []byte) ([]byte, []byte, error) {
	so, se, err := r.R.Run(ctx, args, stdin)
	if err == nil || !isChange("pf "+strings.Join(args, " ")) {
		r.b.rec("pf " + strings.Join(args, " ")) // 改动在成功之后才记（崩溃注入要求改动已经发生）
	} else {
		r.b.rec("pf-failed " + strings.Join(args, " ")) // 失败的改动也记，但不算改动事件
	}
	return so, se, err
}

// pfWrap 给 PF 加上按次数注入失败和不变量检查。
type pfWrap struct {
	PF
	b     *bed
	mu    sync.Mutex
	n     int
	fail  map[int]error // 第几次 Apply 失败（不改 pf）
	failF error
	failR map[int]error
	nR    int
}

func (p *pfWrap) failApply(k int) {
	p.mu.Lock()
	p.n, p.fail = 0, map[int]error{k: errInjected}
	p.mu.Unlock()
}

func (p *pfWrap) Apply(ctx context.Context, utun string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	p.n++
	err := p.fail[p.n]
	p.mu.Unlock()
	if err != nil {
		return err
	}
	err = p.PF.Apply(ctx, utun)
	if err == nil {
		p.b.check("pf apply")
	}
	return err
}

func (p *pfWrap) Flush(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	err := p.failF
	p.failF = nil
	p.mu.Unlock()
	if err != nil {
		return err
	}
	if err := p.PF.Flush(ctx); err != nil {
		return err
	}
	p.b.checkCleared("pf flush")
	return nil
}

// ApplyRestored：换成回国拦截之后，回国途中按“清锚点窗口”核对，RESTORED 里（恢复时）按普通不变量核对。
func (p *pfWrap) ApplyRestored(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.PF.ApplyRestored(ctx); err != nil {
		return err
	}
	if f, err := p.b.view.Load(); err == nil && f.State == Restored {
		p.b.check("pf apply restored")
	} else {
		p.b.checkCleared("pf apply restored")
	}
	return nil
}

func (p *pfWrap) failRelease(k int) {
	p.mu.Lock()
	p.nR, p.failR = 0, map[int]error{k: errInjected}
	p.mu.Unlock()
}

func (p *pfWrap) Release(ctx context.Context, tok string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	p.nR++
	err := p.failR[p.nR]
	p.mu.Unlock()
	if err != nil {
		return err
	}
	err = p.PF.Release(ctx, tok)
	p.b.checkCleared("pf release")
	return err
}

type fakeTun struct {
	b         *bed
	mu        sync.Mutex
	name      string
	dead      bool
	n         int
	failOpen  error
	failClose error
}

func (t *fakeTun) Name() string { t.mu.Lock(); defer t.mu.Unlock(); return t.name }

// Alive 实现 Tun。
func (t *fakeTun) Alive() bool { t.mu.Lock(); defer t.mu.Unlock(); return t.name != "" && !t.dead }

// dieKeepName 模拟 utun 死了但 Name 仍返回旧名字（测试辅助，不回调）。
func (t *fakeTun) dieKeepName() string {
	t.mu.Lock()
	old := t.name
	t.dead = true
	t.mu.Unlock()
	t.b.net.RemoveRoutesVia(old)
	return old
}

// die 模拟 utun 意外消失：经它的路由随之消失（测试辅助，不回调）。返回旧名字。
func (t *fakeTun) die() string {
	t.mu.Lock()
	old := t.name
	t.name = ""
	t.mu.Unlock()
	if old != "" {
		t.b.net.RemoveRoutesVia(old)
	}
	return old
}

func (t *fakeTun) Open(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	t.mu.Lock()
	if err := t.failOpen; err != nil {
		t.failOpen = nil
		t.mu.Unlock()
		return "", err
	}
	t.n++
	t.b.mu.Lock()
	t.b.tunSeq++
	t.name = fmt.Sprintf("utun%d", 6+t.b.tunSeq)
	t.b.mu.Unlock()
	name := t.name
	t.mu.Unlock()
	t.b.rec("tun open " + name)
	t.b.check("tun open")
	return name, nil
}

func (t *fakeTun) Close(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.mu.Lock()
	if err := t.failClose; err != nil {
		t.failClose = nil
		t.mu.Unlock()
		return err
	}
	was := t.name
	t.name, t.dead = "", false
	t.mu.Unlock()
	if was != "" {
		t.b.rec("tun close")
		t.b.check("tun close")
	}
	return nil
}

type fakeVM struct {
	b                   *bed
	mu                  sync.Mutex
	running             bool
	nStarts             int
	failStart, failStop error
	onStop              func() // 在检查 ctx 之前调用（测试用来取消调用方的 ctx）
}

func (v *fakeVM) starts() int { v.mu.Lock(); defer v.mu.Unlock(); return v.nStarts }

// ensureRunning 是复查的检查函数“先确认虚拟机在”（daemon 组装时 VM.Start 再出口检查）；不计入 starts。
func (v *fakeVM) ensureRunning() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.running {
		v.running = true
		v.b.rec("vm start")
	}
}

func (v *fakeVM) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	v.mu.Lock()
	v.nStarts++
	defer v.mu.Unlock()
	if err := v.failStart; err != nil {
		v.failStart = nil
		return err
	}
	v.running = true
	v.b.rec("vm start")
	return nil
}

func (v *fakeVM) Stop(ctx context.Context) error {
	if v.onStop != nil {
		v.onStop()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.failStop; err != nil {
		v.failStop = nil
		return err
	}
	if v.running {
		v.b.rec("vm stop")
	}
	v.running = false
	return nil
}

func (v *fakeVM) Running() bool { v.mu.Lock(); defer v.mu.Unlock(); return v.running }

// fakeErr 是只返回错误的依赖（出口检查、结束 Claude、冲突检查）。block 非 nil 时先通知 entered 再等它关闭。
type fakeErr struct {
	b       *bed
	name    string
	mu      sync.Mutex
	fail    error
	calls   int
	entered chan struct{}
	block   chan struct{}
	hook    func() // 调用时先执行（测试用来取消调用方的 ctx）
	always  error  // 非 nil 时每次都返回它（持续失败）
}

func (e *fakeErr) setAlways(err error) { e.mu.Lock(); e.always = err; e.mu.Unlock() }
func (e *fakeErr) callsN() int         { e.mu.Lock(); defer e.mu.Unlock(); return e.calls }
func (e *fakeErr) nCalls() int         { e.mu.Lock(); defer e.mu.Unlock(); return e.calls }

func (e *fakeErr) do(ctx context.Context) error {
	e.mu.Lock()
	hook := e.hook
	e.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	e.mu.Lock()
	e.calls++
	err := e.fail
	e.fail = nil
	if err == nil {
		err = e.always
	}
	entered, block := e.entered, e.block
	e.mu.Unlock()
	if block != nil {
		close(entered)
		<-block
	}
	if err == nil {
		e.b.rec(e.name)
	}
	return err
}

func (e *fakeErr) Check(ctx context.Context) error      { return e.do(ctx) }
func (e *fakeErr) KillClaude(ctx context.Context) error { return e.do(ctx) }

// fakeGate 按原因开关：加原因立即关；清原因不打开，打开要等复查放行（fakeRecheck.grant）。
type fakeGate struct {
	b       *bed
	mu      sync.Mutex
	open    bool
	opens   int
	reasons map[gatesm.Reason]bool
}

func (g *fakeGate) AddReason(r gatesm.Reason, _ string) {
	g.mu.Lock()
	was := g.open
	g.open = false
	g.reasons[r] = true
	g.mu.Unlock()
	if was {
		g.b.rec("gate close")
		g.b.check("gate close")
	}
}

func (g *fakeGate) ClearReason(r gatesm.Reason) {
	g.mu.Lock()
	delete(g.reasons, r)
	g.mu.Unlock()
}

// grant 模拟复查放行：没有原因时打开。
func (g *fakeGate) grant() bool {
	g.mu.Lock()
	if len(g.reasons) > 0 || g.open {
		ok := g.open
		g.mu.Unlock()
		return ok
	}
	g.open = true
	g.opens++
	g.mu.Unlock()
	g.b.rec("gate open")
	g.b.check("gate open")
	return true
}

func (g *fakeGate) has(r gatesm.Reason) bool { g.mu.Lock(); defer g.mu.Unlock(); return g.reasons[r] }
func (g *fakeGate) IsOpen() bool             { g.mu.Lock(); defer g.mu.Unlock(); return g.open }
func (g *fakeGate) Opens() int               { g.mu.Lock(); defer g.mu.Unlock(); return g.opens }

// fakeRecheck 模拟复查循环：AfterEnable、Kick 时（状态文件是 ENABLED、复查结果 ok）放行。
// 它不调用 bed 的虚拟机和出口检查（监护、Recover 不再调用它们，测试据此断言）。
type fakeRecheck struct {
	b           *bed
	mu          sync.Mutex
	ok          bool
	kicks       int
	afterEnable int
}

func (r *fakeRecheck) AfterEnable(time.Time) {
	r.mu.Lock()
	r.afterEnable++
	ok := r.ok
	r.mu.Unlock()
	if ok {
		r.b.gate.grant()
	}
}

func (r *fakeRecheck) Kick() {
	r.mu.Lock()
	r.kicks++
	ok := r.ok
	r.mu.Unlock()
	if f, err := r.b.view.Load(); ok && err == nil && f.State == Enabled {
		r.b.vm.ensureRunning()
		r.b.gate.grant()
	}
}

func (r *fakeRecheck) setOK(v bool) { r.mu.Lock(); r.ok = v; r.mu.Unlock() }
func (r *fakeRecheck) n() int       { r.mu.Lock(); defer r.mu.Unlock(); return r.kicks }

type fakeTeardown struct {
	b *bed
	n int
}

func (t *fakeTeardown) Teardown(ctx context.Context) { t.n++; t.b.rec("teardown") }

// fakeDataState 是数据面的状态接收方：记进时间线（不算改动事件，不增加崩溃注入点）。
type fakeDataState struct {
	b       *bed
	mu      sync.Mutex
	enabled bool
	n       int
}

func (d *fakeDataState) SetEnabled(e bool) {
	d.mu.Lock()
	d.enabled = e
	d.n++
	d.mu.Unlock()
	if e {
		d.b.rec("data enabled")
	} else {
		d.b.rec("data disabled")
	}
}

func (d *fakeDataState) get() bool { d.mu.Lock(); defer d.mu.Unlock(); return d.enabled }
