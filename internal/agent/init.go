package agent

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"

	"claude-abroad/internal/guestabi"
)

// StopTimeout 是 PID 1 收到停机信号后等 serve 退出的上限，超时直接关机。
const StopTimeout = 10 * time.Second

// Init 是根盘上的 PID 1（设计文档 7.1）。它只做最少的事：补挂 /run、启动
// `abroad-agent serve`、用 wait4(-1) 收割所有子进程、把停机信号转给 serve。
// serve 以任何方式退出，Init 就返回，由调用方 sync + 关机；不在虚拟机里重启 serve。
//
// 其余工作（模块、网卡、控制面、引擎）都在 serve 里：Go 运行时的致命错误 recover 不住，
// 放在 PID 1 上会让内核 panic，虚拟机挂死而不是关机。
// PID 1 自己没有用 exec.Cmd 管理的子进程，所以 wait4(-1) 不会抢走谁的退出状态。
//
// sigs 收到的任何信号都当作停机请求。
func Init(e *Env, sigs <-chan os.Signal) error {
	if err := e.mkdir("/run"); err != nil {
		return err
	}
	if err := e.Sys.Mount("tmpfs", "/run", "tmpfs", MountNoSuid|MountNoDev, "mode=0755,size=16m"); err != nil {
		return fmt.Errorf("挂载 /run：%w", err)
	}
	if err := e.Sys.DisableCtrlAltDel(); err != nil {
		e.logf("init：关闭 Ctrl-Alt-Del 重启失败（宿主的停机请求可能无效）：%v", err)
	}
	pid, err := e.Sys.Spawn([]string{guestabi.AgentPath, "serve"})
	if err != nil {
		return fmt.Errorf("启动 serve：%w", err)
	}
	e.logf("init：serve 已启动（pid %d）", pid)

	var once sync.Once
	go func() {
		defer guardPID1(e)
		for s := range sigs {
			once.Do(func() {
				e.logf("init：收到 %v，通知 serve 退出", s)
				if err := e.Sys.Kill(pid, syscall.SIGTERM); err != nil {
					e.logf("init：通知 serve 失败：%v", err)
				}
				go func() {
					defer guardPID1(e)
					e.sleep(StopTimeout)
					e.logf("init：serve %s 内没有退出，直接关机", StopTimeout)
					PowerOff(e.Sys)
				}()
			})
		}
	}()

	for {
		r, err := e.Sys.WaitAny()
		if err != nil {
			return fmt.Errorf("wait4：%w", err)
		}
		if r.Pid == pid {
			e.logf("init：serve 退出（%s），关机", r.Status)
			return nil
		}
	}
}

// guardPID1 是 PID 1 里每个 goroutine 的兜底：panic 时打印并关机。
// main 的 recover 只管主 goroutine，别的 goroutine panic 会直接让 PID 1 退出（内核 panic）。
func guardPID1(e *Env) {
	if r := recover(); r != nil {
		e.logf("init：致命错误：%v，关机", r)
		PowerOff(e.Sys)
	}
}
