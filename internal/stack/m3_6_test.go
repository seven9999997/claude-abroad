package stack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"claude-abroad/internal/fakedns"
	"claude-abroad/internal/rules"
)

// alive 判断一条长连接是否还通：写一下、读回显。
func alive(c net.Conn) bool {
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte("?")); err != nil {
		return false
	}
	b := make([]byte, 1)
	_, err := io.ReadFull(c, b)
	return err == nil && b[0] == '?'
}

// waitDead 等连接断开（1 秒内）。
func waitDead(c net.Conn) bool {
	_ = c.SetDeadline(time.Now().Add(time.Second))
	_, _ = c.Write([]byte("?"))
	b := make([]byte, 64)
	for {
		if _, err := c.Read(b); err != nil {
			var ne net.Error
			return !(errors.As(err, &ne) && ne.Timeout())
		}
	}
}

const offRules = m3Rules + "offline_allow: [com.app.c]\n"

func (e *m3Env) disable() { e.p.SetEnabled(false); e.gate.open.Store(false) }
func (e *m3Env) enable()  { e.p.SetEnabled(true); e.gate.open.Store(true) }

func (e *m3Env) dialEcho(t *testing.T, port uint16, dst netip.AddrPort) error {
	t.Helper()
	c, err := e.cl.dialFrom(port, dst, ioWait)
	if err != nil {
		return err
	}
	defer c.Close()
	return echoOnce(c, []byte("hi"))
}

// a. 停用状态下：只有 offline_allow 里的应用能直连，置顶、Claude 进程、解析到 Anthropic 段仍被拦截。
func TestDisabledOfflineAllow(t *testing.T) {
	e := newM3Env(t, offRules)
	plain := ap(e.lookupA(t, "plain.example.org"), 443)
	pinned := ap(e.lookupA(t, "claude.ai"), 443)
	local := ap(e.lookupA(t, "git.corp.example"), 22)
	resolvedAnthropic := ap(e.lookupA(t, "edge.example.org"), 443)
	e.direct.pinned["edge.example.org"] = true
	e.disable()
	if err := e.dialEcho(t, portC+1, plain); err != nil {
		t.Fatalf("offline_allow 里的应用应能直连：%v", err)
	}
	n := len(e.direct.list())
	for _, c := range []struct {
		name string
		port uint16
		dst  netip.AddrPort
	}{
		{"名单外的应用", portA + 1, plain},
		{"名单外应用的本地域名", portA + 2, local},
		{"名单内应用访问置顶域名", portC + 2, pinned},
		{"名单内的 Claude 进程", portClaude + 1, plain},
	} {
		if err := e.dialEcho(t, c.port, c.dst); err == nil {
			t.Fatalf("%s：停用时应被拦截", c.name)
		}
	}
	if len(e.direct.list()) != n || len(e.op.all()) != 0 {
		t.Fatalf("被拦截的不应直连或开流：直连 %v，开流 %d", e.direct.list()[n:], len(e.op.all()))
	}
	// 名单内应用、普通域名，但直连拨号模块回 pinned：重判后走代理，停用下被拦截，没有第二次直连。
	if err := e.dialEcho(t, portC+3, resolvedAnthropic); err == nil {
		t.Fatal("解析到 Anthropic 段时停用下应被拦截")
	}
	if len(e.direct.list()) != n+1 || len(e.op.all()) != 0 {
		t.Fatalf("应只直连尝试一次、不开流：%v %d", e.direct.list()[n:], len(e.op.all()))
	}
	// 本地域名：名单内的应用可以直连。
	if err := e.dialEcho(t, portC+4, local); err != nil {
		t.Fatalf("名单内应用的本地域名应能直连：%v", err)
	}
	// 停用且 offline_allow 为空：新连接在 SYN 就被拒。
	if err := e.p.SetRules(mustRules(t, m3Rules), false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cl.dialFrom(portC+5, plain, ioWait); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("停用且 offline_allow 为空时应在 SYN 被拒：%v", err)
	}
}

