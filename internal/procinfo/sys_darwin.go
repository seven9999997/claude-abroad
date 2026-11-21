//go:build darwin

package procinfo

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// SysTable 经 sysctl 读真实的连接表（只读）。
type SysTable struct{}

func (SysTable) Snapshot(network string) ([]Entry, error) {
	name, proto := "net.inet.tcp.pcblist_n", 6
	if network == "udp" {
		name, proto = "net.inet.udp.pcblist_n", 17
	} else if network != "tcp" {
		return nil, fmt.Errorf("不认识的网络类型 %q", network)
	}
	b, err := unix.SysctlRaw(name)
	if err != nil {
		return nil, fmt.Errorf("读连接表失败：%w", err)
	}
	return ParsePCBList(b, proto)
}

// SysProcs 经 sysctl 读进程信息（kern.proc.pid 取父进程，kern.procargs2 取路径和参数）。
type SysProcs struct{}

func (SysProcs) Info(pid int) (Proc, error) {
	if pid <= 0 {
		return Proc{}, errors.New("进程号不对")
	}
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || k == nil || int(k.Proc.P_pid) != pid {
		return Proc{}, errors.New("进程不存在")
	}
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return Proc{}, fmt.Errorf("读进程路径失败：%w", err)
	}
	path, args, err := ParseProcArgs(b)
	if err != nil {
		return Proc{}, err
	}
	p := fromKinfo(k)
	p.Path, p.Args = path, args
	return p, nil
}

func fromKinfo(k *unix.KinfoProc) Proc {
	st := k.Proc.P_starttime
	n := 0
	for n < len(k.Proc.P_comm) && k.Proc.P_comm[n] != 0 {
		n++
	}
	return Proc{PID: int(k.Proc.P_pid), PPID: int(k.Eproc.Ppid), Start: time.Unix(int64(st.Sec), int64(st.Usec)*1000),
		Name: string(k.Proc.P_comm[:n])}
}

// List 经 sysctl kern.proc.all 列出全部进程，再逐个读路径和参数（读不到的留空，不算错）。
func (SysProcs) List() ([]Proc, error) {
	ks, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, fmt.Errorf("读进程列表失败：%w", err)
	}
	out := make([]Proc, 0, len(ks))
	for i := range ks {
		p := fromKinfo(&ks[i])
		if p.PID <= 0 {
			continue // 0 号是内核
		}
		if b, err := unix.SysctlRaw("kern.procargs2", p.PID); err == nil {
			if path, args, err := ParseProcArgs(b); err == nil {
				p.Path, p.Args = path, args
			}
		}
		out = append(out, p)
	}
	return out, nil
}

// Env 读进程的环境变量里环境检测关心的部分（只取白名单里的名字；代理变量只记名字）。读别的用户的进程要 root。
func (SysProcs) Env(pid int) (EnvInfo, error) {
	if pid <= 0 {
		return EnvInfo{}, errors.New("进程号不对")
	}
	b, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return EnvInfo{}, fmt.Errorf("读进程环境失败：%w", err)
	}
	return ParseProcEnv(b)
}
