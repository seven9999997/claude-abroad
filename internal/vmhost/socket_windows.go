//go:build windows

package vmhost

import (
	"errors"
	"net"

	"claude-abroad/internal/platform"
)

// ErrSocketDir 见 socket_unix.go。
var ErrSocketDir = errors.New("socket 所在目录不是属主本人、权限 0700 的真实目录")

// CheckPrivateDir：Windows 上目录权限是 ACL，W-1 不实现。
func CheckPrivateDir(string, int) error { return platform.Unsupported("检查目录是否私有") }

// ListenPrivateUnix：Windows 上的本地接口（命名管道或 AF_UNIX 加 ACL）以后再定。
func ListenPrivateUnix(string, int) (net.Listener, error) {
	return nil, platform.Unsupported("监听私有的本地 socket")
}
