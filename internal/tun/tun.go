// Package tun 是三层虚拟网卡的抽象（设计 4.1）：真实的 macOS utun（utun_darwin.go）和测试用的内存管道（Pipe）。
//
// utun 自己用 golang.org/x/sys/unix 实现，不用 golang.zx2c4.com/wireguard/tun：后者的 go.mod 依赖 gvisor，
// 会把本项目钉住的 gvisor 版本抬高，打过补丁的 third_party/gvisor-tap-vsock 就编不过。
//
// 本包不碰宿主网络的配置（地址、路由由 M2-7、M2-9 负责）。
package tun

import "errors"

// ErrClosed 是设备已关闭。
var ErrClosed = errors.New("tun 设备已关闭")

// ErrShortBuffer 是 Read 的缓冲区比包小（不静默截断）。
var ErrShortBuffer = errors.New("tun 读缓冲区比包小")

// ErrTooBig 是 Write 的包超过 MTU。
var ErrTooBig = errors.New("tun 包超过 MTU")

// Device 是一个三层虚拟网卡。Read、Write 一次一个完整的 IP 包（不含 utun 的 4 字节地址族头）。
// Read 返回 ErrShortBuffer、ErrTooBig 时表示丢了一个包，设备仍可继续读。
// Close 幂等，并且会唤醒阻塞中的 Read、Write，之后它们返回 ErrClosed。
type Device interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	MTU() int
	Name() string
	Close() error
}
