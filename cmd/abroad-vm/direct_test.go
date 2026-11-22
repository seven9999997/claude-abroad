//go:build unix

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"claude-abroad/internal/directdial"
	"claude-abroad/internal/vmhost"
)

// loopDialer 是测试用的“绑物理网卡”拨号器：任何目标都连到本机回环上的回显服务，记下请求的目标。
type loopDialer struct {
	addr  string
	mu    sync.Mutex
	dials []string
}

func newLoopDialer(t *testing.T) *loopDialer {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return &loopDialer{addr: ln.Addr().String()}
}

func (d *loopDialer) DialDirect(ctx context.Context, network string, dst netip.AddrPort) (net.Conn, error) {
	d.mu.Lock()
	d.dials = append(d.dials, network+" "+dst.String())
	d.mu.Unlock()
	var nd net.Dialer
	return nd.DialContext(ctx, "tcp", d.addr)
}

func echoVia(t *testing.T, sock string) error {
	t.Helper()
	cli := &directdial.Client{Path: sock, ServerUID: uint32(os.Getuid())}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := cli.Dial(ctx, "tcp", "", netip.MustParseAddr("93.184.216.34"), 443)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte("abc")); err != nil {
		return fmt.Errorf("经直连拨回来的连接写不了：%w", err)
	}
	b := make([]byte, 3)
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "abc" {
		return fmt.Errorf("经直连拨回来的连接读不到回显：%q %w（本端 %s，对端 %s）", b, err, c.LocalAddr(), c.RemoteAddr())
	}
	return nil
}

func waitSock(t *testing.T, p string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if fi, err := os.Stat(p); err == nil {
			if fi.Mode().Perm() != 0o600 || fi.Mode()&os.ModeSocket == 0 {
				t.Fatalf("%s 应是 0600 的 socket：%v", p, fi.Mode())
			}
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
				t.Fatalf("%s 的属主应是当前用户", p)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s 没有出现", p)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// g. run 带 --direct-sock：0600、属主是本用户；虚拟机停了（起不来）直连照常服务；退出后 socket 删除。
func TestRunDirectSockSurvivesVM(t *testing.T) {
	old := supTiming
	supTiming = vmhost.DefaultTiming
	supTiming.Watchdog = time.Hour
	supTiming.ShutdownTimeout = time.Second
	supTiming.StopWait = time.Second
	// 用 t.Cleanup 而不是 defer：后注册的“停掉 run”先执行，全局变量等 run 退出后才恢复（否则用例失败退出时
	// 会和还在跑的 run 竞争，architect 判 W-2a 时补跑 -race 发现）。
	t.Cleanup(func() { supTiming = old })
	ld := newLoopDialer(t)
	directDialerOverride = ld
	t.Cleanup(func() { directDialerOverride = nil })

	base, err := os.MkdirTemp("", "avm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	img := filepath.Join(base, "img")
	_ = os.Mkdir(img, 0o755)
	for _, n := range []string{"Image", "initramfs.gz", "rootfs.img", "modloop"} {
		_ = os.WriteFile(filepath.Join(img, n), []byte("x"), 0o644)
	}
	state := filepath.Join(base, "st")
	sock := filepath.Join(base, "d.sock")
	m := &portMachine{done: make(chan struct{}), ports: make(chan uint32, 16)}
	var made atomic.Int32
	factory := func(vmhost.Spec) (vmhost.Machine, error) {
		if made.Add(1) == 1 {
			return m, nil
		}
		return nil, errors.New("虚拟机起不来（测试）") // 停了之后一直起不来
	}
	sigs := make(chan os.Signal, 2)
	done := make(chan error, 1)
	go func() {
		var w bytes.Buffer
		done <- run([]string{"--image-dir", img, "--state-dir", state, "--direct-sock", sock, "--direct-dns", "192.0.2.53"},
			sigs, factory, &w)
	}()
	var stopOnce sync.Once
	stop := func() bool { // 发两次 SIGTERM，等 run 退出；只做一次
		ok := false
		stopOnce.Do(func() {
			sigs <- syscall.SIGTERM
			sigs <- syscall.SIGTERM
			select {
			case <-done:
				ok = true
			case <-time.After(10 * time.Second):
			}
		})
		return ok
	}
	t.Cleanup(func() { stop() })
	waitSock(t, sock)
	if err := echoVia(t, sock); err != nil {
		t.Fatalf("虚拟机运行时：%v", err)
	}
	close(m.done) // 虚拟机停了
	deadline := time.Now().Add(3 * time.Second)
	for made.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if made.Load() < 2 {
		t.Fatal("监护应在虚拟机停止后尝试重开")
	}
	if err := echoVia(t, sock); err != nil {
		t.Fatalf("虚拟机停了以后直连拨号模块应照常服务：%v", err)
	}
	if !stop() {
		t.Fatal("没有退出")
	}
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Fatal("退出后直连 socket 应被删除")
	}
	ld.mu.Lock()
	defer ld.mu.Unlock()
	if len(ld.dials) != 2 || ld.dials[0] != "tcp 93.184.216.34:443" {
		t.Fatalf("拨号记录：%v", ld.dials)
	}
}

// g. abroad-vm direct 单独运行同一个服务。
func TestRunDirectStandalone(t *testing.T) {
	ld := newLoopDialer(t)
	old := newDirectDialer
	newDirectDialer = func() directdial.BoundDialer { return ld }
	defer func() { newDirectDialer = old }()
	base, err := os.MkdirTemp("", "avd")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(base)
	sock := filepath.Join(base, "d.sock")
	sigs := make(chan os.Signal, 1)
	done := make(chan error, 1)
	var w bytes.Buffer
	go func() { done <- runDirect([]string{"--sock", sock, "--dns", "192.0.2.53"}, sigs, &w) }()
	waitSock(t, sock)
	if err := echoVia(t, sock); err != nil {
		t.Fatal(err)
	}
	sigs <- syscall.SIGTERM
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Fatal("退出后 socket 应被删除")
	}
	for _, bad := range [][]string{{}, {"--sock", sock, "--dns", "::1"}, {"--sock", sock, "extra"}} {
		if err := runDirect(bad, sigs, &w); err == nil {
			t.Errorf("%v 应报错", bad)
		}
	}
}

func TestParseDirectFlags(t *testing.T) {
	o, err := parseFlags([]string{"--state-dir", "x", "--direct-sock", "/tmp/d.sock", "--direct-dns", "192.168.1.1", "--direct-dns", "8.8.8.8", "--direct-peer-uid", "0"}, io.Discard)
	if err != nil || o.direct.sock != "/tmp/d.sock" || len(o.direct.dns) != 2 || o.direct.peerUID != 0 {
		t.Fatalf("%+v %v", o.direct, err)
	}
	if _, err := parseFlags([]string{"--state-dir", "x", "--direct-dns", "nope"}, io.Discard); err == nil {
		t.Fatal("不合法的 DNS 应报错")
	}
}
