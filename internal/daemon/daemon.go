package daemon

import (
	"claude-abroad/internal/api"
	"claude-abroad/internal/buildinfo"
	"claude-abroad/internal/claudedomains"
	"claude-abroad/internal/console"
	"claude-abroad/internal/envcheck"
	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/gatesm"
	"claude-abroad/internal/integrity"
	"claude-abroad/internal/resolverlock"
	"claude-abroad/internal/vmhost"
	"claude-abroad/internal/vmmgr"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"claude-abroad/internal/pf"
	"claude-abroad/internal/preflight"
	"claude-abroad/internal/rules"
	"claude-abroad/internal/stack"
	"claude-abroad/internal/state"
	"claude-abroad/internal/sysnet"
	"claude-abroad/internal/usererror"
)

// SocketName 是本地接口的 socket 文件名（在状态目录里）。
const SocketName = "api.sock"

// MaxBody 是请求体上限。
const MaxBody = 64 << 10

// 默认值（M2-9a 决定 5）。
const (
	DefaultLockTimeout  = 30 * time.Second
	DefaultLockRetry    = 500 * time.Millisecond
	DefaultShutdownWait = 60 * time.Second
)

// Options 是 daemon 的配置。
type Options struct {
	StateDir          string
	Sys               System
	LockTimeout       time.Duration                    // 拿锁的总时限（0 用默认 30 秒）
	LockRetry         time.Duration                    // 重试间隔（0 用默认 500 毫秒）
	SuperviseInterval time.Duration                    // 监护间隔（0 用默认 5 秒）
	ShutdownWait      time.Duration                    // 退出时等正在进行的操作（0 用默认 60 秒）
	Log               *log.Logger                      // 可为 nil
	PeerUID           func(*net.UnixConn) (int, error) // 取对端 uid（nil 用系统调用；测试注入）
	PeerPID           func(*net.UnixConn) (int, error) // 取对端进程号（nil 用系统调用；测试注入）
	// Verifier 是调用方签名校验（v0.73，callersig；nil 或未启用时“普通”一级对控制台用户开放）。
	Verifier PeerVerifier
	OnReject          func()                           // 对端 uid 不对、连接被断开时调用（测试用）
	ConsoleUID        func() (int, error)              // 取当前控制台用户（nil 用 console.UID；测试注入）
	MaxConns          int                              // 同时连接数上限（0 用 DefaultMaxConns）
	ReadHeaderTimeout time.Duration                    // 每个连接读请求头的时限（0 用 10 秒；只连不发的连接到时被关）
	// SocketDir 是真实模式本地接口 socket 的目录（m5-plan v2.1、design 11.1 v0.22）：给了就在这里按“目录属主本人、
	// 组和其他人不能写，socket 0666”监听，级别只按对端 uid 判；空时沿用状态目录里的私有 socket（目录 0700、socket 0600）。
	SocketDir       string
	Clock           gatesm.Clock  // 闸门和复查的时钟（nil 用真实时钟）
	RecheckInterval time.Duration // 出口复查的间隔（0 用默认 60 秒）
	EnvInterval     time.Duration // 环境检测的间隔（0 用默认 10 分钟）
	// DevExits 是开发用的多出口（M3 的 /v1/dev-exits 建的，指向本机应答的开发上游）：沿用“除非开流明确失败，
	// 否则可用”，规则也可以引用它们。只有开发工具（daemon-sim、测试）在构造时给；没有任何接口能打这个标记
	// （m4-plan v2.12 决定 4）。
	DevExits []string
	// ReturnBlockKillStates：回国拦截被重新加载时只清掉目的地在那几段里的连接状态（M6-11b；默认关，等真机结论）。
	ReturnBlockKillStates bool
	// RunDir 是运行时目录（M6-2b，真实模式是 /var/run/claude-abroad）：给了就为开机时的 guard 另存参数
	// （状态目录里的 guard-params.json）、写“本次开机已加载规则”的标记、收养 guard 取得的 pf 启用引用。空时都不做。
	RunDir string
	// ClaudeProxyAddr 是第四层的本机代理端口（M6-3b；真实模式 127.0.0.1:47821，测试 127.0.0.1:0）；空时不监听。
	// 只能是回环地址。假系统模式默认不监听。
	ClaudeProxyAddr string
	// ClaudeProxyRetry、ClaudeProxyHandshake 是端口被占用时重试的间隔、读完请求头的时限（0 用默认 5 秒、10 秒；测试注入）。
	ClaudeProxyRetry     time.Duration
	ClaudeProxyHandshake time.Duration
	BootID               func() (string, error) // 本次开机的标识（nil 用 guard.BootID；测试注入）
}

