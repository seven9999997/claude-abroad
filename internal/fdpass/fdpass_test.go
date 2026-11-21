//go:build unix

package fdpass

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// openFDs 数本进程打开的 fd（/dev/fd 的条目数）。
func openFDs(t *testing.T) int {
	t.Helper()
	es, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skip("读不了 /dev/fd：", err)
	}
	return len(es)
}

// pair 在临时目录的 unix socket 上建一对连接（测试结束时关闭）。
func pair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	a, b := pair0(t)
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

// pair0 同 pair，但由调用方关闭。
func pair0(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	dir, err := os.MkdirTemp("", "fdp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "s"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan *net.UnixConn, 1)
	go func() { c, _ := ln.AcceptUnix(); ch <- c }()
	a, err := net.DialUnix("unix", nil, ln.Addr().(*net.UnixAddr))
	if err != nil {
		t.Fatal(err)
	}
	b := <-ch
	return a, b
}

func TestPassTCP(t *testing.T) {
	a, b := pair(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := ln.Accept()
	defer srv.Close()
	f, _ := c.(*net.TCPConn).File()
	c.Close()
	if err := Send(a, []byte(`{"ok":true}`), f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	msg, got, err := Recv(b, 0)
	if err != nil || string(msg) != `{"ok":true}` || got == nil {
		t.Fatalf("%q %v %v", msg, got, err)
	}
	gc, err := net.FileConn(got)
	got.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer gc.Close()
	if _, err := gc.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	_ = srv.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := srv.Read(buf); err != nil || string(buf) != "ping" {
		t.Fatalf("经传过来的 fd 发送失败：%q %v", buf, err)
	}
	// close-on-exec 已设置。
	gf, _ := gc.(*net.TCPConn).File()
	defer gf.Close()
	if fl, err := unix.FcntlInt(gf.Fd(), unix.F_GETFD, 0); err != nil || fl&unix.FD_CLOEXEC == 0 {
		t.Fatalf("应设了 close-on-exec：%v %v", fl, err)
	}
}

func TestPassConnectedUDP(t *testing.T) {
	a, b := pair(t)
	srv, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	defer srv.Close()
	cl, _ := net.Dial("udp4", srv.LocalAddr().String())
	f, _ := cl.(*net.UDPConn).File()
	cl.Close()
	if err := Send(a, nil, f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	msg, got, err := Recv(b, 0)
	if err != nil || len(msg) != 0 || got == nil {
		t.Fatal(msg, got, err)
	}
	c, _ := net.FileConn(got)
	got.Close()
	defer c.Close()
	if _, err := c.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	_ = srv.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, from, err := srv.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "hi" || from.String() != c.LocalAddr().String() {
		t.Fatalf("%q %v %v", buf[:n], from, err)
	}
}

func TestNoFDAndLongMessage(t *testing.T) {
	a, b := pair(t)
	big := bytes.Repeat([]byte("x"), MaxMsg)
	if err := Send(a, big, nil); err != nil {
		t.Fatal(err)
	}
	msg, f, err := Recv(b, 0)
	if err != nil || f != nil || !bytes.Equal(msg, big) {
		t.Fatalf("%d %v %v", len(msg), f, err)
	}
	// 默认上限 1 KiB：超过的消息接收方拒收；给了更大的上限就能收。
	go func() { _ = Send(a, append(big, 'y'), nil) }()
	if _, _, err := Recv(b, 0); err == nil {
		t.Fatal("超过默认上限应拒收")
	}
	a2, b2 := pair(t)
	huge := bytes.Repeat([]byte("h"), MaxLimit-1)
	sent := make(chan error, 1)
	go func() { sent <- Send(a2, huge, nil) }() // 大消息超过 socket 缓冲，要边发边收
	if msg, _, err := Recv(b2, MaxLimit); err != nil || len(msg) != MaxLimit-1 {
		t.Fatalf("给了大上限应能收：%d %v", len(msg), err)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	if err := Send(a2, append(huge, 'x'), nil); err == nil {
		t.Fatal("超过 2 字节长度能表示的范围应拒绝发送")
	}
}

func TestFailuresDoNotLeak(t *testing.T) {
	before := openFDs(t)
	for i := 0; i < 20; i++ {
		func() {
			a, b := pair0(t)
			defer a.Close()
			defer b.Close()
			r1, w1, _ := os.Pipe()
			defer r1.Close()
			defer w1.Close()
			// 两个 fd：接收方应全部关掉并报错。
			rights := unix.UnixRights(int(r1.Fd()), int(w1.Fd()))
			if _, _, err := a.WriteMsgUnix([]byte{0, 1, 'x'}, rights, nil); err != nil {
				t.Fatal(err)
			}
			if _, f, err := Recv(b, 0); err == nil || f != nil {
				t.Fatal("两个 fd 应报错")
			}
			// 消息长度超过接收方给的上限：fd 也要关掉。
			if err := Send(a, bytes.Repeat([]byte("z"), 100), r1); err != nil {
				t.Fatal(err)
			}
			if _, f, err := Recv(b, 10); err == nil || f != nil {
				t.Fatal("超过上限应报错")
			}
			// fd 出现在消息中间。
			if _, err := a.Write([]byte{0, 3, 'a'}); err != nil {
				t.Fatal(err)
			}
			time.Sleep(5 * time.Millisecond)
			if _, _, err := a.WriteMsgUnix([]byte{'b', 'c'}, unix.UnixRights(int(r1.Fd())), nil); err != nil {
				t.Fatal(err)
			}
			if _, f, err := Recv(b, 0); err == nil || f != nil {
				t.Fatal("fd 在消息中间应报错")
			}
			// 消息没收完对端就关了。
			_ = Send(a, []byte("ok"), r1)
			a2, b2 := pair0(t)
			defer b2.Close()
			_, _ = a2.Write([]byte{0, 9, 'p'})
			a2.Close()
			if _, f, err := Recv(b2, 0); err == nil || f != nil {
				t.Fatal("没收完应报错")
			}
		}()
	}
	time.Sleep(20 * time.Millisecond)
	if after := openFDs(t); after > before+2 {
		t.Fatalf("fd 泄漏：之前 %d，之后 %d", before, after)
	}
}

// 回归（M3-4 测试发现）：读期限触发时 ReadMsgUnix 可能返回 -1，曾导致切片越界。
func TestRecvDeadline(t *testing.T) {
	a, b := pair(t)
	_ = a
	_ = b.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, f, err := Recv(b, 0); err == nil || f != nil {
		t.Fatal("读期限到了应报错")
	}
	_, _ = a.Write([]byte{0, 5, 'a'})
	_ = b.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, f, err := Recv(b, 0); err == nil || f != nil {
		t.Fatal("消息没收完、读期限到了应报错")
	}
}
