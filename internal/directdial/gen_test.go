package directdial

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// genEnv 是代数测试的环境（m3-plan v2.9）：生产形态的服务端（没有 --dns，等下发），可以重启；
// 客户端带配置来源。
type genEnv struct {
	t    *testing.T
	net  *fakeWire
	path string
	srv  *Server
	stop func()
	cli  *Client
	cfg  atomic.Pointer[Config]
}

func newGenEnv(t *testing.T) *genEnv {
	t.Helper()
	dir, err := os.MkdirTemp("", "dg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	e := &genEnv{t: t, net: newFakeNet(t), path: filepath.Join(dir, "d.sock")}
	e.net.setAnswer(func(q *dns.Msg) []*dns.Msg { return []*dns.Msg{reply(q, "93.184.216.34")} })
	e.setConfig(Config{Gen: 1, DNS: []netip.Addr{dnsAddr}})
	e.cli = &Client{Path: e.path, ServerUID: uint32(os.Getuid())}
	e.cli.SetConfigSource(func() Config { return *e.cfg.Load() })
	e.start()
	return e
}

func (e *genEnv) setConfig(c Config) { e.cfg.Store(&c) }

// start 起一个新的服务端（模拟直连拨号模块被重新拉起：内存里没有配置）。
func (e *genEnv) start() {
	e.t.Helper()
	_ = os.Remove(e.path)
	me := uint32(os.Getuid())
	srv := &Server{Dialer: e.net, AllowUID: func(u uint32) bool { return u == me }}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: e.path, Net: "unix"})
	if err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, ln); close(done) }()
	var once sync.Once
	e.srv, e.stop = srv, func() { once.Do(func() { cancel(); <-done }) }
	e.t.Cleanup(e.stop)
}

func (e *genEnv) dial(host, ip string) (net.Conn, error) {
	var a netip.Addr
	if ip != "" {
		a = netip.MustParseAddr(ip)
	}
	ctx, cancel := context.WithTimeout(bg, 10*time.Second)
	defer cancel()
	return e.cli.Dial(ctx, "tcp", host, a, 443)
}

func (e *genEnv) mustDial(host, ip string) {
	e.t.Helper()
	c, err := e.dial(host, ip)
	if err != nil {
		e.t.Fatalf("直连应成功：%v", err)
	}
	c.Close()
}

func (e *genEnv) stats(keys ...string) []int64 {
	st := e.srv.Stats()
	out := make([]int64, len(keys))
	for i, k := range keys {
		out[i] = st[k]
	}
	return out
}

// 1. 直连拨号模块重启后，下一次直连自动补发配置，只多一次往返（一次 stale_config、一次下发）。
func TestGenRestartRepush(t *testing.T) {
	e := newGenEnv(t)
	e.mustDial("a.example", "")
	if got := e.stats("stale_config", "config", "ok"); got[0] != 1 || got[1] != 1 {
		t.Fatalf("第一次直连应补发一次：stale=%d config=%d", got[0], got[1])
	}
	e.mustDial("b.example", "")
	if got := e.stats("stale_config", "config"); got[0] != 1 || got[1] != 1 {
		t.Fatalf("代数一致时不应再补发：%v", got)
	}
	if g, ok := e.cli.ConfiguredGen(); !ok || g != 1 {
		t.Fatalf("已下发的代数：%d %v", g, ok)
	}
	e.stop()
	e.start()
	e.mustDial("c.example", "")
	if got := e.stats("stale_config", "config"); got[0] != 1 || got[1] != 1 {
		t.Fatalf("重启后应只多一次往返：stale=%d config=%d", got[0], got[1])
	}
	if q := e.net.queryCount(); q != 3 {
		t.Fatalf("三次按域名的直连各解析一次（经下发的 DNS）：%d", q)
	}
}

// 2. 置顶追加名单变化后，带旧代数的按 IP 请求被拒（不拨号）；客户端重试时用新名单。
func TestGenStaleRejectsOldPinned(t *testing.T) {
	e := newGenEnv(t)
	e.mustDial("x.special.example", "") // 代数 1：没有追加名单，可以直连
	e.setConfig(Config{Gen: 2, DNS: []netip.Addr{dnsAddr}, PinnedExtra: []string{"*.special.example"}})
	// 客户端：服务端还是代数 1，请求带代数 2 → stale_config → 补发代数 2 后重试，新名单生效。
	if _, err := e.dial("x.special.example", ""); !errors.Is(err, ErrPinned) {
		t.Fatalf("补发新名单后应回 pinned：%v", err)
	}
	if got := e.stats("config", "stale_config"); got[0] != 2 || got[1] != 2 {
		t.Fatalf("应补发两次（代数 1、2）：%v", got)
	}
	// 服务端已是代数 2：带旧代数的原始请求（绕过客户端）按 IP、按域名都回 stale_config，不拨号、不解析。
	d0, q0 := e.net.dialCount(), e.net.queryCount()
	for _, req := range []Request{
		{V: Version, Net: "tcp", IP: "93.184.216.34", Port: 443, Gen: 1},
		{V: Version, Net: "tcp", Host: "x.special.example", Port: 443, Gen: 1},
		{V: Version, Net: "tcp", IP: "93.184.216.34", Port: 443}, // 不带代数（0）
	} {
		b, _ := json.Marshal(req)
		r := &rig{path: e.path}
		if code := r.rawRequest(t, b); code != CodeStaleConfig {
			t.Fatalf("旧代数的请求应回 stale_config：%s", code)
		}
	}
	if e.net.dialCount() != d0 || e.net.queryCount() != q0 {
		t.Fatal("stale_config 时不应拨号、解析")
	}
}

