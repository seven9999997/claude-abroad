package vmhost

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// backend 起一个 TCP 服务：读到 EOF 后回 "got:<内容>"。
func backend(t *testing.T) net.Listener {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b, _ := io.ReadAll(c)
				_, _ = c.Write([]byte("got:" + string(b)))
			}()
		}
	}()
	return l
}

// noHalfClose 隐藏 CloseWrite，模拟 vz 的 vsock 连接。
type noHalfClose struct{ net.Conn }

func startBridge(t *testing.T, dial func(context.Context) (net.Conn, error)) (string, context.CancelFunc, chan error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Bridge(ctx, l, dial) }()
	t.Cleanup(cancel)
	return l.Addr().String(), cancel, done
}

func TestBridgeHalfClose(t *testing.T) {
	be := backend(t)
	addr, _, _ := startBridge(t, func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", be.Addr().String())
	})
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("hello"))
	_ = c.(*net.TCPConn).CloseWrite()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	b, err := io.ReadAll(c)
	if err != nil || string(b) != "got:hello" {
		t.Fatalf("半关闭后应能读到应答：%q %v", b, err)
	}
}

func TestBridgeWithoutHalfCloseFallsBackToClose(t *testing.T) {
	be := backend(t)
	addr, _, _ := startBridge(t, func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		c, err := d.DialContext(ctx, "tcp", be.Addr().String())
		if err != nil {
			return nil, err
		}
		return noHalfClose{c}, nil
	})
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("hello"))
	_ = c.(*net.TCPConn).CloseWrite()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	b, err := io.ReadAll(c)
	if err != nil || len(b) != 0 {
		t.Fatalf("后端不支持半关闭时应全关（客户端读到 EOF，没有应答）：%q %v", b, err)
	}
}

func TestBridgeRequestResponseWithoutHalfClose(t *testing.T) {
	// 常规的请求-应答（客户端不半关闭）在全关闭退化下照常工作。
	addr, _, _ := startBridge(t, func(context.Context) (net.Conn, error) {
		a, b := net.Pipe()
		go func() {
			buf := make([]byte, 5)
			_, _ = io.ReadFull(b, buf)
			_, _ = b.Write([]byte("pong:" + string(buf)))
			b.Close()
		}()
		return noHalfClose{a}, nil
	})
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("hello"))
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	b, _ := io.ReadAll(c)
	if string(b) != "pong:hello" {
		t.Fatalf("应答：%q", b)
	}
}

func TestBridgeDialFailureClosesClient(t *testing.T) {
	addr, _, _ := startBridge(t, func(context.Context) (net.Conn, error) { return nil, ErrNotRunning })
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := c.Read(make([]byte, 1))
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("拨号失败时客户端应立即读到 EOF：%d %v", n, err)
	}
}

func TestBridgeCancelClosesInFlight(t *testing.T) {
	held := make(chan net.Conn, 1)
	addr, cancel, done := startBridge(t, func(context.Context) (net.Conn, error) {
		a, b := net.Pipe()
		held <- b
		return a, nil
	})
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	<-held
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Bridge 没有返回")
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || strings.Contains(err.Error(), "timeout") {
		t.Fatalf("ctx 结束后在途连接应被关闭：%v", err)
	}
	if _, err := net.Dial("tcp", addr); err == nil {
		t.Fatal("ctx 结束后应不再监听")
	}
}
