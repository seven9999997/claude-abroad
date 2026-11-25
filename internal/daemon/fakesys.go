package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"sync"

	"claude-abroad/internal/claudecfg"
	"claude-abroad/internal/claudeproc"
	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/pf"
	"claude-abroad/internal/preflight"
	"claude-abroad/internal/region"
	"claude-abroad/internal/resolverlock"
	"claude-abroad/internal/state"
	"claude-abroad/internal/sysnet"
)

// System 是 daemon 用到的全部外部依赖（M2-9a 决定 2、4）。9a 只有假系统（FakeSystem.System）。
type System struct {
	PFRunner pf.Runner
	PFUser   string // 放行的用户名（pf 规则里的 user）
	PFUID    string // 它的 UID（Matcher 用）
	Routes   sysnet.Routes
	DNS      sysnet.DNS
	IPv6     sysnet.IPv6
	Ifaces   sysnet.Interfaces
	NewTun   func() state.Tun // 每个 daemon 实例（进程）一个
	VM       state.VM
	Exit     state.ExitCheck
	Teardown state.Teardown
	Killer   state.Killer
	// Claude 给两步恢复的确认框列出要结束的 Claude 进程（M4-4）；nil 时预览为空。
	Claude ClaudePreviewer
	// Preflight 是启用前的冲突检查；实现了 Hinter 时，status 里给出最近一次的提示项。
	Preflight state.Preflight
	// ResolverLock 是 Claude 域名解析锁（M6-3；nil 时不报告）。目录由组装方给出：真实模式 /etc/resolver，测试用临时目录。
	ResolverLock *resolverlock.Lock
	// ClaudeSettings 是 Claude Code 的托管设置分片（M6-3b；nil 时不报告）。目录由组装方给出。
	ClaudeSettings *claudecfg.Settings
	// Data 是数据面（可为 nil：假系统没有数据面）。daemon 启动时绑定闸门和监护的 Kick，闸门由开变关时断开连接。
	Data *DataPlane
	// Env 是环境检测的依赖（M4-6a）：nil 时不做环境检测。
	Env *EnvDeps
	// Locale、VMRegion 是地区（M4-5）：两个都非 nil 才做地区同步（还要有锁定的目标或出口基准）。
	Locale   sysnet.MacRegion
	VMRegion state.VMRegion
	// Baseline 是出口基准（m4-plan v2.2 决定 6）：非 nil 时没有基准不能启用，并提供 POST /v1/main-proxy/apply（应用主代理）。
	// 开发用的最小检查模式（假系统、daemon-sim）为 nil。
	Baseline *BaselineService
	// ExitVM、CheckExit 是分流出口的下发和按出口的检查（M4-8）：ExitVM 为 nil 时出口不下发（正式的出口一律不可用，
	// 开发用的照旧）；CheckExit 为 nil 时不检查（正式的出口永远到不了“通过”）。
	ExitVM    ExitVM
	CheckExit func(ctx context.Context, tag string) exitcheck.Report
	// 主代理由 abroadd 持有（M5-4）：MainProfile 是主代理存储的 profile 目录（空时用状态目录下的 secrets/main）；
	// MainVM 为 nil 时不下发（只管理存储）；FetchSource 抓取订阅（viaHost 为真表示经本机网络；nil 时不能抓取）。
	MainProfile string
	MainVM      MainProxyVM
	FetchSource func(ctx context.Context, url string, viaHost bool) ([]byte, error)
}

// Hint 是一条冲突检查的提示项。
type Hint struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Fix   string `json:"fix"`
}

// Hinter 给出最近一次冲突检查的提示项（只提示、不阻断的那些）。
type Hinter interface{ Hints() []Hint }

// FakeSystem 是内存里的整套假系统（--fake-system 和测试用）：pf 是 pf.FakePF，网络是 sysnet.Fake，
// 其余是简单的假实现。它在 daemon 实例之间保留（模拟“同一台机器”），utun 每个实例各自一个。
type FakeSystem struct {
	PF        *pf.FakePF
	Net       *sysnet.Fake
	VMs       *FakeVM
	ExitCheck *FakeErr
	Kill      *FakeErr // 注入“结束 Claude 失败”（先于 Claude 的假结束流程）
	Tear      *FakeTeardown
	// Procs、Signer、Claude 是假的进程表和签名查询，以及在它们上面的结束流程（M4-4）。
	Procs  *claudeproc.FakeWorld
	Signer *claudeproc.FakeSigner
	Claude *claudeproc.Service
	// VMRegion 是虚拟机地区下发的假实现（M4-5）。
	VMRegion *FakeVMRegion
	Pre      state.Preflight // nil 表示一律通过

	mu     sync.Mutex
	tunSeq int
	// OnTun 在 utun 开、关、消失后调用（可为 nil；测试用来检查不变量）。
	OnTun func(event string)
}