// 3. 补发一直失败时返回失败（带 stale_config），每次直连只补发一次，不无限重试。
func TestGenPushFailsNoLoop(t *testing.T) {
	e := newGenEnv(t)
	five := []netip.Addr{dnsAddr, dnsAddr, dnsAddr, dnsAddr, dnsAddr} // 超过 4 个：服务端拒收
	e.setConfig(Config{Gen: 1, DNS: five})
	for i := 1; i <= 3; i++ {
		_, err := e.dial("a.example", "")
		var ce *CodeError
		if !errors.Is(err, ErrStaleConfig) || !errors.As(err, &ce) || ce.Code != CodeStaleConfig {
			t.Fatalf("补发失败时应返回 stale_config：%v", err)
		}
		// 服务端在发出应答之后才计数：等计数追上。
		for k := 0; k < 100 && (e.stats("stale_config")[0] < int64(i) || e.stats("bad_request")[0] < int64(i)); k++ {
			time.Sleep(10 * time.Millisecond)
		}
		if got := e.stats("stale_config", "bad_request", "config"); got[0] != int64(i) || got[1] != int64(i) || got[2] != 0 {
			t.Fatalf("第 %d 次：每次直连只补发一次：stale=%d bad=%d config=%d", i, got[0], got[1], got[2])
		}
	}
	if _, ok := e.cli.ConfiguredGen(); ok {
		t.Fatal("没有成功下发过")
	}
	if e.net.dialCount() != 0 || e.net.queryCount() != 0 {
		t.Fatal("不应拨号、解析")
	}
	// 没有配置来源的客户端：直接返回 stale_config，不下发。
	bare := &Client{Path: e.path, ServerUID: uint32(os.Getuid())}
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	if _, err := bare.Dial(ctx, "tcp", "a.example", netip.Addr{}, 443); !errors.Is(err, ErrStaleConfig) {
		t.Fatalf("没有配置来源时应回 stale_config：%v", err)
	}
	if got := e.stats("bad_request"); got[0] != 3 {
		t.Fatal("没有配置来源时不应下发")
	}
}

// 4. 直连拨号模块连不上时直连失败；它恢复后第一条直连用当时的新配置。
func TestGenUnreachableThenBack(t *testing.T) {
	e := newGenEnv(t)
	e.mustDial("a.example", "")
	e.stop()
	if _, err := e.dial("a.example", ""); err == nil {
		t.Fatal("连不上时直连应失败")
	}
	e.setConfig(Config{Gen: 2, DNS: []netip.Addr{dnsAddr}, PinnedExtra: []string{"new.example"}})
	e.start()
	if _, err := e.dial("new.example", ""); !errors.Is(err, ErrPinned) {
		t.Fatalf("恢复后第一条直连应用新配置：%v", err)
	}
	if g, _ := e.cli.ConfiguredGen(); g != 2 {
		t.Fatalf("应已下发代数 2：%d", g)
	}
}

// 5. 并发的多条直连同时遇到 stale_config：配置只下发一次，全部成功。
func TestGenConcurrentSinglePush(t *testing.T) {
	e := newGenEnv(t)
	for round := 0; round < 10; round++ {
		if round > 0 {
			e.stop()
			e.start()
		}
		const n = 8
		var wg sync.WaitGroup
		errs := make(chan error, n)
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				c, err := e.dial("", "93.184.216.34")
				if err == nil {
					c.Close()
				}
				errs <- err
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("第 %d 轮：并发直连应全部成功：%v", round, err)
			}
		}
		// 服务端在发出应答之后才计数：等计数追上。
		for i := 0; i < 100 && e.stats("ok")[0] < n+1; i++ {
			time.Sleep(10 * time.Millisecond)
		}
		if got := e.stats("config", "ok"); got[0] != 1 || got[1] != n+1 {
			t.Fatalf("第 %d 轮：配置应只下发一次：config=%d ok=%d", round, got[0], got[1])
		}
	}
}
