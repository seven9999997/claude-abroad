package vmhost

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"claude-abroad/internal/platform"
	"claude-abroad/internal/usererror"
)

// MaxSocketPath 是 unix socket 路径的上限（macOS 的 sun_path 是 104 字节，含结尾的 0）。
const MaxSocketPath = 103

// StateDir 是 abroad-vm 的状态目录（0700）：ctl.sock、控制台日志、pidfile、单实例锁。
// 开发模式见设计文档 3.1；会话内验收用仓库里的 scripts/dev/out/state/。
type StateDir struct {
	Dir  string
	lock *os.File
}

// OpenStateDir 建好目录（0700），拿到单实例锁。目录本身是符号链接、属主不是自己、
// 或已有实例在运行时报错。返回后要调用 Close 放锁。
// fileOwner 取目录属主（测试可替换）。
var fileOwner = platform.FileOwner

func OpenStateDir(dir string) (*StateDir, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(abs); err == nil {
		if fi.Mode()&fs.ModeSymlink != 0 {
			return nil, usererror.New("打开状态目录", abs+" 是符号链接", "请换一个真实目录。", nil)
		}
		if !fi.IsDir() {
			return nil, usererror.New("打开状态目录", abs+" 不是目录", "请换一个目录。", nil)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, usererror.Wrap("创建状态目录", err, "请检查路径和权限。")
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	// 取不到属主一律按检查失败处理，不能跳过（和 state.ownerOK 一致；architect 判 W-1 的建议改 1）。
	owner, oerr := fileOwner(fi)
	if errors.Is(oerr, platform.ErrUnsupported) {
		return nil, usererror.Wrap("检查状态目录的属主", oerr, "这个平台上 abroad-vm 还不能运行。")
	}
	if oerr != nil {
		return nil, usererror.Wrap("检查状态目录的属主", oerr, "请换一个目录。")
	}
	if owner != os.Getuid() {
		return nil, usererror.New("打开状态目录", abs+" 的属主不是当前用户", "请换一个目录。", nil)
	}
	if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(abs, 0o700); err != nil {
			return nil, usererror.Wrap("把状态目录权限改成 0700", err, "请检查权限。")
		}
	}
	lf, err := os.OpenFile(filepath.Join(abs, "abroad-vm.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, usererror.Wrap("打开单实例锁", err, "请检查状态目录权限。")
	}
	if err := platform.LockExclusiveNB(lf); err != nil {
		lf.Close()
		if platform.IsWouldBlock(err) {
			return nil, usererror.New("启动 abroad-vm", "这个状态目录已有实例在运行（"+abs+"）", "请先停掉它，或换一个 --state-dir。", err)
		}
		return nil, usererror.Wrap("加单实例锁", err, "请检查状态目录。")
	}
	return &StateDir{Dir: abs, lock: lf}, nil
}

// Close 放锁。
func (s *StateDir) Close() error {
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close() // 关闭 fd 即释放 flock
	s.lock = nil
	return err
}

func (s *StateDir) CtlSock() string  { return filepath.Join(s.Dir, "ctl.sock") }
func (s *StateDir) DataSock() string { return filepath.Join(s.Dir, "data.sock") }

// HostSock 是 abroad-vm 自己应答的只读诊断接口（不转发进虚拟机）。
func (s *StateDir) HostSock() string   { return filepath.Join(s.Dir, "host.sock") }
func (s *StateDir) ConsoleLog() string { return filepath.Join(s.Dir, "console.log") }
func (s *StateDir) PIDFile() string    { return filepath.Join(s.Dir, "abroad-vm.pid") }

// WritePID 写 pidfile（0600）。
func (s *StateDir) WritePID() error {
	return os.WriteFile(s.PIDFile(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
}

// ListenUnix 在 path 上监听 unix socket，权限 0600。残留的 socket 文件会先删掉
// （调用方已持有单实例锁，所以它不属于正在运行的实例）；同名的非 socket 文件不删，直接报错。
func ListenUnix(path string) (net.Listener, error) {
	if len(path) > MaxSocketPath {
		return nil, usererror.New("监听 "+filepath.Base(path),
			fmt.Sprintf("路径有 %d 字节，超过 unix socket 的上限 %d", len(path), MaxSocketPath),
			"请用更短的 --state-dir。", nil)
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			return nil, usererror.New("监听 "+filepath.Base(path), path+" 已存在且不是 socket", "请检查状态目录。", nil)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, usererror.Wrap("监听 "+filepath.Base(path), err, "请检查状态目录。")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// MaxConsoleLog 是控制台日志的轮转阈值。
const MaxConsoleLog = 4 << 20

// PrepareConsoleLog 在每次启动虚拟机前调用：超过 MaxConsoleLog 就改名为 .1（只留一份），
// 然后确保文件存在且权限 0600。
func PrepareConsoleLog(path string) error {
	if fi, err := os.Stat(path); err == nil && fi.Size() > MaxConsoleLog {
		if err := os.Rename(path, path+".1"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	return os.Chmod(path, 0o600)
}
