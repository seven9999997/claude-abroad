package main

import (
	"claude-abroad/internal/claudeproc"
	"claude-abroad/internal/console"
	"claude-abroad/internal/directdial"
	"claude-abroad/internal/envcheck"
	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/procinfo"
	"claude-abroad/internal/rules"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"claude-abroad/internal/claudecfg"
	"claude-abroad/internal/daemon"
	"claude-abroad/internal/integrity"
	"claude-abroad/internal/redact"
	"claude-abroad/internal/resolverlock"
	"claude-abroad/internal/state"
	"claude-abroad/internal/vmmgr"
	"claude-abroad/internal/wire"
)

// DefaultRealStateDir 是真实模式的状态目录（属主 root，设计 10）。
const DefaultRealStateDir = "/Library/Application Support/claude-abroad"

type realFlags struct {
	stateDir, vmDir, pfUser, mainStore, exitHost, socketDir string
	skipKill, skipRegion                                    bool
	resolverTest                                            bool     // 只有开发构建能设（--dev-resolver-test-domain）
	vmExec, vmUser                                          string   // M6-2a：由 abroadd 管理虚拟机
	vmArgs                                                  []string // 传给 abroad-vm run 的其他参数
	manifest, manifestBase                                  string   // 完整性清单（M6-7）
	launchControl                                           bool     // 打开启动管控（--launch-control）
	rbKillStates                                            bool     // 回国拦截重新加载时定向清状态（--return-block-kill-states，M6-11b）
	fakeClaudeDir                                           string   // 开发构建：只针对这个目录下的进程
}

// checkRealFlags 是真实模式参数的检查（纯函数，M2-9b-2 决定 7、8）。
func checkRealFlags(f realFlags) error {
	switch {
	case f.vmDir == "":
		return errors.New("需要 --vm-dir（M2 阶段虚拟机由外部启动，abroadd 只连它的 socket）")
	case f.mainStore == "":
		return errors.New("需要 --main-proxy-store（启用前的冲突检查要读主代理的第一跳）")
	case f.exitHost == "":
		return errors.New("需要 --exit-check-host（出口检查经主代理访问它）")
	case f.pfUser == "":
		return errors.New("需要 --pf-user")
	case f.vmExec == "" && (f.vmUser != "" || len(f.vmArgs) > 0):
		return errors.New("--vm-user、--vm-arg 只能和 --vm-exec 一起用")
	case f.vmExec != "" && !filepath.IsAbs(f.vmExec):
		return errors.New("--vm-exec 要写绝对路径")
	}
	for _, a := range f.vmArgs {
		switch name := strings.SplitN(strings.TrimLeft(a, "-"), "=", 2)[0]; name {
		case "state-dir", "parent-watch":
			return fmt.Errorf("--vm-arg 里不能有 --%s（由 abroadd 固定传）", name)
		}
	}
	return nil
}

// vmArgList 是可重复的 --vm-arg。
type vmArgList []string

func (l *vmArgList) String() string     { return strings.Join(*l, " ") }
func (l *vmArgList) Set(v string) error { *l = append(*l, v); return nil }

// vmCredential 取运行 abroad-vm 的用户；name 为空时返回 nil（和 abroadd 同一个用户）。
type lookupFunc func(name string) (uid, gid string, err error)

func vmCredential(name string, lookup lookupFunc) (*vmmgr.Credential, error) {
	if name == "" {
		return nil, nil
	}
	us, gs, err := lookup(name)
	if err != nil {
		return nil, fmt.Errorf("找不到运行虚拟机的用户 %s：%w", name, err)
	}
	uid, err1 := strconv.ParseUint(us, 10, 32)
	gid, err2 := strconv.ParseUint(gs, 10, 32)
	if err1 != nil || err2 != nil {
		return nil, fmt.Errorf("用户 %s 的 uid/gid 不是数字", name)
	}
	return &vmmgr.Credential{UID: uint32(uid), GID: uint32(gid)}, nil
}

