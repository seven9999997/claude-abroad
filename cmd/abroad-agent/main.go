// abroad-agent 是虚拟机里的代理程序（设计文档 7.1），linux/arm64 静态链接：
//
//	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o guest/out/abroad-agent ./cmd/abroad-agent
//
// 同一个程序有三种身份：
//   - initramfs 里的 /init（PID 1，argv[0] 是 /init 或子命令 boot）：挂盘、组装根目录、switch_root；
//   - 根盘上的 PID 1（子命令 init）：启动 serve、收割子进程、转发停机信号；
//   - serve（普通进程）：模块、网卡、控制面，M1-4 起管理引擎。
//
// 作为 PID 1 时，任何返回路径（出错、panic、用法错误、serve 退出）最后都是 sync + 关机：
// PID 1 退出会让内核 panic。这条路径上不许出现 os.Exit、log.Fatal（有测试扫描源码）。
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"

	"claude-abroad/internal/agent"
)

func main() {
	code := dispatch(agent.NewSystem(), os.Args, os.Getpid() == 1, os.Stderr, "/")
	if code != 0 {
		exitProcess(code) // 只有不是 PID 1 时才会走到这里
	}
}

const usage = `用法（只在虚拟机里使用）：
  abroad-agent serve     由 PID 1 启动：控制面、模块、网卡
  abroad-agent version
作为 PID 1 时：/init（或 boot）组装根目录；init 管理 serve`

func subcommand(args []string) string {
	if len(args) > 1 {
		return args[1]
	}
	return ""
}

// dispatch 按身份分派。pid1 为 true 时一定以关机结束（真实系统上 PowerOff 不返回），返回值没有意义。
// root 在虚拟机里是 "/"，测试里是临时目录。
func dispatch(sys agent.System, args []string, pid1 bool, w io.Writer, root string) int {
	if pid1 {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(w, "abroad-agent: 致命错误：%v\n%s", r, debug.Stack())
			}
			fmt.Fprintln(w, "abroad-agent: 关机")
			agent.PowerOff(sys)
		}()
		if err := runPID1(sys, args, w, root); err != nil {
			fmt.Fprintf(w, "abroad-agent: %v\n", err)
		}
		return 0
	}
	switch subcommand(args) {
	case "serve":
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		if err := agent.Serve(ctx, &agent.Env{Sys: sys, Root: root, Log: w}); err != nil {
			fmt.Fprintf(w, "abroad-agent serve: %v\n", err)
			return 1
		}
		return 0
	case "socks": // 分流出口命名空间里的 socks 服务（M4-7；由 agent 用 ip netns exec 启动）
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		return agent.RunSocksCmd(ctx, args[2:], w)
	case "version":
		fmt.Fprintln(w, "abroad-agent", agent.Version)
		return 0
	case "boot", "init":
		fmt.Fprintln(w, "abroad-agent: boot、init 只能作为 PID 1 运行")
		return 2
	}
	fmt.Fprintln(w, usage)
	return 2
}

func runPID1(sys agent.System, args []string, w io.Writer, root string) error {
	env := &agent.Env{Sys: sys, Root: root, Log: w}
	cmd := subcommand(args)
	switch {
	case cmd == "init":
		sigs := make(chan os.Signal, 4)
		signal.Notify(sigs, agent.ShutdownSignals...)
		defer signal.Stop(sigs)
		return agent.Init(env, sigs)
	case cmd == "boot" || len(args) > 0 && filepath.Base(args[0]) == "init":
		return agent.Boot(env)
	}
	fmt.Fprintln(w, usage)
	return errors.New("作为 PID 1 只能运行 boot 或 init")
}
