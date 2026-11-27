package main

import (
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"claude-abroad/internal/usernet"
)

type logRec struct {
	mu    sync.Mutex
	lines []string
}

func (r *logRec) logf(format string, a ...any) {
	r.mu.Lock()
	r.lines = append(r.lines, fmt.Sprintf(format, a...))
	r.mu.Unlock()
}

// 第一跳拨号失败：每类一例记事件，只有类别和第几个第一跳，没有地址；同类 30 秒内只记一条；网络计数那行带各类别。
func TestDialFailEvents(t *testing.T) {
	hops, err := usernet.ParseFirstHops([]string{"8.8.4.4:443", "proxy.example.com:8443"})
	if err != nil {
		t.Fatal(err)
	}
	rec := &logRec{}
	now := time.Unix(1000, 0)
	d := &usernet.Dialer{}
	u := &userNet{dialer: d, mode: outboundFirstHop, out: usernet.NewOutbound(nil, hops, nil), logf: rec.logf,
		now: func() time.Time { return now }}
	u.gwdns = &usernet.GatewayDNS{Out: u.out}
	dst := netip.MustParseAddrPort("8.8.4.4:443")
	classes := []string{usernet.DialFailPerm, usernet.DialFailUnreach, usernet.DialFailTimeout, usernet.DialFailRefused,
		usernet.DialFailBind, usernet.DialFailOther}
	for _, c := range classes {
		u.dialFailed("tcp", dst, c)
		u.dialFailed("tcp", dst, c) // 限流：同类第二条不记
	}
	u.resolveFailed("proxy.example.com")
	u.resolveFailed("not-a-hop.example.com") // 不是主代理第一跳：不记
	if len(rec.lines) != len(classes)+1 {
		t.Fatalf("应 %d 条，得到 %d：%v", len(classes)+1, len(rec.lines), rec.lines)
	}
	for i, c := range classes {
		l := rec.lines[i]
		if !strings.Contains(l, "第 1 个第一跳") || !strings.Contains(l, usernet.DialFailLabel(c)) {
			t.Errorf("第 %d 条：%s", i, l)
		}
	}
	if l := rec.lines[len(classes)]; !strings.Contains(l, "第 2 个第一跳") || !strings.Contains(l, "域名解析失败") {
		t.Errorf("dns：%s", l)
	}
	now = now.Add(failLogEvery)
	u.dialFailed("tcp", dst, usernet.DialFailTimeout)
	u.dialFailed("tcp", netip.MustParseAddrPort("9.9.9.9:443"), usernet.DialFailTimeout)
	if n := len(rec.lines); n != len(classes)+3 || !strings.Contains(rec.lines[n-1], "不是主代理的第一跳") {
		t.Fatalf("过了间隔再记、非第一跳：%v", rec.lines[len(classes)+1:])
	}
	if d.Counters().DialFail[usernet.DialFailDNS] != 1 {
		t.Fatal("dns 计数")
	}
	u.logStats()
	last := rec.lines[len(rec.lines)-1]
	for _, c := range usernet.DialFailClasses {
		if !strings.Contains(last, "dial_fail_"+c+"=") {
			t.Errorf("网络计数缺 %s：%s", c, last)
		}
	}
	for _, l := range rec.lines {
		if strings.Contains(l, "8.8.4.4") || strings.Contains(l, "9.9.9.9") || strings.Contains(l, "example.com") {
			t.Errorf("事件里有地址或域名：%s", l)
		}
	}
}
