package sysnet

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"slices"
	"testing"
)

// m3-plan v2.8 决定 2：系统首选解析器的服务器（scutil --dns 的 resolver #1）。
func TestParseScutilDNS(t *testing.T) {
	b, err := os.ReadFile("testdata/scutil-dns.txt") // 本机只读采的样本，地址换成了 192.0.2.x
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseScutilDNS(b)
	if err != nil || len(got) != 1 || got[0] != netip.MustParseAddr("192.0.2.114") {
		t.Fatalf("本机样本：%v %v", got, err)
	}
	cases := map[string]struct {
		in   string
		want []string
		err  bool
	}{
		"多个服务器取前 4 个":       {"DNS configuration\n\nresolver #1\n  search domain[0] : example.lan\n  nameserver[0] : 192.0.2.1\n  nameserver[1] : 192.0.2.2\n  nameserver[2] : 2001:db8::1\n  nameserver[3] : 192.0.2.4\n  nameserver[4] : 192.0.2.5\n\nresolver #2\n  nameserver[0] : 192.0.2.9\n", []string{"192.0.2.1", "192.0.2.2", "2001:db8::1", "192.0.2.4"}, false},
		"没有 resolver #1":    {"DNS configuration\n\nDNS configuration (for scoped queries)\n\nresolver #1\n  nameserver[0] : 192.0.2.7\n", nil, false},
		"resolver #1 没有服务器": {"DNS configuration\n\nresolver #1\n  flags    : Request A records\n\nresolver #2\n  nameserver[0] : 192.0.2.9\n", nil, false},
		"开头不认识":             {"garbage\nresolver #1\n  nameserver[0] : 192.0.2.1\n", nil, true},
		"地址不合法":             {"DNS configuration\n\nresolver #1\n  nameserver[0] : not-an-ip\n", nil, true},
		"空输出":               {"", nil, true},
	}
	for name, c := range cases {
		got, err := ParseScutilDNS([]byte(c.in))
		if (err != nil) != c.err {
			t.Errorf("%s：err=%v", name, err)
			continue
		}
		if c.err && !errors.Is(err, ErrUnknownOutput) {
			t.Errorf("%s：应是 ErrUnknownOutput：%v", name, err)
		}
		var ws []netip.Addr
		for _, w := range c.want {
			ws = append(ws, netip.MustParseAddr(w))
		}
		if !slices.Equal(got, ws) {
			t.Errorf("%s：%v，期望 %v", name, got, ws)
		}
	}
}

func TestSystemDNSFakeAndSim(t *testing.T) {
	f := NewFake(FakeState{SysDNS: []netip.Addr{netip.MustParseAddr("192.168.1.1")}})
	got, err := f.DNS().System(context.Background())
	if err != nil || len(got) != 1 || got[0] != netip.MustParseAddr("192.168.1.1") {
		t.Fatalf("假实现：%v %v", got, err)
	}
	sim := NewSimRunner(f)
	got, err = MacDNS{R: sim}.System(context.Background())
	if err != nil || len(got) != 1 || got[0] != netip.MustParseAddr("192.168.1.1") {
		t.Fatalf("命令模拟器：%v %v", got, err)
	}
	// 只读白名单允许 scutil --dns。
	if err := readOnly(PathScutil, []string{"--dns"}, nil); err != nil {
		t.Fatalf("只读白名单应允许 scutil --dns：%v", err)
	}
}
