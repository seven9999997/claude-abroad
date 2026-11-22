//go:build unix

package vmhost

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"

	"claude-abroad/internal/platform"
)

// ErrSocketDir 是 socket 所在目录不够私有（不是目录、是符号链接、属主不对、权限不是 0700）。
var ErrSocketDir = errors.New("socket 所在目录不是属主本人、权限 0700 的真实目录")

// CheckPrivateDir 检查 dir 是属主为 uid、权限恰好 0700 的真实目录（不是符号链接）。
func CheckPrivateDir(dir string, uid int) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("%w：%v", ErrSocketDir, err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%w：%s 不是真实目录", ErrSocketDir, dir)
	}
	owner, err := platform.FileOwner(fi)
	if err != nil || owner != uid {
		return fmt.Errorf("%w：%s 的属主不对", ErrSocketDir, dir)
	}
	if fi.Mode().Perm() != 0o700 {
		return fmt.Errorf("%w：%s 的权限是 %#o", ErrSocketDir, dir, fi.Mode().Perm())
	}
	return nil
}

// ListenPrivateUnix 在 path 上监听 unix socket，保证任何时刻都不存在权限比 0600 宽的 socket 可被连接：
//  1. 先检查上级目录属主是 uid、权限 0700（别的用户进不了目录）；
//  2. 在同一目录下用临时名字（path 加 “~”）监听，改成 0600 后再改名为 path（改名是原子的，客户端只能看到 0600 的）。
//
// 调用方负责先处理 path 上已有的文件（是 socket 才能删）。关闭监听时删除 path。
// path 加 1 个字节后仍要在 unix socket 路径的上限内（调用方检查）。
func ListenPrivateUnix(path string, uid int) (net.Listener, error) {
	if err := CheckPrivateDir(filepath.Dir(path), uid); err != nil {
		return nil, err
	}
	tmp := path + "~"
	if fi, err := os.Lstat(tmp); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("%s 已存在且不是 socket", tmp)
		}
		if err := os.Remove(tmp); err != nil {
			return nil, err
		}
	}
	l, err := net.Listen("unix", tmp)
	if err != nil {
		return nil, err
	}
	ul := l.(*net.UnixListener)
	ul.SetUnlinkOnClose(false)
	if err := os.Chmod(tmp, 0o600); err != nil {
		ul.Close()
		os.Remove(tmp)
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		ul.Close()
		os.Remove(tmp)
		return nil, err
	}
	return &PrivateListener{UnixListener: ul, path: path}, nil
}
