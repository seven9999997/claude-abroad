// abroad-guard 在开机最早阶段按状态文件加载断网规则（M6-2b，design 10.2、11.4），加载后退出。
//
//	abroad-guard --state-dir <abroadd 的状态目录> --run-dir <运行时目录>
//
// 状态是回国（或没有状态文件）时什么都不做；其余状态和状态文件损坏时，把“只断网、不接管”的规则加载到本项目
// 自己的 pf 锚点并取一个启用引用；abroadd 本次开机已经加载过时不动。不联网、不监听、不常驻、不读机密。
// 没有任何开发参数；两个目录都必须由参数给出。
//
// 退出码：0 已加载；3 不需要加载；1 加载失败；2 参数不对；另有“本平台不支持”“需要 root”。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"claude-abroad/internal/guard"
	"claude-abroad/internal/pf"
	"claude-abroad/internal/platform"
)

// ExitNotRoot 是不是 root 时的退出码。
const ExitNotRoot = 4

func main() { os.Exit(run(os.Args[1:], os.Stderr, platform.IsAdmin(), runtime.GOOS)) }

type options struct{ stateDir, runDir string }

func parse(args []string, w io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("abroad-guard", flag.ContinueOnError)
	fs.SetOutput(w)
	fs.StringVar(&o.stateDir, "state-dir", "", "abroadd 的状态目录（必填）")
	fs.StringVar(&o.runDir, "run-dir", "", "运行时目录（必填；abroadd 的“本次开机已加载”标记和 guard 的启用引用放在这里）")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	switch {
	case fs.NArg() != 0:
		return o, errors.New("多余的参数")
	case o.stateDir == "" || o.runDir == "":
		return o, errors.New("--state-dir 和 --run-dir 都必须给")
	case !filepath.IsAbs(o.stateDir) || !filepath.IsAbs(o.runDir):
		return o, errors.New("目录要写绝对路径")
	}
	return o, nil
}

// run：先判平台和参数，再判 root（非 root 在做任何事之前退出），最后才接真实的 pfctl 执行器。
func run(args []string, stderr io.Writer, root bool, goos string) int {
	if msg, code := platform.MacOnlyEntry("abroad-guard", goos); code != 0 {
		fmt.Fprintln(stderr, msg)
		return code
	}
	o, err := parse(args, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "abroad-guard：", err)
		return 2
	}
	if !root {
		fmt.Fprintln(stderr, "abroad-guard：需要以 root 运行（开机时由系统服务启动）")
		return ExitNotRoot
	}
	return runReal(o, stderr)
}

func runReal(o options, stderr io.Writer) int {
	lg := log.New(stderr, "abroad-guard: ", log.LstdFlags)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := guard.Run(ctx, guard.Deps{
		StateDir: o.stateDir, RunDir: o.runDir,
		PF:     &pf.Controller{R: pf.ExecRunner{}}, // 真实的 pfctl 执行器：只在这里接上（源码检查）
		BootID: guard.BootID,
		Logf:   lg.Printf,
	})
	switch out {
	case guard.Loaded:
		lg.Printf("结果：已加载断网规则")
	case guard.NotNeeded:
		lg.Printf("结果：不需要加载")
	default:
		lg.Printf("结果：加载失败：%v", err)
	}
	return out.Code()
}
