package usernet

import (
	"context"
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func firstHopSnapshot(o *Outbound) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return fmt.Sprint(o.allow, o.hops, o.byDom, o.answered)
}

// m. 替换操作的校验逐条有反例；主代理第一跳那一组在任何替换前后不变。
func TestSetExitEndpointsValidation(t *testing.T) {
	hops := []FirstHop{{Host: "node.example.com", Ports: []PortRange{{443, 443}}}, {Host: "203.0.113.7", Ports: []PortRange{{8443, 8443}}}}
	o := NewOutbound([]netip.AddrPort{netip.MustParseAddrPort("192.168.1.2:7890")}, hops, nil)
	o.Learn("node.example.com", []netip.Addr{netip.MustParseAddr("93.184.216.34")})
	before := firstHopSnapshot(o)
	for name, ep := range map[string]ExitEndpoint{
		"协议":        {"203.0.113.9", "icmp", 1},
		"端口 0":      {"203.0.113.9", "udp", 0},
		"回环":        {"127.0.0.1", "udp", 51820},
		"组播":        {"224.0.0.1", "udp", 51820},
		"未指定":       {"0.0.0.0", "udp", 51820},
		"广播":        {"255.255.255.255", "udp", 51820},
		"链路本地":      {"169.254.1.1", "udp", 51820},
		"fake 段":    {"198.18.3.4", "udp", 51820},
		"Anthropic": {"160.79.104.10", "tcp", 443},
		"IPv6":      {"2001:db8::1", "udp", 51820},
		"大写域名":      {"VPN.Example.com", "udp", 51820},
		"不是域名":      {"not a host", "udp", 51820},
	} {
		err := o.SetExitEndpoints([]ExitEndpoint{{"203.0.113.9", "udp", 51820}, ep}, false)
		if err == nil || !strings.Contains(err.Error(), "第 2 个端点") {
			t.Errorf("%s 应被拒并指出第 2 个：%v", name, err)
		}
	}
	if o.ExitEndpoints() != 0 {
		t.Fatal("被拒的替换不改任何东西")
	}
	if err := o.SetExitEndpoints(make([]ExitEndpoint, MaxExitEndpoints+1), false); err == nil {
		t.Fatal("超过上限应被拒")
	}
	if err := o.SetExitEndpoints([]ExitEndpoint{{"127.0.0.1", "udp", 51820}}, true); err != nil {
		t.Fatalf("开发标记下回环可以：%v", err)
	}
	if err := o.SetExitEndpoints([]ExitEndpoint{{"10.0.0.5", "udp", 1194}, {"vpn.example.com", "udp", 51820}}, false); err != nil {
		t.Fatalf("私有地址和域名可以：%v", err)
	}
	if got := firstHopSnapshot(o); got != before {
		t.Fatalf("主代理第一跳那一组变了：\n%s\n%s", before, got)
	}
	if o.BeforeDial("tcp", netip.MustParseAddrPort("93.184.216.34:443")) != nil || o.BeforeDial("tcp", netip.MustParseAddrPort("192.168.1.2:7890")) != nil {
		t.Fatal("主代理第一跳不受影响")
	}
}

