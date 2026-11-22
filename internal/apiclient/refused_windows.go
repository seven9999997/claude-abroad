package apiclient

import (
	"errors"
	"syscall"
)

// wsaeConnRefused：Windows 上连一个没有进程在听的 unix socket 文件回 WSAECONNREFUSED（10061），和 syscall.ECONNREFUSED 不是同一个值。
var wsaeConnRefused error = syscall.Errno(10061)

func isRefused(err error) bool {
	return errors.Is(err, wsaeConnRefused) || errors.Is(err, syscall.ECONNREFUSED)
}
