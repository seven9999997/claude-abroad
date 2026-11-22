package vmhost

import (
	"io/fs"
	"net"
	"os"
)

// PrivateListener 是 ListenPrivateUnix 返回的监听：关闭时删除改名后的 path（net 包只会删除绑定时的临时名字，
// 已经关掉了）。需要按 unix socket 取对端身份的调用方用 UnixListener。
type PrivateListener struct {
	*net.UnixListener
	path string
}

// Close 关闭监听并删除 path 上的 socket。
func (l *PrivateListener) Close() error {
	err := l.UnixListener.Close()
	if fi, e := os.Lstat(l.path); e == nil && fi.Mode()&fs.ModeSocket != 0 {
		os.Remove(l.path)
	}
	return err
}
