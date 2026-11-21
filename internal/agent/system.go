package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// 挂载标志，取值与 Linux 相同（这里自己定义，好让可移植代码在 macOS 上编译）。
const (
	MountReadOnly uintptr = 0x1 // MS_RDONLY
	MountNoSuid   uintptr = 0x2 // MS_NOSUID
	MountNoDev    uintptr = 0x4 // MS_NODEV
	MountNoExec   uintptr = 0x8 // MS_NOEXEC
)

// ErrUnsupported 是非 Linux 平台上系统操作的返回值。
var ErrUnsupported = errors.New("只能在虚拟机里（linux）运行")

// WaitResult 是 WaitAny 收割到的一个子进程。
type WaitResult struct {
	Pid    int
	Status string // 例如 "exit status 1"、"signal: killed"
}

// NetConfig 是虚拟机网卡的静态配置。
type NetConfig struct {
	Iface string
	Addr  netip.Prefix
	GW    netip.Addr
}

// System 是 agent 用到的全部系统操作。Linux 实现见 sys_linux.go；测试用 fake。
// 路径参数都是虚拟机里的绝对路径。
type System interface {
	Mount(source, target, fstype string, flags uintptr, data string) error
	MoveMount(source, target string) error
	LoadModule(image []byte) error
	KernelRelease() (string, error)
	// SwitchRoot 把 newroot 变成根目录并执行 argv。成功时不返回。
	SwitchRoot(newroot string, argv []string) error
	Sync()
	// PowerOff 关机。真实实现不返回。
	PowerOff()
	// DisableCtrlAltDel 让 Ctrl-Alt-Del（宿主请求停机）变成发给 PID 1 的 SIGINT。
	DisableCtrlAltDel() error

	// Spawn 启动子进程（只给 PID 1 用，所以不经 exec.Cmd）。
	Spawn(argv []string) (pid int, err error)
	// WaitAny 阻塞到任意一个子进程退出（wait4(-1)），EINTR 自己重试。
	WaitAny() (WaitResult, error)
	Kill(pid int, sig syscall.Signal) error

	// ListenVsock 在 vsock 端口上监听（fd 带 CLOEXEC）。
	ListenVsock(port uint32) (net.Listener, error)
	// SetTime 设置墙上时钟（CLOCK_REALTIME）。
	SetTime(t time.Time) error
	// LoopbackUp 拉起 lo（引擎都只听 127.0.0.1；不挂网卡时也要有）。
	LoopbackUp(ctx context.Context) error
	ConfigureNet(ctx context.Context, c NetConfig) error
}

// Env 是 boot、init、serve 共用的运行环境。Root 在虚拟机里是 "/"，测试里是临时目录：
// 只有直接读写文件时才加 Root 前缀；交给 System 的路径保持虚拟机里的绝对路径。
type Env struct {
	Sys   System
	Root  string
	Log   io.Writer
	Now   func() time.Time
	Sleep func(time.Duration)
	// Runner 启动引擎和校验命令；为空时用真实进程（ExecRunner）。
	Runner ProcRunner
	// DialReady 探测引擎端口；为空时用 TCP 连接。
	DialReady func(ctx context.Context, addr string) error
	// DialSocks 是数据面连 sing-box socks 入站用的拨号函数；为空时用 net.Dialer。
	DialSocks func(ctx context.Context, network, addr string) (net.Conn, error)

	logMu sync.Mutex
}

func (e *Env) path(p string) string { return filepath.Join(e.Root, p) }

func (e *Env) logf(format string, a ...any) {
	if e.Log != nil {
		e.logMu.Lock()
		defer e.logMu.Unlock()
		fmt.Fprintf(e.Log, "abroad-agent: "+format+"\n", a...)
	}
}

func (e *Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Env) sleep(d time.Duration) {
	if e.Sleep != nil {
		e.Sleep(d)
		return
	}
	time.Sleep(d)
}

func (e *Env) mkdir(p string) error { return os.MkdirAll(e.path(p), 0o755) }

// PowerOff 先 sync 再关机。
func PowerOff(sys System) {
	sys.Sync()
	sys.PowerOff()
}
