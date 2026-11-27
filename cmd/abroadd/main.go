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
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"

	"claude-abroad/internal/buildinfo"
	"claude-abroad/internal/daemon"
	"claude-abroad/internal/platform"
)

func main() { os.Exit(run(os.Args[1:], os.Stderr, nil)) }

// stdout 是 --version 的输出（测试替换）。
var stdout io.Writer = os.Stdout

// run 是可测试的主流程；ready 非 nil 时在启动成功后收到 socket 路径，stop 由信号或测试关闭。
func run(args []string, stderr io.Writer, ready func(sock string, stop func())) int {
	fs := flag.NewFlagSet("abroadd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	stateDir := fs.String("state-dir", "", "状态目录（0700）")
	vmDir := fs.String("vm-dir", "", "abroad-vm 的状态目录（ctl.sock、host.sock、data.sock；M2 阶段虚拟机由外部启动）")
	pfUser := fs.String("pf-user", daemon.DefaultAllowUser, "pf 放行的用户（abroad-vm 以它运行）")
	mainStore := fs.String("main-proxy-store", "", "主代理存储（managedproxy 的 profile 目录）")
	exitHost := fs.String("exit-check-host", "", "出口检查里 HTTPS 探测的主机（经主代理 GET https://<主机>/generate_204，要 204）")
	vmExec := fs.String("vm-exec", "", "abroad-vm 的路径：给了就由 abroadd 拉起和管理虚拟机（健康检查、崩溃后退避重启、退出时关闭；M6-2a）；不给时虚拟机由外部启动")
	vmUser := fs.String("vm-user", "", "和 --vm-exec 一起：以这个用户运行 abroad-vm（要 root）；不给时和 abroadd 同一个用户")
	var vmArgs vmArgList
	fs.Var(&vmArgs, "vm-arg", "和 --vm-exec 一起：传给 abroad-vm run 的其他参数（如 --image-dir=…），可重复；--state-dir 固定用 --vm-dir")
	manifest := fs.String("integrity-manifest", "", "完整性清单（可选；M6-7）：校验不通过时只保留断网规则、不接管、不能启用")
	manifestBase := fs.String("integrity-base", "", "完整性清单里相对路径的根（和 --integrity-manifest 一起）")
	launchControl := fs.Bool("launch-control", false, "打开启动管控（M6-4b-1 返工）：首次准备写入 Claude 域名解析锁和 Claude Code 的托管设置、接上第三层（发现即结束）、监听本机代理端口。只有开发安装和正式安装带它；不带时真实模式和 M6 之前完全相同")
	rbKill := fs.Bool("return-block-kill-states", false, "回国拦截被重新加载时，只清掉目的地在 Anthropic 地址段里的连接状态（M6-11b；pfctl 在 macOS 上的效果待真机验证，验证之前默认关）")
	sockDir := fs.String("socket-dir", "", "本地接口 socket 的目录（目录属主 root、组和其他人不能写；socket 0666，级别按对端 uid 判）；空时放在状态目录里、只认属主")
	version := fs.Bool("version", false, "打印版本号（提交号、构建时间、状态格式版本号；JSON，和 payload 里的 build.json 相同）后退出")
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
	if *version { // 原地升级（v0.71）：安装脚本核对它和 build.json 一致
		b, _ := json.Marshal(buildinfo.GetVersion())
		fmt.Fprintln(stdout, string(b))
		return 0
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
		exitHost: *exitHost, skipKill: dev.skipKill, skipRegion: dev.skipRegion, socketDir: *sockDir, resolverTest: dev.resolverTest,
		vmExec: *vmExec, vmUser: *vmUser, vmArgs: vmArgs, manifest: *manifest, manifestBase: *manifestBase,
		launchControl: *launchControl, fakeClaudeDir: dev.fakeClaudeDir, rbKillStates: *rbKill}, stderr, ready)
}

// devOptions 是开发构建的参数（默认构建里全是零值）。
type devOptions struct {
	fake, fakeMain, skipKill, skipRegion bool
	resolverTest                         bool   // 解析锁只管测试域名（M6-3）
	fakeClaudeDir                        string // 回国结束和第三层只针对这个目录下的进程（M6-4b-1 返工）
}

// addDevFlags、runDev 由带 abroaddev 构建标签的 devflags_dev.go 设置；默认（发布）构建里为 nil。
// runDev 处理假系统模式和开发参数的组合检查：handled 为真时 run 直接返回 code。
var (
	addDevFlags func(fs *flag.FlagSet, o *devOptions)
	runDev      func(o devOptions, stateDir string, stderr io.Writer, ready func(sock string, stop func())) (code int, handled bool)
)
