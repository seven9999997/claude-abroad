package daemon

import (
	"claude-abroad/internal/region"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"strings"
	"sync"

	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/managedproxy"
	"claude-abroad/internal/pf"
	"claude-abroad/internal/preflight"
	"claude-abroad/internal/state"
	"claude-abroad/internal/sysnet"
	"claude-abroad/internal/tun"
	"claude-abroad/internal/usererror"
)

// utun 的地址（设计 4.1：198.18.0.1/15，点对点的对端同一个地址）。
var (
	UtunLocal = netip.MustParseAddr("198.18.0.1")
	UtunPeer  = netip.MustParseAddr("198.18.0.1")
)

// DefaultAllowUser 是 pf 放行的用户（abroad-vm 以它运行）。
const DefaultAllowUser = "_abroad"

// RealParams 是真实系统的全部参数（M2-9b-1 决定 2）：会碰系统的东西都从这里传入，测试用模拟器和假实现。
type RealParams struct {
	PFRunner  pf.Runner                          // 生产是 pf.ExecRunner（只经 ProductionRunners 取得）
	Net       sysnet.Runner                      // 生产是 sysnet.ExecRunner（同上）
	OpenTun   func() (tun.Device, error)         // 打开 utun 设备（生产是 tun.Open；测试用 tun.Pipe）
	LookupUID func(user string) (string, error)  // 查放行用户的 uid
	Ifaces    func() ([]sysnet.IfaceInfo, error) // 网卡列表（nil 用 net.Interfaces）
	User      string                             // 放行用户（空用 DefaultAllowUser）
	RootFS    fs.FS                              // 只读的根文件系统（冲突检查读网络共享）；可为 nil
	HomeFS    fs.FS                              // 登录用户主目录（冲突检查读 shell 配置）；9b-1 可为 nil（M5 待办）
	FirstHop  func() (string, error)             // 主代理的第一跳；nil 表示还没有主代理，启用被拒绝
	// 9b-2 的钩子：utun 打开并配好地址之后、关闭之前（挂网络栈用）。可为 nil。
	OnTunOpen  func(dev tun.Device) error
	OnTunClose func(dev tun.Device)
	// Data 非 nil 时接上数据面（M2-9b-2）：utun 的钩子挂、拆网络栈，Alive 包含网络栈，Teardown 由它做。
	// 这时 OnTunOpen、OnTunClose 先调数据面，再调上面的（测试用）。
	Data *DataPlane
	// 9b-1 里继续用假实现的（决定 5）。
	VM       state.VM
	Exit     state.ExitCheck
	Baseline *BaselineService // 出口基准（真实模式必有：没有基准不能启用）
	Teardown state.Teardown
	Killer   state.Killer
	Claude   ClaudePreviewer // 两步恢复的预览（M4-4；nil 时预览为空）
	// 地区（M4-5）：Mac 一侧经 Net 执行（region.Mac）；Console 取登录用户（nil 时用 region.ConsoleUser）。
	// VMRegion 为 nil 时不做地区同步。
	VMRegion state.VMRegion
	Console  region.Console
	// Env 是环境检测的依赖（M4-6a；nil 时不做）。
	Env *EnvDeps
	// ExitVM、CheckExit：分流出口的下发和按出口的检查（M4-8；nil 时不下发、不检查）。
	ExitVM    ExitVM
	CheckExit func(ctx context.Context, tag string) exitcheck.Report
	// 主代理由 abroadd 持有（M5-4）。
	MainProfile string
	MainVM      MainProxyVM
	FetchSource func(ctx context.Context, url string, viaHost bool) ([]byte, error)
}

// ErrNoAllowUser 是查不到放行用户。
var ErrNoAllowUser = errors.New("查不到放行用户")

// ErrNoMainProxy 是还没有应用主代理（启用的前提）。
var ErrNoMainProxy = errors.New("还没有应用主代理")

// tunHolder 记当前实例的 utun（MacRoutes.Iface、冲突检查的 OurUtun 要用）。
type tunHolder struct {
	mu  sync.Mutex
	cur *realTun
}