// Daemon 是一个 abroadd 实例。
type Daemon struct {
	// M6-2a：由 abroadd 管理的虚拟机（Sys.VM 是 *vmmgr.Manager 时）；虚拟机由外部启动时为 nil。
	vmm    *vmmgr.Manager
	vmStop func()
	vmDone chan struct{}
	// 借用虚拟机（docs/vm-borrow-plan.md v2）：借用进行中、当前的进度（只在内存里）。
	borrowing atomic.Bool
	borrow    atomic.Pointer[BorrowView]
	// M6-3b：第四层的本机代理端口（Options.ClaudeProxyAddr 非空、第四道打开时；cpMu 保护）。
	cproxy *claudeProxy
	cpMu   sync.Mutex
	// v0.68：第四道的开关（默认关）；cpSwitchMu 让打开、关闭、卸载里的清理串行。
	cpOn        atomic.Bool
	cpSwitchMu  sync.Mutex
	writeSwitch func(path string, b []byte) error       // 测试：替换记开关（注入失败）
	cpListen    func(addr string) (net.Listener, error) // 测试：替换监听（注入失败）
	// M6-4b：启动管控的第三层（Sys.LaunchGuard 非空时）。
	// M6-7：启动时完整性校验的结果（接了校验时）。
	integrity    *integrity.Result
	prepareSt    atomic.Pointer[PrepareStatus] // 首次准备最近一次的结果（M6-8a；没打开时为 nil）
	prepareMu    sync.Mutex                    // 首次准备同时只有一次尝试
	gs           *guardState
	uninstalling atomic.Bool          // 一键卸载进行中（M6-5a 补审返工：整段互斥，期间别的改动请求得到“忙”）
	lgMu         sync.Mutex           // 保护 lgStop、lgDone（卸载和退出都可能停第三层）
	domains      *claudedomains.Store // Claude 访问的域名（M6-12；有数据面时）
	cdStop       func()
	cdDone       chan struct{}
	lgStop       func()
	lgDone       chan struct{}
	resolverLast string // 上一次解析锁自检的摘要（变化时才记事件）
	resolverMu   sync.Mutex
	// 地区待还原重试循环跑完的轮数（startupSettled 用）。
	regionRounds atomic.Int64
	// 测试用：为真时把启动检查当作还没结束（startupSettled 返回假）。
	startupHold atomic.Bool

	opts    Options
	store   *state.Store
	release func()
	tun     state.Tun
	ppf     *state.PatrolPF
	gate    *Gate
	m       *state.Machine
	sup     *state.Supervisor
	re      *gatesm.Rechecker
	events  *Events
	corrupt atomic.Bool
	// confirms 是两步确认的确认码（只在内存里，重启后全部失效）。
	confirms *confirms
	// pfctl 是只读查询也用的 pf 控制器（启用失败的现场快照读规则计数）。
	pfctl *pf.Controller
	// enableSnap 是最近一次启用失败的现场快照（只在内存里，进诊断包）。
	enableSnap atomic.Pointer[EnableSnapshot]
	// regions 是锁定的目标地区（M4-5）；drift 是最近一次出口漂移的种类（只在变化时发事件）。
	regions *Regions
	drift   atomic.Pointer[string]
	// 环境检测（M4-6a；System.Env 为 nil 时都是 nil）。
	env          *envcheck.Monitor
	envSettings  *envcheck.Settings
	envItemsList []envcheck.Item
	envDone      chan struct{}
	realIP       *envcheck.RealIP // 本机真实公网 IP（只在内存，M4-6b）

	jobs jobs // 任务（M5-2）
	// 主代理的存储（M5-4）；mainBoot 是上次下发主代理时虚拟机的启动标识（exitPushMu 保护）。
	main      *MainStore
	mainBoot  string
	mainDirty bool                 // 回退没有完成：下一轮无条件重新下发（exitPushMu 保护）
	mudp      mainUDP              // 补齐二：主代理各节点的 UDP 是否可用
	allowLAN  atomic.Bool          // 补齐三：“允许局域网”
	envHist   *envcheck.RunHistory // 补齐四：环境检测的历史（没有环境检测时为 nil）
	// M5-5：冲突检查接口的缓存、手动检查出口的去重。
	pf      preflightCache
	exitChk exitCheckGuard
	srv     *http.Server
	// quitting 在正常退出开始时关闭：事件流这类不会自己结束的应答据此收尾，不让退出等满时限。
	quitting chan struct{}
	ln       net.Listener
	supStop  context.CancelFunc
	supDone  chan struct{}
	reDone   chan struct{}
	rrDone   chan struct{}
	once     sync.Once

	// 分流出口（M4-8）。
	exits      *ExitStore
	exitAvail  *gatesm.ExitAvail
	exitPushMu sync.Mutex // 出口的增删和下发串行
	exitLoop   *exitLoop
	exitDone   chan struct{}

	rulesMu    sync.Mutex
	rules      *rules.Config                     // 当前生效的分流规则（M3-7）
	writeRules func(path string, b []byte) error // 测试：替换写规则文件（注入失败）
}