func mustRules(t testing.TB, y string) *rules.Config {
	t.Helper()
	c, err := rules.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const transRules = `
rules:
  - {app: com.app.a, action: "proxy:wg-a"}
  - {app: com.app.b, action: direct}
  - {app: com.app.c, action: direct}
local_domains: ["*.corp.example"]
offline_allow: [com.app.c]
`

// b. 从启用到停用的瞬间：四条长连接里只剩名单内应用的直连；再启用后新连接恢复正常。
func TestEnableToDisableTransition(t *testing.T) {
	e := newM3Env(t, transRules)
	e.p.SetEnabled(true)
	plain := ap(e.lookupA(t, "plain.example.org"), 443)
	local := ap(e.lookupA(t, "git.corp.example"), 22)
	conns := map[string]net.Conn{}
	for name, d := range map[string]struct {
		port uint16
		dst  netip.AddrPort
	}{
		"走代理":       {portA + 1, plain},
		"名单内应用的直连":  {portC + 1, plain},
		"名单外应用的直连":  {portB + 1, plain},
		"本地域名（名单外）": {portA + 2, local},
	} {
		c, err := e.cl.dialFrom(d.port, d.dst, ioWait)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if !alive(c) {
			t.Fatalf("%s：启用时应能收发", name)
		}
		conns[name] = c
	}
	// 停用流程：通知停用（重判）→ 关闸门 → 断开走代理的。
	start := time.Now()
	e.p.SetEnabled(false)
	e.gate.open.Store(false)
	e.p.DropAll()
	for name, c := range conns {
		if name == "名单内应用的直连" {
			continue
		}
		if !waitDead(c) {
			t.Fatalf("%s：停用后应断开", name)
		}
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("断开太慢：%v", time.Since(start))
	}
	if !alive(conns["名单内应用的直连"]) {
		t.Fatal("名单内应用的直连应保持")
	}
	// 再启用：新连接恢复正常。
	e.enable()
	if err := e.dialEcho(t, portA+3, plain); err != nil {
		t.Fatalf("再启用后走代理的新连接：%v", err)
	}
	if err := e.dialEcho(t, portB+2, plain); err != nil {
		t.Fatalf("再启用后直连的新连接：%v", err)
	}
}

// c. 监护因保护失效关闸门（状态仍是启用）：走代理的长连接被断，直连的保持。
func TestGateClosedKeepsDirect(t *testing.T) {
	e := newM3Env(t, m3Rules)
	e.p.SetEnabled(true)
	plain := ap(e.lookupA(t, "plain.example.org"), 443)
	px, err := e.cl.dialFrom(portA+1, plain, ioWait)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	dc, err := e.cl.dialFrom(portC+1, plain, ioWait)
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Close()
	if !alive(px) || !alive(dc) {
		t.Fatal("两条都应能收发")
	}
	e.gate.open.Store(false) // 监护关闸门 → Teardown（DropAll）
	e.p.DropAll()
	if !waitDead(px) {
		t.Fatal("走代理的应被断开")
	}
	if !alive(dc) {
		t.Fatal("直连的应保持")
	}
}

// d. 规则热更新：不勾选断开时旧连接都保持；勾选时只断判定结果变了的。
func TestHotUpdate(t *testing.T) {
	e := newM3Env(t, m3Rules+"rules_extra_placeholder: 0\n"[:0])
	e.p.SetEnabled(true)
	plain := ap(e.lookupA(t, "plain.example.org"), 443)
	other := ap(e.lookupA(t, "other.example.org"), 443)
	dial := func(port uint16, dst netip.AddrPort) net.Conn {
		c, err := e.cl.dialFrom(port, dst, ioWait)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		if !alive(c) {
			t.Fatalf("端口 %d：应能收发", port)
		}
		return c
	}
	a := dial(portA+1, plain)  // 出口 a
	b := dial(portB+1, plain)  // 出口 b
	c := dial(portC+1, plain)  // 直连
	c2 := dial(portC+2, other) // 直连到另一个域名
	u := e.cl.udpFrom(t, portA+5, netip.MustParseAddrPort("203.0.113.9:5000"))
	udpAlive := func() bool {
		_, _ = u.Write([]byte("u"))
		_ = u.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 8)
		n, err := u.Read(buf)
		return err == nil && string(buf[:n]) == "u"
	}
	if !udpAlive() {
		t.Fatal("UDP 流应能收发")
	}
	newCfg := `
rules:
  - {app: com.app.a, action: "proxy:clash-b"}
  - {app: com.app.b, action: block}
  - {app: com.app.c, action: direct}
local_domains: ["*.corp.example"]
pinned_extra: [other.example.org]
`
	// 不勾选：旧连接都保持；新连接按新规则走。
	if err := e.p.SetRules(mustRules(t, newCfg), false); err != nil {
		t.Fatal(err)
	}
	for name, cn := range map[string]net.Conn{"a": a, "b": b, "c": c, "c2": c2} {
		if !alive(cn) {
			t.Fatalf("不勾选断开时 %s 应保持", name)
		}
	}
	if !udpAlive() {
		t.Fatal("不勾选断开时 UDP 流应保持")
	}
	n0 := len(e.op.all())
	if err := e.dialEcho(t, portA+2, plain); err != nil || e.op.all()[n0].Exit != "clash-b" {
		t.Fatalf("新连接应按新规则走出口 b：%v", err)
	}
	// 勾选：出口 a→b、走代理→拦截、加进置顶追加名单的直连被断；没变的直连 c 保持。
	if err := e.p.SetRules(mustRules(t, newCfg), true); err != nil {
		t.Fatal(err)
	}
	for name, cn := range map[string]net.Conn{"出口 a 改成出口 b": a, "走代理改成拦截": b, "域名加进置顶追加名单": c2} {
		if !waitDead(cn) {
			t.Fatalf("%s：应被断开", name)
		}
	}
	if !alive(c) {
		t.Fatal("判定没变的直连应保持")
	}
	// UDP 流的出口变了：旧流被断开；客户端再发的数据报会成为一条新流，按新规则走出口 b。
	nu := len(e.op.all())
	if !udpAlive() {
		t.Fatal("新的 UDP 流应能收发")
	}
	if hs := e.op.all(); len(hs) != nu+1 || hs[nu].Exit != "clash-b" || hs[nu].Net != "udp" {
		t.Fatalf("旧的 UDP 流应已断开，新流按新规则走出口 b：%+v", hs[nu:])
	}
	// 直连改成走代理：断开。无关规则变化：不断。
	c3 := dial(portC+3, plain)
	c4 := dial(portC+4, plain)
	_ = c4
	unrelated := strings.Replace(newCfg, "  - {app: com.app.c, action: direct}\n", "  - {app: com.app.c, action: direct}\n  - {app: com.app.zzz, action: block}\n", 1)
	if err := e.p.SetRules(mustRules(t, unrelated), true); err != nil {
		t.Fatal(err)
	}
	if !alive(c3) || !alive(c) {
		t.Fatal("无关规则变化时不应断开")
	}
	toProxy := strings.Replace(newCfg, "{app: com.app.c, action: direct}", "{app: com.app.c, action: \"proxy:wg-a\"}", 1)
	if err := e.p.SetRules(mustRules(t, toProxy), true); err != nil {
		t.Fatal(err)
	}
	if !waitDead(c3) {
		t.Fatal("直连改成走代理：应被断开")
	}
	if e.p.Stats().Rechecked < 5 {
		t.Fatalf("重判断开的计数：%d", e.p.Stats().Rechecked)
	}
}