// vmLifecycle 选虚拟机的启停（M6-2a）：
//   - 没有 --vm-exec：虚拟机由外部启动（ExternalVM，Start 只确认就绪、Stop 什么都不做），和以前相同；
//   - 有：由 abroadd 拉起和管理（vmmgr.Manager + ExecLauncher；这里是整个仓库唯一构造 ExecLauncher 的地方，源码检查）。
//     abroad-vm 带 --parent-watch，abroadd 不管怎么退出它都会自己停下。
func vmLifecycle(f realFlags, lookup lookupFunc, out io.Writer) (state.VM, error) {
	ext := daemon.ExternalVM{Dir: f.vmDir}
	if f.vmExec == "" {
		return ext, nil
	}
	cred, err := vmCredential(f.vmUser, lookup)
	if err != nil {
		return nil, err
	}
	args := append([]string{"run", "--state-dir", f.vmDir, "--parent-watch"}, f.vmArgs...)
	return &vmmgr.Manager{
		Launcher: &vmmgr.ExecLauncher{Path: f.vmExec, Args: args, RunAs: cred, Output: out},
		Health:   ext.Probe,
	}, nil
}

// RealClaudeSettingsDir 是 Claude Code 的托管设置分片目录（主文件在它的上一级）。真实路径只在这里出现（源码检查）。
const RealClaudeSettingsDir = "/Library/Application Support/ClaudeCode/managed-settings.d"

// RealClaudeProxyAddr 是第四层的本机代理端口（静态写在托管设置里）。
var RealClaudeProxyAddr = "127.0.0.1:" + strconv.Itoa(claudecfg.DefaultPort)

// claudeSettings 组装 Claude Code 的托管设置（M6-3b）：只组装，写入在首次准备（M6-8）、删除在卸载（M6-5）。
func claudeSettings(f realFlags) *claudecfg.Settings {
	return &claudecfg.Settings{Dir: RealClaudeSettingsDir, RecordDir: f.stateDir}
}

// integrityChecker 组装完整性校验（M6-7）；没给 --integrity-manifest 时为 nil。
func integrityChecker(f realFlags) *integrity.Checker {
	if f.manifest == "" {
		return nil
	}
	return &integrity.Checker{Manifest: f.manifest, Base: f.manifestBase, Digest: integrity.ManifestDigest}
}

// RealResolverDir 是系统的按域名解析配置目录（resolver(5)）。真实路径只在这里出现（源码检查）。
const RealResolverDir = "/etc/resolver"

// resolverLock 组装 Claude 域名解析锁（M6-3）：真实目录 /etc/resolver，记录放在状态目录；开发构建带
// --dev-resolver-test-domain 时只管保留的测试域名。这里只组装，写入在首次准备（M6-8）、删除在卸载（M6-5）。
func resolverLock(f realFlags) *resolverlock.Lock {
	l := &resolverlock.Lock{Dir: RealResolverDir, RecordDir: f.stateDir}
	if f.resolverTest {
		l.Domains = []string{resolverlock.TestDomain}
	}
	return l
}

func lookupUser(name string) (string, string, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return "", "", err
	}
	return u.Uid, u.Gid, nil
}

// claudeService 选结束 Claude 的实现（m4-plan v2.3 决定 6）：
//   - 不带 --dev-skip-claude-kill：真实的信号发送器（要 root；这里是整个仓库唯一构造它的地方，源码检查）；
//   - 带：不结束任何进程的实现，预览照常如实列出，每次结束时打醒目日志（参数保留到 M6 的发布检查）。
//
// 同一个信号发送器也给启动管控的第三层（M6-4b）；带 --dev-skip-claude-kill 时第三层不接（不结束任何进程）。
func claudeService(skip bool, fakeDir string, logger *log.Logger) (*claudeproc.Service, *claudeproc.LaunchGuard, error) {
	logf := func(format string, a ...any) { logger.Printf(format, a...) }
	if skip {
		svc := claudeproc.NewSystemService(nil, logf)
		_ = svc.KillClaude(context.Background()) // 启动时打一条醒目日志
		return svc, nil, nil
	}
	sig, err := claudeproc.NewRealSignaler()
	if err != nil {
		return nil, nil, err
	}
	svc, lg := claudeproc.NewSystemService(sig, logf), claudeproc.NewSystemLaunchGuard(sig)
	if fakeDir != "" { // 开发构建：只针对指定目录下的进程（真实判定不用）
		claudeproc.RestrictToDir(svc, lg, fakeDir)
		logger.Printf("!!!!!!!! 开发构建：回国结束 Claude 和第三层只针对 %s 下的进程（真实的 Claude 不受影响）!!!!!!!!", fakeDir)
	}
	return svc, lg, nil
}

