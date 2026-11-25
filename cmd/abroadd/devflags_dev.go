//go:build abroaddev

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
	"claude-abroad/internal/redact"
)

// 开发构建（go build -tags abroaddev）才有的参数和假系统模式（M6-1，m6-plan v2.1）。发布构建没有这个文件：
//   - --fake-system：整套系统（pf、路由、DNS、IPv6、utun、虚拟机）都是内存里的假实现，不碰本机；
//   - --fake-main-proxy（只和 --fake-system 一起）：接上假的出口检查和假虚拟机（应用的集成测试、第一级走查用）；
//   - --dev-skip-claude-kill：真实模式下恢复本机网络时不结束 Claude 进程（预览照常列出）；
//   - --dev-skip-region：真实模式下不同步时区和语言（M2、M3 的真机脚本带它）。
func init() {
	addDevFlags = func(fs *flag.FlagSet, o *devOptions) {
		fs.BoolVar(&o.fake, "fake-system", false, "开发构建：整套系统用内存里的假实现，不碰本机")
		fs.BoolVar(&o.fakeMain, "fake-main-proxy", false, "开发构建（只和 --fake-system 一起）：接上假的出口检查和假虚拟机，可以走完应用主代理、完成引导、启用")
		fs.BoolVar(&o.skipKill, "dev-skip-claude-kill", false, "开发构建：恢复本机网络时不结束 Claude 进程（预览照常列出）")
		fs.BoolVar(&o.skipRegion, "dev-skip-region", false, "开发构建：不同步时区和语言")
	}
	runDev = func(o devOptions, stateDir string, stderr io.Writer, ready func(sock string, stop func())) (int, bool) {
		if o.fakeMain && !o.fake {
			fmt.Fprintln(stderr, "abroadd：--fake-main-proxy 只能和 --fake-system 一起用")
			return 2, true
		}
		if !o.fake {
			return 0, false
		}
		return runFake(stateDir, o.fakeMain, stderr, ready), true
	}
}

func runFake(stateDir string, fakeMain bool, stderr io.Writer, ready func(sock string, stop func())) int {
	if stateDir == "" {
		fmt.Fprintln(stderr, "abroadd：需要 --state-dir")
		return 2
	}
	if os.Geteuid() == 0 {
		fmt.Fprintln(stderr, "abroadd：假系统模式不要以 root 运行")
		return 1
	}
	logger := log.New(&redact.Writer{W: stderr}, "abroadd ", log.LstdFlags)
	sys := daemon.NewFakeSystem().System()
	if fakeMain {
		sys = daemon.WithFakeMainProxy(sys, stateDir)
	}
	d, err := daemon.Start(context.Background(), daemon.Options{StateDir: stateDir, Sys: sys, Log: logger})
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