func (o Options) logf(format string, a ...any) {
	if o.Log != nil {
		o.Log.Printf(format, a...)
	}
}

// ErrSocketSymlink 是 socket 路径上已有的文件是符号链接（拒绝启动）。
var ErrSocketSymlink = errors.New("本地接口的 socket 路径是符号链接")

// Start 启动一个实例（M2-9a 决定 5）：拿锁（ErrLocked 时重试）→ 组装 → Recover → 启动监护 → 监听本地接口。
func Start(ctx context.Context, o Options) (*Daemon, error) {
	d := &Daemon{opts: o, events: &Events{Log: o.Log}}
	if o.Clock != nil {
		d.confirms = newConfirms(o.Clock.Now)
	} else {
		d.confirms = newConfirms(nil)
	}
	d.store = state.NewStore(o.StateDir)
	mp := o.Sys.MainProfile
	if mp == "" {
		mp = filepath.Join(o.StateDir, "secrets", "main")
	}
	d.main = &MainStore{Profile: mp}
	if a, _ := d.main.applied(); a != nil { // 补齐二：启动时下发着的就是“已下发”记录里的那个节点
		d.mudp.pushed = a.Store.Selected
	}
	if fe, ok := o.Sys.Exit.(FullExitCheck); ok && fe.Checker != nil && fe.Checker.OnReport == nil {
		fe.Checker.OnReport = d.recordMainReport // 主代理的每一轮检查（复查、应用主代理时的检查）都经这里
	}
	timeout, retry := o.LockTimeout, o.LockRetry
	if timeout <= 0 {
		timeout = DefaultLockTimeout
	}
	if retry <= 0 {
		retry = DefaultLockRetry
	}
	deadline := time.Now().Add(timeout)
	for {
		rel, err := d.store.Lock()
		if err == nil {
			d.release = rel
			break
		}
		if !errors.Is(err, state.ErrLocked) || time.Now().After(deadline) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retry):
		}
	}
	sock := d.SocketPath()
	if info, err := os.Lstat(sock); err == nil {
		if info.Mode()&fs.ModeSymlink != 0 || !(info.Mode()&fs.ModeSocket != 0) {
			d.release()
			return nil, usererror.New("启动 abroadd", "本地接口的 socket 路径上有不认识的文件（可能是符号链接）", "请检查状态目录。", ErrSocketSymlink)
		}
		if err := os.Remove(sock); err != nil {
			d.release()
			return nil, err
		}
	}

	s := o.Sys
	d.tun = s.NewTun()
	ctl := &pf.Controller{R: s.PFRunner, Matcher: pf.PrintMatcher{User: s.PFUser, UID: s.PFUID}}
	d.pfctl = ctl
	d.allowLAN.Store(d.loadAllowLAN()) // 补齐三：“允许局域网”开关
	d.ppf = state.NewPatrolPF(ctl, d.store, &netWatch{inner: sysnet.NetSource(s.Ifaces), d: d}, s.PFUser, d.allowLAN.Load, d.stateEvent("pf"))
	d.ppf.P.OnEvent = func(e pf.Event) { d.events.Add("patrol", string(e.Kind), e.Detail) }
	d.ppf.P.KillReturnBlockStates = o.ReturnBlockKillStates
	d.ppf.P.ReturnBlockNets = func() []netip.Prefix { return d.rbChoice().Nets } // M6-11b：按选择文件
	d.gate = newGate(o.Clock)
	d.gate.onEvent = func(kind, detail string) { d.events.Add("gate", kind, detail) }
	// 出口复查（m4-plan M4-3）：只在状态是启用时运行；检查前先确认虚拟机在。
	d.regions = &Regions{Path: filepath.Join(o.StateDir, RegionFileName)}
	if s.Baseline != nil {
		d.regions.BaselinePath = s.Baseline.Path
	}
	d.re = &gatesm.Rechecker{Gate: d.gate.g, Check: recheckFunc(s.VM, s.Exit, d.noteDrift), Clock: o.Clock, Interval: o.RecheckInterval,
		Enabled: func() bool { f, err := d.store.Load(); return err == nil && f.State == state.Enabled }}
	d.re.OnEvent = func(kind, detail string) { d.events.Add("recheck", kind, detail) }
	d.m = &state.Machine{Store: d.store, D: state.Deps{
		PF: d.ppf, Tun: d.tun, Routes: s.Routes, DNS: s.DNS, IPv6: s.IPv6,
		VM: mainPushVM{inner: s.VM, d: d}, Check: enableCheck(s.Exit, d), Gate: d.gate, Recheck: d.re, Teardown: s.Teardown, Killer: s.Killer, Preflight: s.Preflight,
		ReturnBlock: d.returnBlockOn,
	}, OnEvent: d.stateEvent("state")}
	if o.Clock != nil {
		d.m.Now = o.Clock.Now
	}
	// 改动类操作拿到状态机的忙标记后再判一次（M6-5a 补审返工：入口检查和拿到互斥之间可能开始了卸载）。
	d.m.Admit = func() error {
		if d.uninstalling.Load() || d.uninstalled() {
			return ErrUninstalledOp
		}
		return nil
	}
	d.setupEnv()      // 环境检测（M4-6a）：启用第 ⑦ 步和运行中的调度
	if d.regionOn() { // 地区同步（M4-5）：Mac、虚拟机、目标都有才做
		d.m.D.Locale, d.m.D.VMRegion, d.m.D.Targets = s.Locale, s.VMRegion, d.regions
	}
	if s.Baseline != nil { // 没有出口基准不能启用（m4-plan v2.2 决定 6；不能把 nil 指针包进接口）
		d.m.D.Baseline = s.Baseline
	}
	// 状态机的“状态接收方”：有数据面时是它（m3-plan v2.7 决定 3；不能把 nil 指针包进接口），外面包一层
	// “进入启用时下发分流出口”（M4-8）。
	if s.Data != nil {
		d.m.D.DataState = enableHook{inner: s.Data, d: d}
	} else {
		d.m.D.DataState = enableHook{d: d}
	}
	d.sup = &state.Supervisor{M: d.m, Patrol: d.ppf.P, Interval: o.SuperviseInterval, OnEvent: d.stateEvent("supervise")}
	if s.Data != nil {
		// 数据面：闸门由开变关时断开连接；网络栈意外退出时立刻 Kick 监护（M2-9b-2 决定 2、3）。
		s.Data.bind(d.gate.View(), d.sup.Kick)
		s.Data.mu.Lock()
		s.Data.onSlow = func() { d.events.Add("dataplane", "detach_slow", "拆网络栈超过时限，留在后台收尾") }
		s.Data.mu.Unlock()
		d.gate.hook = s.Data.gateChanged
	}

	d.wireExitAvail()
	d.wireExits()         // 出口要在规则之前：规则引用的出口要能查到
	d.wireRules()         // 规则要在 Recover 之前接上：Recover 到 ENABLED 时会通知数据面启用（重新计算直连配置）
	d.prepare()           // 首次准备（M6-8a）：要在解析锁自检之前
	d.checkResolverLock() // 解析锁自检（只读；不在或被改了记事件，不自动重写）
	d.checkIntegrity()    // 完整性校验（M6-7）：要在恢复状态和拉起虚拟机之前；不通过时闸门带 integrity 原因、不拉起虚拟机
	d.wireGuard()         // 要在 Recover 之前：Recover 到 ENABLED、DISABLED 时会应用 pf
	if !d.integrityBad() {
		d.startVMManager() // 要在 Recover 之前：Recover 里可能调用虚拟机的启停
	}
	if err := d.m.Recover(ctx); err != nil {
		o.logf("恢复上次的状态：%v", err)
		d.events.Add("daemon", "recover_failed", err.Error())
		if errors.Is(err, state.ErrCorrupt) {
			d.corrupt.Store(true)
		}
	}
	if d.opts.RunDir != "" {
		_ = d.ppf.AdoptExternal() // 开机时 guard 取得的 pf 启用引用：收养进状态文件，回国时一起释放
	}
	if dp := s.Data; dp != nil && dp.direct != nil {
		_ = dp.pushDirect(ctx) // 预热：abroadd 启动后下发一次，失败不算错（直连时按代数补发，m3-plan v2.9）
	}
	supCtx, cancel := context.WithCancel(context.Background())
	d.supStop, d.supDone = cancel, make(chan struct{})
	go func() { defer close(d.supDone); d.sup.Run(supCtx) }()
	d.reDone = make(chan struct{})
	go func() { defer close(d.reDone); d.re.Run(supCtx) }()
	d.exitDone = make(chan struct{})
	go d.exitLoop.run(supCtx, d.exitDone)
	d.envDone = make(chan struct{})
	go func() {
		defer close(d.envDone)
		if d.env != nil {
			d.env.Run(supCtx)
		}
	}()
	d.rrDone = make(chan struct{})
	go func() { // 待还原的地区（m4-plan v2.5）：启动后和每 5 分钟重试
		defer close(d.rrDone)
		if d.regionOn() {
			d.regionRetryLoop(supCtx)
		}
	}()

	d.loadClaudeSwitch()   // 第四道的开关（v0.68；默认关，没有开关的安装什么都不做）
	d.startClaudeProxy()   // 第四层的本机代理端口（M6-3b；开关关着、没配置时什么都不做）
	d.startClaudeDomains() // 记录 Claude 访问的域名（M6-12；有数据面时）
	d.startLaunchGuard()   // 启动管控的第三层（M6-4b；没配置时什么都不做）
	if chk := preflightChecker(s.Preflight); chk != nil && d.cpAvailable() {
		chk.ClaudeProxyDown = d.claudeProxyDown // 端口被占用：冲突检查的“必须处理”项
		chk.ClaudeLayerOn = d.cpOn.Load         // 第四道关着（默认）时和它有关的两项都不出现（v0.68）
	}
	if chk := preflightChecker(s.Preflight); chk != nil && s.FirstRun {
		chk.PrepareProblem = d.prepareProblem // 首次准备没完成：阻止启用，并借这次检查再试一次（M6-8a 补审返工）
	}
	if chk := preflightChecker(s.Preflight); chk != nil && d.integrity != nil && !d.integrity.OK {
		bad := d.integrity.Summary()
		chk.IntegrityProblem = func() string { return bad } // 完整性校验不通过：不能启用（必须处理）
	}
	// 状态目录必须是属主本人、0700（store 建目录时已保证，这里在监听前再查一次）；先用临时名字监听、改成 0600 再改名，
	// 没有权限更宽的窗口（architect 判 W-2a 时指出）。
	listen := vmhost.ListenPrivateUnix
	if o.SocketDir != "" {
		listen = vmhost.ListenSharedUnix
		// /var/run 开机时清空（M6-8b 的开发安装）：目录不在就建（只建最后一级，0755）；属主和权限照样由 ListenSharedUnix 核对。
		if err := os.Mkdir(o.SocketDir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			d.stopInternals(false)
			return nil, err
		}
	}
	ln, err := listen(sock, d.store.UID)
	if err != nil {
		d.stopInternals(false)
		return nil, err
	}
	peer := o.PeerUID
	if peer == nil {
		peer = peerUID
	}
	cons := o.ConsoleUID
	if cons == nil {
		cons = console.UID
	}
	maxc := o.MaxConns
	if maxc <= 0 {
		maxc = DefaultMaxConns
	}
	rht := o.ReadHeaderTimeout
	if rht <= 0 {
		rht = 10 * time.Second
	}
	ppid := o.PeerPID
	if ppid == nil {
		ppid = peerPID
	}
	d.ln = &peerListener{Listener: ln, uid: d.store.UID, peer: peer, console: cons, onReject: o.OnReject, max: int64(maxc),
		verify: o.Verifier, peerPID: ppid}
	d.srv = &http.Server{Handler: d.handler(), ReadHeaderTimeout: rht, ConnContext: connContext}
	d.quitting = make(chan struct{})
	d.srv.RegisterOnShutdown(func() { close(d.quitting) })
	go func() { _ = d.srv.Serve(d.ln) }()
	o.logf("abroadd 已启动，本地接口 %s", sock)
	return d, nil
}

