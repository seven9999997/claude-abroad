//go:build unix

package main

import (
	"claude-abroad/internal/fdpass"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestBuildRules(t *testing.T) {
	s, err := buildRules(ruleConfig{User: "_abroadm0", LAN: []string{"192.168.1.0/24", "224.0.0.0/4"}, VMIfs: []string{"bridge100"}})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if lines[0] != "table <abroad_lan> { 192.168.1.0/24, 224.0.0.0/4 }" {
		t.Errorf("table 行不对: %q", lines[0])
	}
	if !strings.HasPrefix(lines[len(lines)-1], "block drop out quick all") {
		t.Errorf("最后一条必须是兜底拦截: %q", lines[len(lines)-1])
	}
	if !strings.Contains(s, "user _abroadm0") || !strings.Contains(s, "on { bridge100 }") {
		t.Errorf("缺少 user 或网桥规则:\n%s", s)
	}
}

func TestBuildRulesWithoutLANOrVM(t *testing.T) {
	s, err := buildRules(ruleConfig{User: "nobody"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s, "table <abroad_lan> {  }\n") || strings.Contains(s, "m0_vmif") {
		t.Errorf("关闭局域网、没有网桥时规则不对:\n%s", s)
	}
}

func TestBuildRulesRejectsInjection(t *testing.T) {
	for _, cfg := range []ruleConfig{
		{User: "a b"},
		{User: "x\npass out all"},
		{User: "ok", LAN: []string{"1.2.3.4/24 }"}},
		{User: "ok", VMIfs: []string{"bridge100 }"}},
	} {
		if _, err := buildRules(cfg); err == nil {
			t.Errorf("应拒绝 %+v", cfg)
		}
	}
}

func TestLANCIDR(t *testing.T) {
	mk := func(s string) net.Addr {
		ip, n, err := net.ParseCIDR(s)
		if err != nil {
			t.Fatal(err)
		}
		n.IP = ip
		return n
	}
	got, err := lanCIDR([]net.Addr{mk("fe80::1/64"), mk("169.254.3.4/16"), mk("192.168.31.20/24")})
	if err != nil || got != "192.168.31.0/24" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := lanCIDR([]net.Addr{mk("127.0.0.1/8")}); err == nil {
		t.Fatal("只有回环地址时应报错")
	}
}

func TestFilterVMIfs(t *testing.T) {
	got := filterVMIfs([]string{"en0", "bridge0", "bridge101", "bridge100", "vmenet0", "utun3", "lo0"})
	want := "bridge100,bridge101,vmenet0"
	if strings.Join(got, ",") != want {
		t.Fatalf("got %v, want %s", got, want)
	}
}

func TestLabelDiff(t *testing.T) {
	before, err := parseLabels(strings.NewReader("m0_user 10 4 400 2 200 2 200 1\nm0_user 3 1 50 0 0 1 50 0\nm0_block_all 9 0 0 0 0 0 0 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if before["m0_user"].Packets != 5 {
		t.Fatalf("同名标签应相加，got %+v", before["m0_user"])
	}
	after, err := parseLabels(strings.NewReader("m0_user 12 8 800 4 400 4 400 1\nm0_user 3 1 50 0 0 1 50 0\nm0_block_all 10 2 120 0 0 2 120 0\nm0_lan 1 1 60 0 0 1 60 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := diffLabels(before, after); got != "m0_block_all:+2,m0_lan:+1,m0_user:+4" {
		t.Fatalf("got %q", got)
	}
	if got := diffLabels(after, after); got != "-" {
		t.Fatalf("没有变化时应为 -，got %q", got)
	}
	if _, err := parseLabels(strings.NewReader("m0_user x y\n")); err == nil {
		t.Fatal("非数字应报错")
	}
}

func TestFDPassing(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	conn := func(fd int) *net.UnixConn {
		f := os.NewFile(uintptr(fd), "pair")
		defer f.Close()
		c, err := net.FileConn(f)
		if err != nil {
			t.Fatal(err)
		}
		return c.(*net.UnixConn)
	}
	a, b := conn(fds[0]), conn(fds[1])
	defer a.Close()
	defer b.Close()

	// 传一个 UDP socket 过去，在接收端用它收发，证明拿到的是同一个 socket。
	srv, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	cl, err := net.Dial("udp4", srv.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	f, err := cl.(*net.UDPConn).File()
	if err != nil {
		t.Fatal(err)
	}
	cl.Close()
	if err := fdpass.Send(a, nil, f); err != nil {
		t.Fatal(err)
	}
	f.Close()
	_, got, err := fdpass.Recv(b, 0)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.FileConn(got)
	got.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	_ = srv.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := srv.ReadFrom(buf)
	if err != nil || string(buf[:n]) != "hi" {
		t.Fatalf("接收端没收到数据: %q %v", buf[:n], err)
	}
}

func TestLoopbackIPv4(t *testing.T) {
	if r := loopback("127.0.0.1:0", 2*time.Second); !r.OK {
		t.Fatal(r)
	}
}

func TestDNSReplyCheck(t *testing.T) {
	q, b, err := dnsQuery()
	if err != nil {
		t.Fatal(err)
	}
	if checkDNSReply(q, b) == nil {
		t.Fatal("查询报文本身不是回应，应报错")
	}
	r := q.Copy()
	r.Response = true
	rb, _ := r.Pack()
	if err := checkDNSReply(q, rb); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyRefused(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	r := dial("tcp4", addr, nil, "-", time.Second)
	if r.Kind != "refused" || !r.OK {
		t.Fatalf("连接被拒应归为 refused 且 ok=true: %v", r)
	}
}
