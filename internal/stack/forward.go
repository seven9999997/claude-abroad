package stack

import (
	"context"
	"io"
	"net"
	"sync"

	"claude-abroad/internal/wire"
)

// pipeTCP 双向转发，半关闭照实透传：应用 FIN → 上游 CloseWrite；上游 EOF → 对应用 CloseWrite（发 FIN，不改成 RST）。
// prefix 是读首包时已经从应用读到的字节，先原样发给上游。proxied 为真时统计“ok 之后零字节结束”（只用于诊断）。
func (p *Proxy) pipeTCP(ctx context.Context, conn, up net.Conn, prefix []byte, proxied bool) {
	if len(prefix) > 0 {
		if _, err := up.Write(prefix); err != nil {
			abort(up)
			abort(conn)
			return
		}
	}
	finished := make(chan struct{}, 2)
	// 读到正常 EOF 只半关闭对端；读到错误（RST、被关、超时）说明这一侧已经没了，两端一起强制断开，
	// 免得另一个方向一直等着、映射引用一直不释放。
	go func() {
		if _, err := io.Copy(up, conn); err != nil {
			abort(up)
			abort(conn)
		} else {
			halfClose(up)
		}
		finished <- struct{}{}
	}()
	go func() {
		n, err := io.Copy(conn, up)
		if proxied && n == 0 && err == nil {
			p.st.zeroEOF.Add(1)
		}
		if err != nil {
			abort(up)
			abort(conn)
		} else {
			halfClose(conn)
		}
		finished <- struct{}{}
	}()
	for remaining := 2; remaining > 0; {
		select {
		case <-finished:
			remaining--
		case <-ctx.Done():
			abort(conn)
			abort(up)
			// 只等还没结束的那几个方向（M2-9b-2 查出：一个方向先正常结束、之后流才被取消时，固定收两次会永远卡住）。
			for ; remaining > 0; remaining-- {
				<-finished
			}
			return
		}
	}
}

// udpPending 缓存判定期间到达的后续数据报（最多 8 个或 16 KiB，超出的丢弃），判定后按到达顺序送出。
type udpPending struct {
	mu     sync.Mutex
	q      [][]byte
	bytes  int
	send   func([]byte) error // 判定后设置；nil 表示还在判定
	failed bool
	stop   func()
}

// udpReader 从应用读后续数据报：判定前缓存，判定后直接送上游。fail 后丢弃并退出。
func (p *Proxy) udpReader(pc net.PacketConn, u *udpPending) {
	buf := make([]byte, wire.MaxDatagram)
	for {
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			u.mu.Lock()
			stop := u.stop
			u.failed = true
			u.mu.Unlock()
			if stop != nil {
				stop()
			}
			return
		}
		u.mu.Lock()
		switch {
		case u.failed:
			u.mu.Unlock()
			return
		case u.send == nil:
			if len(u.q) < udpPendingMax && u.bytes+n <= udpPendingMaxBytes {
				u.q = append(u.q, append([]byte(nil), buf[:n]...))
				u.bytes += n
			} else {
				p.st.udpPendingDropped.Add(1)
			}
			u.mu.Unlock()
		default:
			send := u.send
			u.mu.Unlock()
			if send(buf[:n]) != nil {
				u.mu.Lock()
				stop := u.stop
				u.mu.Unlock()
				if stop != nil {
					stop()
				}
				return
			}
		}
	}
}

// fail 判定为拦截或开流失败：丢掉缓存，读到的后续数据报也丢弃。
func (u *udpPending) fail() {
	u.mu.Lock()
	u.failed, u.q = true, nil
	u.mu.Unlock()
}

// pipeUDP 转发一条 UDP 流：先送第一个数据报和缓存，再双向转发。代理用 wire 的数据报分帧；
// 直连是拨号模块给的已连接 UDP 套接字（一次读写一个数据报）。回包的源地址由栈固定为原目的地址。
func (p *Proxy) pipeUDP(ctx context.Context, pc net.PacketConn, up net.Conn, first []byte, u *udpPending, proxied bool) {
	var send func([]byte) error
	var recv func([]byte) (int, error)
	if proxied {
		dc := wire.NewDatagramConn(up)
		send, recv = dc.WriteDatagram, dc.ReadDatagram
	} else {
		send = func(b []byte) error { _, err := up.Write(b); return err }
		recv = up.Read
	}
	stop := func() { pc.Close(); abort(up) }
	stopOnCancel := context.AfterFunc(ctx, stop)
	defer stopOnCancel()
	// 先起“上游 → 应用”方向，再送第一个数据报和缓存：上游可能要等回包被读走才收下一个（同步的连接）。
	down := make(chan struct{})
	go func() {
		defer close(down)
		buf := make([]byte, wire.MaxDatagram)
		for {
			n, err := recv(buf)
			if err != nil {
				stop()
				return
			}
			if _, err := pc.WriteTo(buf[:n], nil); err != nil {
				stop()
				return
			}
		}
	}()
	if send(first) != nil {
		stop()
		u.fail()
		<-down
		return
	}
	u.mu.Lock()
	for _, b := range u.q { // 按到达顺序送出缓存（持锁：保证后到的不会插队）
		if send(b) != nil {
			break
		}
	}
	u.q, u.bytes, u.send, u.stop = nil, 0, send, stop
	failed := u.failed
	u.mu.Unlock()
	if failed { // 应用一侧在判定期间已经结束
		stop()
	}
	<-down
}