// FakeInitialNet 是假系统的初始网络：默认路由经 en0，四个网络服务覆盖四种 IPv6 模式。
func FakeInitialNet() sysnet.FakeState {
	return sysnet.FakeState{
		Default: "en0",
		IPv6: map[string]sysnet.IPv6Setting{
			"Ethernet": {Mode: sysnet.IPv6Manual, Address: netip.MustParseAddr("fd00::5"), PrefixLen: 64, Router: netip.MustParseAddr("fd00::1")},
			"Thunder":  {Mode: sysnet.IPv6LinkLocal},
			"USB":      {Mode: sysnet.IPv6Off},
			"Wi-Fi":    {Mode: sysnet.IPv6Automatic},
		},
		Net: sysnet.NetInfo{Direct: []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")}, VMIfs: []string{"bridge100"}},
		// 地区（M4-5）：系统级的语言原来没有这个键。
		Locale: sysnet.MacLocale{Timezone: "Europe/Berlin", AutoTZ: sysnet.Some("1"),
			UserLangs: sysnet.SomeList("zh-Hans-CN", "en-US"), UserLocale: sysnet.Some("zh-Hans_CN")},
		Console: "alice",
	}
}

// NewFakeSystem 建一套假系统。
func NewFakeSystem() *FakeSystem {
	f := &FakeSystem{PF: pf.NewFakePF(), Net: sysnet.NewFake(FakeInitialNet())}
	f.PF.UIDs = map[string]string{"_abroad": "250"}
	f.VMs = &FakeVM{}
	f.ExitCheck = &FakeErr{}
	f.Kill = &FakeErr{}
	f.Tear = &FakeTeardown{}
	f.Procs = claudeproc.NewFakeWorld()
	f.Signer = &claudeproc.FakeSigner{Teams: map[string]string{}, Err: map[string]bool{}}
	f.Claude = claudeproc.NewFakeService(f.Procs, f.Signer, os.Getpid(), false, nil)
	f.VMRegion = &FakeVMRegion{}
	return f
}

// FakeVMRegion 记下虚拟机的地区下发。
type FakeVMRegion struct {
	mu   sync.Mutex
	Last region.Target
	N    int
}

// SetRegion 实现 state.VMRegion。
func (v *FakeVMRegion) SetRegion(_ context.Context, t region.Target) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.Last, v.N = t, v.N+1
	return nil
}

// Count 返回下发次数。
func (v *FakeVMRegion) Count() int { v.mu.Lock(); defer v.mu.Unlock(); return v.N }

// fakeKill 先走注入的失败，再走假的结束流程。
type fakeKill struct {
	inj *FakeErr
	svc *claudeproc.Service
}

func (k fakeKill) KillClaude(ctx context.Context) error {
	if err := k.inj.KillClaude(ctx); err != nil {
		return err
	}
	return k.svc.KillClaude(ctx)
}

// System 返回 daemon 用的依赖。
func (f *FakeSystem) System() System {
	pre := f.Pre
	if pre == nil {
		pre = allowAll{}
	}
	return System{
		PFRunner: f.PF, PFUser: "_abroad", PFUID: "250",
		Routes: f.Net.Routes(), DNS: f.Net.DNS(), IPv6: f.Net.IPv6(), Ifaces: f.Net,
		NewTun: func() state.Tun { return &FakeTun{sys: f} },
		VM:     f.VMs, Exit: f.ExitCheck, Teardown: f.Tear, Killer: fakeKill{f.Kill, f.Claude},
		Claude: f.Claude, Preflight: pre, Locale: f.Net.Locale(), VMRegion: f.VMRegion,
	}
}

type allowAll struct{}

func (allowAll) Check(context.Context) error { return nil }

