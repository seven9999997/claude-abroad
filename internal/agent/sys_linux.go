//go:build linux

package agent

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// NewSystem 返回 Linux 上的真实系统操作。
func NewSystem() System { return linuxSystem{} }

type linuxSystem struct{}

func (linuxSystem) Mount(source, target, fstype string, flags uintptr, data string) error {
	return unix.Mount(source, target, fstype, flags, data)
}

func (linuxSystem) MoveMount(source, target string) error {
	return unix.Mount(source, target, "", unix.MS_MOVE, "")
}

// LoadModule 用 init_module 加载已解压的模块（内核没开 CONFIG_MODULE_DECOMPRESS，
// 不能直接把 .ko.gz 交给 finit_module）。
func (linuxSystem) LoadModule(image []byte) error { return unix.InitModule(image, "") }

func (linuxSystem) KernelRelease() (string, error) {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return "", err
	}
	return unix.ByteSliceToString(u.Release[:]), nil
}

// SwitchRoot 与 busybox switch_root 相同的做法：chdir 新根 → 把它 move 到 / → chroot → exec。
// initramfs 不清空（只有几 MB）。
func (linuxSystem) SwitchRoot(newroot string, argv []string) error {
	if err := unix.Chdir(newroot); err != nil {
		return fmt.Errorf("chdir：%w", err)
	}
	if err := unix.Mount(".", "/", "", unix.MS_MOVE, ""); err != nil {
		return fmt.Errorf("move 到 /：%w", err)
	}
	if err := unix.Chroot("."); err != nil {
		return fmt.Errorf("chroot：%w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return fmt.Errorf("chdir /：%w", err)
	}
	return unix.Exec(argv[0], argv, baseEnv)
}

func (linuxSystem) Sync() { unix.Sync() }

func (linuxSystem) PowerOff() {
	for {
		_ = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
		// 关机调用失败时不能返回：PID 1 退出会让内核 panic。
		time.Sleep(time.Second)
	}
}

func (linuxSystem) DisableCtrlAltDel() error { return unix.Reboot(unix.LINUX_REBOOT_CMD_CAD_OFF) }

// baseEnv 是 agent 启动子进程时用的环境变量：不继承内核传进来的任何东西。
var baseEnv = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root"}

func (linuxSystem) Spawn(argv []string) (int, error) {
	return syscall.ForkExec(argv[0], argv, &syscall.ProcAttr{
		Dir:   "/",
		Env:   baseEnv,
		Files: []uintptr{0, 1, 2}, // 控制台
		Sys:   &syscall.SysProcAttr{Setsid: true},
	})
}

func (linuxSystem) WaitAny() (WaitResult, error) {
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, 0, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return WaitResult{}, err
		}
		return WaitResult{Pid: pid, Status: describeWait(ws)}, nil
	}
}

func describeWait(ws unix.WaitStatus) string {
	switch {
	case ws.Exited():
		return fmt.Sprintf("exit status %d", ws.ExitStatus())
	case ws.Signaled():
		return "signal: " + ws.Signal().String()
	}
	return fmt.Sprintf("wait status %#x", uint32(ws))
}

func (linuxSystem) Kill(pid int, sig syscall.Signal) error { return unix.Kill(pid, sig) }

func (linuxSystem) ListenVsock(port uint32) (net.Listener, error) { return listenVsock(port) }

func (linuxSystem) SetTime(t time.Time) error {
	ts := unix.NsecToTimespec(t.UnixNano())
	return unix.ClockSettime(unix.CLOCK_REALTIME, &ts)
}

func (linuxSystem) LoopbackUp(ctx context.Context) error {
	return runIP(ctx, [][]string{{"link", "set", "lo", "up"}})
}

// ConfigureNet 用根盘上 busybox 的 ip 配置静态地址和默认路由。
func (linuxSystem) ConfigureNet(ctx context.Context, c NetConfig) error {
	return runIP(ctx, [][]string{
		{"link", "set", c.Iface, "up"},
		{"addr", "flush", "dev", c.Iface},
		{"addr", "add", c.Addr.String(), "dev", c.Iface},
		{"route", "replace", "default", "via", c.GW.String(), "dev", c.Iface},
	})
}

// runIP 依次执行 /sbin/ip（busybox）：绝对路径、空环境变量、每条命令带超时。
func runIP(ctx context.Context, steps [][]string) error {
	for _, args := range steps {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		cmd := exec.CommandContext(cctx, "/sbin/ip", args...)
		cmd.Env = []string{}
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		cancel()
		if err != nil {
			return fmt.Errorf("ip %s：%v（%s）", strings.Join(args, " "), err, strings.TrimSpace(out.String()))
		}
	}
	return nil
}

// ShutdownSignals 是 PID 1 当作停机请求的信号。
// Ctrl-Alt-Del 在 DisableCtrlAltDel 之后变成 SIGINT；SIGPWR 是电源事件。
var ShutdownSignals = []os.Signal{unix.SIGTERM, unix.SIGINT, unix.SIGPWR}

// 编译期断言：system.go 里自定义的挂载标志与 Linux 的取值相同（不同时数组长度非 0，编译失败）。
var _ [0]struct{} = [MountReadOnly ^ unix.MS_RDONLY | MountNoSuid ^ unix.MS_NOSUID | MountNoDev ^ unix.MS_NODEV | MountNoExec ^ unix.MS_NOEXEC]struct{}{}