// e. 重判的性质测试：随机的两份配置和若干在途流，保留下来的流按新配置、当前状态独立判定的结果与当时的动作相同，
// 被断开的都不同。直接调用 Proxy.TCP（net.Pipe），不经 gVisor。不少于 1000 组。
func TestRecheckProperty(t *testing.T) {
	r := rand.New(rand.NewSource(20261120))
	actions := []string{"direct", "block", "proxy:wg-a", "proxy:clash-b", "proxy:main"}
	pool := fakedns.DefaultPool()
	names := []string{"www.example.org", "claude.ai", "git.corp.example", "other.example.org"}
	fakes := map[string]netip.Addr{}
	for _, n := range names {
		a, _ := pool.Assign(n, time.Now())
		fakes[n] = a
	}
	randCfg := func() *rules.Config {
		y := "rules:\n"
		for _, app := range []string{"com.app.a", "com.app.b", "com.app.c"} {
			if r.Intn(5) > 0 {
				act := actions[r.Intn(len(actions))]
				if r.Intn(2) == 0 {
					act = "direct"
				}
				y += fmt.Sprintf("  - {app: %s, action: %q}\n", app, act)
			}
		}
		if r.Intn(2) == 0 {
			y += "local_domains: [\"*.corp.example\"]\n"
		}
		if r.Intn(2) == 0 {
			y += "offline_allow: [com.app.c, com.app.b]\n"
		}
		if r.Intn(3) == 0 {
			y += "pinned_extra: [other.example.org]\n"
		}
		return mustRules(t, y)
	}
	apps := []uint16{portA, portB, portC, portClaude, portOther}
	exitUp := func(x string) bool { return x == "wg-a" || x == "clash-b" }
	udpOK := func(string) bool { return true }
	kept, dropped, flowsN := 0, 0, 0
	const groups = 1000
	for gi := 0; gi < groups; gi++ {
		cfg1, cfg2 := randCfg(), randCfg()
		ident, direct, op := &fakeIdent{}, &fakeDirect{pinned: map[string]bool{}}, &fakeOpener{}
		g := openGate()
		p := &Proxy{Pool: pool, Gate: g, Opener: op, Ident: ident, Direct: direct, ExitUp: exitUp,
			SyncRules: func(c *rules.Config) error { pool.SetLocalMatcher(c.IsLocal); return nil }}
		_ = p.SetRules(cfg1, false)
		p.SetEnabled(true)
		type flow struct {
			f    Flow
			cli  net.Conn
			done chan struct{}
			was  rules.Decision
		}
		var flows []*flow
		for k := 0; k < 3; k++ {
			name := names[r.Intn(len(names))]
			f := Flow{Src: netip.AddrPortFrom(clientIP, apps[r.Intn(len(apps))]+uint16(r.Intn(900))), Dst: netip.AddrPortFrom(fakes[name], 443)}
			app, claude := ident.Identify(context.Background(), "tcp", f.Src, f.Dst)
			who := rules.Who{App: rules.Unknown}
			if cfg1.NeedApp() {
				who = rules.Who{App: app, Claude: claude}
			}
			was := cfg1.Decide(who, rules.Dest{Net: "tcp", Addr: f.Dst, FakeDomain: name, Local: cfg1.IsLocal(name)},
				rules.Env{State: rules.Enabled, GateOpen: true, ExitUp: exitUp, UDPOK: udpOK})
			a, b := net.Pipe()
			fl := &flow{f: f, cli: a, done: make(chan struct{}), was: was}
			go func() { p.TCP(context.Background(), f, b); b.Close(); close(fl.done) }()
			_ = a.SetDeadline(time.Now().Add(2 * time.Second))
			_, werr := a.Write([]byte("x"))
			_, rerr := io.ReadFull(a, make([]byte, 1))
			taken := werr == nil && rerr == nil
			if taken != (was.Action.Kind != rules.Block) {
				t.Fatalf("第 %d 组：建连结果 %v 与旧配置的判定 %v 不符", gi, taken, was.Action)
			}
			if taken {
				flows = append(flows, fl)
			} else {
				a.Close()
				<-fl.done
			}
		}
		// 一次换好状态和规则（状态先静默设好，只在 SetRules 时重判一次），再独立判定。
		enabled2 := r.Intn(3) > 0
		st := rules.Enabled
		if !enabled2 {
			st = rules.Disabled
			g.open.Store(false)
		}
		p.stateV.Store(int32(st))
		_ = p.SetRules(cfg2, true)
		for _, fl := range flows {
			flowsN++
			app, claude := ident.Identify(context.Background(), "tcp", fl.f.Src, fl.f.Dst)
			who := rules.Who{App: rules.Unknown}
			if cfg1.NeedApp() || cfg2.NeedApp() {
				who = rules.Who{App: app, Claude: claude}
			}
			m, rel, _ := pool.AcquireMapping(fl.f.Dst.Addr(), time.Now())
			rel()
			after := cfg2.Decide(who, rules.Dest{Net: "tcp", Addr: fl.f.Dst, FakeDomain: m.Name, Local: cfg2.IsLocal(m.Name)},
				rules.Env{State: st, GateOpen: g.Open(), ExitUp: exitUp, UDPOK: udpOK})
			wantDrop := after.Action != fl.was.Action
			// 断开是异步的（取消之后处理方才强制关 pipe）：期望断开的，等处理函数 2 秒内结束；
			// 期望保留的，回显正常且处理函数没结束。
			var gotDrop bool
			if wantDrop {
				select {
				case <-fl.done:
					gotDrop = true
				case <-time.After(2 * time.Second):
				}
			} else {
				gotDrop = !alive(fl.cli)
				select {
				case <-fl.done:
					gotDrop = true
				default:
				}
			}
			if gotDrop != wantDrop {
				t.Fatalf("第 %d 组：当时 %v，新判定 %v（%s），实际断开=%v", gi, fl.was.Action, after.Action, after.Reason, gotDrop)
			}
			if gotDrop {
				dropped++
			} else {
				kept++
			}
			fl.cli.Close()
			<-fl.done
		}
		if p.FlowRecords() != 0 {
			t.Fatalf("第 %d 组：流结束后记录应清掉，还剩 %d", gi, p.FlowRecords())
		}
	}
	if kept < 100 || dropped < 100 {
		t.Fatalf("覆盖不够：保留 %d、断开 %d", kept, dropped)
	}
	t.Logf("%d 组、%d 条在途流：保留 %d、断开 %d", groups, flowsN, kept, dropped)
}

