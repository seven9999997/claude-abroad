package rules

import (
	"math/rand"
	"net/netip"
	"testing"
)

func yes(string) bool { return true }
func no(string) bool  { return false }

func envOK() Env { return Env{State: Enabled, GateOpen: true, ExitUp: yes, UDPOK: yes} }

func mustParse(t testing.TB, s string) *Config {
	t.Helper()
	c, err := Parse([]byte(s))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return c
}

const testConf = `
default: proxy:main
rules:
  - {app: com.google.Chrome, action: "proxy:wg-a"}
  - {app: com.tencent.xinWeChat, action: direct}
  - {app: "path:/opt/homebrew/bin/curl", action: "proxy:clash-b"}
  - {app: com.example.Telemetry, action: block}
offline_allow: [com.tencent.xinWeChat, com.example.Telemetry]
local_domains: ["*.corp.example"]
`

var (
	chrome = Who{App: "com.google.Chrome"}
	wechat = Who{App: "com.tencent.xinWeChat"}
	curl   = Who{App: "path:/opt/homebrew/bin/curl"}
	telem  = Who{App: "com.example.Telemetry"}
	other  = Who{App: "com.example.Other"}
)

func ap(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

// 真实 IP、自有协议的首包：阶段 2 不会收紧。
func plain(port uint16) Dest {
	return Dest{Net: "tcp", Addr: netip.AddrPortFrom(netip.MustParseAddr("203.0.113.9"), port), Sniff: NotTLS}
}

func fake(name string) Dest {
	return Dest{Net: "tcp", Addr: ap("198.18.1.7:443"), FakeDomain: name}
}

func TestPriorityRows(t *testing.T) {
	c := mustParse(t, testConf)
	dis := envOK()
	dis.State, dis.GateOpen = Disabled, false
	closed := envOK()
	closed.GateOpen = false
	exitDown := envOK()
	exitDown.ExitUp = func(x string) bool { return x == ExitMain }
	udpNo := envOK()
	udpNo.UDPOK = func(x string) bool { return x == ExitMain }
	udpNone := envOK()
	udpNone.UDPOK = no
	udpExitOnly := envOK()
	udpExitOnly.UDPOK = func(x string) bool { return x != ExitMain }
	blk443 := mustParse(t, testConf+"block_udp_443: true\n")
	defBlock := mustParse(t, "default: block\n")

	udp := func(d Dest) Dest { d.Net = "udp"; return d }
	with := func(d Dest, f func(*Dest)) Dest { f(&d); return d }

	cases := []struct {
		name string
		c    *Config
		w    Who
		d    Dest
		e    Env
		want Action
		row  string
	}{
		{"1.1 block 规则", c, telem, plain(80), envOK(), ActBlock, "1.1"},
		{"1.2 本地域名直连（无视应用规则）", c, chrome, with(fake("git.corp.example"), func(d *Dest) { d.Local = true }), envOK(), ActDirect, "1.2"},
		{"1.3 停用时 offline_allow 直连", c, wechat, plain(80), dis, ActDirect, "1.3"},
		{"1.4 停用时其他应用拦截", c, chrome, plain(80), dis, ActBlock, "1.4"},
		{"1.5 应用规则 direct", c, wechat, plain(80), envOK(), ActDirect, "1.5"},
		{"1.5 应用规则 proxy", c, chrome, plain(80), envOK(), Action{Kind: Proxy, Exit: "wg-a"}, "1.5"},
		{"1.6 default 主代理", c, other, plain(80), envOK(), ActMain, "1.6"},
		{"1.6 default block", defBlock, other, plain(80), envOK(), ActBlock, "1.6"},
		{"1.6 unknown 只走 default", c, Who{App: Unknown}, plain(80), envOK(), ActMain, "1.6"},
		{"2.1 Claude 进程", c, Who{App: wechat.App, Claude: true}, plain(80), envOK(), ActMain, "2.1"},
		{"2.1 Claude 应用 ID", c, Who{App: "com.anthropic.claudefordesktop"}, plain(80), envOK(), ActMain, "1.6"},
		{"2.2 置顶：fake 域名", c, wechat, fake("api.anthropic.com"), envOK(), ActMain, "2.2"},
		{"2.2 置顶：首包名字", c, wechat, with(plain(443), func(d *Dest) { d.Sniff, d.SniffName = TLSName, "Claude.AI." }), envOK(), ActMain, "2.2"},
		{"2.2 置顶：反查", c, wechat, with(plain(443), func(d *Dest) { d.Reverse = "claude.com" }), envOK(), ActMain, "2.2"},
		{"2.3 解析落在 Anthropic 段", c, wechat, with(fake("example.org"), func(d *Dest) { d.Refused = true }), envOK(), ActMain, "2.3"},
		{"2.4 目的在 Anthropic 段", c, wechat, with(plain(80), func(d *Dest) { d.Addr = ap("160.79.104.10:8080") }), envOK(), ActMain, "2.4"},
		{"2.5 TLS 无 SNI", c, wechat, with(plain(443), func(d *Dest) { d.Sniff = TLSNoName }), envOK(), ActMain, "2.5"},
		{"2.5 TLS 带 ECH", c, wechat, with(plain(443), func(d *Dest) { d.Sniff, d.SniffName = TLSECH, "public.example" }), envOK(), ActMain, "2.5"},
		{"2.5 QUIC 无名字", c, wechat, udp(with(plain(443), func(d *Dest) { d.Sniff = QUICNoName })), envOK(), ActMain, "2.5"},
		{"2.5 首包名字不合法", c, wechat, with(plain(443), func(d *Dest) { d.Sniff, d.SniffName = TLSName, "bad_name-.x-" }), envOK(), ActMain, "2.5"},
		{"2.6 443 超时", c, wechat, with(plain(443), func(d *Dest) { d.Sniff = Timeout }), envOK(), ActMain, "2.6"},
		{"2.6 非 443 超时可直连", c, wechat, with(plain(5222), func(d *Dest) { d.Sniff = Timeout }), envOK(), ActDirect, "1.5"},
		{"2.7 没看首包", c, wechat, with(plain(80), func(d *Dest) { d.Sniff = SniffNone }), envOK(), ActMain, "2.7"},
		{"3.1 出口不可用回退主代理", c, chrome, plain(80), exitDown, ActMain, "3.1"},
		{"3.2 出口不支持 UDP 回退主代理", c, chrome, udp(plain(53)), udpNo, ActMain, "3.2"},
		{"3.3 主代理不支持 UDP 拦截", c, chrome, udp(plain(53)), udpNone, ActBlock, "3.3"},
		{"3.3 不触发：分流出口支持 UDP、主代理不支持", c, chrome, udp(plain(53)), udpExitOnly, Action{Kind: Proxy, Exit: "wg-a"}, "1.5"},
		{"3.3 由 3.2 回退到 main 后触发", c, chrome, udp(plain(53)), udpNone, ActBlock, "3.3"},
		{"4.1 拦截 UDP 443（走代理）", blk443, chrome, udp(plain(443)), envOK(), ActBlock, "4.1"},
		{"5.1 闸门关闭拦截走代理", c, chrome, plain(80), closed, ActBlock, "5.1"},
		{"5.2 闸门关闭时直连照常", c, wechat, plain(80), closed, ActDirect, "1.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.c.Decide(tc.w, tc.d, tc.e)
			if got.Action != tc.want || got.Row != tc.row {
				t.Fatalf("得到 %v（行 %s，%s），期望 %v（行 %s）", got.Action, got.Row, got.Reason, tc.want, tc.row)
			}
		})
	}
}

