//go:build windows

package platform

import (
	"io/fs"
	"os"
)

// Windows 的占位（W-1）：不跟随符号链接的打开、属主、跨进程锁一律失败，绝不返回“校验通过”。
// Windows 的真实实现（重解析点、ACL、LockFileEx）见 design-windows。

// OpenNoFollow 在 Windows 上一律失败。
func OpenNoFollow(name string, flag int, perm fs.FileMode) (*os.File, error) {
	return nil, Unsupported("不跟随符号链接地打开文件")
}

// FileOwner 在 Windows 上一律失败。
func FileOwner(info fs.FileInfo) (int, error) {
	return 0, Unsupported("取文件属主")
}

// LockExclusiveNB 在 Windows 上一律失败。
func LockExclusiveNB(f *os.File) error {
	return Unsupported("跨进程文件锁")
}

// IsWouldBlock 在 Windows 上恒为 false（纯判断，不做系统调用；加锁本身已经失败）。
func IsWouldBlock(err error) bool { return false }
