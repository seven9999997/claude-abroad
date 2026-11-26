//go:build !windows

package main

import "syscall"

// socketAnswers 报告 abroadd 的本地接口 socket 有进程在听（只做一次本机 IPC 的 connect，不收发数据；不用 net 包）。
func socketAnswers(path string) bool {
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return false
	}
	defer syscall.Close(fd)
	return syscall.Connect(fd, &syscall.SockaddrUnix{Name: path}) == nil
}