func (h *tunHolder) name() string {
	h.mu.Lock()
	t := h.cur
	h.mu.Unlock()
	if t == nil {
		return ""
	}
	return t.Name()
}

// NewRealSystem 组装真实系统（M2-9b-1）。它本身不发任何命令；查不到放行用户时返回 usererror。
func NewRealSystem(p RealParams) (System, error) {
	user := p.User
	if user == "" {
		user = DefaultAllowUser
	}
	if p.PFRunner == nil || p.Net == nil || p.OpenTun == nil || p.LookupUID == nil {
		return System{}, errors.New("NewRealSystem：缺少必要的参数")
	}
	uid, err := p.LookupUID(user)
	if err != nil || uid == "" {
		return System{}, usererror.New("准备系统组件", "查不到放行用户 "+user, "请重新安装 Claude 出国（安装时会创建这个用户）。", errors.Join(ErrNoAllowUser, err))
	}
	h := &tunHolder{}
	onOpen, onClose, extra := p.OnTunOpen, p.OnTunClose, func() bool { return true }
	teardown := p.Teardown
	if dp := p.Data; dp != nil {
		userOpen, userClose := p.OnTunOpen, p.OnTunClose
		onOpen = func(d tun.Device) error {
			if err := dp.Attach(d); err != nil {
				return err
			}
			if userOpen != nil {
				return userOpen(d)
			}
			return nil
		}
		onClose = func(d tun.Device) {
			dp.Detach(d)
			if userClose != nil {
				userClose(d)
			}
		}
		extra = dp.StackAlive
		teardown = dp
	}
	routes := sysnet.MacRoutes{R: p.Net, Iface: h.name}
	chk := &preflight.Checker{R: p.Net, Root: p.RootFS, Home: p.HomeFS, OurUtun: h.name, FirstHop: p.FirstHop}
	if p.Env != nil && p.Env.Conflicts == nil { // 第 11 项复用冲突检查（带上我们自己的 utun 名）
		env := *p.Env
		env.Conflicts = chk.TakeoverConflicts
		p.Env = &env
	}
	var locale sysnet.MacRegion
	if p.VMRegion != nil {
		con := p.Console
		if con == nil {
			con = region.ConsoleUser
		}
		locale = region.Mac{Run: p.Net, Console: con}
	}
	return System{
		PFRunner: p.PFRunner, PFUser: user, PFUID: uid,
		Routes: routes,
		DNS:    sysnet.MacDNS{R: p.Net, Server: state.TakeoverDNS},
		IPv6:   sysnet.MacIPv6{R: p.Net},
		Ifaces: sysnet.MacInterfaces{List: p.Ifaces},
		NewTun: func() state.Tun {
			t := &realTun{open: p.OpenTun, net: p.Net, onOpen: onOpen, onClose: onClose, aliveExtra: extra}
			h.mu.Lock()
			h.cur = t
			h.mu.Unlock()
			return t
		},
		VM: p.VM, Exit: p.Exit, Baseline: p.Baseline, Teardown: teardown, Killer: p.Killer, Claude: p.Claude, Locale: locale, VMRegion: p.VMRegion, Env: p.Env,
		ExitVM: p.ExitVM, CheckExit: p.CheckExit,
		MainProfile: p.MainProfile, MainVM: p.MainVM, FetchSource: p.FetchSource,
		Preflight: realPreflight{PreflightHints{C: chk}, p.FirstHop != nil},
		Data:      p.Data,
	}, nil
}

// realPreflight：没有主代理时直接拒绝（“前提：主代理已应用”），有才跑冲突检查。
type realPreflight struct {
	PreflightHints
	hasMain bool
}

func (r realPreflight) Check(ctx context.Context) error {
	noMain := !r.hasMain
	if r.hasMain && r.C.FirstHop != nil {
		if _, err := r.C.FirstHop(); errors.Is(err, ErrNoMainProxy) {
			noMain = true
		}
	}
	if noMain {
		return usererror.New("启用", "还没有应用主代理", "前提：主代理已应用。请先导入并应用一个主代理，再启用。", ErrNoMainProxy)
	}
	return r.PreflightHints.Check(ctx)
}

