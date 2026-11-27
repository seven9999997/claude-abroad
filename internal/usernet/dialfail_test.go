package usernet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// 每个类别一例（2026-11-27 真机：连不上的拨号原来一个都没计）。
func TestClassifyDialError(t *testing.T) {
	opErr := func(e error) error {
		return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", e)}
	}
	cases := []struct {
		err  error
		want string
	}{
		{opErr(syscall.EPERM), DialFailPerm},
		{opErr(syscall.EACCES), DialFailPerm},
		{opErr(syscall.EHOSTUNREACH), DialFailUnreach},
		{opErr(syscall.ENETUNREACH), DialFailUnreach},
		{opErr(syscall.ENETDOWN), DialFailUnreach},
		{opErr(syscall.ETIMEDOUT), DialFailTimeout},
		{fmt.Errorf("拨号：%w", context.DeadlineExceeded), DialFailTimeout},
		{opErr(syscall.ECONNREFUSED), DialFailRefused},
		{&net.DNSError{Err: "no such host", Name: "example.invalid"}, DialFailDNS},
		{fmt.Errorf("%w：网卡 en9：没有", errBind), DialFailBind},
		{fmt.Errorf("%w：结果为空", errPhysical), DialFailBind},
		{opErr(syscall.ECONNRESET), DialFailOther},
		{errors.New("别的"), DialFailOther},
	}
	for _, c := range cases {
		if got := ClassifyDialError(c.err); got != c.want {
			t.Errorf("%v：得到 %q，应为 %q", c.err, got, c.want)
		}
	}
	if ClassifyDialError(nil) != "" {
		t.Error("nil 应为空")
	}
	for _, c := range DialFailClasses {
		if DialFailLabel(c) == "" {
			t.Errorf("%s 没有说明", c)
		}
	}
}

type failRec struct {
	mu  sync.Mutex
	got []string
}

func (r *failRec) hook(network string, _ netip.AddrPort, class string) {
	r.mu.Lock()
	r.got = append(r.got, network+"/"+class)
	r.mu.Unlock()
}

// 拨号器：连接被拒（回环白名单，不绑网卡）和网卡识别失败（绑定类）都计数、调钩子；成功不计。
func TestDialerCountsFailures(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := netip.MustParseAddrPort(l.Addr().String())
	l.Close() // 端口关掉：连接被拒
	rec := &failRec{}
	d := &Dialer{Rules: openRules{white: map[netip.AddrPort]bool{closed: true}}, OnDialFailed: rec.hook,
		Interface: func(context.Context) (string, error) { return "", errors.New("没有网卡") }, Timeout: 2 * time.Second}
	if _, err := d.DialContext(context.Background(), "tcp", closed.String()); err == nil {
		t.Fatal("应连接被拒")
	}
	if _, err := d.DialContext(context.Background(), "tcp", "203.0.113.9:443"); err == nil {
		t.Fatal("识别网卡失败应拒绝")
	}
	c := d.Counters()
	if c.DialFail[DialFailRefused] != 1 || c.DialFail[DialFailBind] != 1 || c.DialOK != 0 {
		t.Fatalf("计数：%+v", c)
	}
	if strings.Join(rec.got, ",") != "tcp/refused,tcp/bind" {
		t.Fatalf("钩子：%v", rec.got)
	}
	d.NoteResolveFailed()
	if d.Counters().DialFail[DialFailDNS] != 1 {
		t.Fatal("dns 应计 1")
	}
	for _, k := range DialFailClasses {
		if _, ok := c.DialFail[k]; !ok {
			t.Errorf("计数里缺 %s", k)
		}
	}
}

func TestHopIndex(t *testing.T) {
	hops, err := ParseFirstHops([]string{"8.8.4.4:443", "proxy.example.com:8443"})
	if err != nil {
		t.Fatal(err)
	}
	o := NewOutbound(nil, hops, nil)
	if got := o.HopIndex(netip.MustParseAddrPort("8.8.4.4:443")); got != 1 {
		t.Fatalf("IP 第一跳：%d", got)
	}
	if got := o.HopIndex(netip.MustParseAddrPort("8.8.4.4:80")); got != 0 {
		t.Fatalf("端口不对：%d", got)
	}
	o.Learn("proxy.example.com", []netip.Addr{netip.MustParseAddr("9.9.9.9")})
	if got := o.HopIndex(netip.MustParseAddrPort("9.9.9.9:8443")); got != 2 {
		t.Fatalf("域名第一跳答过的地址：%d", got)
	}
	if o.HopIndexByName("proxy.example.com") != 2 || o.HopIndexByName("other.example.com") != 0 {
		t.Fatal("按域名")
	}
}

// 网关 DNS：第一跳域名解析失败时调 OnFailed。
func TestGatewayDNSOnFailed(t *testing.T) {
	hops, _ := ParseFirstHops([]string{"proxy.example.com:443"})
	o := NewOutbound(nil, hops, nil)
	var names []string
	g := &GatewayDNS{Out: o, OnFailed: func(n string) { names = append(names, n) },
		Resolve: func(context.Context, string) ([]netip.Addr, time.Duration, error) {
			return nil, 0, errors.New("失败")
		}}
	q := new(dns.Msg)
	q.SetQuestion("proxy.example.com.", dns.TypeA)
	r, _ := g.Exchange(context.Background(), q)
	if r.Rcode != dns.RcodeServerFailure || len(names) != 1 || names[0] != "proxy.example.com" || g.Counters().Failed != 1 {
		t.Fatalf("rcode %d，回调 %v", r.Rcode, names)
	}
}
