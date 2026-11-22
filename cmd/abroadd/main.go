// abroadd 是 Claude 出国的后台服务（M2-9a：只有假系统模式）。
//
//	abroadd --state-dir <目录> --fake-system
//
// --fake-system：整套系统（pf、路由、DNS、IPv6、utun、虚拟机）都是内存里的假实现，不碰本机。
// 不带 --fake-system 是真实模式（M2-9b-2）：先取生产执行器，非 root 在这里就退出，不创建任何目录；
// 还要 --vm-dir、--main-proxy-store、--exit-check-host（见 checkRealFlags）。恢复本机网络前会强制结束 Claude 进程
// （design 10.1）；开发时可加 --dev-skip-claude-kill 换成不结束任何进程的实现（发布构建不能带）。
//
// 开发时可加 --dev-skip-region：不同步时区和语言（M2、M3 的真机脚本带它）。
//
//	abroadd --vm-dir <目录> --main-proxy-store <目录> --exit-check-host <主机> [--dev-skip-claude-kill] [--dev-skip-region] [--state-dir …] [--pf-user …]
//
// 收到 SIGTERM、SIGINT 时走退出流程：不恢复网络、不清 pf，状态文件保持原样。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"claude-abroad/internal/daemon"
	"claude-abroad/internal/platform"
	"claude-abroad/internal/redact"
	"runtime"
)

func main() { os.Exit(run(os.Args[1:], os.Stderr, nil)) }

// run 是可测试的主流程；ready 非 nil 时在启动成功后收到 socket 路径，stop 由信号或测试关闭。
func run(args []string, stderr io.Writer, ready func(sock string, stop func())) int {
	fs := flag.NewFlagSet("abroadd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	stateDir := fs.String("state-dir", "", "状态目录（0700；开发模式用当前用户）")
	fake := fs.Bool("fake-system", false, "开发用：整套系统用内存里的假实现，不碰本机")
	vmDir := fs.String("vm-dir", "", "真实模式：abroad-vm 的状态目录（ctl.sock、host.sock、data.sock；M2 阶段虚拟机由外部启动）")
	pfUser := fs.String("pf-user", daemon.DefaultAllowUser, "真实模式：pf 放行的用户（abroad-vm 以它运行）")
	mainStore := fs.String("main-proxy-store", "", "真实模式：主代理存储（managedproxy 的 profile 目录）")
	exitHost := fs.String("exit-check-host", "", "真实模式：出口检查里 HTTPS 探测的主机（经主代理 GET https://<主机>/generate_204，要 204）")
	skipKill := fs.Bool("dev-skip-claude-kill", false, "开发用：恢复本机网络时不结束 Claude 进程（预览照常列出；发布构建不能带）")
	skipRegion := fs.Bool("dev-skip-region", false, "开发用：不同步时区和语言（发布构建不能带）")
	sockDir := fs.String("socket-dir", "", "真实模式：本地接口 socket 的目录（目录属主 root、组和其他人不能写；socket 0666，级别按对端 uid 判）；空时放在状态目录里、只认属主")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "多余的参数")
		return 2
	}
	if !*fake {
		if msg, code := platform.RealModeEntry(runtime.GOOS); code != 0 { // W-1：Windows 上真实模式尚未支持
			fmt.Fprintln(stderr, msg)
			return code
		}
		return runReal(realFlags{stateDir: *stateDir, vmDir: *vmDir, pfUser: *pfUser, mainStore: *mainStore,
			exitHost: *exitHost, skipKill: *skipKill, skipRegion: *skipRegion, socketDir: *sockDir}, stderr, ready)
	}
	if *stateDir == "" {
		fmt.Fprintln(stderr, "abroadd：需要 --state-dir")
		return 2
	}
	if os.Geteuid() == 0 {
		fmt.Fprintln(stderr, "abroadd：假系统模式不要以 root 运行")
		return 1
	}
	logger := log.New(&redact.Writer{W: stderr}, "abroadd ", log.LstdFlags)
	sys := daemon.NewFakeSystem()
	d, err := daemon.Start(context.Background(), daemon.Options{StateDir: *stateDir, Sys: sys.System(), Log: logger})
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
	logger.Printf("收到 %v，退出", s)
	if err := d.Shutdown(); err != nil {
		logger.Printf("等正在进行的操作结束：%v", err)
	}
	return 0
}