// realTun 是 utun 的真实实现的前半部分（决定 4）：打开设备 + ifconfig 配地址和 MTU + 关闭。挂网络栈在 9b-2（钩子）。
type realTun struct {
	open       func() (tun.Device, error)
	net        sysnet.Runner
	onOpen     func(tun.Device) error
	onClose    func(tun.Device)
	aliveExtra func() bool // 数据面：网络栈还在运行（M2-9b-2 决定 2）

	mu  sync.Mutex
	dev *trackedDev
}

// trackedDev 记设备是否已关闭（不论是谁关的），Alive 据此判断。
type trackedDev struct {
	tun.Device
	mu     sync.Mutex
	closed bool
}

func (d *trackedDev) Close() error {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
	return d.Device.Close()
}

func (d *trackedDev) isClosed() bool { d.mu.Lock(); defer d.mu.Unlock(); return d.closed }

// Open 实现 state.Tun。
func (t *realTun) Open(ctx context.Context) (string, error) {
	raw, err := t.open()
	if err != nil {
		return "", fmt.Errorf("打开 utun：%w", err)
	}
	dev := &trackedDev{Device: raw}
	path, args, err := sysnet.UtunConfigArgs(dev.Name(), UtunLocal, UtunPeer, dev.MTU())
	if err == nil {
		_, se, rerr := t.net.Run(ctx, path, args, nil)
		if rerr != nil {
			err = fmt.Errorf("配置 utun：%w（%s）", rerr, se)
		}
	}
	if err == nil && t.onOpen != nil {
		err = t.onOpen(dev)
	}
	if err != nil {
		_ = dev.Close()
		return "", err
	}
	t.mu.Lock()
	t.dev = dev
	t.mu.Unlock()
	return dev.Name(), nil
}

// Close 实现 state.Tun。
func (t *realTun) Close(ctx context.Context) error {
	t.mu.Lock()
	dev := t.dev
	t.dev = nil
	t.mu.Unlock()
	if dev == nil {
		return nil
	}
	if t.onClose != nil {
		t.onClose(dev)
	}
	return dev.Close()
}

// Name 实现 state.Tun（设备死了也报旧名字，Alive 为假，rebuild 会先 Close 再 Open）。
func (t *realTun) Name() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dev == nil {
		return ""
	}
	return t.dev.Name()
}

// Alive 实现 state.Tun：设备关闭后为假。
func (t *realTun) Alive() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dev == nil || t.dev.isClosed() {
		return false
	}
	return t.aliveExtra == nil || t.aliveExtra()
}

// ProcessExit：进程退出时 utun 随之消失（daemon 的退出流程调用）。
func (t *realTun) ProcessExit() { _ = t.Close(context.Background()) }

// MainProxyFirstHop 从 managedproxy 的存储（profile 目录）读主代理的第一跳主机（M2-9b-2 决定 6）。
// 远端第一跳取 VMConfig.FirstHop 的第一项；回环和局域网第一跳在 Allow 里（冲突检查要据此判断回环）。
// 读不出来或没有主代理时返回 ErrNoMainProxy（启用被拒绝）。
func MainProxyFirstHop(profile string) func() (string, error) {
	return func() (string, error) {
		s, err := managedproxy.Load(profile)
		if err != nil {
			return "", errors.Join(ErrNoMainProxy, err)
		}
		cfg, err := s.VMRuntimeConfig()
		if err != nil {
			return "", errors.Join(ErrNoMainProxy, err)
		}
		hop := ""
		switch {
		case len(cfg.FirstHop) > 0:
			hop = cfg.FirstHop[0]
		case len(cfg.Allow) > 0:
			hop = cfg.Allow[0]
		default:
			return "", ErrNoMainProxy
		}
		host, _, ok := strings.Cut(hop, ":")
		if !ok || host == "" {
			return "", fmt.Errorf("%w：第一跳的格式不对", ErrNoMainProxy)
		}
		return host, nil
	}
}