// f. 原子替换：并发地一边换规则一边建连接，每条流的结果都是某一份完整配置下的结果（-race 下跑）。
// 两份配置给 A 指定不同的出口（wg-a、clash-b），并且只在其中一份里把 A 设成直连的“本地域名”目标不会被混用：
// 看到的出口只能是两者之一，不会出现“用 v1 的 NeedApp、v2 的规则”导致的 main。
func TestAtomicSwapConcurrent(t *testing.T) {
	e := newM3Env(t, m3Rules)
	e.p.SetEnabled(true)
	v1 := mustRules(t, "rules: [{app: com.app.a, action: \"proxy:wg-a\"}]\n")
	v2 := mustRules(t, "rules: [{app: com.app.a, action: \"proxy:clash-b\"}]\n")
	dst := ap(e.lookupA(t, "plain.example.org"), 443)
	stop := make(chan struct{})
	swapped := make(chan struct{})
	go func() {
		defer close(swapped)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			c := v1
			if i%2 == 1 {
				c = v2
			}
			_ = e.p.SetRules(c, false)
		}
	}()
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(port uint16) {
			defer wg.Done()
			if err := e.dialEcho(t, port, dst); err != nil {
				t.Errorf("端口 %d：%v", port, err)
			}
		}(portA + uint16(i))
	}
	wg.Wait()
	close(stop)
	<-swapped
	for _, h := range e.op.all() {
		if h.Exit != "wg-a" && h.Exit != "clash-b" {
			t.Fatalf("每条流应只用一份配置：出口 %s", h.Exit)
		}
	}
	waitFor(t, "记录清零", func() bool { return e.p.FlowRecords() == 0 })
}

