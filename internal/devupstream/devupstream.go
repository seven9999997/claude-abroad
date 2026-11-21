// Package devupstream 是开发工具 abroad-dev 的“开发用上游”（不进发布包）：
// 本机回环上的 SOCKS5 服务，充当“本机其他代理软件”，给 M1 的阶段 C 用。
//
// 它**绝不向外拨号**：所有应答都在本地固定生成（有源码检查测试守着）。
//   - 认证：对方提供了“无认证”就用它；只提供“用户名口令”时接受任意凭据（进程内测试里 agent 的 SOCKS 客户端
//     只提供这一种）；两种都没有就回 0xFF 并关闭。只听本机回环，所以接受任意凭据不增加风险；
//   - CONNECT：对任何目标都回成功，然后当最小的 HTTP 服务——/generate_204 回 204，其余回 404。
//     读不出 HTTP（例如 TLS）就关掉；
//   - UDP ASSOCIATE：中继开在与 TCP 相同的端口上（设计 4.5：本机代理当主代理时，只有 UDP 中继与 TCP
//     同端口的才能转 UDP），BND 回 0.0.0.0:端口；收到数据报原样回显，回包来源写成请求的目标。
//
// 每个请求记一行 JSON：{"cmd","atyp","host","port","result"}（只有测试域名和文档网段地址，不算敏感）。
// 只依赖标准库。
package devupstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// Entry 是一行请求日志。
type Entry struct {
	Cmd    string `json:"cmd"`
	Atyp   int    `json:"atyp"`
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Result string `json:"result"`
}

// Server 是开发用上游。
type Server struct {
	Log  io.Writer
	Name string // 非空时 /whoami 回这个名字（M3-7：分辨请求经的是哪个出口）
	mu   sync.Mutex
}

func (s *Server) record(e Entry) {
	if s.Log == nil {
		return
	}
	b, _ := json.Marshal(e)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.Log.Write(append(b, '\n'))
}

// Listen 在 127.0.0.1 上开同端口的 TCP 和 UDP（port 为 0 时由系统分配，UDP 跟随 TCP 的端口）。
func Listen(port int) (net.Listener, *net.UDPConn, error) {
	var lastErr error
	for range 10 {
		l, err := net.Listen("tcp", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port)).String())
		if err != nil {
			return nil, nil, err
		}
		p := l.Addr().(*net.TCPAddr).Port
		u, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: p})
		if err == nil {
			return l, u, nil
		}
		l.Close()
		lastErr = err
		if port != 0 {
			break
		}
	}
	return nil, nil, lastErr
}

// Serve 运行到 ctx 结束。
func (s *Server) Serve(ctx context.Context, l net.Listener, u *net.UDPConn) error {
	stop := context.AfterFunc(ctx, func() { l.Close(); u.Close() })
	defer stop()
	port := l.Addr().(*net.TCPAddr).Port
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); s.serveUDP(u) }()
	defer wg.Wait()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer c.Close()
			stopc := context.AfterFunc(ctx, func() { c.Close() })
			defer stopc()
			s.handle(c, port)
		}()
	}
}

func readAddr(r io.Reader) (atyp byte, host string, port int, err error) {
	var t [1]byte
	if _, err = io.ReadFull(r, t[:]); err != nil {
		return
	}
	atyp = t[0]
	switch atyp {
	case 1:
		var a [4]byte
		if _, err = io.ReadFull(r, a[:]); err != nil {
			return
		}
		host = netip.AddrFrom4(a).String()
	case 4:
		var a [16]byte
		if _, err = io.ReadFull(r, a[:]); err != nil {
			return
		}
		host = netip.AddrFrom16(a).String()
	case 3:
		var n [1]byte
		if _, err = io.ReadFull(r, n[:]); err != nil {
			return
		}
		b := make([]byte, n[0])
		if _, err = io.ReadFull(r, b); err != nil {
			return
		}
		host = string(b)
	default:
		err = errors.New("地址类型不支持")
		return
	}
	var p [2]byte
	if _, err = io.ReadFull(r, p[:]); err != nil {
		return
	}
	port = int(binary.BigEndian.Uint16(p[:]))
	return
}

func (s *Server) handle(c net.Conn, udpPort int) {
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	var g [2]byte
	if _, err := io.ReadFull(c, g[:]); err != nil || g[0] != 5 {
		return
	}
	methods := make([]byte, g[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return
	}
	switch {
	case bytes.IndexByte(methods, 0) >= 0:
		_, _ = c.Write([]byte{5, 0}) // 无认证（mihomo 走这条）
	case bytes.IndexByte(methods, 2) >= 0:
		// 用户名口令（RFC 1929），接受任意凭据：给进程内测试里的 agent.SocksDialer 用（它只提供方法 2）。
		_, _ = c.Write([]byte{5, 2})
		var vl [2]byte
		if _, err := io.ReadFull(c, vl[:]); err != nil || vl[0] != 1 {
			return
		}
		if _, err := io.CopyN(io.Discard, c, int64(vl[1])); err != nil {
			return
		}
		var pl [1]byte
		if _, err := io.ReadFull(c, pl[:]); err != nil {
			return
		}
		if _, err := io.CopyN(io.Discard, c, int64(pl[0])); err != nil {
			return
		}
		_, _ = c.Write([]byte{1, 0})
	default:
		_, _ = c.Write([]byte{5, 0xff})
		return
	}
	var req [3]byte
	if _, err := io.ReadFull(c, req[:]); err != nil {
		return
	}
	atyp, host, port, err := readAddr(c)
	if err != nil {
		return
	}
	switch req[1] {
	case 1: // CONNECT：回成功，再当最小 HTTP 服务
		_, _ = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
		e := Entry{Cmd: "connect", Atyp: int(atyp), Host: host, Port: port}
		r, err := http.ReadRequest(bufio.NewReader(c))
		if err != nil {
			e.Result = "closed"
			s.record(e)
			return
		}
		r.Body.Close()
		if r.URL.Path == "/generate_204" {
			_, _ = io.WriteString(c, "HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n")
			e.Result = "204"
		} else if r.URL.Path == "/whoami" && s.Name != "" {
			_, _ = fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(s.Name), s.Name)
			e.Result = "whoami"
		} else {
			_, _ = io.WriteString(c, "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			e.Result = "404"
		}
		s.record(e)
	case 3: // UDP ASSOCIATE：中继与 TCP 同端口，BND 0.0.0.0:端口
		rep := binary.BigEndian.AppendUint16([]byte{5, 0, 0, 1, 0, 0, 0, 0}, uint16(udpPort))
		_, _ = c.Write(rep)
		_ = c.SetDeadline(time.Time{})
		_, _ = io.Copy(io.Discard, c) // 控制连接保持到对端关闭
	default:
		_, _ = c.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
	}
}

// serveUDP 原样回显：回包头和请求头相同（来源 = 请求的目标，ATYP 和名字不变）。只收回环来源。
func (s *Server) serveUDP(u *net.UDPConn) {
	buf := make([]byte, 65536)
	for {
		n, from, err := u.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if !from.IP.IsLoopback() || n < 4 || buf[0] != 0 || buf[1] != 0 || buf[2] != 0 {
			continue
		}
		hdrStart := 3
		r := &sliceReader{b: buf[hdrStart:n]}
		atyp, host, port, err := readAddr(r)
		if err != nil {
			continue
		}
		_, _ = u.WriteToUDP(append([]byte(nil), buf[:n]...), from)
		s.record(Entry{Cmd: "udp", Atyp: int(atyp), Host: host, Port: port, Result: "echo"})
	}
}

type sliceReader struct{ b []byte }

func (r *sliceReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}
