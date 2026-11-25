// abroadd 是 Claude 出国的后台服务。
//
//	abroadd --vm-dir <目录> --main-proxy-store <目录> --exit-check-host <主机> [--state-dir …] [--pf-user …] [--socket-dir …]
//
// 真实模式（M2-9b-2）：先取生产执行器，非 root 在这里就退出，不创建任何目录；还要 --vm-dir、--main-proxy-store、
// --exit-check-host（见 checkRealFlags）。恢复本机网络前会强制结束 Claude 进程（design 10.1）。
//
// 开发构建（go build -tags abroaddev）另有假系统模式和开发参数（devflags_dev.go；M6-1，m6-plan v2.1）。默认构建就是
// 发布形态：没有假系统、没有任何开发参数，传入时按“不认识的参数”退出。
//
// 收到 SIGTERM、SIGINT 时走退出流程：不恢复网络、不清 pf，状态文件保持原样。
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"

	"claude-abroad/internal/daemon"
	"claude-abroad/internal/platform"
)

func main() { os.Exit(run(os.Args[1:], os.Stderr, nil)) }

// run 是可测试的主流程；ready 非 nil 时在启动成功后收到 socket 路径，stop 由信号或测试关闭。
func run(args []string, stderr io.Writer, ready func(sock string, stop func())) int {
	fs := flag.NewFlagSet("abroadd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	stateDir := fs.String("state-dir", "", "状态目录（0700）")
	vmDir := fs.String("vm-dir", "", "abroad-vm 的状态目录（ctl.sock、host.sock、data.sock；M2 阶段虚拟机由外部启动）")
	pfUser := fs.String("pf-user", daemon.DefaultAllowUser, "pf 放行的用户（abroad-vm 以它运行）")
	mainStore := fs.String("main-proxy-store", "", "主代理存储（managedproxy 的 profile 目录）")
	exitHost := fs.String("exit-check-host", "", "出口检查里 HTTPS 探测的主机（经主代理 GET https://<主机>/generate_204，要 204）")
	sockDir := fs.String("socket-dir", "", "本地接口 socket 的目录（目录属主 root、组和其他人不能写；socket 0666，级别按对端 uid 判）；空时放在状态目录里、只认属主")
	var dev devOptions
	if addDevFlags != nil { // 只有开发构建有（devflags_dev.go）
		addDevFlags(fs, &dev)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "多余的参数")
		return 2
	}
	if runDev != nil {
		if code, handled := runDev(dev, *stateDir, stderr, ready); handled {
			return code
		}
	}
	if msg, code := platform.RealModeEntry(runtime.GOOS); code != 0 { // W-1：Windows 上真实模式尚未支持
		fmt.Fprintln(stderr, msg)
		return code
	}
	return runReal(realFlags{stateDir: *stateDir, vmDir: *vmDir, pfUser: *pfUser, mainStore: *mainStore,
		exitHost: *exitHost, skipKill: dev.skipKill, skipRegion: dev.skipRegion, socketDir: *sockDir}, stderr, ready)
}

// devOptions 是开发构建的参数（默认构建里全是零值）。
type devOptions struct {
	fake, fakeMain, skipKill, skipRegion bool
}

// addDevFlags、runDev 由带 abroaddev 构建标签的 devflags_dev.go 设置；默认（发布）构建里为 nil。
// runDev 处理假系统模式和开发参数的组合检查：handled 为真时 run 直接返回 code。
var (
	addDevFlags func(fs *flag.FlagSet, o *devOptions)
	runDev      func(o devOptions, stateDir string, stderr io.Writer, ready func(sock string, stop func())) (code int, handled bool)
)
