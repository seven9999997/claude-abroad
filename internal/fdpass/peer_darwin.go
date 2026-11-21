package fdpass

import (
	"net"

	"golang.org/x/sys/unix"
)

// PeerUID 取 unix socket 对端进程的 uid（LOCAL_PEERCRED）。
func PeerUID(c *net.UnixConn) (uint32, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid uint32
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		x, e := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if e != nil {
			cerr = e
			return
		}
		uid = x.Uid
	}); err != nil {
		return 0, err
	}
	return uid, cerr
}