// f. 替换时 fake-IP DNS 的本地名单同步更新；同步失败时规则保持旧的、返回错误。
func TestSwapSyncsLocalList(t *testing.T) {
	e := newM3Env(t, m3Rules)
	pool := e.p.Pool
	sync := func(c *rules.Config) error { pool.SetLocalMatcher(c.IsLocal); return nil }
	e.p.SyncRules = sync
	a := e.lookupA(t, "git.corp.example")
	b := e.lookupA(t, "wiki.intra.example")
	local := func(addr netip.Addr) bool {
		m, rel, _ := pool.AcquireMapping(addr, time.Now())
		rel()
		return m.Local
	}
	if err := e.p.SetRules(mustRules(t, "local_domains: [\"*.intra.example\"]\n"), false); err != nil {
		t.Fatal(err)
	}
	if local(a) || !local(b) {
		t.Fatal("换规则后本地标记应立刻跟着新名单变")
	}
	e.p.SyncRules = func(*rules.Config) error { return errors.New("注入的同步失败") }
	before := e.p.config()
	if err := e.p.SetRules(mustRules(t, "local_domains: [\"*.corp.example\"]\n"), true); err == nil {
		t.Fatal("同步失败时应返回错误")
	}
	if e.p.config() != before || local(a) || !local(b) {
		t.Fatal("同步失败时规则和名单都应保持旧的")
	}
}

