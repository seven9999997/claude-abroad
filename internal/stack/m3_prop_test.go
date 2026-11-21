package stack

import (
	"context"
	"crypto/tls"
	"fmt"
	"go/ast"
	"io"
	"math/rand"
	"net"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-abroad/internal/fakedns"
	"claude-abroad/internal/rules"
	"claude-abroad/internal/sniff"
	"claude-abroad/internal/srccheck"
)

// h. 性质测试：随机的配置、应用、去向下，转发层实际执行的动作与对同样输入独立调用 Decide 的结果一致。
// 直接调用 Proxy.AcceptTCP、Proxy.TCP（net.Pipe），不经 gVisor，跑得快。
func TestDispatchMatchesDecide(t *testing.T) {
	r := rand.New(rand.NewSource(20261119))
	hellos := map[string][]byte{
		"sni-plain":  tlsHello(t, &tls.Config{ServerName: "www.example.com"}),
		"sni-pinned": tlsHello(t, &tls.Config{ServerName: "claude.ai"}),
		"no-sni":     tlsHello(t, &tls.Config{InsecureSkipVerify: true}),
		"own":        []byte("\x00\x01own-protocol"),
		"http":       []byte("GET / HTTP/1.1\r\nHost: www.example.com\r\n\r\n"),
	}
	hkeys := []string{"sni-plain", "sni-pinned", "no-sni", "own", "http"}
	actions := []string{"direct", "block", "proxy:wg-a", "proxy:clash-b", "proxy:main"}
	pool := fakedns.DefaultPool()
	names := []string{"www.example.org", "claude.ai", "git.corp.example", "api.anthropic.com"}
	fakes := map[string]netip.Addr{}
	for _, n := range names {
		a, _ := pool.Assign(n, time.Now())
		fakes[n] = a
	}
	reals := []string{"203.0.113.9", "160.79.104.10"}
	ports := []uint16{443, 80, 5222}
	apps := []uint16{portA, portB, portC, portClaude, portOther}
	counts := map[rules.Kind]int{}
	const N = 3000
	for i := 0; i < N; i++ {
		y := "rules:\n"
		for _, app := range []string{"com.app.a", "com.app.b", "com.app.c"} {
			if r.Intn(4) > 0 {
				act := actions[r.Intn(len(actions))]
				if app == "com.app.c" && r.Intn(2) == 0 {
					act = "direct" // 让直连规则多一些，直连那一类才覆盖得到
				}
				y += fmt.Sprintf("  - {app: %s, action: %q}\n", app, act)
			}
		}
		if r.Intn(2) == 0 {
			y += "local_domains: [\"*.corp.example\"]\n"
		}
		if r.Intn(3) == 0 {
			y += "offline_allow: [com.app.c]\n"
		}
		cfg, err := rules.Parse([]byte(y))
		if err != nil {
			t.Fatal(err)
		}
		pool.SetLocalMatcher(cfg.IsLocal)
		state := rules.Enabled
		if r.Intn(4) == 0 {
			state = rules.Disabled
		}
		g := openGate()
		g.open.Store(state == rules.Enabled && r.Intn(4) > 0)
		ident, direct, op := &fakeIdent{}, &fakeDirect{pinned: map[string]bool{}}, &fakeOpener{}
		p := &Proxy{Pool: pool, Gate: g, Opener: op, Ident: ident, Direct: direct,
			Rules: func() *rules.Config { return cfg }, State: func() rules.State { return state },
			ExitUp: func(x string) bool { return x == "wg-a" || x == "clash-b" }}
		port := apps[r.Intn(len(apps))] + uint16(r.Intn(900))
		var dst netip.AddrPort
		if r.Intn(2) == 0 {
			dst = netip.AddrPortFrom(fakes[names[r.Intn(len(names))]], ports[r.Intn(len(ports))])
		} else {
			dst = netip.AddrPortFrom(netip.MustParseAddr(reals[r.Intn(len(reals))]), ports[r.Intn(len(ports))])
		}
		data := hellos[hkeys[r.Intn(len(hkeys))]]
		f := Flow{Src: netip.AddrPortFrom(clientIP, port), Dst: dst}

		// 独立计算期望的结果。
		app, claude := ident.Identify(context.Background(), "tcp", f.Src, f.Dst)
		ident.calls.Store(0)
		who := rules.Who{App: rules.Unknown}
		if cfg.NeedApp() {
			who = rules.Who{App: app, Claude: claude}
		}
		d := rules.Dest{Net: "tcp", Addr: dst}
		if m, rel, ok := pool.AcquireMapping(dst.Addr(), time.Now()); ok {
			d.FakeDomain, d.Local = m.Name, m.Local
			rel()
		}
		env := rules.Env{State: state, GateOpen: g.Open(), ExitUp: p.ExitUp, UDPOK: func(string) bool { return true }}
		if cfg.NeedSniff(who, d, env) {
			res, _ := sniff.Classify(data)
			d.Sniff, d.SniffName = res.Kind, res.Name
		}
		want := cfg.Decide(who, d, env)
		accepted := p.AcceptTCP(f)
		if !accepted && (want.Action.Kind != rules.Block) {
			t.Fatalf("第 %d 组：SYN 被拒，但判定是 %v（%s）", i, want.Action, want.Reason)
		}
		if accepted {
			a, b := net.Pipe()
			go func() {
				_ = a.SetDeadline(time.Now().Add(2 * time.Second))
				_, _ = a.Write(data)
				_, _ = io.ReadFull(a, make([]byte, len(data)))
				a.Close()
			}()
			p.TCP(context.Background(), f, b)
			b.Close()
		}
		var got rules.Action
		switch hs, dl := op.all(), direct.list(); {
		case len(hs) == 1 && len(dl) == 0:
			got = rules.Action{Kind: rules.Proxy, Exit: hs[0].Exit}
		case len(hs) == 0 && len(dl) == 1:
			got = rules.ActDirect
		case len(hs) == 0 && len(dl) == 0:
			got = rules.ActBlock
		default:
			t.Fatalf("第 %d 组：开流 %d 次、直连 %d 次", i, len(hs), len(dl))
		}
		if got != want.Action {
			t.Fatalf("第 %d 组：实际 %v，独立判定 %v（%s）\n配置 %s\n应用端口 %d 目的 %v 首包 %d 字节 状态 %v 闸门 %v",
				i, got, want.Action, want.Reason, y, port, dst, len(data), state, g.Open())
		}
		counts[got.Kind]++
	}
	for k, min := range map[rules.Kind]int{rules.Direct: 200, rules.Proxy: 200, rules.Block: 200} {
		if counts[k] < min {
			t.Errorf("%v 只覆盖到 %d 组，下限 %d", k, counts[k], min)
		}
	}
	t.Logf("%d 组：%v", N, counts)
}

