package vmhost

import (
	"context"
	"errors"
	"net"
)

// Machine 是一台虚拟机。实现必须保证：
//   - Start 只调用一次；
//   - Done 在虚拟机停止（无论原因）后关闭，Err 返回停止原因；
//   - ForceStop 幂等，已停止时返回 nil；
//   - DialVsock 遵守 ctx（Code-Hex/vz 的 Connect 本身没有超时）。
type Machine interface {
	Start() error
	ForceStop() error
	Done() <-chan struct{}
	Err() error
	DialVsock(ctx context.Context, port uint32) (net.Conn, error)
}

// Factory 按配置创建一台虚拟机（还没启动）。
type Factory func(Spec) (Machine, error)

// ErrNotSupported 是没有 Virtualization.framework（非 macOS 或没开 cgo）时的错误。
var ErrNotSupported = errors.New("这个平台不支持运行虚拟机（需要 macOS + cgo）")