// h. 在途流的记录不进任何输出；流结束后记录清掉。
func TestFlowRecordsCleared(t *testing.T) {
	e := newM3Env(t, offRules)
	e.p.SetEnabled(true)
	dst := ap(e.lookupA(t, "secret-flow.example.org"), 443)
	var conns []net.Conn
	for _, port := range []uint16{portA + 1, portC + 1, portB + 1} {
		c, err := e.cl.dialFrom(port, dst, ioWait)
		if err != nil {
			t.Fatal(err)
		}
		if !alive(c) {
			t.Fatal("应能收发")
		}
		conns = append(conns, c)
	}
	if n := e.p.FlowRecords(); n != 3 {
		t.Fatalf("应有 3 条记录：%d", n)
	}
	e.p.SetEnabled(false) // 触发重判
	b, _ := json.Marshal(e.p.Stats())
	for _, s := range []string{"secret-flow", "com.app", "198.18", "198.19"} {
		if strings.Contains(string(b), s) {
			t.Fatalf("计数里出现了 %q：%s", s, b)
		}
	}
	for _, c := range conns {
		c.Close()
	}
	waitFor(t, "记录清零", func() bool { return e.p.FlowRecords() == 0 })
}

// 第 0 步（architect 审 M3-6 的建议改 1）：全新的 Proxy 不调 SetEnabled 时按停用处理：带直连规则也不直连。
func TestDefaultDisabled(t *testing.T) {
	direct := &fakeDirect{pinned: map[string]bool{}}
	cfg := mustRules(t, "rules: [{app: com.app.c, action: direct}]\n")
	p := &Proxy{Gate: openGate(), Opener: &fakeOpener{}, Ident: &fakeIdent{}, Direct: direct,
		Rules: func() *rules.Config { return cfg }}
	if p.Enabled() {
		t.Fatal("没设状态时应按停用")
	}
	f := Flow{Src: netip.AddrPortFrom(clientIP, portC+1), Dst: netip.MustParseAddrPort("203.0.113.9:5222")}
	if p.AcceptTCP(f) {
		a, b := net.Pipe()
		go func() { _, _ = a.Write([]byte("\x00own")); a.Close() }()
		p.TCP(context.Background(), f, b)
	}
	if len(direct.list()) != 0 {
		t.Fatalf("没设状态时不应直连：%v", direct.list())
	}
}
