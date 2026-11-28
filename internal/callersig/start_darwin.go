package callersig

import "golang.org/x/sys/unix"

// ProcStart 取进程的启动时间（微秒，kern.proc.pid）。
func ProcStart(pid int) (int64, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	t := kp.Proc.P_starttime
	return int64(t.Sec)*1_000_000 + int64(t.Usec), nil
}
