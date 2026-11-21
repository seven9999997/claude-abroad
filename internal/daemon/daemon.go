package daemon

import (
	"claude-abroad/internal/envcheck"
	"claude-abroad/internal/gatesm"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	OnReject          func()                           // 对端 uid 不对、连接被断开时调用（测试用）
	Clock             gatesm.Clock                     // 闸门和复查的时钟（nil 用真实时钟）
	RecheckInterval   time.Duration                    // 出口复查的间隔（0 用默认 60 秒）
	EnvInterval       time.Duration                    // 环境检测的间隔（0 用默认 10 分钟）
	// DevExits 是开发用的多出口（M3 的 /v1/dev-exits 建的，指向本机应答的开发上游）：沿用“除非开流明确失败，
	// 否则可用”，规则也可以引用它们。只有开发工具（daemon-sim、测试）在构造时给；没有任何接口能打这个标记
	// （m4-plan v2.12 决定 4）。
	DevExits []string
}

// Daemon 是一个 abroadd 实例。
type Daemon struct {
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
	// regions 是锁定的目标地区（M4-5）；drift 是最近一次出口漂移的种类（只在变化时发事件）。
	regions *Regions
	drift   atomic.Pointer[string]
	// 环境检测（M4-6a；System.Env 为 nil 时都是 nil）。
	env          *envcheck.Monitor
	envSettings  *envcheck.Settings
	envItemsList []envcheck.Item
	envDone      chan struct{}
	realIP       *envcheck.RealIP // 本机真实公网 IP（只在内存，M4-6b）

	srv     *http.Server
	ln      net.Listener
	supStop context.CancelFunc
	supDone chan struct{}
	reDone  chan struct{}
	rrDone  chan struct{}
	once    sync.Once

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
	sock := filepath.Join(o.StateDir, SocketName)
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
	d.ppf = state.NewPatrolPF(ctl, d.store, &netWatch{inner: sysnet.NetSource(s.Ifaces), d: d}, s.PFUser, nil, d.stateEvent("pf"))
	d.ppf.P.OnEvent = func(e pf.Event) { d.events.Add("patrol", string(e.Kind), e.Detail) }
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
		VM: s.VM, Check: s.Exit, Gate: d.gate, Recheck: d.re, Teardown: s.Teardown, Killer: s.Killer, Preflight: s.Preflight,
	}, OnEvent: d.stateEvent("state")}
	if o.Clock != nil {
		d.m.Now = o.Clock.Now
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
	d.wireExits() // 出口要在规则之前：规则引用的出口要能查到
	d.wireRules() // 规则要在 Recover 之前接上：Recover 到 ENABLED 时会通知数据面启用（重新计算直连配置）
	if err := d.m.Recover(ctx); err != nil {
		o.logf("恢复上次的状态：%v", err)
		d.events.Add("daemon", "recover_failed", err.Error())
		if errors.Is(err, state.ErrCorrupt) {
			d.corrupt.Store(true)
		}
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

	ln, err := net.Listen("unix", sock)
	if err != nil {
		d.stopInternals(false)
		return nil, err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		d.stopInternals(false)
		return nil, err
	}
	peer := o.PeerUID
	if peer == nil {
		peer = peerUID
	}
	d.ln = &peerListener{Listener: ln, uid: d.store.UID, peer: peer, onReject: o.OnReject}
	d.srv = &http.Server{Handler: d.handler(), ReadHeaderTimeout: 10 * time.Second}
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
	if t, ok := d.tun.(interface{ ProcessExit() }); ok {
		t.ProcessExit()
	}
	if d.release != nil {
		d.release()
	}
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

// peerListener 只接受对端 uid 等于属主的连接，其余直接断开（不交给 HTTP）。
type peerListener struct {
	net.Listener
	uid      int
	peer     func(*net.UnixConn) (int, error)
	onReject func()
}

func (l *peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uc, ok := c.(*net.UnixConn)
		if ok {
			if id, err := l.peer(uc); err == nil && id == l.uid {
				return c, nil
			}
		}
		// 先记拒绝再关：对端看到连接被关时，计数已经加上（测试据此断言，不会读到旧值）。
		if l.onReject != nil {
			l.onReject()
		}
		c.Close()
	}
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
	// M4-8：分流出口（正式的和开发用的）的摘要、可用状态、最近一次检查；各出口（含 main）的开流计数。
	// 不含出口 IP、端点地址、配置。
	Exits []ExitStatus          `json:"exits,omitempty"`
	Opens map[string]ExitCounts `json:"opens,omitempty"`
	// M4-5：锁定的目标地区（目标和来源；不含原值）。
	Region *regionStatus `json:"region,omitempty"`
	// M4-6a：最近一轮环境检测（每项的结果、说明、修复办法；不含地址）。
	Env *envcheck.Report `json:"envcheck,omitempty"`
}

// Status 返回当前状态。
func (d *Daemon) Status() Status {
	gs := d.gate.Status()
	st := Status{GateOpen: gs.Open, Gate: gs, Protected: d.ppf.P.Protected(), Utun: d.tun.Name(), Events: d.events.List(), Hints: []Hint{}}
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
	return st
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

type errBody struct {
	Error string `json:"error"`
}

// errCode 把操作的错误映射成 HTTP 状态码（决定 6）。
func errCode(err error) int {
	switch {
	case errors.Is(err, state.ErrBusy):
		return http.StatusConflict
	case errors.Is(err, preflight.ErrBlocked):
		return http.StatusUnprocessableEntity
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
	if !allowCorrupt && d.corrupt.Load() {
		if _, err := d.store.Load(); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, errBody{usererror.New("执行操作", "状态文件已损坏", "请执行 sudo abroad restore --force。", ErrCorruptMode).Error()})
			return ErrCorruptMode
		}
		d.corrupt.Store(false)
	}
	// 操作不跟着请求的连接一起取消（回滚和恢复本来就脱离调用方的取消）。
	if err := fn(context.WithoutCancel(r.Context())); err != nil {
		writeJSON(w, errCode(err), errBody{err.Error()})
		return err
	}
	writeJSON(w, http.StatusOK, d.Status())
	return nil
}

func (d *Daemon) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/enable", d.handleEnable) // 地区会变时两步（m4-plan v2 必须改 2）
	mux.Handle("POST /v1/disable", d.op(func(ctx context.Context) error { return d.m.Disable(ctx, false) }, false))
	mux.HandleFunc("POST /v1/restore", d.handleRestore) // 两步（m4-plan v2.3 决定 5）
	mux.HandleFunc("GET /v1/exits", d.handleExitsList)  // 分流出口（M4-8）
	mux.HandleFunc("GET /v1/exits/{tag}", d.handleExitGet)
	mux.HandleFunc("PUT /v1/exits/{tag}", d.handleExitPut)
	mux.HandleFunc("DELETE /v1/exits/{tag}", d.handleExitDelete)
	mux.HandleFunc("GET /v1/rules", d.handleGetRules)
	mux.HandleFunc("PUT /v1/rules", d.handlePutRules)
	mux.HandleFunc("POST /v1/main-proxy/apply", d.handleApplyMainProxy) // 应用主代理（m4-plan v2.4 决定 4）
	mux.HandleFunc("POST /v1/baseline", d.handleApplyMainProxy)         // 别名（M4-3 的建立基准）
	mux.HandleFunc("PUT /v1/region", d.handleSwitchRegion)              // 切换地区（决定 5）
	mux.HandleFunc("GET /v1/envcheck", d.handleEnvGet)                  // 环境检测（M4-6a）
	mux.HandleFunc("POST /v1/envcheck/run", d.handleEnvRun)
	mux.HandleFunc("PUT /v1/envcheck/settings", d.handleEnvSettings)
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			writeJSON(w, http.StatusRequestEntityTooLarge, errBody{"请求体超过 64 KiB"})
			return
		}
		writeJSON(w, http.StatusOK, d.Status())
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := int64(MaxBody)
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/exits/") {
			limit = MaxExitBody // 出口的配置（订阅最大 1 MiB）
		}
		if r.ContentLength > limit {
			writeJSON(w, http.StatusRequestEntityTooLarge, errBody{"请求体太大"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		mux.ServeHTTP(w, r)
	})
}

// SocketPath 是本地接口的 socket 路径。
func (d *Daemon) SocketPath() string { return filepath.Join(d.opts.StateDir, SocketName) }

// String 便于日志。
func (d *Daemon) String() string { return fmt.Sprintf("abroadd(%s)", d.opts.StateDir) }
