//go:build windows

package fdpass

import (
	"net"
	"os"

	"claude-abroad/internal/platform"
)

// Windows 上没有 SCM_RIGHTS：传 fd 和取对端身份一律失败（W-1 的占位；Windows 的直连拨号模块另行设计，见 design-windows）。

// Send 在 Windows 上一律失败。
func Send(c *net.UnixConn, msg []byte, f *os.File) error {
	return platform.Unsupported("经 unix socket 传递文件描述符")
}

// Recv 在 Windows 上一律失败。
func Recv(c *net.UnixConn, limit int) (msg []byte, f *os.File, err error) {
	return nil, nil, platform.Unsupported("经 unix socket 接收文件描述符")
}

// PeerUID 在 Windows 上一律失败：取不到对端身份就不能当作校验通过。
func PeerUID(c *net.UnixConn) (uint32, error) {
	return 0, platform.Unsupported("取 unix socket 对端的 uid")
}