func TestPriorityConflicts(t *testing.T) {
	c := mustParse(t, testConf)
	dis := envOK()
	dis.State, dis.GateOpen = Disabled, false
	local := fake("git.corp.example")
	local.Local = true

	check := func(name string, got Decision, want Action, row string) {
		t.Helper()
		if got.Action != want || got.Row != row {
			t.Errorf("%s：得到 %v（行 %s），期望 %v（行 %s）", name, got.Action, got.Row, want, row)
		}
	}
	// 1.1 与 1.2：block 规则的应用访问本地域名，仍拦截。
	check("1.1+1.2", c.Decide(telem, local, envOK()), ActBlock, "1.1")
	// 1.1 与 1.3：block 规则的应用即使在 offline_allow 里，停用时也拦截。
	check("1.1+1.3", c.Decide(telem, plain(80), dis), ActBlock, "1.1")
	// 1.2 与 2.1：Claude 进程访问本地域名走主代理。
	check("1.2+2.1", c.Decide(Who{App: chrome.App, Claude: true}, local, envOK()), ActMain, "2.1")
	// 1.2 与 2.2：本地域名的映射同时被首包指到置顶域名（映射里是本地域名，首包是 claude.ai）。
	l2 := local
	l2.Sniff, l2.SniffName = TLSName, "claude.ai"
	check("1.2+2.2", c.Decide(chrome, l2, envOK()), ActMain, "2.2")
	// 1.2 在停用时不生效：不在 offline_allow 的应用访问本地域名被拦截。
	check("1.2 停用", c.Decide(chrome, local, dis), ActBlock, "1.4")
	// 1.3 与 2.2：offline_allow 的应用停用时访问置顶域名：先改走主代理，再被闸门拦下。
	check("1.3+2.2", c.Decide(wechat, fake("claude.ai"), dis), ActBlock, "5.1")
	// 2.5 在有 fake 映射时不触发：fake 映射到普通域名，首包是 ECH，仍直连。
	f := fake("example.org")
	f.Sniff = TLSECH
	check("2.5 有映射", c.Decide(wechat, f, envOK()), ActDirect, "1.5")
	// 3.1 与 3.2 同时成立：按 3.1 回退，并标 Fallback。
	e := envOK()
	e.ExitUp = func(x string) bool { return x == ExitMain }
	e.UDPOK = func(x string) bool { return x == ExitMain }
	u := plain(53)
	u.Net = "udp"
	got := c.Decide(chrome, u, e)
	check("3.1+3.2", got, ActMain, "3.1")
	if !got.Fallback {
		t.Error("3.1+3.2：应标 Fallback")
	}
	// 4.1 对直连和走代理都生效。
	b := mustParse(t, testConf+"block_udp_443: true\n")
	u443 := plain(443)
	u443.Net = "udp"
	check("4.1 直连", b.Decide(wechat, u443, envOK()), ActBlock, "4.1")
	check("4.1 代理", b.Decide(chrome, u443, envOK()), ActBlock, "4.1")
	// 5.1：ENABLED 闸门关闭、DISABLED 两种情况。
	closed := envOK()
	closed.GateOpen = false
	check("5.1 闸门关闭", c.Decide(other, plain(80), closed), ActBlock, "5.1")
	odd := envOK()
	odd.State = Disabled // 状态是停用但闸门标成开着：仍按停用处理
	check("5.1 停用", c.Decide(wechat, fake("claude.ai"), odd), ActBlock, "5.1")
}

