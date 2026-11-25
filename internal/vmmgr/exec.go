package vmmgr

import "io"

// ExecLauncher 以子进程拉起 abroad-vm（真实实现；只在 abroadd 的真实模式入口构造，源码检查守着）。
//   - 标准输入是一根管道，写端由 abroadd 持有：abroadd 不管怎么退出（包括崩溃、被强制结束），写端都会被内核关闭，
//     abroad-vm 带 --parent-watch 时读到 EOF 就按收到 SIGTERM 处理，优雅停止虚拟机（不留没人管的虚拟机）；
//   - 不继承 abroadd 的环境变量（不带任何代理变量），只给固定的 PATH；
//   - 单独的进程组（终端的 Ctrl-C 不会直接打到它）；
//   - RunAs 为 nil 时以 abroadd 自己的用户运行，否则切到指定的 uid/gid（R1：以专用用户运行还是 root 宿主加专用出站进程，
//     结论回来只换这里的取值）。
type ExecLauncher struct {
	Path   string
	Args   []string
	RunAs  *Credential
	Output io.Writer // abroad-vm 的标准输出和标准错误；nil 丢弃
}

// Credential 是运行 abroad-vm 的用户。
type Credential struct{ UID, GID uint32 }

var _ Launcher = (*ExecLauncher)(nil)