// controlParts 是启动管控的几样东西（M6-4b-1 返工）：只有带 --launch-control 才有——首次准备（写解析锁）、
// 第三层、第四道（托管设置和本机代理端口；v0.68 起是持久化的开关、默认关，开关由 abroadd 自己存在状态目录里，
// 这里只把它们接上）。不带时都为空：真实模式和 M6 之前完全相同（M2–M5 的真机脚本就是这样运行的）。
type controlParts struct {
	lock      *resolverlock.Lock
	settings  *claudecfg.Settings
	guard     *claudeproc.LaunchGuard
	firstRun  bool
	proxyAddr string
}

func launchControlParts(f realFlags, guard *claudeproc.LaunchGuard) controlParts {
	if !f.launchControl {
		return controlParts{}
	}
	return controlParts{lock: resolverLock(f), settings: claudeSettings(f), guard: guard, firstRun: true, proxyAddr: RealClaudeProxyAddr}
}

// regionVM 选地区同步的虚拟机一侧：带 --dev-skip-region 时返回 nil，地区同步的依赖都不接（启用、应用主代理都不读
// 也不写 Mac 的时区和语言），启动时打醒目日志（m4-plan v2.5 建议改 3；参数保留到 M6 的发布检查）。
func regionVM(f realFlags, logger *log.Logger) state.VMRegion {
	if f.skipRegion {
		logger.Printf("!!!!!!!! 开发构建：不同步时区和语言（只在开发构建里能打开）!!!!!!!!")
		return nil
	}
	return daemon.ExternalVM{Dir: f.vmDir}
}