func TestNilEnvFuncsAreStrict(t *testing.T) {
	c := mustParse(t, testConf)
	e := Env{State: Enabled, GateOpen: true}
	if got := c.Decide(chrome, plain(80), e); got.Action != ActMain || !got.Fallback {
		t.Fatalf("ExitUp 为 nil 时分流出口应视为不可用：%+v", got)
	}
	u := plain(53)
	u.Net = "udp"
	if got := c.Decide(chrome, u, e); got.Action != ActBlock {
		t.Fatalf("UDPOK 为 nil 时 UDP 应拦截：%+v", got)
	}
	var nilc *Config
	if got := nilc.Decide(wechat, plain(80), envOK()); got.Action != ActMain {
		t.Fatalf("nil 配置应按默认配置：%+v", got)
	}
}

func TestNeedSniff(t *testing.T) {
	c := mustParse(t, testConf)
	d := plain(443)
	d.Sniff = SniffNone
	if !c.NeedSniff(wechat, d, envOK()) {
		t.Error("直连应用、真实 IP、没看首包：要读首包")
	}
	if c.NeedSniff(chrome, d, envOK()) {
		t.Error("走代理的应用不需要首包")
	}
	if c.NeedSniff(wechat, fake("example.org"), envOK()) {
		t.Error("有 fake 映射时不需要首包")
	}
	if c.NeedSniff(Who{App: wechat.App, Claude: true}, d, envOK()) {
		t.Error("Claude 进程不可能直连，不需要首包")
	}
}

func TestMayDirectAndNeedApp(t *testing.T) {
	cases := []struct {
		y               string
		enabled, disabl bool
		needApp         bool
	}{
		{"", false, false, false},
		{"default: block\n", false, false, false},
		{"rules: [{app: com.a.b, action: \"proxy:wg-a\"}]\n", false, false, true},
		{"rules: [{app: com.a.b, action: block}]\n", false, false, true},
		{"rules: [{app: com.a.b, action: direct}]\n", true, false, true},
		{"local_domains: [\"*.corp.example\"]\n", true, false, true},
		{"offline_allow: [com.a.b]\n", true, true, true},
	}
	for _, c := range cases {
		cfg := mustParse(t, c.y)
		if cfg.MayDirect(Enabled) != c.enabled || cfg.MayDirect(Disabled) != c.disabl || cfg.NeedApp() != c.needApp {
			t.Errorf("%q：MayDirect(启用)=%v MayDirect(停用)=%v NeedApp=%v", c.y, cfg.MayDirect(Enabled), cfg.MayDirect(Disabled), cfg.NeedApp())
		}
	}
	var nilc *Config
	if nilc.MayDirect(Enabled) || nilc.NeedApp() {
		t.Fatal("nil 配置不可能直连、不需要识别")
	}
}

// MayDirect 为假时，任何输入都不会得到直连（与 Decide 一致）。
func TestMayDirectConsistent(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 5000; i++ {
		c := randConfig(r)
		w, d, e := randCase(r)
		if c.Decide(w, d, e).Action.Kind == Direct && !c.MayDirect(e.State) {
			t.Fatalf("MayDirect 为假却得到了直连：%+v %+v", w, d)
		}
		if !c.NeedApp() {
			// 不需要识别时，应用是谁不影响结果。
			w2 := w
			w2.App = Unknown
			w2.Claude = false
			if c.Decide(w, d, e) != c.Decide(w2, d, e) && !w.Claude && !IsClaudeApp(w.App) {
				t.Fatalf("NeedApp 为假时应用影响了结果")
			}
		}
	}
}