// FakeTun 是假 utun。进程退出（ProcessExit）时设备消失，经它的路由随之消失。
type FakeTun struct {
	sys  *FakeSystem
	mu   sync.Mutex
	name string
}

func (t *FakeTun) notify(ev string) {
	if t.sys.OnTun != nil {
		t.sys.OnTun(ev)
	}
}

// Open 实现 state.Tun。
func (t *FakeTun) Open(ctx context.Context) (string, error) {
	t.sys.mu.Lock()
	t.sys.tunSeq++
	name := fmt.Sprintf("utun%d", 6+t.sys.tunSeq)
	t.sys.mu.Unlock()
	t.mu.Lock()
	t.name = name
	t.mu.Unlock()
	t.notify("open")
	return name, nil
}

// Close 实现 state.Tun。
func (t *FakeTun) Close(ctx context.Context) error {
	t.mu.Lock()
	old := t.name
	t.name = ""
	t.mu.Unlock()
	if old != "" {
		t.sys.Net.RemoveRoutesVia(old)
		t.notify("close")
	}
	return nil
}

// Name 实现 state.Tun。
func (t *FakeTun) Name() string { t.mu.Lock(); defer t.mu.Unlock(); return t.name }

// Alive 实现 state.Tun。
func (t *FakeTun) Alive() bool { return t.Name() != "" }

// ProcessExit 模拟进程退出：utun 消失，经它的路由随之消失（不算状态机的改动）。
func (t *FakeTun) ProcessExit() {
	t.mu.Lock()
	old := t.name
	t.name = ""
	t.mu.Unlock()
	if old != "" {
		t.sys.Net.RemoveRoutesVia(old)
	}
}

// FakeVM 是假虚拟机（幂等）。
type FakeVM struct {
	mu      sync.Mutex
	running bool
}

// Start 实现 state.VM。
func (v *FakeVM) Start(context.Context) error {
	v.mu.Lock()
	v.running = true
	v.mu.Unlock()
	return nil
}

// Stop 实现 state.VM。
func (v *FakeVM) Stop(context.Context) error {
	v.mu.Lock()
	v.running = false
	v.mu.Unlock()
	return nil
}

// Running 报告虚拟机是否在运行。
func (v *FakeVM) Running() bool { v.mu.Lock(); defer v.mu.Unlock(); return v.running }

// FakeErr 是只返回错误的假依赖（出口检查、结束 Claude）：Fail 非 nil 时每次都返回它。
type FakeErr struct {
	mu   sync.Mutex
	Fail error
	n    int
}

// SetFail 设置要返回的错误（nil 表示成功）。
func (e *FakeErr) SetFail(err error) { e.mu.Lock(); e.Fail = err; e.mu.Unlock() }

func (e *FakeErr) do() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.n++
	return e.Fail
}

// Calls 是调用次数。
func (e *FakeErr) Calls() int { e.mu.Lock(); defer e.mu.Unlock(); return e.n }

// Check 实现 state.ExitCheck。
func (e *FakeErr) Check(context.Context) error { return e.do() }

// KillClaude 实现 state.Killer。
func (e *FakeErr) KillClaude(context.Context) error { return e.do() }

// FakeTeardown 记调用次数。
type FakeTeardown struct {
	mu sync.Mutex
	n  int
}

// Teardown 实现 state.Teardown。
func (t *FakeTeardown) Teardown(context.Context) { t.mu.Lock(); t.n++; t.mu.Unlock() }

// Calls 是调用次数。
func (t *FakeTeardown) Calls() int { t.mu.Lock(); defer t.mu.Unlock(); return t.n }

// ErrFakeKill 是假的“结束 Claude 失败”。
var ErrFakeKill = errors.New("Claude 进程没能结束（假系统）")

// PreflightHints 把 preflight.Checker 接成 state.Preflight + Hinter（status 里给出最近一次的提示项）。
type PreflightHints struct{ C *preflight.Checker }

// Check 实现 state.Preflight。
func (p PreflightHints) Check(ctx context.Context) error { return p.C.Check(ctx) }

// Hints 实现 Hinter。
func (p PreflightHints) Hints() []Hint {
	var out []Hint
	for _, f := range p.C.Last().Warnings() {
		out = append(out, Hint{ID: f.ID, Title: f.Title, Fix: f.Fix})
	}
	return out
}
