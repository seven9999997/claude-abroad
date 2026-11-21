// abroad-vm 运行 Claude 出国的虚拟机（设计文档 3.1、4.4、7.1）。
//
//	abroad-vm run --image-dir guest/out --state-dir scripts/dev/out/state [--cpus 1] [--memory 256]
//	              [--allow-outbound IPv4:端口]… [--first-hop host:port[,port|lo-hi…]]… [--outbound-open]
//
// 出站默认收紧（设计 4.5，M2-1a）：虚拟机只能连白名单和路线第一跳；第一跳是域名时由网关 DNS 代为解析
// （abroad-vm 自己经 DoH 查，答出的地址才放行）。--outbound-open 只给开发用，恢复 M1 的“公网全放行”。
//
// 开发模式下以当前用户运行（不需要 sudo），状态目录里有：
//
//	ctl.sock       → 虚拟机 vsock 1081（abroad-agent 控制面），0600
//	data.sock      → 虚拟机 vsock 1080（数据面，wire/yamux），0600
//	host.sock      abroad-vm 自己应答的诊断接口（GET /v1/netstats；PUT /v1/exit-endpoints 替换分流出口的端点），
//	               不转发进虚拟机，0600，只接受 abroad-vm 自己的用户和 root
//	console.log    虚拟机控制台输出，0600
//	abroad-vm.pid  本进程的 pid
//	abroad-vm.lock 单实例锁
//
// 需要 com.apple.security.virtualization 授权：用 scripts/dev/sign.sh 做 ad-hoc 签名。
// 虚拟机的网卡接到进程内的用户态网络（gvisor-tap-vsock，出站绑定物理网卡，见 net.go）。
// SIGINT/SIGTERM 优雅停止，再来一次立即强停。
package main

import (
	"github.com/sirupsen/logrus"

	"claude-abroad/internal/redact"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"claude-abroad/internal/directdial"
	"claude-abroad/internal/guestabi"
	"claude-abroad/internal/platform"
	"claude-abroad/internal/usernet"
	"claude-abroad/internal/vmhost"
	"runtime"
)

func main() {
	if msg, code := platform.MacOnlyEntry("abroad-vm", runtime.GOOS); code != 0 { // W-1：Windows 上只打印说明
		fmt.Fprintln(os.Stderr, msg)
		os.Exit(code)
	}
	if len(os.Args) >= 2 && os.Args[1] == "direct" {
		sigs := make(chan os.Signal, 2)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		if err := runDirect(os.Args[2:], sigs, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, "abroad-vm direct:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) < 2 || os.Args[1] != "run" {
		fmt.Fprintln(os.Stderr, "用法：abroad-vm run --image-dir <目录> --state-dir <目录> [--cpus 1] [--memory 256] [--direct-sock <路径> --direct-dns <IPv4>…]")
		fmt.Fprintln(os.Stderr, "      abroad-vm direct --sock <路径> --dns <IPv4>… [--peer-uid <uid>]")
		os.Exit(2)
	}
	redactThirdPartyLogs(os.Stderr)
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	if err := run(os.Args[2:], sigs, vmhost.NewVZ, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "abroad-vm:", err)
		os.Exit(1)
	}
}

// supTiming 是生命周期时长（测试里换成短的）。
var supTiming = vmhost.DefaultTiming

// redactThirdPartyLogs：第三方库（gvisor-tap-vsock）用 logrus 打日志，可能带目标 IP
// （例如 udp_proxy 的 "dial udp a.b.c.d:port: …"），一律经脱敏后再写出。
func redactThirdPartyLogs(w io.Writer) { logrus.SetOutput(&redact.Writer{W: w}) }

type options struct {
	imageDir, stateDir string
	cpus               uint
	memory             uint64
	allow              []netip.AddrPort
	firstHops          []usernet.FirstHop
	outboundOpen       bool
	direct             directOpts
	devAgent           bool // 只有 abroaddev 构建标签的版本有这个开关（devflags_dev.go）
}

// addDevFlags 由 abroaddev 构建标签的文件设置；默认（发布）构建里为 nil，没有任何能打开开发接口的参数。
var addDevFlags func(fs *flag.FlagSet, o *options)

// listFlag 是可重复的字符串参数。
type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func parseFlags(args []string, w io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(w)
	fs.StringVar(&o.imageDir, "image-dir", "guest/out", "abroad-imagebuild 的输出目录")
	fs.StringVar(&o.stateDir, "state-dir", "", "状态目录（必填；会话内验收用 scripts/dev/out/state）")
	fs.UintVar(&o.cpus, "cpus", 1, "虚拟 CPU 数（1–4）")
	fs.Uint64Var(&o.memory, "memory", 256, "内存 MiB（128–2048）")
	var allow listFlag
	fs.Var(&allow, "allow-outbound", "出站白名单，IPv4:端口，可重复（127.0.0.1 或局域网地址；设计 4.5）。在启动时确定，改动要重启 abroad-vm")
	var hops listFlag
	fs.Var(&hops, "first-hop", "路线第一跳，host:port[,port|lo-hi…]，可重复，最多 8 项（设计 4.5）。虚拟机只能连它和白名单；"+
		"两个都不给时什么公网都连不上")
	fs.BoolVar(&o.outboundOpen, "outbound-open", false, "开发用：公网一律放行（M1 的行为），与 --first-hop 互斥")
	var ddns listFlag
	addDirectFlags(fs, "direct-", &o.direct, &ddns)
	if addDevFlags != nil {
		addDevFlags(fs, &o)
	}
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	var derr error
	if o.direct.dns, derr = parseDirectDNS(ddns); derr != nil {
		return o, derr
	}
	a, err := usernet.ParseAllow(allow)
	if err != nil {
		return o, err
	}
	o.allow = a
	if o.outboundOpen && len(hops) > 0 {
		return o, errors.New("--outbound-open 和 --first-hop 不能同时使用")
	}
	if o.firstHops, err = usernet.ParseFirstHops(hops); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("多余的参数：%v", fs.Args())
	}
	if o.stateDir == "" {
		return o, errors.New("必须指定 --state-dir")
	}
	return o, nil
}

