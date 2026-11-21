//go:build unix

package main

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// 从 net.go 原样挪来（W-1）。

// socketpair 建一对 SOCK_DGRAM 的 unix socket（VZFileHandleNetworkDeviceAttachment 要求数据报语义）。
// vmEnd 交给虚拟机，hostConn 交给虚拟网络。缓冲区大小参考 Lima。
func socketpair() (vmEnd *os.File, hostConn net.Conn, err error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("socketpair：%w", err)
	}
	for _, fd := range fds {
		syscall.CloseOnExec(fd)
		if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 1<<20); err != nil {
			syscall.Close(fds[0])
			syscall.Close(fds[1])
			return nil, nil, fmt.Errorf("设置发送缓冲：%w", err)
		}
		if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4<<20); err != nil {
			syscall.Close(fds[0])
			syscall.Close(fds[1])
			return nil, nil, fmt.Errorf("设置接收缓冲：%w", err)
		}
	}
	vmEnd = os.NewFile(uintptr(fds[0]), "vm-net")
	hostFile := os.NewFile(uintptr(fds[1]), "host-net")
	hostConn, err = net.FileConn(hostFile) // FileConn 会 dup，原 fd 关掉
	hostFile.Close()
	if err != nil {
		vmEnd.Close()
		return nil, nil, fmt.Errorf("包装 socketpair：%w", err)
	}
	return vmEnd, hostConn, nil
}
