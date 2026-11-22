package usernet

import (
	"net/netip"
	"strings"
	"sync"
	"testing"
)

// m5-plan v2.2 验收 e（宿主一侧）：主代理那一组整体替换——新第一跳可达、旧的不可达，分流出口那一组和启动时的
// 静态白名单逐项不变；回环、组播、fake 段、Anthropic 段、IPv6 被拒；局域网节点不带确认被拒；被移除的域名答过的地址失效。
func TestSetMainHops(t *testing.T) {
	hops, err := ParseFirstHops([]string{"9.9.9.5:443", "old.example.com:443"})
	if err != nil {
		t.Fatal(err)
	}
	o := NewOutbound([]netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:9000")}, hops, nil)
	if err := o.SetExitEndpoints([]ExitEndpoint{{Host: "198.51.100.7", Proto: "udp", Port: 51820}}, false); err != nil {
		t.Fatal(err)
	}
	o.Learn("old.example.com", []netip.Addr{netip.MustParseAddr("9.9.9.20")})
	ok := func(dst string) bool { return o.BeforeDial("tcp", netip.MustParseAddrPort(dst)) == nil }
	if !ok("9.9.9.5:443") || !ok("9.9.9.20:443") {
		t.Fatal("替换前旧第一跳应可达")
	}
	if err := o.SetMainHops(MainHops{Hops: []string{"8.8.4.9:8443", "new.example.com:443"}, LAN: []MainLAN{{Addr: "192.168.1.20:7890", Confirmed: true}}}); err != nil {
		t.Fatal(err)
	}
	if ok("9.9.9.5:443") || ok("9.9.9.20:443") {
		t.Fatal("替换后旧第一跳（含旧域名答过的地址）应不可达")
	}
	if !ok("8.8.4.9:8443") || !ok("192.168.1.20:7890") || ok("192.168.1.20:7891") {
		t.Fatal("新第一跳和确认过的局域网节点应可达，端口要精确")
	}
	if o.BeforeDial("udp", netip.MustParseAddrPort("198.51.100.7:51820")) != nil || !ok("127.0.0.1:9000") {
		t.Fatal("分流出口那一组和启动时的静态白名单不受影响")
	}
	if o.Registered("old.example.com") || !o.Registered("new.example.com") {
		t.Fatal("网关代解析名单跟着换")
	}
	o.Learn("new.example.com", []netip.Addr{netip.MustParseAddr("8.8.8.30")})
	if !ok("8.8.8.30:443") {
		t.Fatal("新域名答出的公网地址应可达")
	}
	before := firstHopSnapshot(o)
	for name, m := range map[string]MainHops{
		"第一跳是回环":       {Hops: []string{"127.0.0.1:7890"}},
		"局域网是回环":       {LAN: []MainLAN{{Addr: "127.0.0.1:7890", Confirmed: true}}},
		"局域网是组播":       {LAN: []MainLAN{{Addr: "224.0.0.9:1", Confirmed: true}}},
		"局域网是 fake 段":   {LAN: []MainLAN{{Addr: "198.18.0.5:1", Confirmed: true}}},
		"局域网是 Anthropic": {LAN: []MainLAN{{Addr: "160.79.104.10:443", Confirmed: true}}},
		"局域网是 IPv6":      {LAN: []MainLAN{{Addr: "[fd00::1]:443", Confirmed: true}}},
		"局域网是公网":        {LAN: []MainLAN{{Addr: "203.0.113.9:443", Confirmed: true}}},
		"局域网是虚拟机子网":     {LAN: []MainLAN{{Addr: "192.168.127.1:443", Confirmed: true}}},
		"局域网没确认":        {LAN: []MainLAN{{Addr: "192.168.1.20:7890"}}},
		"第一跳是私有地址":      {Hops: []string{"192.168.1.20:7890"}},
		"第一跳 IPv6":      {Hops: []string{"[2001:db8::1]:443"}},
		"第一跳主机名无效":      {Hops: []string{"bad_host:443"}},
		"端口 0":          {LAN: []MainLAN{{Addr: "192.168.1.20:0", Confirmed: true}}},
	} {
		if err := o.SetMainHops(m); err == nil {
			t.Errorf("%s：应被拒", name)
		}
	}
	if firstHopSnapshot(o) != before {
		t.Fatal("被拒的替换不能改任何东西")
	}
	// 清空主代理那一组（例如还没有选节点）：什么都不放行，分流出口照旧。
	if err := o.SetMainHops(MainHops{}); err != nil || ok("8.8.4.9:8443") || o.BeforeDial("udp", netip.MustParseAddrPort("198.51.100.7:51820")) != nil {
		t.Fatalf("清空后主代理那一组不放行、分流出口照旧：%v", err)
	}
}

// 并发替换与拨号（-race）。
func TestSetMainHopsConcurrent(t *testing.T) {
	o := NewOutbound(nil, nil, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				h := []string{"a.example.com:443"}
				if (i+j)%2 == 0 {
					h = []string{"8.8.4.9:443", "b.example.com:8443"}
				}
				_ = o.SetMainHops(MainHops{Hops: h})
			}
		}(i)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				o.Learn("b.example.com", []netip.Addr{netip.MustParseAddr("8.8.8.40")})
				_ = o.BeforeDial("tcp", netip.MustParseAddrPort("8.8.8.40:8443"))
				_ = o.Registered("a.example.com")
				_ = o.Keep("b.example.com", netip.MustParseAddr("8.8.8.40"))
			}
		}()
	}
	wg.Wait()
	if !strings.Contains(firstHopSnapshot(o), "example.com") {
		t.Fatal("最后应有一组第一跳")
	}
}
