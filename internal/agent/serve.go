package agent

import (
	"claude-abroad/internal/wire"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"claude-abroad/internal/guestabi"
	"claude-abroad/internal/kmod"
)

// 从 modloop 加载的模块。vsock 是必需的（没有它控制面不可用）；其余失败只记进 health。
// M1 不加载 ipv6：虚拟机只走 IPv4（设计文档 4.5 的出站策略也禁止 IPv6）。
var (
	essentialModules = []string{"vmw_vsock_virtio_transport"} // 依赖带出 vsock、vmw_vsock_virtio_transport_common
	optionalModules  = []string{"virtio_net", "virtio_balloon", "virtio_rng"}
)

// NetWait 是加载 virtio_net 后等网卡出现的上限。
const NetWait = 5 * time.Second

// Serve 是 PID 1 启动的 `abroad-agent serve`：加载模块、配网卡、在 vsock 1081 上提供控制面。
// ctx 取消（SIGTERM）或收到 POST /v1/shutdown 时正常返回 nil；
// 必需的部分（vsock 模块、控制面监听）失败时返回错误。无论哪种，PID 1 随后关机。
func Serve(ctx context.Context, e *Env) error {
	kver, err := e.Sys.KernelRelease()
	if err != nil {
		return fmt.Errorf("取内核版本：%w", err)
	}
	st := NewState(kver)

	loader := kmod.Loader{
		Dir:  os.DirFS(e.path(path.Join(guestabi.ModloopDir, "modules", kver))),
		Load: e.Sys.LoadModule,
	}
	rs, err := loader.LoadNames(essentialModules)
	if err != nil {
		return fmt.Errorf("加载 vsock 模块：%w", err)
	}
	st.AddModules(rs)
	for _, r := range rs {
		if !r.OK {
			return fmt.Errorf("加载 vsock 模块 %s：%s", r.Name, r.Error)
		}
	}
	for _, m := range optionalModules {
		rs, err := loader.LoadNames([]string{m})
		if err != nil {
			rs = []kmod.Result{{Name: m, Error: err.Error()}}
		}
		st.AddModules(rs)
	}
	for _, f := range kmod.Failed(st.Snapshot().Modules) {
		e.logf("serve：模块 %s 加载失败（详见 /v1/health）", f)
	}

	// lo 先拉起来：引擎都只听 127.0.0.1，没挂网卡时也要有。
	if err := e.Sys.LoopbackUp(ctx); err != nil {
		st.AddProblem("拉起 lo 失败：" + err.Error())
		e.logf("serve：拉起 lo 失败：%v", err)
	}

	// 引擎：sing-box 开机就起；主代理 mihomo 等 PUT /v1/main-proxy 下发配置后才起。
	ectx, ecancel := context.WithCancel(context.Background())
	eng, err := startEngines(ectx, e, st)
	if err != nil {
		ecancel()
		return err
	}
	defer func() {
		t0 := time.Now()
		ecancel()
		eng.wait(10 * time.Second)
		e.logf("serve：停引擎用时 %s", time.Since(t0).Round(time.Millisecond))
	}()

	st.SetNet(configureNet(ctx, e))
	if h := st.Snapshot(); !h.Net.OK {
		e.logf("serve：网卡没有配好：%s", h.Net.Error)
	}

	// 数据面（vsock 1080）先于控制面监听：health 通了，数据面就已经在听。
	dl, err := e.Sys.ListenVsock(guestabi.DataPort)
	if err != nil {
		return fmt.Errorf("监听数据面 vsock %d：%w", guestabi.DataPort, err)
	}
	// 开发用的多出口：只有内核命令行带 abroad.dev=1 时才有（m3-plan v2.8 决定 7）。
	// sing-box 配置的唯一写入者：main 加开发出口加分流出口（M4-7）。
	sbset := &SingBoxSet{Path: e.path(guestabi.SingBoxConfig), Password: eng.socksPassword, Engine: eng.singBox, Base: ectx}
	var exitTable ExitTable = MainExits{SingBox: eng.singBox, Mihomo: eng.mihomo}
	var dev devHooks
	if devSetup != nil { // 只有带 abroaddev 构建标签的版本有（serve_dev.go；M6-1）
		if t, h := devSetup(e, sbset, MainExits{SingBox: eng.singBox, Mihomo: eng.mihomo}); h != nil {
			exitTable, dev = t, h
		}
	}
	// 分流出口（M4-7）：PUT /v1/exits 下发；每个出口的引擎随 ectx 停。
	exits := newServeExits(ectx, e, loader, sbset, dev != nil)
	defer func() { // 后登记先执行：先拆出口，再停 sing-box 和 mihomo
		t0 := time.Now()
		exits.Close()
		e.logf("serve：拆分流出口用时 %s", time.Since(t0).Round(time.Millisecond))
	}()
	exitTable = exitsTable{base: exitTable, ex: exits, singBox: eng.singBox}
	st.SetExits(exits.Status)
	dp := &DataPlane{
		Exits:   exitTable,
		Session: wire.DefaultSessionConfig(),
		Logf:    func(format string, a ...any) { e.logf("数据面："+format, a...) },
		Socks: func(exit string) SocksDialer {
			return SocksDialer{Addr: fmt.Sprintf("127.0.0.1:%d", guestabi.SingBoxSocksPort),
				User: exit, Password: eng.socksPassword, Dial: e.DialSocks}
		},
	}
	st.SetData(dp.Stats)
	dctx, dcancel := context.WithCancel(context.Background())
	dpDone := make(chan error, 1)
	go func() { dpDone <- dp.Serve(dctx, dl) }()
	defer func() { // 先停数据面，再停引擎（引擎的 defer 更早登记，后执行）
		t0 := time.Now()
		dcancel()
		select {
		case <-dpDone:
		case <-time.After(10 * time.Second):
			e.logf("serve：数据面 10 秒内没有停下")
		}
		e.logf("serve：停数据面用时 %s", time.Since(t0).Round(time.Millisecond))
	}()
	e.logf("serve：数据面已在 vsock %d 上监听", guestabi.DataPort)

	l, err := e.Sys.ListenVsock(guestabi.CtlPort)
	if err != nil {
		return fmt.Errorf("监听控制面 vsock %d：%w", guestabi.CtlPort, err)
	}
	stop := make(chan struct{})
	var once sync.Once
	clock := &Clock{Sys: e.Sys, Now: e.Now, Logf: func(format string, a ...any) { e.logf("serve："+format, a...) }}
	st.SetClock(clock.Status)
	st.SetMainDigest(eng.mainProxy.Digest)
	api := &API{State: st, Shutdown: func() { once.Do(func() { close(stop) }) },
		Logs: eng.logs, MainProxy: eng.mainProxy, Clock: clock, Exits: exits}
	// 地区（M4-5）：写三个文件，有变化时重启引擎（主代理已下发时连 mihomo 一起）。
	api.Region = &Region{FS: rootFS{root: e.Root}, Base: ectx,
		Logf: func(format string, a ...any) { e.logf("serve："+format, a...) },
		Restart: func(ctx context.Context) {
			eng.singBox.Restart(ctx)
			if eng.mainProxy.Configured() {
				eng.mihomo.Restart(ctx)
			}
		}}
	if dev != nil {
		if teardown := dev.register(api, e, loader, exits); teardown != nil {
			defer teardown()
		}
	}
	srv := NewServer(api.Handler())
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	e.logf("serve：控制面已在 vsock %d 上监听", guestabi.CtlPort)

	select {
	case <-ctx.Done():
		e.logf("serve：收到停止信号")
	case <-stop:
		e.logf("serve：收到关机请求")
	case err := <-errc:
		return fmt.Errorf("控制面意外退出：%w", err)
	case err := <-dpDone:
		dpDone <- err // 让 defer 里的等待立即返回
		return fmt.Errorf("数据面意外退出：%v", err)
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	return nil
}

type engines struct {
	singBox, mihomo *Engine
	mainProxy       *MainProxy
	logs            map[string]*RingLog
	// SocksPassword 是 sing-box socks 入站的口令（每次开机随机，只在内存和 0600 的配置文件里）。
	socksPassword string
}

func (g *engines) wait(d time.Duration) {
	deadline := time.After(d)
	for _, en := range []*Engine{g.singBox, g.mihomo} {
		select {
		case <-en.Done():
		case <-deadline:
			return
		}
	}
}

// startEngines 建运行目录（0700）、生成 sing-box 配置（0600）并启动 sing-box，准备好 mihomo 和主代理下发。
func startEngines(ctx context.Context, e *Env, st *State) (*engines, error) {
	for _, d := range []string{guestabi.RunDir, guestabi.MihomoDir, guestabi.SingBoxDir} {
		if err := os.MkdirAll(e.path(d), 0o700); err != nil {
			return nil, fmt.Errorf("建运行目录 %s：%w", d, err)
		}
		if err := os.Chmod(e.path(d), 0o700); err != nil {
			return nil, err
		}
	}
	var pw [16]byte
	if _, err := rand.Read(pw[:]); err != nil {
		return nil, err
	}
	g := &engines{socksPassword: hex.EncodeToString(pw[:]),
		logs: map[string]*RingLog{"sing-box": NewRingLog(RingLogBytes), "mihomo": NewRingLog(RingLogBytes)}}
	cfg, err := SingBoxConfig([]SingBoxExit{{Tag: guestabi.ExitMain, Port: guestabi.MainProxyPort}}, g.socksPassword)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(e.path(guestabi.SingBoxConfig), cfg, 0o600); err != nil {
		return nil, fmt.Errorf("写 sing-box 配置：%w", err)
	}
	runner := e.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	logf := func(format string, a ...any) { e.logf("serve："+format, a...) }
	g.singBox = &Engine{
		Name:      "sing-box",
		Check:     []string{guestabi.SingBoxPath, "check", "-c", guestabi.SingBoxConfig},
		Run:       []string{guestabi.SingBoxPath, "run", "-c", guestabi.SingBoxConfig, "-D", guestabi.SingBoxDir},
		Dir:       guestabi.SingBoxDir,
		ReadyAddr: fmt.Sprintf("127.0.0.1:%d", guestabi.SingBoxSocksPort),
		Log:       g.logs["sing-box"], Runner: runner, DialReady: e.DialReady, Logf: logf,
	}
	g.mihomo = &Engine{
		Name:      "mihomo",
		Check:     []string{guestabi.MihomoPath, "-t", "-d", guestabi.MihomoDir, "-f", guestabi.MainProxyConfig},
		Run:       []string{guestabi.MihomoPath, "-d", guestabi.MihomoDir, "-f", guestabi.MainProxyConfig},
		Dir:       guestabi.MihomoDir,
		ReadyAddr: fmt.Sprintf("127.0.0.1:%d", guestabi.MainProxyPort),
		Log:       g.logs["mihomo"], Runner: runner, DialReady: e.DialReady, Logf: logf,
	}
	g.mainProxy = &MainProxy{
		Path: e.path(guestabi.MainProxyConfig),
		Check: func(ctx context.Context, file string) ([]byte, error) {
			guest := path.Join(guestabi.RunDir, filepath.Base(file)) // 临时文件在虚拟机里的路径
			return runner.Run(ctx, []string{guestabi.MihomoPath, "-t", "-d", guestabi.MihomoDir, "-f", guest},
				EngineEnv, guestabi.MihomoDir)
		},
		Engine: g.mihomo, Base: ctx, Logf: logf,
	}
	st.SetEngines(func() map[string]EngineStatus {
		return map[string]EngineStatus{"sing-box": g.singBox.Status(), "mihomo": g.mihomo.Status()}
	}, g.mainProxy.Configured)
	g.singBox.Start(ctx)
	e.logf("serve：sing-box 已启动（socks 127.0.0.1:%d）", guestabi.SingBoxSocksPort)
	return g, nil
}

func configureNet(ctx context.Context, e *Env) error {
	cmdline, err := os.ReadFile(e.path("/proc/cmdline"))
	if err != nil {
		return fmt.Errorf("读取内核命令行：%w", err)
	}
	nc, err := ParseNetCmdline(string(cmdline))
	if err != nil {
		return err
	}
	deadline := e.now().Add(NetWait)
	for !exists(e.path("/sys/class/net/" + nc.Iface)) {
		if !e.now().Before(deadline) {
			return fmt.Errorf("等了 %s 仍没有网卡 %s（虚拟机没有配网卡，或 virtio_net 没加载）", NetWait, nc.Iface)
		}
		e.sleep(100 * time.Millisecond)
	}
	// 虚拟机只走 IPv4（设计 4.5）。先在 up 之前关掉 all、default，避免 up 的瞬间发出 RS、MLD。
	for _, k := range []string{"all", "default"} {
		p := e.path("/proc/sys/net/ipv6/conf/" + k + "/disable_ipv6")
		if exists(p) {
			if err := os.WriteFile(p, []byte("1\n"), 0o644); err != nil {
				return fmt.Errorf("关闭 IPv6（%s）：%w", k, err)
			}
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := e.Sys.ConfigureNet(cctx, nc); err != nil {
		return fmt.Errorf("配置 %s：%w", nc.Iface, err)
	}
	// 虚拟机只走 IPv4（设计 4.5）。M1 不加载 ipv6 模块；万一内核内置了 IPv6，就在 eth0 上关掉。
	v6 := e.path("/proc/sys/net/ipv6/conf/" + nc.Iface + "/disable_ipv6")
	if exists(v6) {
		if err := os.WriteFile(v6, []byte("1\n"), 0o644); err != nil {
			return fmt.Errorf("关闭 %s 的 IPv6：%w", nc.Iface, err)
		}
		e.logf("serve：已关闭 %s 的 IPv6", nc.Iface)
	}
	return nil
}

// ParseNetCmdline 从内核命令行取 abroad.ip、abroad.gw，缺省用 gvisor-tap-vsock 的默认网段。
// 只接受 IPv4；网关必须在地址所在的网段里且不等于地址本身。
func ParseNetCmdline(cmdline string) (NetConfig, error) {
	ip, gw := guestabi.DefaultIP, guestabi.DefaultGW
	for _, f := range strings.Fields(cmdline) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		switch k {
		case guestabi.CmdlineIP:
			ip = v
		case guestabi.CmdlineGW:
			gw = v
		}
	}
	p, err := netip.ParsePrefix(ip)
	if err != nil || !p.Addr().Is4() || p.Bits() < 8 || p.Bits() > 30 {
		return NetConfig{}, fmt.Errorf("%s=%q 不是有效的 IPv4 地址/前缀（前缀长度 8–30）", guestabi.CmdlineIP, ip)
	}
	g, err := netip.ParseAddr(gw)
	if err != nil || !g.Is4() {
		return NetConfig{}, fmt.Errorf("%s=%q 不是有效的 IPv4 地址", guestabi.CmdlineGW, gw)
	}
	if !p.Masked().Contains(g) || g == p.Addr() {
		return NetConfig{}, errors.New("网关必须在虚拟机地址的网段里，且不能等于虚拟机地址")
	}
	return NetConfig{Iface: guestabi.NetIface, Addr: p, GW: g}, nil
}

// newServeExits 准备分流出口的管理器（还不建任何东西：等 PUT /v1/exits）。
// devHooks 是开发用的接口（多出口、M4-1 的探查、同机对端）：只有带 abroaddev 构建标签的版本有实现（serve_dev.go）。
// 默认（发布）构建里 devSetup 为 nil，这些接口的代码整段不编译（M6-1，m6-plan v2.1）。
type devHooks interface {
	register(api *API, e *Env, loader kmod.Loader, exits *Exits) (teardown func())
}

// devSetup 读内核命令行的开发标记（abroad.dev=1），打开时返回带开发出口的出口表和开发接口。
var devSetup func(e *Env, sbset *SingBoxSet, main MainExits) (ExitTable, devHooks)

func newServeExits(ctx context.Context, e *Env, loader kmod.Loader, sbset *SingBoxSet, devMode bool) *Exits {
	runner := e.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	return &Exits{
		Ops: execExitOps{Root: e.Root}, Runner: runner, DialReady: e.DialReady, Base: ctx,
		SelfExe: guestabi.AgentPath, OnChange: sbset.SetExits, Now: e.Now,
		AllowLoopback: devMode, // 服务器地址允许回环只给开发标记
		Logf:          func(format string, a ...any) { e.logf("serve："+format, a...) },
		LoadModules: func(names []string) error {
			var bad []string
			for _, n := range names {
				rs, err := loader.LoadNames([]string{n})
				if err != nil {
					bad = append(bad, n)
					continue
				}
				bad = append(bad, kmod.Failed(rs)...)
			}
			if len(bad) > 0 {
				return fmt.Errorf("没加载上：%s", strings.Join(bad, "、"))
			}
			return nil
		},
	}
}
