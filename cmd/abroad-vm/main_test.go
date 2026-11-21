package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/sirupsen/logrus"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"claude-abroad/internal/vmhost"
)

func TestParseAllowFlags(t *testing.T) {
	var w bytes.Buffer
	o, err := parseFlags([]string{"--state-dir", "s", "--allow-outbound", "127.0.0.1:7890", "--allow-outbound", "192.168.1.2:1080"}, &w)
	if err != nil || len(o.allow) != 2 {
		t.Fatalf("%+v %v", o, err)
	}
	for _, bad := range []string{"1.1.1.1:443", "127.0.0.1", "[::1]:80"} {
		if _, err := parseFlags([]string{"--state-dir", "s", "--allow-outbound", bad}, &w); err == nil {
			t.Errorf("%s 应被拒", bad)
		}
	}
}

func TestParseFlags(t *testing.T) {
	var w bytes.Buffer
	if _, err := parseFlags([]string{}, &w); err == nil {
		t.Error("没有 --state-dir 应报错")
	}
	if _, err := parseFlags([]string{"--state-dir", "x", "extra"}, &w); err == nil {
		t.Error("多余参数应报错")
	}
	o, err := parseFlags([]string{"--state-dir", "s", "--cpus", "2", "--memory", "512"}, &w)
	if err != nil || o.cpus != 2 || o.memory != 512 || o.imageDir != "guest/out" {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestParseOutboundFlags(t *testing.T) {
	var w bytes.Buffer
	o, err := parseFlags([]string{"--state-dir", "s"}, &w)
	if err != nil || o.outboundOpen || len(o.firstHops) != 0 {
		t.Fatalf("两个都不给时应是收紧模式：%+v %v", o, err)
	}
	o, err = parseFlags([]string{"--state-dir", "s", "--first-hop", "node.example.com:443,8000-8010", "--first-hop", "8.8.4.4:443"}, &w)
	if err != nil || len(o.firstHops) != 2 || o.outboundOpen {
		t.Fatalf("%+v %v", o, err)
	}
	o, err = parseFlags([]string{"--state-dir", "s", "--outbound-open"}, &w)
	if err != nil || !o.outboundOpen {
		t.Fatalf("%+v %v", o, err)
	}
	if _, err := parseFlags([]string{"--state-dir", "s", "--outbound-open", "--first-hop", "8.8.4.4:443"}, &w); err == nil {
		t.Error("--outbound-open 和 --first-hop 应互斥")
	}
	for _, bad := range []string{"8.8.4.4", "10.0.0.1:443", "bad_host:443", "x.com:0"} {
		if _, err := parseFlags([]string{"--state-dir", "s", "--first-hop", bad}, &w); err == nil {
			t.Errorf("--first-hop %s 应被拒", bad)
		}
	}
}

func TestNewUserNetModes(t *testing.T) {
	o, _ := parseFlags([]string{"--state-dir", "s", "--first-hop", "8.8.4.4:443"}, io.Discard)
	u, err := newUserNet(o, nil)
	if err != nil {
		t.Fatal(err)
	}
	st := u.stats()
	if st.OutboundMode != "first_hop" || st.FirstHopCount != 1 || u.dialer.BeforeDial == nil || u.gwdns == nil {
		t.Fatalf("默认应收紧：%+v", st)
	}
	o, _ = parseFlags([]string{"--state-dir", "s", "--outbound-open"}, io.Discard)
	u, err = newUserNet(o, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st := u.stats(); st.OutboundMode != "open" || u.dialer.BeforeDial != nil || u.gwdns != nil {
		t.Fatalf("--outbound-open 应是开放模式：%+v", st)
	}
}

// 首次启动失败：返回错误，并清理 pidfile、ctl.sock，放掉锁。
func TestRunFirstStartFailureCleansUp(t *testing.T) {
	// 不用 t.TempDir()：macOS 上它的路径可能超过 unix socket 的 103 字节上限。
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
	factory := func(vmhost.Spec) (vmhost.Machine, error) { return nil, errors.New("缺少虚拟化授权") }
	var w bytes.Buffer
	err = run([]string{"--image-dir", img, "--state-dir", state}, make(chan os.Signal), factory, &w)
	if err == nil || !strings.Contains(err.Error(), "缺少虚拟化授权") {
		t.Fatalf("应返回启动错误：%v", err)
	}
	for _, n := range []string{"ctl.sock", "data.sock", "host.sock", "abroad-vm.pid"} {
		if _, err := os.Lstat(filepath.Join(state, n)); !os.IsNotExist(err) {
			t.Errorf("%s 应已删除", n)
		}
	}
	sd, err := vmhost.OpenStateDir(state)
	if err != nil {
		t.Fatalf("锁应已释放：%v", err)
	}
	sd.Close()

	// 镜像缺文件：确定性错误，也直接返回。
	_ = os.Remove(filepath.Join(img, "modloop"))
	if err := run([]string{"--image-dir", img, "--state-dir", state}, make(chan os.Signal), factory, &w); err == nil || !strings.Contains(err.Error(), "modloop") {
		t.Fatalf("缺 modloop 应报错：%v", err)
	}
}

// stuckMachine 启动成功，但 health 不通、强停也停不下来。
type stuckMachine struct{ done chan struct{} }

func (m *stuckMachine) Start() error          { return nil }
func (m *stuckMachine) ForceStop() error      { return nil }
func (m *stuckMachine) Done() <-chan struct{} { return m.done }
func (m *stuckMachine) Err() error            { return nil }
func (m *stuckMachine) DialVsock(context.Context, uint32) (net.Conn, error) {
	return nil, errors.New("no vsock")
}

// 强停无效：以错误返回（main 随之非 0 退出），只起了一台，并照常清理 pidfile、ctl.sock、锁。
func TestRunStuckMachineExitsNonZero(t *testing.T) {
	old := supTiming
	supTiming = vmhost.DefaultTiming
	supTiming.Watchdog = 100 * time.Millisecond
	supTiming.StopWait = 100 * time.Millisecond
	supTiming.ProbeEvery = 10 * time.Millisecond
	supTiming.ProbeTimeout = 20 * time.Millisecond
	defer func() { supTiming = old }()

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
	var made atomic.Int32
	factory := func(vmhost.Spec) (vmhost.Machine, error) {
		made.Add(1)
		return &stuckMachine{done: make(chan struct{})}, nil
	}
	var w bytes.Buffer
	err = run([]string{"--image-dir", img, "--state-dir", state}, make(chan os.Signal), factory, &w)
	if !errors.Is(err, vmhost.ErrStuck) || made.Load() != 1 {
		t.Fatalf("应返回 ErrStuck 且只起一台：%v，%d 台", err, made.Load())
	}
	for _, n := range []string{"ctl.sock", "data.sock", "host.sock", "abroad-vm.pid"} {
		if _, err := os.Lstat(filepath.Join(state, n)); !os.IsNotExist(err) {
			t.Errorf("%s 应已删除", n)
		}
	}
	sd, err := vmhost.OpenStateDir(state)
	if err != nil {
		t.Fatalf("锁应已释放：%v", err)
	}
	sd.Close()
}

// portMachine 记录被拨的 vsock 端口，用 Done 控制停止。
type portMachine struct {
	done  chan struct{}
	ports chan uint32
}

func (m *portMachine) Start() error          { return nil }
func (m *portMachine) ForceStop() error      { return nil }
func (m *portMachine) Done() <-chan struct{} { return m.done }
func (m *portMachine) Err() error            { return nil }
func (m *portMachine) DialVsock(_ context.Context, port uint32) (net.Conn, error) {
	select { // 不阻塞：健康探测也会走这里
	case m.ports <- port:
	default:
	}
	a, b := net.Pipe()
	go func() { b.Write([]byte("hi")); b.Close() }()
	return a, nil
}

// data.sock：0600，桥到 vsock 1080，退出后删除。
func TestRunDataSock(t *testing.T) {
	old := supTiming
	supTiming = vmhost.DefaultTiming
	supTiming.Watchdog = time.Hour
	// 1 秒：全量测试时 CPU 很满（state 包会再跑一遍模拟器后端），50 毫秒会让假虚拟机来不及停，误报“强停后仍没有停止”。
	supTiming.ShutdownTimeout = time.Second
	supTiming.StopWait = time.Second
	defer func() { supTiming = old }()

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
	m := &portMachine{done: make(chan struct{}), ports: make(chan uint32, 16)}
	sigs := make(chan os.Signal, 2)
	done := make(chan error, 1)
	go func() {
		var w bytes.Buffer
		done <- run([]string{"--image-dir", img, "--state-dir", state}, sigs,
			func(vmhost.Spec) (vmhost.Machine, error) { return m, nil }, &w)
	}()
	sock := filepath.Join(state, "data.sock")
	deadline := time.Now().Add(3 * time.Second)
	for {
		if fi, err := os.Stat(sock); err == nil {
			if fi.Mode().Perm() != 0o600 {
				t.Fatalf("data.sock 权限应为 0600：%v", fi.Mode().Perm())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("data.sock 没有出现")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 健康探测也会拨 1081；只看经 data.sock 这次拨的端口。
	for len(m.ports) > 0 {
		<-m.ports
	}
	var c net.Conn
	for i := 0; i < 100; i++ { // 等 Supervisor 进入运行态
		c, err = net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(c)
		c.Close()
		if string(got) == "hi" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	found := false
	for len(m.ports) > 0 {
		if p := <-m.ports; p == 1080 {
			found = true
		}
	}
	if !found {
		t.Fatal("经 data.sock 应拨到虚拟机的 1080 端口")
	}
	sigs <- syscall.SIGTERM
	sigs <- syscall.SIGTERM // 第二次：立即强停（假机器不会自己停）
	close(m.done)
	if err := <-done; err != nil {
		t.Fatalf("应正常退出：%v", err)
	}
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Fatal("退出后 data.sock 应被删除")
	}
}

func TestThirdPartyLogsRedacted(t *testing.T) {
	var buf bytes.Buffer
	redactThirdPartyLogs(&buf)
	defer redactThirdPartyLogs(os.Stderr)
	logrus.Errorf("cannot dial udp 203.0.113.77:443: timeout")
	if strings.Contains(buf.String(), "203.0.113.77") || !strings.Contains(buf.String(), "cannot dial") {
		t.Fatalf("logrus 输出应已脱敏：%q", buf.String())
	}
}