func (d *Daemon) stateEvent(source string) func(state.Event) {
	return func(e state.Event) { d.events.Add(source, string(e.Kind), e.Detail) }
}

// stopInternals 停监护、（可选）关闸门、utun 随进程消失、释放锁。
func (d *Daemon) stopInternals(closeGate bool) {
	if d.supStop != nil {
		d.supStop()
		<-d.supDone
		<-d.reDone
		<-d.exitDone
		<-d.rrDone
		<-d.envDone
	}
	if closeGate {
		d.gate.AddReason(gatesm.ReasonState, "abroadd 退出")
	}
	d.stopLaunchGuard()   // 启动管控的第三层
	d.stopClaudeDomains() // 记录 Claude 访问的域名：最后落一次盘
	d.stopCP()            // 第四层的本机代理端口（没在听时什么都不做）
	if d.vmStop != nil {  // 由 abroadd 管理的虚拟机：关掉自己拉起的（先请它退出，超时强制结束）
		d.vmStop()
		<-d.vmDone
	}
	if t, ok := d.tun.(interface{ ProcessExit() }); ok {
		t.ProcessExit()
	}
	if d.release != nil {
		d.release()
	}
}

// startupSettled 报告启动后的后台工作（监护的第一轮、接了地区时待还原重试的第一轮）是否都已跑完。
// 这两样会短暂占用状态机的忙标记：刚启动时的请求可能被回“忙”（正确行为，应用和命令行提示重试）；测试要等它为真再发请求。
func (d *Daemon) startupSettled() bool {
	if d.startupHold.Load() || d.sup == nil || d.sup.Rounds() < 1 {
		return false
	}
	return !d.regionOn() || d.regionRounds.Load() >= 1
}