// h. AST：开代理流和直连拨号只从分发函数调用；Decide 只有判定入口和 Refused 重判两处；
// Opener.Open 只在 open 里，Direct.Dial 只在 dialDirect 里。
func TestSingleDispatchPath(t *testing.T) {
	calls := map[string][]string{}
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af := srccheck.Parse(t, f)
		for _, decl := range af.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := c.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				name := sel.Sel.Name
				if x, ok := sel.X.(*ast.SelectorExpr); ok && (x.Sel.Name == "Opener" || x.Sel.Name == "Direct") {
					name = x.Sel.Name + "." + name // p.Opener.Open、p.Direct.Dial
				}
				switch name {
				case "open", "dialDirect", "Decide", "Opener.Open", "Direct.Dial", "dispatch", "probe":
					calls[name] = append(calls[name], fn.Name.Name)
				}
				return true
			})
		}
	}
	want := map[string]string{
		"open":        "dispatch",
		"dialDirect":  "dispatch",
		"Opener.Open": "probe,open",            // M4-6a：探测窗口的放行路径是唯一的例外（出口固定是主代理，见 TestProbePathMainOnly）
		"Direct.Dial": "dialDirect,dialDirect", // 有域名、没有域名各一处
		"Decide":      "decide,dispatch",
		"dispatch":    "TCP,UDP,dispatch",
		"probe":       "TCP,UDP",
	}
	for k, w := range want {
		if got := strings.Join(calls[k], ","); got != w {
			t.Errorf("%s 的调用点应是 %s，实际 %s", k, w, got)
		}
	}
}

// l. 网络栈没有不经数据通道、直连拨号模块的直接拨号。
func TestStackNoDial(t *testing.T) {
	srccheck.NoDial(t, ".", nil)
}

// j. 单条连接的零字节结束只进诊断计数，不影响后续连接的判定。
func TestZeroEOFOnlyCounted(t *testing.T) {
	e := newM3Env(t, m3Rules)
	dst := ap(e.lookupA(t, "www.example.org"), 443)
	e.op.mode = "eof"
	c, err := e.cl.dialFrom(portA+1, dst, ioWait)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(ioWait))
	// 客户端不先写：写往已关闭的上游会出错、两端被强制断开，那就不是“零字节正常结束”了。
	if n, err := c.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("上游零字节结束时，对应用应是正常的 EOF（FIN，不是重置）：%d %v", n, err)
	}
	c.Close()
	waitFor(t, "零字节计数", func() bool { return e.p.Stats().ZeroEOF == 1 })
	e.op.mode = ""
	c2, err := e.cl.dialFrom(portA+2, dst, ioWait)
	if err != nil {
		t.Fatal(err)
	}
	if err := echoOnce(c2, []byte("next")); err != nil {
		t.Fatalf("后续连接应照常：%v", err)
	}
	c2.Close()
	if hs := e.op.all(); len(hs) != 2 || hs[1].Exit != "wg-a" {
		t.Fatalf("后续连接应照常走出口 a：%+v", hs)
	}
}
