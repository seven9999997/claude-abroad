//go:build darwin

package main

import (
	"strings"

	"golang.org/x/sys/unix"
)

// bindToInterface 用 IP_BOUND_IF / IPV6_BOUND_IF 把 socket 绑定到网卡，
// 之后的流量只从这块网卡发出，不受路由表（例如 utun 的 0/1 路由）影响。
func bindToInterface(fd int, network string, ifindex int) error {
	if strings.HasSuffix(network, "6") {
		return unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF, ifindex)
	}
	return unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_BOUND_IF, ifindex)
}