func runReal(f realFlags, stderr io.Writer, ready func(string, func())) int {
	// 第一步取生产执行器：非 root 在这里就退出，不创建任何目录（决定 8）。
	pfR, netR, err := daemon.ProductionRunners()
	if err != nil {
		fmt.Fprintf(stderr, "abroadd：%v\n", err)
		return 1
	}
	if err := checkRealFlags(f); err != nil {
		fmt.Fprintf(stderr, "abroadd：%v\n", err)
		return 2
	}
	if f.stateDir == "" {
		f.stateDir = DefaultRealStateDir
	}
	logger := log.New(&redact.Writer{W: stderr}, "abroadd ", log.LstdFlags)
	claude, launchGuard, err := claudeService(f.skipKill, f.fakeClaudeDir, logger)
	if err != nil {
		fmt.Fprintf(stderr, "abroadd：%v\n", err)
		return 1
	}
	ctl := launchControlParts(f, launchGuard)
	dataSock := filepath.Join(f.vmDir, "data.sock")
	dialer := &wire.Dialer{Config: wire.DefaultSessionConfig(), Dial: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", dataSock)
	}}
	dp := daemon.NewDataPlane(dialer, nil)
	var direct *directdial.Client
	// 直连拨号模块（design 6.3）：abroad-vm 以专用用户运行，在 vm-dir 里提供 direct.sock；只信任这个用户（m3-plan v2.8）。
	if u, err := user.Lookup(f.pfUser); err == nil {
		if uid, err := strconv.ParseUint(u.Uid, 10, 32); err == nil {
			direct = &directdial.Client{Path: filepath.Join(f.vmDir, "direct.sock"), ServerUID: uint32(uid)}
			dp.SetDirect(direct)
		}
	}
	// 应用识别：procinfo 的真实实现（abroadd 以 root 运行，能看到所有进程的套接字；R3 待真机验证）。
	ident := &procinfo.Identifier{Table: procinfo.SysTable{}, Procs: procinfo.SysProcs{}, Bundle: (&procinfo.FileBundles{}).Read}
	dp.SetIdent(procIdent{ident})
	dp.SetSelf(selfIdent{id: ident, pid: os.Getpid()}) // 探测窗口只放行 abroadd 自己（M4-6a）
	// 出口检查（m4-plan M4-2、M4-3）：经数据通道按出口开流，不经闸门；出口基准在状态目录里。
	checker := &exitcheck.Checker{Opener: dp.Streams(), HTTPSURL: "https://" + f.exitHost + "/generate_204",
		Logf: func(format string, a ...any) { logger.Printf(format, a...) }}
	baseline := daemon.NewBaselineService(checker, f.stateDir)
	vm, err := vmLifecycle(f, lookupUser, logger.Writer())
	if err != nil {
		fmt.Fprintf(stderr, "abroadd：%v\n", err)
		return 1
	}
	sys, err := daemon.NewRealSystem(daemon.RealParams{
		PFRunner: pfR, Net: netR, OpenTun: openUtun,
		LookupUID: func(name string) (string, error) {
			u, err := user.Lookup(name)
			if err != nil {
				return "", err
			}
			return u.Uid, nil
		},
		User: f.pfUser, RootFS: os.DirFS("/"), FirstHop: daemon.MainProxyFirstHop(f.mainStore),
		// 冲突检查读 shell 配置：当前控制台用户的主目录（每次读时重新取；没人登录时这一项跳过；M5-5）。
		HomeFS: console.HomeFS{},
		Data:   dp, VM: vm, ExitVM: daemon.ExternalVM{Dir: f.vmDir},
		// 主代理由 abroadd 持有（M5-4）：存储就是 --main-proxy-store；订阅启用时经数据通道走主代理，经本机网络时经直连拨号模块。
		MainProfile: f.mainStore, MainVM: daemon.ExternalVM{Dir: f.vmDir}, FetchSource: realFetch(dp.Streams(), direct),
		// 按出口的检查（M4-8）：经该出口开流，不做基准比对（判定只看 HTTPS、出口 IP、大陆段、DoH）。
		CheckExit: func(ctx context.Context, tag string) exitcheck.Report {
			return checker.Check(ctx, tag, exitcheck.Options{})
		},
		Exit: daemon.FullExitCheck{Checker: checker, Baseline: baseline}, Baseline: baseline,
		Teardown: dp, Killer: claude, Claude: claude,
		VMRegion: regionVM(f, logger),             // 地区（M4-5）：Mac 一侧经 netR 执行，登录用户读 /dev/console 的属主
		Env:      realEnvDeps(claude, direct, dp), // 环境检测（M4-6a、6b）
		// Claude 域名解析锁（M6-3）：状态接口和冲突检查读它；写入和删除不在这里。
		// 启动管控（解析锁、托管设置、第三层、首次准备、本机代理端口）只有带 --launch-control 才接（M6-4b-1 返工）。
		ResolverLock: ctl.lock,
		// 第四层（M6-3b）：Claude Code 的托管设置（状态和冲突检查读它）。
		ClaudeSettings: ctl.settings,
		// 启动管控的第三层（M6-4b）：不允许启动时每 200 毫秒扫描、结束确认过的 Claude 进程。
		LaunchGuard: ctl.guard, LaunchControl: f.launchControl,
		// 程序完整性校验（M6-7）：清单的摘要在构建时写进程序；没给清单时不校验。
		Integrity: integrityChecker(f),
		// 首次准备（M6-8a）：第一次启动时写入解析锁（v0.68 起不写托管设置）；开发构建只管测试域名时第四道打开也只监听、
		// 不写托管设置（测试模式）。
		FirstRun: ctl.firstRun, PrepareTestOnly: f.resolverTest,
	})
	if err != nil {
		fmt.Fprintf(stderr, "abroadd：%v\n", err)
		return 1
	}
	d, err := daemon.Start(context.Background(), daemon.Options{StateDir: f.stateDir, Sys: sys, Log: logger, SocketDir: f.socketDir,
		ClaudeProxyAddr: ctl.proxyAddr, // 第四层的本机代理端口（M6-3b；只有带 --launch-control；第四道打开时才监听）
		// M6-2b：运行时目录沿用 socket 目录（/var/run/claude-abroad，重启后清空）：给开机时的 guard 写标记、收养它的启用引用。
		RunDir: f.socketDir,
		// M6-11b：回国拦截被重新加载时只清到那几段的连接状态（默认关，等真机结论）。
		ReturnBlockKillStates: f.rbKillStates})
	if err != nil {
		fmt.Fprintf(stderr, "abroadd：%v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "listening %s\n", d.SocketPath())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sig)
	if ready != nil {
		ready(d.SocketPath(), func() { sig <- syscall.SIGTERM })
	}
	s := <-sig
	logger.Printf("收到 %v，退出（网络和 pf 保持原样）", s)
	if err := d.Shutdown(); err != nil {
		logger.Printf("等正在进行的操作结束：%v", err)
	}
	return 0
}

// procIdent 把 procinfo.Identifier 适配成网络栈要的识别器。
type procIdent struct{ id *procinfo.Identifier }

// selfIdent 判断连接是不是 abroadd 自己发起的（探测窗口，M4-6a）：procinfo 查到的进程号等于自己的。识别不到时不算。
type selfIdent struct {
	id  *procinfo.Identifier
	pid int
}

func (s selfIdent) IsSelf(ctx context.Context, network string, src, dst netip.AddrPort) bool {
	r := s.id.Identify(ctx, network, src, dst)
	return r.PID > 0 && r.PID == s.pid
}

// realEnvDeps 是真实模式的环境检测依赖（M4-6a、6b）。第 11 项由 NewRealSystem 接上冲突检查。
func realEnvDeps(claude *claudeproc.Service, direct *directdial.Client, dp *daemon.DataPlane) *daemon.EnvDeps {
	e := &daemon.EnvDeps{
		// 06：连一个只有 IPv6 的地址（公共解析器的 IPv6 地址，443）。真实模式下系统直接拒绝或被 pf 拦住，不经网络栈。
		DialV6: func(ctx context.Context) error {
			var d net.Dialer
			c, err := d.DialContext(ctx, "tcp6", "[2606:4700:4700::1111]:443")
			if err == nil {
				c.Close()
			}
			return err
		},
		// 07：经系统解析器解析（应该到我们自己的 fake-IP DNS）。
		Resolve: func(ctx context.Context, name string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip4", name)
		},
		// 07 后半：经主代理的 DoH 查回显解析器地址的 TXT 记录。在 abroadd 进程里直接查（dp.ProbeTXT），不经系统解析器：
		// 启用第 ⑦ 步闸门还关着，经系统解析器的查询会被我们的 DNS 答 SERVFAIL（2026-11-27 真机）。
		Echo: func(ctx context.Context) (netip.Addr, error) {
			txts, err := dp.ProbeTXT(ctx, "o-o.myaddr.l.google.com")
			if err != nil {
				return netip.Addr{}, err
			}
			for _, t := range txts {
				if a, err := netip.ParseAddr(strings.TrimSpace(t)); err == nil {
					return a, nil
				}
			}
			return netip.Addr{}, errors.New("回显记录里没有地址")
		},
		// 12、17：claudeproc 扫描到的 Claude 根进程的环境（只取白名单里的变量名和语言变量）。
		ProcEnvs: func(ctx context.Context) ([]envcheck.ProcEnv, error) {
			rep, err := claude.Scanner.Scan()
			if err != nil {
				return nil, err
			}
			var out []envcheck.ProcEnv
			for _, t := range rep.Targets {
				if t.Reason != "识别" {
					continue
				}
				env, err := procinfo.SysProcs{}.Env(t.PID)
				if err != nil {
					continue // 进程可能刚退出
				}
				out = append(out, envcheck.ProcEnv{App: t.App, Env: env})
			}
			return out, nil
		},
		// 15、16：当前登录用户（/dev/console 的属主）的主目录。
		Home: console.HomeDir, // M5-5：由控制台用户的 uid 查出（和冲突检查同一个取法）
	}
	// 08：STUN 服务器先经主代理的 DoH 解析成真实地址再拨（目标是域名的 UDP 在链式主代理上没有应答，2026-11-27 真机）。
	e.StunResolve = dp.ProbeA
	// 08 的本机真实公网 IP：经直连拨号模块按域名取一个连到 STUN 服务器的 UDP 连接（只在内存里）。
	if direct != nil {
		e.RealDial = func(ctx context.Context) (net.Conn, error) {
			return direct.Dial(ctx, "udp", "stun.l.google.com", netip.Addr{}, 19302)
		}
	}
	return e
}

func (p procIdent) Identify(ctx context.Context, network string, src, dst netip.AddrPort) (rules.AppID, bool) {
	r := p.id.Identify(ctx, network, src, dst)
	return r.App, r.Claude
}
