package daemon

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID 取 unix socket 对端进程的 uid（LOCAL_PEERCRED）。
func peerUID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return -1, err
	}
	uid, cerr := -1, error(nil)
	if err := raw.Control(func(fd uintptr) {
		x, e := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if e != nil {
			cerr = e
			return
		}
		uid = int(x.Uid)
	}); err != nil {
		return -1, err
	}
	return uid, cerr
}
