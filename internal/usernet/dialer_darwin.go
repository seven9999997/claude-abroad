//go:build darwin

package usernet

import (
	"fmt"
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// bindControl 返回 net.Dialer.Control：用 IP_BOUND_IF 把 socket 绑定到网卡 ifname。
// 网卡不存在或设置失败都返回错误（拨号随之失败），不退回不绑定。
func bindControl(ifname string) (func(network, address string, c syscall.RawConn) error, error) {
	ifi, err := net.InterfaceByName(ifname)
	if err != nil {
		return nil, fmt.Errorf("%w：网卡 %s：%v", errBind, ifname, err)
	}
	idx := ifi.Index
	return func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, idx)
		}); err != nil {
			return fmt.Errorf("%w：%v", errBind, err)
		}
		if serr != nil {
			return fmt.Errorf("%w：%s：%v", errBind, ifname, serr)
		}
		return nil
	}, nil
}
