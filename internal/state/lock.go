package state

import (
	"errors"
	"os"
	"path/filepath"

	"claude-abroad/internal/platform"
	"claude-abroad/internal/usererror"
)

// LockName 是状态目录下的锁文件名。
const LockName = "state.lock"

// RescueHint 是状态文件损坏、要走应急恢复时给用户的下一步（M6-5b：应急程序只在后台服务不在时运行）。
const RescueHint = "请先按应急文档停掉后台服务，再执行 sudo abroad-rescue（应急恢复）"

// ErrLocked 是锁被别的进程（abroadd、开机 guard、应急程序 abroad-rescue）拿着。
var ErrLocked = errors.New("状态文件被别的进程占用")

// ErrNotLocked 是没持锁就调用了要求持锁的操作（Recover、Boot）。
var ErrNotLocked = errors.New("没有持有状态文件的锁")

// Lock 对状态目录下的锁文件加排他、非阻塞的 flock（M2-7c 决定 5），返回释放函数。
// abroadd 在进程存续期间一直持有；Boot 运行期间持有。进程退出（文件描述符关闭）时锁自动释放。
func (s *Store) Lock() (release func(), err error) {
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	if s.lockFd != nil {
		return nil, usererror.New("锁定状态文件", "本进程已经持有锁", "这是程序错误，请重启 Claude 出国。", ErrLocked)
	}
	if err := s.ensureDir(); err != nil {
		return nil, err
	}
	p := filepath.Join(s.Dir, LockName)
	fd, err := platform.OpenNoFollow(p, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, usererror.New("锁定状态文件", "锁文件打不开（可能是符号链接）", "请重新安装后执行一键卸载。", err)
	}
	info, err := fd.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !ownerOK(info, s.UID) {
		fd.Close()
		return nil, usererror.New("锁定状态文件", "锁文件不安全（不是普通文件、权限过宽或属主不对）", "请重新安装后执行一键卸载。", ErrCorrupt)
	}
	if err := platform.LockExclusiveNB(fd); err != nil {
		fd.Close()
		if platform.IsWouldBlock(err) {
			return nil, usererror.New("锁定状态文件", "另一个 Claude 出国进程正在使用", "请等它完成，或先退出另一个进程。", ErrLocked)
		}
		return nil, usererror.New("锁定状态文件", "加锁失败", "请重启 Claude 出国后再试。", err)
	}
	s.lockFd = fd
	return func() {
		s.lockMu.Lock()
		defer s.lockMu.Unlock()
		if s.lockFd == fd {
			fd.Close() // 关闭即释放 flock
			s.lockFd = nil
		}
	}, nil
}

// HoldsLock 报告本 Store 是否持有跨进程锁。
func (s *Store) HoldsLock() bool {
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	return s.lockFd != nil
}

func notLocked(action string) error {
	return usererror.New(action, "没有持有状态文件的锁", "这是程序错误，请重启 Claude 出国。", ErrNotLocked)
}
