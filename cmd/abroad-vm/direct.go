package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"sync"
	"time"

	"claude-abroad/internal/directdial"
	"claude-abroad/internal/physif"
	"claude-abroad/internal/usernet"
	"claude-abroad/internal/vmhost"
)

// 直连拨号模块（design 6.3、m3-plan M3-4）：abroad-vm run 带 --direct-sock 时在同一进程里提供，
// 生命周期和虚拟机分开（虚拟机停了它照常服务）；也能用 abroad-vm direct 单独运行
// （R1 不通过、虚拟机宿主改用 root 时，直连拨号要由另一个以专用用户运行的进程来做）。
//
//	abroad-vm direct --sock <路径> --dns <本机 DNS IPv4>… [--peer-uid <uid>]

type directOpts struct {
	sock    string
	dns     []netip.Addr
	peerUID int // 允许的对端 uid；-1 表示与本进程同一用户（开发模式）
}

// addDirectFlags 把直连拨号的参数加到 fs 上（prefix 是 run 子命令里的 "direct-"，direct 子命令里为空）。
func addDirectFlags(fs *flag.FlagSet, prefix string, o *directOpts, dns *listFlag) {
	fs.StringVar(&o.sock, prefix+"sock", "", "直连拨号模块的 unix socket 路径（0600）")
	fs.Var(dns, prefix+"dns", "启用前记下的本机 DNS 服务器（IPv4，可重复），直连解析域名用")
	fs.IntVar(&o.peerUID, prefix+"peer-uid", -1, "允许连上来的对端 uid（生产是 0，即 abroadd；-1 表示与本进程同一用户）")
}

func parseDirectDNS(list listFlag) ([]netip.Addr, error) {
	var out []netip.Addr
	for _, s := range list {
		a, err := netip.ParseAddr(s)
		if err != nil || !a.Is4() {
			return nil, fmt.Errorf("--dns %q 不是 IPv4 地址", s)
		}
		out = append(out, a)
	}
	return out, nil
}

// newDirectDialer 是直连拨号的出口（测试里替换）：绑物理网卡的 usernet.Dialer.DialDirect。
var newDirectDialer = func() directdial.BoundDialer {
	w := physif.NewWatcher(physif.Query, InterfaceFresh)
	return &usernet.Dialer{Interface: w.Current, OnBindFailed: w.Invalidate}
}

// startDirect 监听 socket 并在后台提供服务。返回的 stop 关闭服务、等它退出并删除 socket。
func startDirect(o directOpts, dialer directdial.BoundDialer, logf func(string, ...any)) (*directdial.Server, func(), error) {
	if o.sock == "" {
		return nil, func() {}, nil
	}
	ln, err := vmhost.ListenUnix(o.sock)
	if err != nil {
		return nil, nil, err
	}
	want := uint32(os.Getuid())
	if o.peerUID >= 0 {
		want = uint32(o.peerUID)
	}
	srv := &directdial.Server{
		Dialer:   dialer,
		AllowUID: func(u uint32) bool { return u == want },
	}
	// 只有带 --dns 时才是“单独运行”（没收到下发的配置时按代数 0 处理）；生产形态等 abroadd 按代数下发（m3-plan v2.9）。
	if dns := o.dns; len(dns) > 0 {
		srv.DNSServers = func() []netip.Addr { return dns }
	}
	pl, ok := ln.(*vmhost.PrivateListener)
	if !ok {
		ln.Close()
		return nil, nil, errors.New("直连拨号模块的监听不是 unix socket")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ctx, pl.UnixListener)
	}()
	logf("直连拨号模块已启动（socket %s，本机 DNS %d 个，允许的对端 uid %d）", o.sock, len(o.dns), want)
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-done
			_ = os.Remove(o.sock)
			logf("直连拨号模块已停止，计数 %v", srv.Stats())
		})
	}
	return srv, stop, nil
}

// runDirect 是 abroad-vm direct：只运行直连拨号模块，直到收到信号。
func runDirect(args []string, sigs <-chan os.Signal, w io.Writer) error {
	var o directOpts
	var dns listFlag
	fs := flag.NewFlagSet("direct", flag.ContinueOnError)
	fs.SetOutput(w)
	addDirectFlags(fs, "", &o, &dns)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("多余的参数：%v", fs.Args())
	}
	if o.sock == "" {
		return errors.New("必须指定 --sock")
	}
	var err error
	if o.dns, err = parseDirectDNS(dns); err != nil {
		return err
	}
	var mu sync.Mutex
	logf := func(format string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, "%s abroad-vm direct: %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, a...))
	}
	_, stop, err := startDirect(o, newDirectDialer(), logf)
	if err != nil {
		return err
	}
	defer stop()
	s := <-sigs
	logf("收到 %v，停止", s)
	return nil
}
