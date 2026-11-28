//go:build windows

package daemon

import (
	"net"

	"claude-abroad/internal/platform"
)

// peerUID 在 Windows 上一律失败（W-1 的占位）：取不到对端身份就拒绝连接，绝不当作身份校验通过。
func peerUID(c *net.UnixConn) (int, error) {
	return 0, platform.Unsupported("取控制接口对端的 uid")
}

func peerPID(c *net.UnixConn) (int, error) {
	return 0, platform.Unsupported("取控制接口对端的进程号")
}