// f、m. 新端点可达、被移除的不可达；精确到协议和端口；域名端点经网关代解析后才放行，被禁的地址不放行；
// 被移除的域名连同学到的地址一起失效；名单外的域名照旧 REFUSED。
func TestExitEndpointsDialAndGateway(t *testing.T) {
	o := NewOutbound(nil, []FirstHop{{Host: "node.example.com", Ports: []PortRange{{443, 443}}}}, nil)
	answers := map[string][]netip.Addr{
		"vpn.example.com": {netip.MustParseAddr("203.0.113.20"), netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("198.18.0.9"), netip.MustParseAddr("10.1.2.3")},
		"bad.example.com": {netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("160.79.104.1")},
		"other.example":   {netip.MustParseAddr("203.0.113.99")},
	}
	g := &GatewayDNS{Out: o, Resolve: func(_ context.Context, name string) ([]netip.Addr, time.Duration, error) {
		return answers[name], time.Minute, nil
	}}
	ask := func(name string) *dns.Msg {
		q := new(dns.Msg)
		q.SetQuestion(name+".", dns.TypeA)
		m, err := g.Exchange(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if err := o.SetExitEndpoints([]ExitEndpoint{{"203.0.113.5", "udp", 51820}, {"vpn.example.com", "udp", 1194}, {"bad.example.com", "tcp", 443}}, false); err != nil {
		t.Fatal(err)
	}
	ok := func(n, a string) bool { return o.BeforeDial(n, netip.MustParseAddrPort(a)) == nil }
	if !ok("udp", "203.0.113.5:51820") || ok("tcp", "203.0.113.5:51820") || ok("udp", "203.0.113.5:51821") {
		t.Fatal("IP 端点精确到协议和端口")
	}
	if ok("udp", "203.0.113.20:1194") {
		t.Fatal("域名端点没解析前不放行")
	}
	if m := ask("other.example"); m.Rcode != dns.RcodeRefused {
		t.Fatal("名单外的域名照旧 REFUSED")
	}
	m := ask("vpn.example.com")
	var got []string
	for _, rr := range m.Answer {
		got = append(got, rr.(*dns.A).A.String())
	}
	if m.Rcode != dns.RcodeSuccess || !reflect.DeepEqual(got, []string{"203.0.113.20", "10.1.2.3"}) {
		t.Fatalf("网关交给虚拟机的只有能放行的地址（回环、fake 段被剔掉，私有地址可以）：%v", got)
	}
	if !ok("udp", "203.0.113.20:1194") || !ok("udp", "10.1.2.3:1194") || ok("udp", "127.0.0.1:1194") || ok("tcp", "203.0.113.20:1194") {
		t.Fatal("解析后放行同一批地址，精确到协议和端口；被禁的不放行")
	}
	if m := ask("bad.example.com"); m.Rcode != dns.RcodeServerFailure || ok("tcp", "160.79.104.1:443") {
		t.Fatal("解析出的地址全部被禁时：解析失败、不放行")
	}
	// 替换掉 vpn.example.com：连同学到的地址一起失效
	if err := o.SetExitEndpoints([]ExitEndpoint{{"203.0.113.5", "udp", 51820}}, false); err != nil {
		t.Fatal(err)
	}
	if ok("udp", "203.0.113.20:1194") || o.Registered("vpn.example.com") {
		t.Fatal("被移除的域名连同学到的地址一起失效")
	}
	if m := ask("vpn.example.com"); m.Rcode != dns.RcodeRefused {
		t.Fatal("被移除的域名不再代解析")
	}
	if err := o.SetExitEndpoints(nil, false); err != nil || ok("udp", "203.0.113.5:51820") {
		t.Fatal("清空后不可达")
	}
	if !o.Registered("node.example.com") {
		t.Fatal("第一跳域名照旧登记")
	}
}

// m. 并发替换和拨号、代解析在 -race 下没有问题。
func TestExitEndpointsConcurrent(t *testing.T) {
	o := NewOutbound(nil, []FirstHop{{Host: "203.0.113.7", Ports: []PortRange{{443, 443}}}}, nil)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = o.BeforeDial("udp", netip.MustParseAddrPort("203.0.113.5:51820"))
				o.Learn("vpn.example.com", []netip.Addr{netip.MustParseAddr("203.0.113.20")})
				_ = o.Registered("vpn.example.com")
				if o.BeforeDial("tcp", netip.MustParseAddrPort("203.0.113.7:443")) != nil {
					t.Error("第一跳在替换期间也要一直可达")
					return
				}
			}
		}()
	}
	for k := 0; k < 200; k++ {
		eps := []ExitEndpoint{{"203.0.113.5", "udp", 51820}}
		if k%2 == 0 {
			eps = append(eps, ExitEndpoint{"vpn.example.com", "udp", 1194})
		}
		if err := o.SetExitEndpoints(eps, false); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}
