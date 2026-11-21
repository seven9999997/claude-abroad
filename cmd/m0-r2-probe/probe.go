package main

import (
	"claude-abroad/internal/fdpass"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/miekg/dns"
)

// result 是一次探测的结果，打印成一行 RESULT。
type result struct {
	OK     bool
	Kind   string
	Detail string
}

func (r result) String() string {
	return fmt.Sprintf("RESULT ok=%t kind=%s detail=%s", r.OK, r.Kind, strings.ReplaceAll(r.Detail, "\n", " "))
}

func okResult(detail string) result { return result{OK: true, Kind: "ok", Detail: detail} }

// classify 把网络错误归类。refused 说明包到达了对方（对方回了 RST），
// 对 pf 测试来说等同于"放行了"。
func classify(err error) result {
	var ne net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return result{OK: true, Kind: "refused", Detail: err.Error()}
	case errors.As(err, &ne) && ne.Timeout(), errors.Is(err, context.DeadlineExceeded):
		return result{Kind: "timeout", Detail: err.Error()}
	default:
		return result{Kind: "error", Detail: err.Error()}
	}
}

// boundControl 返回一个 net.Dialer/ListenConfig 的 Control 函数：iface 非空时
// 用 IP_BOUND_IF 把 socket 绑定到该网卡。
func boundControl(iface string) (func(network, address string, c syscall.RawConn) error, error) {
	if iface == "" {
		return nil, nil
	}
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("找不到网卡 %s: %w", iface, err)
	}
	return func(network, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = bindToInterface(int(fd), network, ifi.Index)
		}); err != nil {
			return err
		}
		return serr
	}, nil
}

// dnsQuery 生成一个查询 www.apple.com A 记录的 DNS 报文。
func dnsQuery() (*dns.Msg, []byte, error) {
	m := new(dns.Msg)
	m.SetQuestion("www.apple.com.", dns.TypeA)
	b, err := m.Pack()
	return m, b, err
}

// checkDNSReply 判断 b 是否是对 q 的回应（只比对 ID 和 QR 位，不关心内容）。
func checkDNSReply(q *dns.Msg, b []byte) error {
	r := new(dns.Msg)
	if err := r.Unpack(b); err != nil {
		return fmt.Errorf("回应不是 DNS 报文: %w", err)
	}
	if r.Id != q.Id || !r.Response {
		return fmt.Errorf("回应与查询不匹配")
	}
	return nil
}

// useConn 在一条已经建立的连接上收发数据：TCP 做 TLS 握手，UDP 发 DNS 查询。
func useConn(c net.Conn, sni string, timeout time.Duration) result {
	_ = c.SetDeadline(time.Now().Add(timeout))
	switch c.(type) {
	case *net.TCPConn:
		tc := tls.Client(c, &tls.Config{ServerName: sni})
		if err := tc.Handshake(); err != nil {
			return classify(fmt.Errorf("TLS 握手: %w", err))
		}
		return okResult("TLS 握手完成")
	case *net.UDPConn:
		q, b, err := dnsQuery()
		if err != nil {
			return classify(err)
		}
		if _, err := c.Write(b); err != nil {
			return classify(fmt.Errorf("发送 DNS 查询: %w", err))
		}
		buf := make([]byte, 1500)
		n, err := c.Read(buf)
		if err != nil {
			return classify(fmt.Errorf("等待 DNS 回应: %w", err))
		}
		if err := checkDNSReply(q, buf[:n]); err != nil {
			return classify(err)
		}
		return okResult("收到 DNS 回应")
	default:
		return result{Kind: "error", Detail: fmt.Sprintf("不支持的连接类型 %T", c)}
	}
}

