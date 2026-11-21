//go:build integration && darwin

package usernet_test

// 真实 DoH 链路（M2-1b 第 1 步）：真实的物理网卡查询 + Dialer.DialResolver（绑物理网卡）+ DefaultDoHEndpoints。
// 只读：只发 DNS 查询，不改任何网络设置。运行：go test -tags integration -run TestRealDoH ./internal/usernet/

import (
	"context"
	"net"
	"testing"
	"time"

	"claude-abroad/internal/physif"
	"claude-abroad/internal/usernet"
)

func TestRealDoH(t *testing.T) {
	d := &usernet.Dialer{Interface: physif.Query}
	eps := usernet.DefaultDoHEndpoints
	r := &usernet.DoH{Endpoints: eps, Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return d.DialResolver(ctx, network, addr, eps)
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	addrs, ttl, err := r.Lookup(ctx, "dns.alidns.com")
	if err != nil {
		t.Fatalf("真实 DoH 解析失败：%v（计数 %+v）", err, d.Counters())
	}
	public := 0
	for _, a := range addrs {
		if a.Is4() && !a.IsPrivate() && !a.IsLoopback() {
			public++
		}
	}
	c := d.Counters()
	t.Logf("地址 %d 个（公网 IPv4 %d 个），TTL %s，网卡 %s，计数 %+v", len(addrs), public, ttl, d.CurrentInterface(), c)
	if public == 0 || c.ResolverDialOK < 1 || c.DialOK != 0 || c.BindFailed != 0 {
		t.Fatalf("应拿到公网 IPv4，resolver_dial_ok ≥ 1，dial_ok 不变：%+v", c)
	}
}
