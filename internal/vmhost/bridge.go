package vmhost

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// BridgeDialTimeout 是桥接时拨 vsock 的上限。
const BridgeDialTimeout = 5 * time.Second

type closeWriter interface{ CloseWrite() error }

// Bridge 把 l 上的每条连接转到 dial 拨出的连接（ctl.sock → vsock 1081，设计文档 4.4）。
// dial 每次都取"当前这台"虚拟机（Supervisor.Dial），所以虚拟机重启后新连接自动到新虚拟机；
// 拨号失败就直接关掉客户端连接。连接只由宿主发起：这里不监听来自虚拟机的连接。
//
// 一个方向读到 EOF 时，如果另一端支持 CloseWrite 就半关闭，否则两端全关。
// vz 的 vsock 连接不支持 CloseWrite，所以 ctl.sock 的客户端不得依赖半关闭
// （先关写方向再读应答）；HTTP 客户端不会这样做。
//
// ctx 结束时关闭监听和所有在途连接，等全部 goroutine 退出后返回。
func Bridge(ctx context.Context, l net.Listener, dial func(context.Context) (net.Conn, error)) error {
	var (
		mu    sync.Mutex
		conns = map[net.Conn]bool{}
		wg    sync.WaitGroup
	)
	track := func(c net.Conn) bool {
		mu.Lock()
		defer mu.Unlock()
		if ctx.Err() != nil {
			c.Close()
			return false
		}
		conns[c] = true
		return true
	}
	untrack := func(c net.Conn) {
		mu.Lock()
		delete(conns, c)
		mu.Unlock()
		c.Close()
	}
	stop := context.AfterFunc(ctx, func() {
		l.Close()
		mu.Lock()
		for c := range conns {
			c.Close()
		}
		mu.Unlock()
	})
	defer stop()

	var err error
	for {
		c, aerr := l.Accept()
		if aerr != nil {
			if ctx.Err() == nil && !errors.Is(aerr, net.ErrClosed) {
				err = aerr
			}
			break
		}
		if !track(c) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer untrack(c)
			dctx, cancel := context.WithTimeout(ctx, BridgeDialTimeout)
			v, derr := dial(dctx)
			cancel()
			if derr != nil {
				return
			}
			if !track(v) {
				return
			}
			defer untrack(v)
			proxy(c, v)
		}()
	}
	l.Close()
	wg.Wait()
	return err
}

// proxy 双向拷贝，两个方向都结束后返回。
func proxy(a, b net.Conn) {
	var wg sync.WaitGroup
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(closeWriter); ok {
			if cw.CloseWrite() == nil {
				return
			}
		}
		// 不支持半关闭：两端全关，另一个方向也随之结束。
		dst.Close()
		src.Close()
	}
	wg.Add(2)
	go cp(b, a)
	go cp(a, b)
	wg.Wait()
}