// Shutdown 是收到 SIGTERM、SIGINT 时的退出流程（决定 5）：停止接收请求，等正在进行的操作结束（最多 ShutdownWait），
// 停监护，关闸门，释放锁。不恢复网络、不清 pf，状态文件保持原样（服务停止时保持断网）。
func (d *Daemon) Shutdown() error {
	var err error
	d.once.Do(func() {
		wait := d.opts.ShutdownWait
		if wait <= 0 {
			wait = DefaultShutdownWait
		}
		ctx, cancel := context.WithTimeout(context.Background(), wait)
		defer cancel()
		err = d.srv.Shutdown(ctx)
		d.stopInternals(true)
		d.opts.logf("abroadd 已退出（网络和 pf 保持原样）")
	})
	return err
}

// abandon 模拟进程被杀（测试用）：不走退出流程，只有进程消失时操作系统会做的事（连接断开、锁释放、utun 消失）。
func (d *Daemon) abandon() {
	d.once.Do(func() {
		_ = d.srv.Close()
		d.stopInternals(false)
	})
}

// ---- 本地接口 ----

// Status 是 GET /v1/status 的内容。不含出口 IP、节点地址、token。
type Status struct {
	State     string        `json:"state"`
	Corrupt   bool          `json:"corrupt,omitempty"`
	GateOpen  bool          `json:"gate_open"`
	Protected bool          `json:"protected"`
	Utun      string        `json:"utun,omitempty"`
	Events    []EventRecord `json:"events"`
	Hints     []Hint        `json:"hints"`
	// M3-7：不含域名、地址、应用路径。
	Rules  *RulesSummary     `json:"rules,omitempty"`
	Stack  *stack.ProxyStats `json:"stack,omitempty"`
	Direct *DirectStatus     `json:"direct,omitempty"`
	// M4-3：闸门的状态、关闭原因、放行期剩余；分流出口的可用状态（不含出口 IP）。
	Gate gatesm.Status `json:"gate"`
	// M6-2a：由 abroadd 管理虚拟机时的管理器状态（阶段、重启次数、最近一次失败的原因）；虚拟机由外部启动时没有。
	VMManager *vmmgr.Status `json:"vm_manager,omitempty"`
	// 借用虚拟机的进度（未启用时应用主代理、检查主代理临时拉起虚拟机；没有在借用时没有这一项）。
	Borrow *BorrowView `json:"borrow,omitempty"`
	// M6-3：Claude 域名解析锁（第一层）的状态：是否写入、锁的域名、写法、自检结果、冲突（没有接上时没有这一项）。
	ResolverLock *resolverlock.Status `json:"resolver_lock,omitempty"`
	// M6-3b：第四层（Claude Code 的托管设置和本机代理端口）的状态（都没接时没有这一项）。
	ClaudeCode *ClaudeProxyStatus `json:"claude_code,omitempty"`
	// M6-7：程序完整性校验的结果（接了校验时；不通过时只保留断网规则、不接管、不能启用）。
	Integrity *integrity.Result `json:"integrity,omitempty"`
	// M6-4b-3 补审返工：最近一次回国时，用户选了“同时强制退出浏览器”但还在运行的浏览器（没能结束的、签名确认不了
	// 所以没动的、只列出的）。只有名字。
	BrowsersLeft []string `json:"browsers_left,omitempty"`
	// ClaudeProxyChange：这次回国要求切换第四道时的结果（v0.72；只在回国那次应答里有，/v1/status 不带）。
	ClaudeProxyChange *ClaudeProxyChange `json:"claude_proxy_change,omitempty"`
	// M6-4b-1 返工：这次安装是否打开了启动管控（--launch-control）。没打开时解析锁、托管设置、第三层都“未启用”。
	LaunchControl bool `json:"launch_control,omitempty"`
	// M6-8a：首次准备（写入解析锁和托管设置）的结果（打开了首次准备时）。
	Prepare *PrepareStatus `json:"prepare,omitempty"`
	// M6-11a：回国拦截（回国后 pf 只拦 Anthropic 的地址段）。打开了首次准备时才有。
	ReturnBlock *ReturnBlockStatus `json:"return_block,omitempty"`
	// 补齐三：“允许局域网”开关（默认开）。
	AllowLAN *bool `json:"allow_lan,omitempty"` // 总是给（指针只为生成的类型是可选的、旧客户端不受影响）
	// 补齐二：当前下发的主代理节点的 UDP 是否可用（available、unavailable、unknown）；abroadd 持有主代理配置时才有。
	MainUDP string `json:"main_udp,omitempty"`
	// 补齐一：内置的置顶域名（基础域名，和规则引擎、解析锁同一个来源 rules.BuiltinPinnedBases）。删不掉，命令行和应用据此
	// 拦下“删除内置置顶域名”、只读显示。管控开不开都有。
	BuiltinPinned []string `json:"builtin_pinned"`
	// M4-8：分流出口（正式的和开发用的）的摘要、可用状态、最近一次检查；各出口（含 main）的开流计数。
	// 不含出口 IP、端点地址、配置。
	Exits []ExitStatus          `json:"exits,omitempty"`
	Opens map[string]ExitCounts `json:"opens,omitempty"`
	// M4-5：锁定的目标地区（目标和来源；不含原值）。
	Region *regionStatus `json:"region,omitempty"`
	// M4-6a：最近一轮环境检测（每项的结果、说明、修复办法；不含地址）。
	Env *envcheck.Report `json:"envcheck,omitempty"`
	// M5-2：只在 GET /v1/status 里给（调用方签名校验未启用的说明、这次调用方的身份）；别的接口返回的状态不带它。
	Security *api.Security `json:"security,omitempty"`
	// v0.71 原地升级：后台服务的版本号（构建时写进程序；没写入时是 unknown）和状态目录的格式版本号。
	// 安装脚本据此决定升级、什么都不做还是拒绝（不降级；格式不一样时改用卸载再重装）。
	Build       *buildinfo.Info `json:"build,omitempty"` // 总是给（指针只为生成的类型是可选的：老版本没有这一项）
	StateFormat int             `json:"state_format,omitempty"`
	// Busy：状态机此刻有操作占着忙标记（监护的轮次也会短暂占用）；原地升级只在没有时进行。
	Busy bool `json:"busy,omitempty"`
	// Starting：后台服务刚启动，启动检查（监护第一轮、地区待还原重试第一轮）还没结束（2026-11-27 真机）。这期间
	// 改动类请求可能被回 409 busy（文字是 StartingBusyText）；安装、升级结尾等它变假，应用可以在按钮旁边显示“正在启动”。
	Starting bool `json:"starting,omitempty"`
	// Uninstall：一键卸载进行中、已经卸载过（总是给）。
	Uninstall *UninstallFlags `json:"uninstall,omitempty"`
}

