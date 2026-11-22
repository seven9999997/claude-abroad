package main

import (
	"claude-abroad/internal/claudeproc"
	"claude-abroad/internal/directdial"
	"claude-abroad/internal/envcheck"
	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/procinfo"
	"claude-abroad/internal/region"
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

	"claude-abroad/internal/daemon"
	"claude-abroad/internal/redact"
	"claude-abroad/internal/state"
	"claude-abroad/internal/wire"
)

// DefaultRealStateDir 是真实模式的状态目录（属主 root，设计 10）。
const DefaultRealStateDir = "/Library/Application Support/claude-abroad"

type realFlags struct {
	stateDir, vmDir, pfUser, mainStore, exitHost, socketDir string
	skipKill, skipRegion                                    bool
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
	}
	return nil
}

// claudeService 选结束 Claude 的实现（m4-plan v2.3 决定 6）：
//   - 不带 --dev-skip-claude-kill：真实的信号发送器（要 root；这里是整个仓库唯一构造它的地方，源码检查）；
//   - 带：不结束任何进程的实现，预览照常如实列出，每次结束时打醒目日志（参数保留到 M6 的发布检查）。
func claudeService(skip bool, logger *log.Logger) (*claudeproc.Service, error) {
	logf := func(format string, a ...any) { logger.Printf(format, a...) }
	if skip {
		svc := claudeproc.NewSystemService(nil, logf)
		_ = svc.KillClaude(context.Background()) // 启动时打一条醒目日志
		return svc, nil
	}
	sig, err := claudeproc.NewRealSignaler()
	if err != nil {
		return nil, err
	}
	return claudeproc.NewSystemService(sig, logf), nil
}

// regionVM 选地区同步的虚拟机一侧：带 --dev-skip-region 时返回 nil，地区同步的依赖都不接（启用、应用主代理都不读
// 也不写 Mac 的时区和语言），启动时打醒目日志（m4-plan v2.5 建议改 3；参数保留到 M6 的发布检查）。
func regionVM(f realFlags, logger *log.Logger) state.VMRegion {
	if f.skipRegion {
		logger.Printf("!!!!!!!! --dev-skip-region：不同步时区和语言（开发用；发布构建不能带这个参数）!!!!!!!!")
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
	claude, err := claudeService(f.skipKill, logger)
	if err != nil {
		fmt.Fprintf(stderr, "abroadd：%v\n", err)
		return 1
	}
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
		Data: dp, VM: daemon.ExternalVM{Dir: f.vmDir}, ExitVM: daemon.ExternalVM{Dir: f.vmDir},
		// 按出口的检查（M4-8）：经该出口开流，不做基准比对（判定只看 HTTPS、出口 IP、大陆段、DoH）。
		CheckExit: func(ctx context.Context, tag string) exitcheck.Report {
			return checker.Check(ctx, tag, exitcheck.Options{})
		},
		Exit: daemon.FullExitCheck{Checker: checker, Baseline: baseline}, Baseline: baseline,
		Teardown: dp, Killer: claude, Claude: claude,
		VMRegion: regionVM(f, logger),         // 地区（M4-5）：Mac 一侧经 netR 执行，登录用户读 /dev/console 的属主
		Env:      realEnvDeps(claude, direct), // 环境检测（M4-6a、6b）
	})
	if err != nil {
		fmt.Fprintf(stderr, "abroadd：%v\n", err)
		return 1
	}
	d, err := daemon.Start(context.Background(), daemon.Options{StateDir: f.stateDir, Sys: sys, Log: logger, SocketDir: f.socketDir})
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
func realEnvDeps(claude *claudeproc.Service, direct *directdial.Client) *daemon.EnvDeps {
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
		// 07 后半：经我们的 DNS 查回显解析器地址的 TXT 记录（经主代理的 DoH 上游）。
		Echo: func(ctx context.Context) (netip.Addr, error) {
			txts, err := net.DefaultResolver.LookupTXT(ctx, "o-o.myaddr.l.google.com")
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
		Home: func() (string, error) {
			name, _, err := region.ConsoleUser()
			if err != nil {
				return "", err
			}
			u, err := user.Lookup(name)
			if err != nil {
				return "", err
			}
			return u.HomeDir, nil
		},
	}
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
