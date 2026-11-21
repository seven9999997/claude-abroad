//go:build unix

package platform

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// 这几个是从 state、vmhost、fakedns、envcheck 里原样挪来的 unix 实现（W-1）：调用方的逻辑不变。

// OpenNoFollow 等于 os.OpenFile(name, flag|O_NOFOLLOW, perm)：最后一级是符号链接时打开失败。
func OpenNoFollow(name string, flag int, perm fs.FileMode) (*os.File, error) {
	return os.OpenFile(name, flag|syscall.O_NOFOLLOW, perm)
}

// FileOwner 取文件的属主 uid（fs.FileInfo.Sys() 是 *syscall.Stat_t）；取不到时返回错误，不能当作属主正确。
func FileOwner(info fs.FileInfo) (int, error) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("取不到文件的属主")
	}
	return int(st.Uid), nil
}

// LockExclusiveNB 对 f 加排他、非阻塞的 flock；关闭 f 即释放。
func LockExclusiveNB(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// IsWouldBlock 判断加锁失败是不是因为锁被别人拿着。
func IsWouldBlock(err error) bool { return errors.Is(err, syscall.EWOULDBLOCK) }