func cmdDial(args []string) error {
	fs := flag.NewFlagSet("dial", flag.ExitOnError)
	netw := fs.String("net", "tcp", "tcp | udp | udp-sendto | tcp6")
	to := fs.String("to", "", "目标 ip:port，udp-sendto 可用逗号分隔多个")
	iface := fs.String("iface", "", "用 IP_BOUND_IF 绑定的网卡，空表示不绑定")
	sni := fs.String("sni", "dns.alidns.com", "TCP 用例做 TLS 握手时的 SNI；为 - 时只做 TCP 握手")
	timeout := fs.Duration("timeout", 3*time.Second, "超时")
	_ = fs.Parse(args)
	if *to == "" {
		return errors.New("缺少 -to")
	}
	ctrl, err := boundControl(*iface)
	if err != nil {
		return err
	}
	fmt.Println(dial(*netw, *to, ctrl, *sni, *timeout))
	return nil
}

func dial(netw, to string, ctrl func(string, string, syscall.RawConn) error, sni string, timeout time.Duration) result {
	if netw == "udp-sendto" {
		return dialSendTo(strings.Split(to, ","), ctrl, timeout)
	}
	d := net.Dialer{Timeout: timeout, Control: ctrl}
	c, err := d.Dial(netw, to)
	if err != nil {
		return classify(err)
	}
	defer c.Close()
	if strings.HasPrefix(netw, "tcp") && sni == "-" {
		return okResult("TCP 握手完成")
	}
	return useConn(c, sni, timeout)
}

// dialSendTo 用一个未 connect 的 UDP socket 向多个目标发 DNS 查询，
// 全部收到回应才算通过。
func dialSendTo(targets []string, ctrl func(string, string, syscall.RawConn) error, timeout time.Duration) result {
	lc := net.ListenConfig{Control: ctrl}
	pc, err := lc.ListenPacket(context.Background(), "udp4", ":0")
	if err != nil {
		return classify(err)
	}
	defer pc.Close()
	q, b, err := dnsQuery()
	if err != nil {
		return classify(err)
	}
	want := map[string]bool{}
	for _, t := range targets {
		ua, err := net.ResolveUDPAddr("udp4", t)
		if err != nil {
			return classify(err)
		}
		want[ua.String()] = false
		if _, err := pc.WriteTo(b, ua); err != nil {
			return classify(fmt.Errorf("sendto %s: %w", ua, err))
		}
	}
	_ = pc.SetReadDeadline(time.Now().Add(timeout))
	buf := make([]byte, 1500)
	got := 0
	for got < len(want) {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			r := classify(err)
			r.OK = false
			r.Detail = fmt.Sprintf("收到 %d/%d 个目标的回应: %s", got, len(want), err)
			return r
		}
		if seen, ok := want[from.String()]; ok && !seen && checkDNSReply(q, buf[:n]) == nil {
			want[from.String()] = true
			got++
		}
	}
	return okResult(fmt.Sprintf("收到全部 %d 个目标的回应", got))
}

func cmdListen(args []string) error {
	fs := flag.NewFlagSet("listen", flag.ExitOnError)
	addr := fs.String("addr", "[::1]:0", "监听地址")
	timeout := fs.Duration("timeout", 3*time.Second, "超时")
	_ = fs.Parse(args)
	fmt.Println(loopback(*addr, *timeout))
	return nil
}

