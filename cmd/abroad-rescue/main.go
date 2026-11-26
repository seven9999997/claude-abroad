// Command abroad-rescue 是应急恢复程序（M6-5b，设计 10.3、m6-plan v2.7）：后台服务 abroadd 没在运行、或状态文件损坏时，
// 以 root 按固定顺序恢复本机网络：先结束 Claude（看过清单后确认），再把状态写成“已回国”，清空本项目的 pf 锚点、
// 删 DNS 设置和接管路由，按待还原记录恢复 IPv6、时区、语言。逻辑在 internal/rescue；这里只组装真实的系统操作。
//
//	sudo abroad-rescue [--yes] [--state-dir <目录>] [--run-dir <目录>]
//
// 不删解析锁和 Claude Code 的托管设置；不关闭 pf；不停止 abroadd；不调用系统服务管理命令；不联网、不监听；没有开发参数。
// 退出码：0 完成；1 某一步失败；2 参数不对；3 前提不满足（不是 root、abroadd 还在、没有确认），什么都没改。
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"claude-abroad/internal/claudeproc"
	"claude-abroad/internal/guard"
	"claude-abroad/internal/pf"
	"claude-abroad/internal/platform"
	"claude-abroad/internal/region"
	"claude-abroad/internal/rescue"
	"claude-abroad/internal/state"
	"claude-abroad/internal/sysnet"
)

// 默认位置和开发安装（scripts/m6/install）一致。
const (
	DefaultStateDir = "/Library/Application Support/claude-abroad/state/abroadd"
	DefaultRunDir   = "/var/run/claude-abroad"
	socketName      = "api.sock"
)

type options struct {
	stateDir, runDir string
	yes              bool
}

func parse(args []string, stderr io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("abroad-rescue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.stateDir, "state-dir", DefaultStateDir, "abroadd 的状态目录")
	fs.StringVar(&o.runDir, "run-dir", DefaultRunDir, "运行时目录（本地接口、开机 guard 的启用引用）")
	fs.BoolVar(&o.yes, "yes", false, "不问确认（要结束的 Claude 进程清单照样打印）")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() != 0 {
		return o, fmt.Errorf("多余的参数：%s", strings.Join(fs.Args(), " "))
	}
	for _, p := range []string{o.stateDir, o.runDir} {
		if !filepath.IsAbs(p) {
			return o, fmt.Errorf("目录要写绝对路径：%s", p)
		}
	}
	return o, nil
}

func main() {
	if msg, code := platform.MacOnlyEntry("abroad-rescue", runtime.GOOS); code != 0 {
		fmt.Fprintln(os.Stderr, msg)
		os.Exit(code)
	}
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	o, err := parse(args, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "用法：sudo abroad-rescue [--yes] [--state-dir <目录>] [--run-dir <目录>]")
		return rescue.ExitUsage
	}
	d := rescue.Deps{
		IsRoot: func() bool { return os.Geteuid() == 0 },
		Yes:    o.yes,
		Out:    stdout,
		Confirm: func(claudeproc.Preview) bool {
			fmt.Fprint(stdout, "确认结束上面这些 Claude 进程并恢复本机网络？输入 rescue 继续：")
			line, _ := bufio.NewReader(stdin).ReadString('\n')
			return strings.TrimSpace(line) == "rescue"
		},
	}
	if os.Geteuid() == 0 {
		if err := wireReal(&d, o, stderr); err != nil {
			fmt.Fprintln(stderr, err)
			return rescue.ExitFailed
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return rescue.Run(ctx, d)
}

// wireReal 组装真实的系统操作（只在 root 下）。真实的 pf 执行器、系统命令执行器和信号发送器在这里取得（源码检查的例外，同 guard）。
func wireReal(d *rescue.Deps, o options, stderr io.Writer) error {
	sig, err := claudeproc.NewRealSignaler()
	if err != nil {
		return err
	}
	logf := func(format string, a ...any) { fmt.Fprintf(stderr, format+"\n", a...) }
	run := sysnet.ExecRunner{}
	st := state.NewStore(o.stateDir)
	d.Store = st
	d.Claude = claudeproc.NewSystemService(sig, logf)
	d.PF = &pf.Controller{R: pf.ExecRunner{}} // 默认锚点 com.apple/250.abroad；只用 Flush、Load（回国拦截那一组）和 Release
	d.DNS = sysnet.MacDNS{R: run, Server: state.TakeoverDNS}
	d.Routes = sysnet.MacRoutes{R: run, Iface: func() string { // 我们的 utun：状态文件里记着（abroadd 不在时通常已经消失）
		if f, err := st.Load(); err == nil {
			return f.Utun
		}
		return ""
	}}
	d.IPv6 = sysnet.MacIPv6{R: run}
	d.Locale = region.Mac{Run: run, Console: region.ConsoleUser}
	d.GuardTokens = func() ([]string, error) { return guard.TakeTokens(o.runDir) }
	d.DaemonAlive = func(context.Context) bool { return socketAnswers(filepath.Join(o.runDir, socketName)) }
	return nil
}
