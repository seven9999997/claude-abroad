//go:build windows

package main

import (
	"net"
	"os"

	"claude-abroad/internal/platform"
)

// socketpair 在 Windows 上一律失败（W-1 的占位；abroad-vm 本来就只用于 macOS，main 一开始就会退出）。
func socketpair() (vmEnd *os.File, hostConn net.Conn, err error) {
	return nil, nil, platform.Unsupported("建虚拟网卡用的 socketpair")
}