// loopback 在 addr 上监听并自己连上去，收发一次数据。
func loopback(addr string, timeout time.Duration) result {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return result{Kind: "error", Detail: err.Error()}
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(timeout))
		_, _ = io.Copy(c, io.LimitReader(c, 4))
	}()
	c, err := net.DialTimeout("tcp", ln.Addr().String(), timeout)
	if err != nil {
		return classify(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(timeout))
	if _, err := c.Write([]byte("ping")); err != nil {
		return classify(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil {
		return classify(err)
	}
	return okResult("回环收发正常 " + ln.Addr().String())
}

func cmdFDSend(args []string) error {
	fs := flag.NewFlagSet("fdsend", flag.ExitOnError)
	sock := fs.String("sock", "", "fdrecv 监听的 unix socket")
	netw := fs.String("net", "udp", "tcp | udp")
	to := fs.String("to", "", "目标 ip:port")
	iface := fs.String("iface", "", "用 IP_BOUND_IF 绑定的网卡")
	presend := fs.Bool("presend", false, "UDP：传 fd 之前先发一个 DNS 查询（不等回应）")
	timeout := fs.Duration("timeout", 3*time.Second, "超时")
	_ = fs.Parse(args)
	if *sock == "" || *to == "" {
		return errors.New("缺少 -sock 或 -to")
	}
	ctrl, err := boundControl(*iface)
	if err != nil {
		return err
	}
	d := net.Dialer{Timeout: *timeout, Control: ctrl}
	c, err := d.Dial(*netw, *to)
	if err != nil {
		// TCP 在 connect 阶段就被拦了：fd 传不过去，直接报告。
		fmt.Println(classify(fmt.Errorf("fdsend connect: %w", err)))
		return nil
	}
	defer c.Close()
	if *presend && *netw == "udp" {
		_, b, err := dnsQuery()
		if err != nil {
			return err
		}
		if _, err := c.Write(b); err != nil {
			fmt.Println(classify(fmt.Errorf("fdsend presend: %w", err)))
			return nil
		}
	}
	f, err := c.(interface{ File() (*os.File, error) }).File()
	if err != nil {
		return err
	}
	defer f.Close()
	uc, err := net.DialTimeout("unix", *sock, *timeout)
	if err != nil {
		return fmt.Errorf("连接 fdrecv: %w", err)
	}
	defer uc.Close()
	if err := fdpass.Send(uc.(*net.UnixConn), nil, f); err != nil {
		return fmt.Errorf("发送 fd: %w", err)
	}
	// 等 fdrecv 用完这个 fd 并回传结果。
	_ = uc.SetReadDeadline(time.Now().Add(*timeout * 3))
	reply, err := io.ReadAll(uc)
	if err != nil && len(reply) == 0 {
		return fmt.Errorf("等待 fdrecv 结果: %w", err)
	}
	fmt.Print(string(reply))
	return nil
}

func cmdFDRecv(args []string) error {
	fs := flag.NewFlagSet("fdrecv", flag.ExitOnError)
	sock := fs.String("sock", "", "监听的 unix socket 路径")
	sni := fs.String("sni", "dns.alidns.com", "TCP 用例 TLS 握手的 SNI")
	timeout := fs.Duration("timeout", 3*time.Second, "超时")
	wait := fs.Duration("wait", 15*time.Second, "等 fdsend 连上来的最长时间")
	_ = fs.Parse(args)
	if *sock == "" {
		return errors.New("缺少 -sock")
	}
	_ = os.Remove(*sock)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: *sock, Net: "unix"})
	if err != nil {
		return err
	}
	defer os.Remove(*sock)
	defer ln.Close()
	// 允许另一个身份的 fdsend 连上来（目录权限由脚本控制）。
	if err := os.Chmod(*sock, 0o666); err != nil {
		return err
	}
	_ = ln.SetDeadline(time.Now().Add(*wait))
	uc, err := ln.AcceptUnix()
	if err != nil {
		return fmt.Errorf("等待 fdsend: %w", err)
	}
	defer uc.Close()
	_, f, err := fdpass.Recv(uc, 0)
	if err == nil && f == nil {
		err = errors.New("没有收到 fd")
	}
	if err != nil {
		return fmt.Errorf("接收 fd: %w", err)
	}
	defer f.Close()
	c, err := net.FileConn(f)
	if err != nil {
		return fmt.Errorf("fd 转连接: %w", err)
	}
	defer c.Close()
	r := useConn(c, *sni, *timeout)
	line := r.String() + fmt.Sprintf(" user=%d\n", os.Getuid())
	fmt.Print(line)
	_, _ = uc.Write([]byte(line))
	return nil
}
