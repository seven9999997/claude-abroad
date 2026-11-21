package stack

import (
	"context"
	"go/ast"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"claude-abroad/internal/fakedns"
	"claude-abroad/internal/guestabi"
	"claude-abroad/internal/rules"
	"claude-abroad/internal/srccheck"
)

// ---- M4-6a：探测窗口的放行路径（m4-plan v2.6 决定 2，验收 d、e）----

type fakeProbe struct{ open atomic.Bool }

func (f *fakeProbe) ProbeOpen() bool { return f.open.Load() }

// fakeSelf：源端口在 [lo, hi) 的是 abroadd 自己（按源端口给出进程号）；ok 为 false 时一律识别不到。
type fakeSelf struct {
	lo, hi uint16
	calls  atomic.Int32
	broken atomic.Bool
}

func (f *fakeSelf) IsSelf(_ context.Context, _ string, src, _ netip.AddrPort) bool {
	f.calls.Add(1)
	if f.broken.Load() {
		return false
	}
	return src.Port() >= f.lo && src.Port() < f.hi
}

type probeEnv struct {
	*m3Env
	probe *fakeProbe
	self  *fakeSelf
}

// 自己的源端口落在 com.app.c 的段里：规则按 com.app.c 判（直连或拦截），放行路径不能受它影响。
const selfLo = portC + 500

func newProbeEnv(t *testing.T, y string) *probeEnv {
	t.Helper()
	e := &m3Env{ident: &fakeIdent{}, direct: &fakeDirect{pinned: map[string]bool{}}}
	pe := &probeEnv{m3Env: e, probe: &fakeProbe{}, self: &fakeSelf{lo: selfLo, hi: selfLo + 100}}
	pool := fakedns.DefaultPool()
	e.setRules(t, y, pool)
	e.proxyEnv = newProxyEnvWith(t, pool, Options{}, func(p *Proxy) {
		p.Rules = func() *rules.Config { return e.cfg.Load() }
		p.ExitUp = func(x string) bool { return x == "wg-a" || x == "clash-b" }
		p.Ident = e.ident
		p.Direct = e.direct
		p.Probe, p.Self = pe.probe, pe.self
	})
	return pe
}

// d. 窗口打开、连接来自自己：经主代理到达（数据面停用、闸门关、规则是直连或拦截都一样）；别的进程照常被拒；
// 窗口关闭后自己的也被拒；识别不到不放行；直连调用 0 次；单独计数。
func TestProbeWindowSelfOnly(t *testing.T) {
	for _, act := range []string{"direct", "block"} {
		t.Run(act, func(t *testing.T) {
			e := newProbeEnv(t, "rules:\n  - {app: com.app.c, action: "+act+"}\n  - {app: com.app.a, action: \"proxy:wg-a\"}\n")
			dst := ap(e.lookupA(t, "probe.example.org"), 443)
			e.gate.open.Store(false)
			e.p.SetEnabled(false) // 数据面停用
			e.probe.open.Store(true)
			c, err := e.cl.dialFrom(selfLo+1, dst, ioWait)
			if err != nil {
				t.Fatalf("自己的探测应被放行：%v", err)
			}
			if err := echoOnce(c, []byte("probe")); err != nil {
				t.Fatalf("应经主代理到达：%v", err)
			}
			c.Close()
			h := e.op.last()
			if h.Exit != guestabi.ExitMain || h.Host != "probe.example.org" {
				t.Fatalf("应固定走主代理：%+v", h)
			}
			// 别的进程：照常被拒（规则是走代理，闸门关着）。
			opens := e.op.calls.Load()
			if c, err := e.cl.dialFrom(portA+7, dst, ioWait); err == nil {
				if echoOnce(c, []byte("x")) == nil {
					t.Fatal("别的进程不应被放行")
				}
				c.Close()
			}
			// 识别不到：不放行。
			e.self.broken.Store(true)
			if c, err := e.cl.dialFrom(selfLo+2, dst, ioWait); err == nil {
				if echoOnce(c, []byte("x")) == nil {
					t.Fatal("识别不到时不应放行")
				}
				c.Close()
			}
			e.self.broken.Store(false)
			// 窗口关闭：自己的也被拒。
			e.probe.open.Store(false)
			if c, err := e.cl.dialFrom(selfLo+3, dst, ioWait); err == nil {
				if echoOnce(c, []byte("x")) == nil {
					t.Fatal("窗口关闭后不应放行")
				}
				c.Close()
			}
			if e.op.calls.Load() != opens {
				t.Fatalf("被拒的连接不应开流：%d → %d", opens, e.op.calls.Load())
			}
			if d := e.direct.list(); len(d) != 0 {
				t.Fatalf("放行路径和被拒的连接都不应直连：%v", d)
			}
			st := e.p.Stats()
			if st.ProbeProxied != 1 || st.ProbeRefused < 2 {
				t.Fatalf("计数：%+v", st)
			}
		})
	}
}

