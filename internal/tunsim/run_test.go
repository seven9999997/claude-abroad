package tunsim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"

	"claude-abroad/internal/agent"
	"claude-abroad/internal/devupstream"
	"claude-abroad/internal/wire"
)

// 进程内整条链：tun.Pipe → stack → Proxy → wire.Dialer（net.Pipe）→ 真实 agent.DataPlane → devupstream（当 sing-box）。

type exits struct{ up bool }

func (e exits) Lookup(tag string) (bool, bool) { return tag == "main", e.up }

type pipeLn struct {
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func (l *pipeLn) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *pipeLn) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *pipeLn) Addr() net.Addr { return &net.UnixAddr{Name: "pipe", Net: "unix"} }
func (l *pipeLn) Dial(ctx context.Context) (net.Conn, error) {
	a, b := net.Pipe()
	select {
	case l.ch <- b:
		return a, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) entries(t *testing.T) []devupstream.Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []devupstream.Entry
	for _, l := range strings.Split(strings.TrimSpace(s.b.String()), "\n") {
		if l == "" {
			continue
		}
		var e devupstream.Entry
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// chain 起 devupstream 和 DataPlane，返回 wire.Dialer（做 Config.Opener）和上游日志。
func chain(t *testing.T, up bool) (*wire.Dialer, *syncBuf) {
	t.Helper()
	l, u, err := devupstream.Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	log := &syncBuf{}
	ctx, cancel := context.WithCancel(context.Background())
	upDone := make(chan struct{})
	go func() { (&devupstream.Server{Log: log}).Serve(ctx, l, u); close(upDone) }()
	dp := &agent.DataPlane{Exits: exits{up: up},
		Timing: agent.DataTiming{DialTimeout: 5 * time.Second, UDPIdle: 5 * time.Second},
		Socks: func(e string) agent.SocksDialer {
			return agent.SocksDialer{Addr: l.Addr().String(), User: e, Password: "0123456789abcdef0123456789abcdef"}
		}}
	pl := &pipeLn{ch: make(chan net.Conn), closed: make(chan struct{})}
	dpDone := make(chan struct{})
	go func() { dp.Serve(ctx, pl); close(dpDone) }()
	d := &wire.Dialer{Dial: pl.Dial, Config: wire.DefaultSessionConfig()}
	t.Cleanup(func() {
		d.Close()
		cancel()
		pl.Close()
		<-dpDone
		<-upDone
	})
	return d, log
}

func stepOf(r Report, name string) Step {
	for _, s := range r.Steps {
		if s.Name == name {
			return s
		}
	}
	return Step{Name: name, Err: "没有这一步"}
}

func TestRunAllStepsOK(t *testing.T) {
	d, log := chain(t, true)
	rep := Run(context.Background(), Config{Opener: d, Timeout: 10 * time.Second})
	b, _ := json.Marshal(rep)
	if !rep.OK || len(rep.Steps) != 7 {
		t.Fatalf("应全部 ok：%s", b)
	}
	if rep.OpensAfterGate != rep.OpensBeforeGate || rep.OpensBeforeGate == 0 {
		t.Fatalf("关闸门后 Opener 不应再被调用：%s", b)
	}
	time.Sleep(50 * time.Millisecond)
	var connectDomain, udpDomain, connectIP bool
	for _, e := range log.entries(t) {
		switch {
		case e.Cmd == "connect" && e.Atyp == 3 && e.Host == "tunsim.test" && e.Result == "204":
			connectDomain = true
		case e.Cmd == "udp" && e.Atyp == 3 && e.Host == "tunsim.test" && e.Result == "echo":
			udpDomain = true
		case e.Cmd == "connect" && e.Atyp == 1 && e.Host == "203.0.113.9" && e.Result == "204":
			connectIP = true
		}
	}
	if !connectDomain || !udpDomain || !connectIP {
		t.Fatalf("开发上游日志应有三类条目：connect 域名 %v、udp 域名 %v、connect IP %v", connectDomain, udpDomain, connectIP)
	}
	if strings.Contains(string(b), "127.0.0.1") {
		t.Fatalf("报告里不应有地址：%s", b)
	}
}

func TestRunExitDownFailsButContinues(t *testing.T) {
	d, _ := chain(t, false)
	rep := Run(context.Background(), Config{Opener: d, Timeout: 5 * time.Second})
	b, _ := json.Marshal(rep)
	if rep.OK {
		t.Fatalf("出口不可用时整体不应 ok：%s", b)
	}
	if s := stepOf(rep, "b_tcp_fake"); s.OK || s.Err == "" {
		t.Fatalf("步骤 b 应失败并写明原因：%s", b)
	}
	if rep.Proxy.ExitDown < 1 {
		t.Fatalf("应计 exit_down：%s", b)
	}
	for _, name := range []string{"a_dns", "e_no_mapping", "g_close"} {
		if !stepOf(rep, name).OK {
			t.Fatalf("失败后应继续跑，%s 仍应 ok：%s", name, b)
		}
	}
	if len(rep.Steps) != 7 {
		t.Fatalf("每一步都应有结果：%s", b)
	}
}

func TestRunWithUDPTarget(t *testing.T) {
	d, log := chain(t, true)
	target := netip.MustParseAddrPort("198.51.100.7:4500")
	rep := Run(context.Background(), Config{Opener: d, Timeout: 10 * time.Second, UDPTarget: target})
	b, _ := json.Marshal(rep)
	if !rep.OK || len(rep.Steps) != 7 || !stepOf(rep, "c_udp_realip").OK {
		t.Fatalf("带 UDPTarget 应全部 ok，c 的步骤名为 c_udp_realip：%s", b)
	}
	if rep.OpensAfterGate != rep.OpensBeforeGate {
		t.Fatalf("关闸门后 Opener 不应再被调用：%s", b)
	}
	time.Sleep(50 * time.Millisecond)
	found := false
	for _, e := range log.entries(t) {
		if e.Cmd == "udp" && e.Atyp == 1 && e.Host == "198.51.100.7" && e.Port == 4500 && e.Result == "echo" {
			found = true
		}
	}
	if !found {
		t.Fatal("开发上游日志应有 udp ATYP=1 198.51.100.7:4500 echo")
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestHoldDropProblem(t *testing.T) {
	if holdDropProblem(io.EOF) != "" || holdDropProblem(syscall.ECONNRESET) != "" || holdDropProblem(net.ErrClosed) != "" {
		t.Fatal("EOF、reset、已关闭都算被断开")
	}
	if p := holdDropProblem(timeoutErr{}); !strings.Contains(p, "读超时") {
		t.Fatalf("读超时应报“没有被断开（读超时）”：%q", p)
	}
	if holdDropProblem(nil) == "" {
		t.Fatal("还能读到数据应算没有被断开")
	}
}

// fakeUpstream 对 TXT 查询回一条记录；err 非 nil 时返回它。
type fakeUpstream struct {
	err   error
	calls atomic.Int32
}

func (u *fakeUpstream) Exchange(_ context.Context, q *dns.Msg) (*dns.Msg, error) {
	u.calls.Add(1)
	if u.err != nil {
		return nil, u.err
	}
	m := new(dns.Msg)
	m.SetReply(q)
	if q.Question[0].Qtype == dns.TypeTXT {
		m.Answer = append(m.Answer, &dns.TXT{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60}, Txt: []string{"ok"}})
	}
	return m, nil
}

func TestRunWithDoHName(t *testing.T) {
	d, _ := chain(t, true)
	up := &fakeUpstream{}
	rep := Run(context.Background(), Config{Opener: d, Timeout: 10 * time.Second, DoHName: "example.com", Upstream: up})
	b, _ := json.Marshal(rep)
	if !rep.OK || len(rep.Steps) != 8 || !stepOf(rep, "h_doh").OK {
		t.Fatalf("给了 DoHName 应 8 步全 ok：%s", b)
	}
	if rep.OpensAfterGate != rep.OpensBeforeGate {
		t.Fatalf("关闸门后 Opener 不应再被调用：%s", b)
	}
	if up.calls.Load() != 1 {
		t.Fatalf("关闸门后的查询不应到达上游（应在闸门处答 SERVFAIL）：%d 次", up.calls.Load())
	}
}

func TestRunDoHUpstreamError(t *testing.T) {
	d, _ := chain(t, true)
	rep := Run(context.Background(), Config{Opener: d, Timeout: 10 * time.Second, DoHName: "example.com", Upstream: &fakeUpstream{err: errors.New("boom")}})
	b, _ := json.Marshal(rep)
	if rep.OK || stepOf(rep, "h_doh").OK || stepOf(rep, "h_doh").Err == "" {
		t.Fatalf("上游出错时 h_doh 应失败并写明原因：%s", b)
	}
	for _, s := range rep.Steps {
		if s.Name != "h_doh" && !s.OK {
			t.Fatalf("其余步骤应照常 ok：%s", b)
		}
	}
}
