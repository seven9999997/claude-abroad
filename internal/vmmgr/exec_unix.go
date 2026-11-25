//go:build !windows

package vmmgr

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
)

// Launch 实现 Launcher。
func (e *ExecLauncher) Launch() (Proc, error) {
	if e.Path == "" {
		return nil, errors.New("没有指定 abroad-vm 的路径")
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	out := e.Output
	if out == nil {
		out = io.Discard
	}
	cmd := exec.Command(e.Path, e.Args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = r, out, out
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.SysProcAttr.Credential = sysCredential(e.RunAs)
	if err := cmd.Start(); err != nil {
		r.Close()
		w.Close()
		return nil, err
	}
	r.Close() // 读端只留给子进程
	p := &execProc{cmd: cmd, w: w, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		w.Close()
		close(p.done)
	}()
	return p, nil
}

// execProc 是一个已拉起的子进程。结束它只经进程句柄（Wait 回收之前进程号不会被复用）。
type execProc struct {
	cmd  *exec.Cmd
	w    *os.File // 标准输入管道的写端：关闭即通知子进程父进程不在了
	done chan struct{}
	err  error // done 关闭之前写入
}

func (p *execProc) Done() <-chan struct{} { return p.done }
func (p *execProc) ExitErr() error        { <-p.done; return p.err }

func (p *execProc) Terminate() error {
	select {
	case <-p.done:
		return nil
	default:
	}
	return p.cmd.Process.Signal(syscall.SIGTERM)
}

func (p *execProc) Kill() error {
	select {
	case <-p.done:
		return nil
	default:
	}
	return p.cmd.Process.Kill()
}

// sysCredential 把运行用户换成系统调用的凭据；nil 表示不切用户。附加组只留这个用户自己的主组：
// abroadd 是 root，不显式给附加组的话子进程会带着 root 的附加组（wheel、admin 等），权限分离不完整。
func sysCredential(c *Credential) *syscall.Credential {
	if c == nil {
		return nil
	}
	return &syscall.Credential{Uid: c.UID, Gid: c.GID, Groups: []uint32{c.GID}}
}
