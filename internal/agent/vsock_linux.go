//go:build linux

package agent

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// VsockAddr 是 vsock 地址（CID + 端口）。
type VsockAddr struct {
	CID  uint32
	Port uint32
}

func (VsockAddr) Network() string  { return "vsock" }
func (a VsockAddr) String() string { return fmt.Sprintf("vsock:%d:%d", a.CID, a.Port) }

func sockaddrVM(sa unix.Sockaddr) VsockAddr {
	if v, ok := sa.(*unix.SockaddrVM); ok {
		return VsockAddr{CID: v.CID, Port: v.Port}
	}
	return VsockAddr{}
}

// vsockListener 用 x/sys/unix 直接实现（Go 的 net.FileListener 不认 AF_VSOCK）。
// fd 带 SOCK_CLOEXEC：引擎子进程不能继承控制面、数据面的 fd。
// 非阻塞 fd 经 os.NewFile 接入运行时的轮询器，所以 Close 能打断阻塞中的 Accept。
type vsockListener struct {
	f    *os.File
	rc   syscall.RawConn
	addr VsockAddr
}

func listenVsock(port uint32) (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, os.NewSyscallError("socket", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		unix.Close(fd)
		return nil, os.NewSyscallError("bind", err)
	}
	if err := unix.Listen(fd, 128); err != nil {
		unix.Close(fd)
		return nil, os.NewSyscallError("listen", err)
	}
	f := os.NewFile(uintptr(fd), fmt.Sprintf("vsock-listen:%d", port))
	rc, err := f.SyscallConn()
	if err != nil {
		f.Close()
		return nil, err
	}
	addr := VsockAddr{CID: unix.VMADDR_CID_ANY, Port: port}
	if sa, err := unix.Getsockname(fd); err == nil {
		addr = sockaddrVM(sa)
	}
	return &vsockListener{f: f, rc: rc, addr: addr}, nil
}

func (l *vsockListener) Accept() (net.Conn, error) {
	var (
		nfd  int
		sa   unix.Sockaddr
		aerr error
	)
	err := l.rc.Read(func(fd uintptr) bool {
		nfd, sa, aerr = unix.Accept4(int(fd), unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK)
		// EAGAIN 等下一次可读；EINTR、ECONNABORTED（对端在 accept 前就断了）直接再试。
		return aerr != unix.EAGAIN && aerr != unix.EINTR && aerr != unix.ECONNABORTED
	})
	if err != nil {
		if errors.Is(err, os.ErrClosed) {
			return nil, net.ErrClosed
		}
		return nil, err
	}
	if aerr != nil {
		// 其余错误（EMFILE、ENOMEM 等）标成临时错误：http.Server 会退避后重试，
		// 不会因为一次偶发错误退出控制面、让虚拟机关机。
		return nil, temporaryError{os.NewSyscallError("accept4", aerr)}
	}
	f := os.NewFile(uintptr(nfd), "vsock-conn")
	rc, err := f.SyscallConn()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &vsockConn{f: f, rc: rc, local: l.addr, remote: sockaddrVM(sa)}, nil
}

func (l *vsockListener) Close() error   { return l.f.Close() }
func (l *vsockListener) Addr() net.Addr { return l.addr }

// vsockConn 是一条 vsock 连接：读写走运行时轮询（支持超时），CloseWrite 是 shutdown(SHUT_WR)。
type vsockConn struct {
	f             *os.File
	rc            syscall.RawConn
	local, remote VsockAddr
}

func (c *vsockConn) Read(b []byte) (int, error) {
	n, err := c.f.Read(b)
	return n, wrapDeadline(err)
}

func (c *vsockConn) Write(b []byte) (int, error) {
	n, err := c.f.Write(b)
	return n, wrapDeadline(err)
}

func (c *vsockConn) Close() error                       { return c.f.Close() }
func (c *vsockConn) LocalAddr() net.Addr                { return c.local }
func (c *vsockConn) RemoteAddr() net.Addr               { return c.remote }
func (c *vsockConn) SetDeadline(t time.Time) error      { return c.f.SetDeadline(t) }
func (c *vsockConn) SetReadDeadline(t time.Time) error  { return c.f.SetReadDeadline(t) }
func (c *vsockConn) SetWriteDeadline(t time.Time) error { return c.f.SetWriteDeadline(t) }

// CloseWrite 半关闭：只关本端写方向。
func (c *vsockConn) CloseWrite() error {
	var serr error
	if err := c.rc.Control(func(fd uintptr) { serr = unix.Shutdown(int(fd), unix.SHUT_WR) }); err != nil {
		return err
	}
	return os.NewSyscallError("shutdown", serr)
}

// wrapDeadline 让读写超时满足 net.Error（Timeout() 为 true），http.Server 等依赖这一点。
func wrapDeadline(err error) error {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return timeoutError{err}
	}
	return err
}

type timeoutError struct{ error }

type temporaryError struct{ error }

func (temporaryError) Timeout() bool   { return false }
func (temporaryError) Temporary() bool { return true }
func (e temporaryError) Unwrap() error { return e.error }

func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }
func (e timeoutError) Unwrap() error { return e.error }
