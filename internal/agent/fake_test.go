package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const testKver = "6.12.115-0-virt"

// fakeSys 记录调用序列；挂 squashfs 时按 disks 在目标目录里造出对应的内容。
type fakeSys struct {
	t    *testing.T
	root string
	kver string

	mu        sync.Mutex
	calls     []string
	disks     map[string]string // 盘名 → "root" | "modloop" | "other"
	mountFail map[string]int    // 盘名 → 先失败几次
	loadFail  map[string]error  // 模块内容（去掉 ELF 头）→ 错误
	loaded    []string

	spawnErr   error
	waits      chan WaitResult
	waitErr    error
	killed     chan int
	listener   net.Listener     // 控制面（1081）的监听器
	listenErr  map[uint32]error // 按端口注入监听失败
	extra      []net.Listener   // 其他端口（数据面 1080）自动建的回环监听器
	netErr     error
	netCfg     *NetConfig
	poweroffs  int
	syncs      int
	runner     *fakeRunner
	setTimes   []time.Time
	setTimeErr error
}

func newFakeSys(t *testing.T) *fakeSys {
	return &fakeSys{t: t, root: t.TempDir(), kver: testKver, disks: map[string]string{},
		mountFail: map[string]int{}, loadFail: map[string]error{}, waits: make(chan WaitResult, 8), killed: make(chan int, 4)}
}

// env 返回测试用的环境：进程启动器和端口探测都是假的，测试里不会真的起任何进程。
func (f *fakeSys) env() *Env {
	if f.runner == nil {
		f.runner = newFakeRunner()
		f.runner.ready.Store(true)
	}
	r := f.runner
	return &Env{Sys: f, Root: f.root, Log: &syncBuffer{}, Sleep: func(time.Duration) {}, Runner: r,
		DialReady: func(context.Context, string) error {
			if r.ready.Load() {
				return nil
			}
			return errors.New("not ready")
		}}
}

// syncBuffer 是并发安全的 bytes.Buffer（引擎的 goroutine 也会写日志）。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (f *fakeSys) record(format string, a ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf(format, a...))
}

func (f *fakeSys) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeSys) Mount(source, target, fstype string, flags uintptr, data string) error {
	if fstype == "squashfs" {
		disk := path.Base(source)
		f.mu.Lock()
		if f.mountFail[disk] > 0 {
			f.mountFail[disk]--
			f.mu.Unlock()
			return syscall.ENOENT
		}
		kind := f.disks[disk]
		f.mu.Unlock()
		dst := filepath.Join(f.root, target)
		switch kind {
		case "root":
			writeFile(f.t, filepath.Join(dst, "sbin/abroad-agent"), []byte("agent"))
		case "modloop":
			writeModloop(f.t, filepath.Join(dst, "modules", f.kver))
		}
	}
	f.record("mount %s %s %s %#x %s", source, target, fstype, flags, data)
	return nil
}

func (f *fakeSys) MoveMount(source, target string) error {
	f.record("move %s %s", source, target)
	return nil
}

func (f *fakeSys) LoadModule(img []byte) error {
	name := strings.TrimPrefix(string(img), "\x7fELF")
	f.mu.Lock()
	err := f.loadFail[name]
	if err == nil {
		f.loaded = append(f.loaded, name)
	}
	f.mu.Unlock()
	f.record("load %s", name)
	return err
}

func (f *fakeSys) KernelRelease() (string, error) { return f.kver, nil }

func (f *fakeSys) SwitchRoot(newroot string, argv []string) error {
	f.record("switch_root %s %s", newroot, strings.Join(argv, " "))
	return errors.New("fake: switch_root 返回")
}

func (f *fakeSys) Sync() {
	f.mu.Lock()
	f.syncs++
	f.mu.Unlock()
	f.record("sync")
}

func (f *fakeSys) PowerOff() {
	f.mu.Lock()
	f.poweroffs++
	f.mu.Unlock()
	f.record("poweroff")
}

func (f *fakeSys) DisableCtrlAltDel() error { f.record("cad_off"); return nil }

func (f *fakeSys) Spawn(argv []string) (int, error) {
	f.record("spawn %s", strings.Join(argv, " "))
	if f.spawnErr != nil {
		return 0, f.spawnErr
	}
	return 100, nil
}

func (f *fakeSys) WaitAny() (WaitResult, error) {
	if f.waitErr != nil {
		return WaitResult{}, f.waitErr
	}
	r := <-f.waits
	f.record("reaped %d %s", r.Pid, r.Status)
	return r, nil
}

func (f *fakeSys) Kill(pid int, sig syscall.Signal) error {
	f.record("kill %d %v", pid, sig)
	f.killed <- pid
	return nil
}

func (f *fakeSys) ListenVsock(port uint32) (net.Listener, error) {
	f.record("listen vsock %d", port)
	if err := f.listenErr[port]; err != nil {
		return nil, err
	}
	if port == 1081 {
		return f.listener, nil
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.extra = append(f.extra, l)
	f.mu.Unlock()
	return l, nil
}

func (f *fakeSys) SetTime(t time.Time) error {
	f.mu.Lock()
	f.setTimes = append(f.setTimes, t)
	err := f.setTimeErr
	f.mu.Unlock()
	f.record("settime")
	return err
}

func (f *fakeSys) LoopbackUp(context.Context) error { f.record("lo up"); return nil }

func (f *fakeSys) ConfigureNet(_ context.Context, c NetConfig) error {
	f.record("net %s %s %s", c.Iface, c.Addr, c.GW)
	f.netCfg = &c
	return f.netErr
}

func writeFile(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func gzELF(t *testing.T, name string) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte("\x7fELF" + name))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeModloop 造一个最小的模块目录：overlay、vsock 三件套、virtio_net、virtio_balloon、virtio-rng。
func writeModloop(t *testing.T, dir string) {
	mods := map[string][]string{
		"kernel/fs/overlayfs/overlay.ko.gz":                            nil,
		"kernel/net/vmw_vsock/vsock.ko.gz":                             nil,
		"kernel/net/vmw_vsock/vmw_vsock_virtio_transport_common.ko.gz": {"kernel/net/vmw_vsock/vsock.ko.gz"},
		"kernel/net/vmw_vsock/vmw_vsock_virtio_transport.ko.gz":        {"kernel/net/vmw_vsock/vmw_vsock_virtio_transport_common.ko.gz", "kernel/net/vmw_vsock/vsock.ko.gz"},
		"kernel/drivers/net/virtio_net.ko.gz":                          {"kernel/drivers/net/net_failover.ko.gz"},
		"kernel/drivers/net/net_failover.ko.gz":                        nil,
		"kernel/drivers/virtio/virtio_balloon.ko.gz":                   nil,
		"kernel/drivers/char/hw_random/virtio-rng.ko.gz":               nil,
	}
	var dep strings.Builder
	for p, deps := range mods {
		fmt.Fprintf(&dep, "%s: %s\n", p, strings.Join(deps, " "))
		writeFile(t, filepath.Join(dir, p), gzELF(t, ModuleNameForTest(p)))
	}
	writeFile(t, filepath.Join(dir, "modules.dep"), []byte(dep.String()))
}

// ModuleNameForTest 与 kmod.ModuleName 相同，测试里直接用文件名作为模块内容。
func ModuleNameForTest(p string) string {
	b := path.Base(p)
	b = b[:strings.Index(b, ".ko")]
	return strings.ReplaceAll(b, "-", "_")
}
