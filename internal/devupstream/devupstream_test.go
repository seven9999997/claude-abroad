package devupstream

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) entries(t *testing.T) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Entry
	for _, l := range strings.Split(strings.TrimSpace(s.b.String()), "\n") {
		if l == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func start(t *testing.T) (string, *syncBuf) {
	l, u, err := Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	log := &syncBuf{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { (&Server{Log: log}).Serve(ctx, l, u); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return l.Addr().String(), log
}

func TestConnectDomain204(t *testing.T) {
	addr, log := start(t)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write([]byte{5, 1, 0})
	io.ReadFull(c, make([]byte, 2))
	c.Write(append(append([]byte{5, 1, 0, 3, 13}, "generate.test"...), 0, 80))
	io.ReadFull(c, make([]byte, 10))
	c.Write([]byte("GET /generate_204 HTTP/1.1\r\nHost: generate.test\r\n\r\n"))
	got, _ := io.ReadAll(c)
	if !strings.HasPrefix(string(got), "HTTP/1.1 204") {
		t.Fatalf("应回 204：%q", got)
	}
	time.Sleep(20 * time.Millisecond)
	es := log.entries(t)
	if len(es) != 1 || es[0] != (Entry{Cmd: "connect", Atyp: 3, Host: "generate.test", Port: 80, Result: "204"}) {
		t.Fatalf("日志：%+v", es)
	}
}

func TestUDPEchoSamePort(t *testing.T) {
	addr, log := start(t)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write([]byte{5, 1, 0})
	io.ReadFull(c, make([]byte, 2))
	c.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0})
	rep := make([]byte, 10)
	io.ReadFull(c, rep)
	tcpPort := c.RemoteAddr().(*net.TCPAddr).Port
	if !bytes.Equal(rep[4:8], []byte{0, 0, 0, 0}) || int(binary.BigEndian.Uint16(rep[8:])) != tcpPort {
		t.Fatalf("BND 应是 0.0.0.0:%d：%v", tcpPort, rep)
	}
	u, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: tcpPort})
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	pkt := append(append([]byte{0, 0, 0, 3, 8}, "udp.test"...), 0, 53, 'h', 'i')
	u.Write(pkt)
	u.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 128)
	n, err := u.Read(buf)
	if err != nil || !bytes.Equal(buf[:n], pkt) {
		t.Fatalf("应原样回显（来源 = 请求目标）：%v %v", buf[:n], err)
	}
	time.Sleep(20 * time.Millisecond)
	es := log.entries(t)
	if len(es) != 1 || es[0] != (Entry{Cmd: "udp", Atyp: 3, Host: "udp.test", Port: 53, Result: "echo"}) {
		t.Fatalf("日志：%+v", es)
	}
}

// 源码检查：这个包绝不向外拨号。
func TestNoDialing(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, p := range files {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				switch sel.Sel.Name {
				case "Dial", "DialContext", "DialUDP", "DialTCP", "DialIP", "DialUnix", "DialTimeout":
					t.Errorf("%s 用了 %s：开发上游不许向外拨号", p, sel.Sel.Name)
				}
			}
			return true
		})
	}
}

func TestGreetingMethods(t *testing.T) {
	addr, _ := start(t)
	greet := func(methods ...byte) (net.Conn, []byte) {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(3 * time.Second))
		c.Write(append([]byte{5, byte(len(methods))}, methods...))
		r := make([]byte, 2)
		if _, err := io.ReadFull(c, r); err != nil {
			t.Fatal(err)
		}
		return c, r
	}
	// 方法 0 和 2 都提供：仍选 0（mihomo 的路径不变）。
	c, r := greet(2, 0)
	if !bytes.Equal(r, []byte{5, 0}) {
		t.Fatalf("提供了方法 0 应选 0：%v", r)
	}
	c.Close()
	// 只提供方法 2：任意凭据都接受，之后 CONNECT 正常。
	c, r = greet(2)
	if !bytes.Equal(r, []byte{5, 2}) {
		t.Fatalf("只提供方法 2 应选 2：%v", r)
	}
	c.Write(append(append(append([]byte{1, 4}, "main"...), 6), "secret"...))
	ar := make([]byte, 2)
	if _, err := io.ReadFull(c, ar); err != nil || !bytes.Equal(ar, []byte{1, 0}) {
		t.Fatalf("子协商应回 {1,0}：%v %v", ar, err)
	}
	c.Write(append(append([]byte{5, 1, 0, 3, 13}, "generate.test"...), 0, 80))
	rep := make([]byte, 10)
	if _, err := io.ReadFull(c, rep); err != nil || rep[1] != 0 {
		t.Fatalf("认证后 CONNECT 应成功：%v %v", rep, err)
	}
	c.Close()
	// 两种都没有：回 0xFF 并关连接。
	c, r = greet(1, 3)
	if !bytes.Equal(r, []byte{5, 0xff}) {
		t.Fatalf("不支持的方法应回 0xFF：%v", r)
	}
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("回 0xFF 后应关连接")
	}
	c.Close()
}

// M3-7：配了名字时 /whoami 回名字（分辨请求经的是哪个出口）；没配时仍是 404。
func TestWhoami(t *testing.T) {
	for _, name := range []string{"wg-a", ""} {
		l, u, err := Listen(0)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { (&Server{Name: name}).Serve(ctx, l, u); close(done) }()
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(3 * time.Second))
		c.Write([]byte{5, 1, 0})
		io.ReadFull(c, make([]byte, 2))
		c.Write(append(append([]byte{5, 1, 0, 3, 11}, "whoami.test"...), 0, 80))
		io.ReadFull(c, make([]byte, 10))
		c.Write([]byte("GET /whoami HTTP/1.1\r\nHost: whoami.test\r\n\r\n"))
		got, _ := io.ReadAll(c)
		c.Close()
		cancel()
		<-done
		if name != "" && !strings.HasSuffix(string(got), "\r\n\r\n"+name) {
			t.Fatalf("应回名字 %s：%q", name, got)
		}
		if name == "" && !strings.HasPrefix(string(got), "HTTP/1.1 404") {
			t.Fatalf("没配名字时应是 404：%q", got)
		}
	}
}