// UninstallFlags 是 /v1/status 里一键卸载的两个标记（v0.71）。
type UninstallFlags struct {
	Running bool `json:"running"`
	Done    bool `json:"done"`
}

// Status 返回当前状态。
func (d *Daemon) Status() Status {
	gs := d.gate.Status()
	st := Status{GateOpen: gs.Open, Gate: gs, Protected: d.ppf.P.Protected(), Utun: d.tun.Name(), Events: d.events.List(), Hints: []Hint{}}
	if d.vmm != nil {
		vs := d.vmm.Status()
		st.VMManager = &vs
	}
	if b := d.borrow.Load(); b != nil {
		bv := *b
		st.Borrow = &bv
	}
	if lk := d.opts.Sys.ResolverLock; lk != nil {
		ls := lk.Check()
		st.ResolverLock = &ls
	}
	st.ClaudeCode = d.claudeCodeStatus()
	if d.integrity != nil {
		ir := *d.integrity
		st.Integrity = &ir
	}
	st.LaunchControl = d.opts.Sys.LaunchControl
	st.BuiltinPinned = rules.BuiltinPinnedBases()
	st.MainUDP = d.mainUDPState()
	al := d.allowLAN.Load()
	st.AllowLAN = &al
	st.BrowsersLeft = d.browsersLeft()
	if p := d.prepareSt.Load(); p != nil {
		ps := *p
		st.Prepare = &ps
	}
	if d.opts.Sys.FirstRun {
		st.ReturnBlock = d.returnBlockStatus()
	}
	if f, err := d.store.Load(); err != nil {
		st.State, st.Corrupt = "UNKNOWN", true
	} else {
		st.State = string(f.State)
	}
	if h, ok := d.opts.Sys.Preflight.(Hinter); ok {
		st.Hints = append(st.Hints, h.Hints()...)
	}
	if st.Events == nil {
		st.Events = []EventRecord{}
	}
	d.rulesStatus(&st)
	st.Region = d.regionStatus()
	st.Env = d.envStatus()
	bi := buildinfo.Get()
	st.Build, st.StateFormat = &bi, buildinfo.StateFormat
	st.Busy = d.m.Busy()
	st.Starting = !d.startupSettled()
	st.Uninstall = &UninstallFlags{Running: d.uninstalling.Load(), Done: d.uninstalled()}
	return st
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	if e, ok := v.(errBody); ok { // 只有文字的错误：按状态码补上代码（m5-plan v2.7 决定 0）
		v = api.ErrorBody{Error: e.Error, Code: api.DefaultCode(code)}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// errBody 是只有文字的错误；writeJSON 按状态码补上代码。要特定的代码或结构化数据时用 writeErr。
type errBody struct {
	Error string `json:"error"`
}

// writeErr 写一个带特定代码的错误应答。
func writeErr(w http.ResponseWriter, status int, b api.ErrorBody) {
	if b.Code == "" {
		b.Code = api.DefaultCode(status)
	}
	writeJSON(w, status, b)
}

// opErrCode 是操作出错时的代码（状态码见 errCode）。
func opErrCode(err error, status int) api.ErrorCode {
	switch {
	case errors.Is(err, state.ErrBusy):
		return api.CodeBusy
	case errors.Is(err, exitcheck.ErrNoBaseline):
		return api.CodeNoBaseline
	case errors.Is(err, ErrUninstalledOp):
		return api.CodeUninstalled
	case isMainPushErr(err):
		return api.CodeMainPushFailed
	case isMainNotReady(err):
		return api.CodeMainUnreachable
	}
	return api.DefaultCode(status)
}

// errCode 把操作的错误映射成 HTTP 状态码（决定 6）。
func errCode(err error) int {
	switch {
	case errors.Is(err, state.ErrBusy):
		return http.StatusConflict
	case errors.Is(err, preflight.ErrBlocked):
		return http.StatusUnprocessableEntity
	case errors.Is(err, ErrUninstalledOp):
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// ErrCorruptMode 是状态文件损坏时拒绝启用、停用（只开放状态查询和恢复）。
var ErrCorruptMode = errors.New("状态文件损坏，只能查询状态或恢复")

func (d *Daemon) op(fn func(ctx context.Context) error, allowCorrupt bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			writeJSON(w, http.StatusRequestEntityTooLarge, errBody{"请求体超过 64 KiB"})
			return
		}
		_ = d.exec(w, r, fn, allowCorrupt)
	}
}

// exec 执行一个操作并写应答（请求体已经读过）。返回操作的错误（调用方用来判断要不要放回确认码）。
func (d *Daemon) exec(w http.ResponseWriter, r *http.Request, fn func(ctx context.Context) error, allowCorrupt bool) error {
	return d.execThen(w, r, fn, allowCorrupt, nil)
}

// execThen 同 exec；fn 成功之后先调用 after（可为 nil；v0.72 回国之后切第四道），再取状态写 200，after 的结果放进
// claude_proxy_change。fn 失败时 after 不会被调用。
func (d *Daemon) execThen(w http.ResponseWriter, r *http.Request, fn func(ctx context.Context) error, allowCorrupt bool, after func() *ClaudeProxyChange) error {
	if !allowCorrupt && d.corrupt.Load() {
		if _, err := d.store.Load(); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, errBody{usererror.New("执行操作", "状态文件已损坏", state.RescueHint+"。", ErrCorruptMode).Error()})
			return ErrCorruptMode
		}
		d.corrupt.Store(false)
	}
	// 操作不跟着请求的连接一起取消（回滚和恢复本来就脱离调用方的取消）。
	if err := fn(context.WithoutCancel(r.Context())); err != nil {
		writeErr(w, errCode(err), api.ErrorBody{Error: err.Error(), Code: opErrCode(err, errCode(err))})
		return err
	}
	var chg *ClaudeProxyChange
	if after != nil {
		chg = after()
	}
	st := d.Status()
	st.ClaudeProxyChange = chg
	writeJSON(w, http.StatusOK, st)
	return nil
}

// SocketPath 是本地接口的 socket 路径。
func (d *Daemon) SocketPath() string {
	if d.opts.SocketDir != "" {
		return filepath.Join(d.opts.SocketDir, SocketName)
	}
	return filepath.Join(d.opts.StateDir, SocketName)
}

// String 便于日志。
func (d *Daemon) String() string { return fmt.Sprintf("abroadd(%s)", d.opts.StateDir) }
