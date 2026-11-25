//go:build abroaddev && linux

package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"claude-abroad/internal/kmod"
)

// linuxProbeOps 是探查的真实系统操作（只在虚拟机里，agent 以 root 运行）。
type linuxProbeOps struct {
	load func(names []string) []kmod.Result
}

func newProbeOps(load func(names []string) []kmod.Result) probeOps { return linuxProbeOps{load: load} }

func (o linuxProbeOps) Run(ctx context.Context, argv ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var b bytes.Buffer
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	out := b.String()
	if len(out) > 4096 {
		out = out[:4096]
	}
	return out, err
}

func (o linuxProbeOps) Start(_ context.Context, argv ...string) (func(), error) {
	cmd := exec.Command(argv[0], argv[1:]...) // 不随请求的 ctx 结束：由 stop 结束
	cmd.Stdout, cmd.Stderr = nil, nil         // 日志不收（里面可能有证书主题等）
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = cmd.Process.Signal(unix.SIGTERM)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				_ = cmd.Process.Kill()
				<-done
			}
		})
	}, nil
}

// InNetns 在锁定的线程上切到命名空间执行 fn（fn 里创建的套接字属于那个命名空间），再切回来；
// 切不回来时不解锁，这个线程随 goroutine 结束被丢弃，不会把别的 goroutine 带进去。
func (o linuxProbeOps) InNetns(ns string, fn func() error) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		orig, err := unix.Open("/proc/thread-self/ns/net", unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			errc <- fmt.Errorf("打开当前的网络命名空间：%w", err)
			return
		}
		defer unix.Close(orig)
		target, err := unix.Open("/var/run/netns/"+ns, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			runtime.UnlockOSThread()
			errc <- fmt.Errorf("打开命名空间 %s：%w", ns, err)
			return
		}
		defer unix.Close(target)
		if err := unix.Setns(target, unix.CLONE_NEWNET); err != nil {
			runtime.UnlockOSThread()
			errc <- fmt.Errorf("进入命名空间 %s：%w", ns, err)
			return
		}
		ferr := fn()
		if unix.Setns(orig, unix.CLONE_NEWNET) == nil {
			runtime.UnlockOSThread()
		}
		errc <- ferr
	}()
	return <-errc
}

func (o linuxProbeOps) ServeEcho(ns string) (func(), error) {
	var stop func()
	err := o.InNetns(ns, func() error {
		var err error
		stop, err = serveEchoHere()
		return err
	})
	return stop, err
}

func (o linuxProbeOps) Probe(ns, kind, addr string) error {
	return o.InNetns(ns, func() error { return probeNet(kind, addr) })
}

func (o linuxProbeOps) LoadModules(names []string) []kmod.Result { return o.load(names) }

func (o linuxProbeOps) WriteFile(p string, data []byte) error {
	if err := os.MkdirAll(probeDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600)
}

func (o linuxProbeOps) RemoveAll(p string) error { return os.RemoveAll(p) }
