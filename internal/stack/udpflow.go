package stack

import (
	"net"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
)

// udpFlow 把 gVisor 的已连接 UDP 端点包成 net.PacketConn（同时满足 net.Conn，便于统一登记和关闭）：
//   - 对端固定是 Flow.Src：WriteTo 忽略传入的地址，ReadFrom 返回 Flow.Src；
//   - 每次读写成功都刷新空闲计时，超过 idle 没有活动就关闭。
type udpFlow struct {
	*gonet.UDPConn
	peer   *net.UDPAddr
	idle   time.Duration
	mu     sync.Mutex
	timer  *time.Timer
	closed bool // Close 之后 touch 不再重装定时器
}

func newUDPFlow(c *gonet.UDPConn, id Flow, idle time.Duration) *udpFlow {
	u := &udpFlow{UDPConn: c, peer: net.UDPAddrFromAddrPort(id.Src), idle: idle}
	u.timer = time.AfterFunc(idle, func() { u.UDPConn.Close() })
	return u
}

func (u *udpFlow) touch() {
	u.mu.Lock()
	if !u.closed {
		u.timer.Reset(u.idle)
	}
	u.mu.Unlock()
}

// ReadFrom 读一个数据报，来源一律是 Flow.Src。
func (u *udpFlow) ReadFrom(p []byte) (int, net.Addr, error) {
	n, err := u.UDPConn.Read(p)
	if err != nil {
		return n, nil, err
	}
	u.touch()
	return n, u.peer, nil
}

// WriteTo 写一个数据报给 Flow.Src（忽略 addr）。
func (u *udpFlow) WriteTo(p []byte, _ net.Addr) (int, error) {
	n, err := u.UDPConn.Write(p)
	if err == nil {
		u.touch()
	}
	return n, err
}

func (u *udpFlow) Read(p []byte) (int, error) {
	n, err := u.UDPConn.Read(p)
	if err == nil {
		u.touch()
	}
	return n, err
}

func (u *udpFlow) Write(p []byte) (int, error) {
	n, err := u.UDPConn.Write(p)
	if err == nil {
		u.touch()
	}
	return n, err
}

func (u *udpFlow) Close() error {
	u.mu.Lock()
	u.closed = true
	u.timer.Stop()
	u.mu.Unlock()
	return u.UDPConn.Close()
}
