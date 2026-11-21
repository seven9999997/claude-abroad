//go:build unix

package claudeproc

import (
	"errors"
	"os"
	"syscall"
)

// realSignaler 是真实的信号发送器。整个仓库里只有这个文件发信号（源码检查）。
type realSignaler struct{ self int }

// NewRealSignaler 构造真实的信号发送器：有效 uid 不是 0 时返回错误。只在 cmd/abroadd 的真实模式入口调用（源码检查）。
func NewRealSignaler() (Signaler, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("结束 Claude 进程需要 root：真实的信号发送器只能在 abroadd 以 root 运行时使用")
	}
	return realSignaler{self: os.Getpid()}, nil
}

func (r realSignaler) Signal(pid int, sig syscall.Signal) error {
	if pid <= 1 || pid == r.self {
		return errors.New("拒绝向自己或 1 号进程发信号")
	}
	if sig != syscall.SIGTERM && sig != syscall.SIGKILL {
		return errors.New("只发 TERM 和 KILL")
	}
	return syscall.Kill(pid, sig)
}
