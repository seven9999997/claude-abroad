package directdial

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func cfgCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// b. 直连拨号模块的配置下发（m3-plan v2.8 决定 1）。
func TestConfigRequest(t *testing.T) {
	r := newRig(t, func(s *Server) { s.DNSServers = nil }) // 生产形态：没有 --dns，等 abroadd 下发
	r.net.setAnswer(func(q *dns.Msg) []*dns.Msg { return []*dns.Msg{reply(q, "93.184.216.34")} })
	// 未配置（客户端也没有配置来源）：按域名、按 IP 都回 stale_config，不发查询、不拨号（m3-plan v2.9）。
	if _, err := r.dial(t, "tcp", "a.example", "", 443); !errors.Is(err, ErrStaleConfig) {
		t.Fatalf("未配置时按域名应回 stale_config：%v", err)
	}
	if _, err := r.dial(t, "tcp", "", "93.184.216.34", 443); !errors.Is(err, ErrStaleConfig) {
		t.Fatalf("未配置时按 IP 也应回 stale_config：%v", err)
	}
	if r.net.queryCount() != 0 || r.net.dialCount() != 0 {
		t.Fatal("未配置时不应发查询、拨号")
	}
	if r.srv.Configured() {
		t.Fatal("还没下发配置")
	}
	// 下发：解析用下发的 DNS。
	ctx, cancel := cfgCtx()
	defer cancel()
	if err := r.cli.Configure(ctx, Config{DNS: []netip.Addr{dnsAddr}, PinnedExtra: []string{"*.special.example", "Exact.Example."}}); err != nil {
		t.Fatal(err)
	}
	if !r.srv.Configured() {
		t.Fatal("应已收到配置")
	}
	c, err := r.dial(t, "tcp", "a.example", "", 443)
	if err != nil {
		t.Fatalf("下发后应能按域名直连：%v", err)
	}
	c.Close()
	if r.net.queryCount() != 1 {
		t.Fatalf("应经下发的 DNS 查询一次：%d", r.net.queryCount())
	}
	// 追加的置顶域名回 pinned（大小写、末尾的点归一）；经别名链到达的也一样。
	for _, h := range []string{"x.special.example", "exact.example"} {
		if _, err := r.dial(t, "tcp", h, "", 443); !errors.Is(err, ErrPinned) {
			t.Fatalf("%s 应回 pinned：%v", h, err)
		}
	}
	r.net.setAnswer(func(q *dns.Msg) []*dns.Msg {
		m := reply(q)
		c1, _ := dns.NewRR(q.Question[0].Name + " 60 IN CNAME y.special.example.")
		a, _ := dns.NewRR("y.special.example. 60 IN A 93.184.216.34")
		m.Answer = append(m.Answer, c1, a)
		return []*dns.Msg{m}
	})
	before := r.net.dialCount()
	if _, err := r.dial(t, "tcp", "alias.example", "", 443); !errors.Is(err, ErrPinned) || r.net.dialCount() != before {
		t.Fatalf("别名到追加的置顶域名应回 pinned、不拨号：%v", err)
	}
	// 不合法的配置被拒，旧配置保持（仍能用下发的 DNS、追加名单仍在）。
	bad := []struct {
		name   string
		dns    []netip.Addr
		pinned []string
	}{
		{"超过 4 个 DNS", []netip.Addr{dnsAddr, dnsAddr, dnsAddr, dnsAddr, dnsAddr}, nil},
		{"域名不合法", []netip.Addr{dnsAddr}, []string{"bad_-.x-"}},
		{"通配写在中间", []netip.Addr{dnsAddr}, []string{"a.*.example"}},
		{"超过上限", []netip.Addr{dnsAddr}, []string{strings.Repeat("a", 60) + ".example"}},
	}
	big := make([]string, 0, 2000)
	for i := 0; i < 2000; i++ {
		big = append(big, strings.Repeat("b", 40)+".example")
	}
	bad[3].pinned = big
	for _, b := range bad {
		ctx, cancel := cfgCtx()
		err := r.cli.Configure(ctx, Config{DNS: b.dns, PinnedExtra: b.pinned})
		cancel()
		if err == nil {
			t.Fatalf("%s：应被拒", b.name)
		}
	}
	if _, err := r.dial(t, "tcp", "exact.example", "", 443); !errors.Is(err, ErrPinned) {
		t.Fatalf("被拒后旧配置应保持：%v", err)
	}
	// DNS 全是回环、fake 段：被剔除，按域名回 resolve_failed、不发查询。
	ctx2, cancel2 := cfgCtx()
	defer cancel2()
	if err := r.cli.Configure(ctx2, Config{DNS: []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("198.18.0.2")}}); err != nil {
		t.Fatal(err)
	}
	q := r.net.queryCount()
	if _, err := r.dial(t, "tcp", "b.example", "", 443); !errors.Is(err, ErrResolveFailed) || r.net.queryCount() != q {
		t.Fatalf("DNS 全被剔除时应回 resolve_failed、不发查询：%v", err)
	}
	if r.srv.Stats()["config"] != 2 {
		t.Fatalf("配置计数：%v", r.srv.Stats())
	}
}

// b. 配置请求同样要过对端身份校验。
func TestConfigRequiresPeerUID(t *testing.T) {
	r := newRig(t, func(s *Server) { s.DNSServers = nil; s.AllowUID = func(uint32) bool { return false } })
	ctx, cancel := cfgCtx()
	defer cancel()
	if err := r.cli.Configure(ctx, Config{DNS: []netip.Addr{dnsAddr}}); err == nil {
		t.Fatal("对端 uid 不符时配置应失败")
	}
	if r.srv.Configured() {
		t.Fatal("不应收下配置")
	}
}