// d. 当前规则不需要识别应用时（没有按应用的规则），窗口期间仍然识别是不是自己；窗口关着时一次都不识别。
func TestProbeWindowForcesIdent(t *testing.T) {
	e := newProbeEnv(t, "rules: []\n")
	if e.cfg.Load().NeedApp() {
		t.Fatal("前提：这份规则不需要识别应用")
	}
	dst := ap(e.lookupA(t, "probe.example.org"), 443)
	c, err := e.cl.dialFrom(portA+1, dst, ioWait) // 窗口关、闸门开：照常走主代理
	if err != nil || echoOnce(c, []byte("a")) != nil {
		t.Fatalf("%v", err)
	}
	c.Close()
	if e.self.calls.Load() != 0 {
		t.Fatal("窗口关着时不应识别自己")
	}
	e.gate.open.Store(false)
	e.probe.open.Store(true)
	c, err = e.cl.dialFrom(selfLo+5, dst, ioWait)
	if err != nil || echoOnce(c, []byte("b")) != nil {
		t.Fatalf("窗口期间应识别并放行：%v", err)
	}
	c.Close()
	if e.self.calls.Load() == 0 || e.p.Stats().ProbeProxied != 1 {
		t.Fatal("窗口期间应识别")
	}
}

// d. UDP 也一样：窗口打开时自己的 UDP 经主代理。
func TestProbeWindowUDP(t *testing.T) {
	e := newProbeEnv(t, "rules:\n  - {app: com.app.c, action: direct}\n")
	dst := ap(e.lookupA(t, "probe.example.org"), 4500)
	e.gate.open.Store(false)
	e.probe.open.Store(true)
	u := e.cl.udpFrom(t, selfLo+9, dst)
	if err := echoOnce(u, []byte("udp-probe")); err != nil {
		t.Fatalf("%v", err)
	}
	if h := e.op.last(); h.Exit != guestabi.ExitMain || h.Net != "udp" || len(e.direct.list()) != 0 {
		t.Fatalf("%+v", h)
	}
}

// e. 源码检查：放行路径里开流的帧头出口是主代理的常量 guestabi.ExitMain，不调用直连、不调用规则引擎。
func TestProbePathMainOnly(t *testing.T) {
	af := srccheck.Parse(t, "router.go")
	found := false
	for _, decl := range af.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "probe" {
			continue
		}
		found = true
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := c.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "header":
				arg, ok := c.Args[1].(*ast.SelectorExpr)
				if !ok || arg.Sel.Name != "ExitMain" || arg.X.(*ast.Ident).Name != "guestabi" {
					t.Errorf("放行路径的出口必须是 guestabi.ExitMain")
				}
			case "Dial", "dialDirect", "decide", "Decide", "dispatch", "open":
				t.Errorf("放行路径不应调用 %s", sel.Sel.Name)
			}
			return true
		})
	}
	if !found {
		t.Fatal("找不到 probe")
	}
	src := strings.Join(srccheck.StringLits(af), "\n")
	_ = src
}
