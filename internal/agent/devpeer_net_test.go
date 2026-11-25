//go:build abroaddev

package agent

import (
	"context"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestTagEchoAndCountDNS(t *testing.T) {
	lo := netip.MustParseAddr("127.0.0.1")
	e, err := listenTagEcho(netip.AddrPortFrom(lo, 0), "wg")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	c, err := net.Dial("tcp", e.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte("hi"))
	b := make([]byte, 5)
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, b); err != nil || string(b) != "wg:hi" {
		t.Fatalf("%q %v", b, err)
	}
	c.Close()
	u, _ := net.Dial("udp", e.pc.LocalAddr().String())
	_, _ = u.Write([]byte("x"))
	_ = u.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := u.Read(b)
	if err != nil || string(b[:n]) != "wg:x" {
		t.Fatalf("%q %v", b[:n], err)
	}
	u.Close()
	if e.n.Load() != 2 {
		t.Fatal(e.n.Load())
	}

	d, err := listenCountDNS(netip.AddrPortFrom(lo, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var nd net.Dialer
		return nd.DialContext(ctx, "udp", d.pc.LocalAddr().String())
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	as, err := r.LookupNetIP(ctx, "ip4", "echo.exit.test")
	if err != nil || len(as) != 1 || as[0] != lo {
		t.Fatalf("%v %v", as, err)
	}
	if d.n.Load() == 0 {
		t.Fatal("应计数")
	}
	if _, err := dnsAnswerA([]byte{1, 2}, lo); err == nil {
		t.Fatal("坏报文应报错")
	}
}

func TestParseNFTCounters(t *testing.T) {
	out := `table inet abx_peer {
	chain input {
		ip saddr 10.200.0.0/16 udp dport 51820 counter packets 12 bytes 900 comment "wg_endpoint"
		ip saddr 10.200.0.0/16 counter packets 0 bytes 0 comment "exitns_other"
		udp dport 53 counter packets 3 bytes 10
		counter packets 7 bytes 400 drop
	}
}`
	c := parseNFTCounters(out)
	if c["wg_endpoint"] != 12 || c["exitns_other"] != 0 || c["#0"] != 3 || c["#1"] != 7 || len(c) != 4 {
		t.Fatalf("%v", c)
	}
	if !strings.Contains(nftRules(1, "wg1", nil), "counter drop") || !strings.Contains(rootNATRules("abxp0"), `{ "eth0", "abxp0" }`) ||
		strings.Contains(rootNATRules(""), "abxp0") {
		t.Fatal("规则")
	}
}