// run 的退出顺序：停虚拟机 → 关监听 → 删 socket → 删 pidfile → 放锁。
func run(args []string, sigs <-chan os.Signal, factory vmhost.Factory, w io.Writer) error {
	o, err := parseFlags(args, w)
	if err != nil {
		return err
	}
	var logMu sync.Mutex
	logf := func(format string, a ...any) { // 会被多个 goroutine 调用
		logMu.Lock()
		defer logMu.Unlock()
		fmt.Fprintf(w, "%s abroad-vm: %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, a...))
	}

	sd, err := vmhost.OpenStateDir(o.stateDir)
	if err != nil {
		return err
	}
	defer sd.Close()
	spec := vmhost.SpecFromImageDir(o.imageDir, sd.ConsoleLog())
	spec.CPUs, spec.MemoryMiB = o.cpus, o.memory
	if o.devAgent {
		spec.Cmdline += " " + guestabi.CmdlineDev + "=1"
		logf("警告：开发构建，内核命令行带 %s=1（agent 注册开发用的多出口接口）", guestabi.CmdlineDev)
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	if err := sd.WritePID(); err != nil {
		return err
	}
	defer os.Remove(sd.PIDFile())
	l, err := vmhost.ListenUnix(sd.CtlSock())
	if err != nil {
		return err
	}
	defer os.Remove(sd.CtlSock())
	dl, err := vmhost.ListenUnix(sd.DataSock())
	if err != nil {
		l.Close()
		return err
	}
	defer os.Remove(sd.DataSock())
	hl, err := vmhost.ListenUnix(sd.HostSock())
	if err != nil {
		l.Close()
		dl.Close()
		return err
	}
	defer os.Remove(sd.HostSock())

	if o.outboundOpen {
		logf("警告：--outbound-open（开发用）：虚拟机可以连任意公网地址，不按路线第一跳收紧")
	}
	un, err := newUserNet(o, logf)
	if err != nil {
		l.Close()
		dl.Close()
		hl.Close()
		return err
	}
	factory = un.factory(factory)
	// 直连拨号模块：和虚拟机共用同一个绑物理网卡的拨号器（同一个网卡缓存），但生命周期分开。
	var dd directdial.BoundDialer = un.dialer
	if directDialerOverride != nil {
		dd = directDialerOverride
	}
	dsrv, stopDirect, err := startDirect(o.direct, dd, logf)
	if err != nil {
		l.Close()
		dl.Close()
		hl.Close()
		return err
	}
	defer stopDirect()
	un.direct = dsrv
	hostSrv := &http.Server{Handler: un.hostHandler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second}
	go func() { _ = hostSrv.Serve(newPeerListener(hl)) }() // 只接受 abroad-vm 自己的用户和 root（M4-8）
	defer hostSrv.Close()
	statsStop := make(chan struct{})
	defer close(statsStop)
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-statsStop:
				return
			case <-t.C:
				un.logStats()
			}
		}
	}()
	defer un.logStats()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	force := make(chan struct{})
	go func() {
		s, ok := <-sigs
		if !ok {
			return
		}
		logf("收到 %v，停止虚拟机（再按一次立即强停）", s)
		cancel()
		if _, ok := <-sigs; ok {
			close(force)
		}
	}()

	sup := &vmhost.Supervisor{Factory: factory, Spec: spec, Log: logf, Force: force, Timing: supTiming}
	wake := make(chan struct{}, 1)
	sup.OnWake = func() { // 不得阻塞
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	// 每次开机（以及 boot_id 变化）health 首次通过时：对时（之后每 5 分钟、唤醒时立即）。
	// 主代理配置的重新下发在 M1-6 接到这里。
	sup.OnReady = func(ctx context.Context, bootID string) {
		logf("虚拟机已就绪（boot_id %s）", bootID)
		k := &vmhost.ClockKeeper{
			Dial: func(ctx context.Context) (net.Conn, error) { return sup.Dial(ctx, guestabi.CtlPort) },
			Wake: wake, Logf: logf,
		}
		k.Run(ctx)
	}
	bctx, bcancel := context.WithCancel(context.Background())
	bridge := func(l net.Listener, port uint32) chan error {
		done := make(chan error, 1)
		go func() {
			done <- vmhost.Bridge(bctx, l, func(ctx context.Context) (net.Conn, error) { return sup.Dial(ctx, port) })
		}()
		return done
	}
	ctlDone := bridge(l, guestabi.CtlPort)
	dataDone := bridge(dl, guestabi.DataPort)
	logf("状态目录 %s，控制台日志 %s，出站白名单 %d 项，出站模式 %s，第一跳 %d 项", sd.Dir, sd.ConsoleLog(), len(o.allow), un.mode, len(o.firstHops))
	runErr := sup.Run(ctx)
	bcancel()
	for name, ch := range map[string]chan error{"ctl.sock": ctlDone, "data.sock": dataDone} {
		if err := <-ch; err != nil && runErr == nil {
			runErr = fmt.Errorf("%s 桥接：%w", name, err)
		}
	}
	if runErr == nil {
		logf("已停止")
	}
	return runErr
}

// directDialerOverride 只给测试用：替换 run 里直连拨号模块的拨号器。
var directDialerOverride directdial.BoundDialer
