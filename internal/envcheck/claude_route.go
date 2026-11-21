package envcheck

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"time"

	"claude-abroad/internal/exitpolicy"
	"claude-abroad/internal/wire"

	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/rules"
)

// 第 09 项（m4-plan v2.6 决定 6）。Claude 的域名字面量只允许出现在规则引擎的置顶名单和第 09、10 项里（源码检查）。
const claudeHost = "claude.ai"

// localProbeApp 是模拟的“设成本机网络的普通应用”（只用于判定，不对应真实应用）。
const localProbeApp rules.AppID = "com.claude-abroad.envcheck-local"

var claudeApps = []struct {
	name string
	who  rules.Who
}{
	{"Claude 桌面版", rules.Who{App: rules.AppID(rules.ClaudeBundleIDs[0]), Claude: true}},
	{"Claude Code", rules.Who{App: "path:/usr/local/bin/claude", Claude: true}},
}

func claudeRoute(d RouteDeps) Outcome {
	cfg := d.Config()
	if cfg == nil {
		return unk("无法判断：读不出当前的分流规则")
	}
	decide := d.Decide
	if decide == nil {
		decide = func(c *rules.Config, w rules.Who, de rules.Dest, e rules.Env) rules.Decision {
			return c.Decide(w, de, e)
		}
	}
	// 配置本身：Claude 不能被设成直连（解析时会拒；这里防绕过校验构造出来的配置——判定会把它收紧回主代理，看不出来）。
	for _, r := range cfg.Rules {
		if rules.IsClaudeApp(r.App) && r.Action.Kind != rules.Proxy {
			return fail("分流规则把 Claude 设成了不走代理", "在分流规则里把 Claude 设为走主代理或一个可用的分流出口。")
		}
	}
	env := rules.Env{State: rules.Enabled, GateOpen: true, ExitUp: d.ExitUp}
	dest := rules.Dest{Net: "tcp", Addr: netip.AddrPortFrom(netip.MustParseAddr("198.18.0.10"), 443), FakeDomain: claudeHost}
	up := func(x string) bool { return x == rules.ExitMain || (d.ExitUp != nil && d.ExitUp(x)) }
	for _, a := range claudeApps {
		dec := decide(cfg, a.who, dest, env)
		switch {
		case dec.Action.Kind != rules.Proxy:
			return fail(a.name+"访问 Claude 时不走代理", "在分流规则里把 Claude 设为走主代理或一个可用的分流出口。")
		case !up(dec.Action.Exit):
			return fail(a.name+"走的出口当前不可用", "换一个可用的出口，或把 Claude 设为走主代理。")
		}
	}
	// 设成本机网络的普通应用访问 Claude：置顶规则必须把它改走主代理。
	local := *cfg
	local.Rules = append([]rules.Rule{{App: localProbeApp, Action: rules.ActDirect}}, cfg.Rules...)
	dec := decide(&local, rules.Who{App: localProbeApp}, dest, env)
	if dec.Action.Kind != rules.Proxy || dec.Action.Exit != rules.ExitMain {
		return fail("设成本机网络的应用访问 Claude 时没有被改走主代理", "这是 Claude 出国的问题：请执行“恢复本机网络”并反馈。")
	}
	return Outcome{Result: exitcheck.Pass, Detail: "Claude 走代理且出口可用；本机网络的应用访问 Claude 会被改走主代理"}
}

// ClaudeRouteKey 是“Claude 访问 Claude 时的去向”的摘要（daemon 换规则时比较前后，变了就补一轮检测；决定 4）。
func ClaudeRouteKey(cfg *rules.Config, exitUp func(string) bool) string {
	if cfg == nil {
		return ""
	}
	env := rules.Env{State: rules.Enabled, GateOpen: true, ExitUp: exitUp}
	dest := rules.Dest{Net: "tcp", Addr: netip.AddrPortFrom(netip.MustParseAddr("198.18.0.10"), 443), FakeDomain: claudeHost}
	key := ""
	for _, a := range claudeApps {
		key += cfg.Decide(a.who, dest, env).Action.String() + ";"
	}
	return key
}

// ---- 10 Claude 可达性（m4-plan v2.7 决定 6）----

// apiHost 是 Claude 的接口域名（第 10 项）。
const apiHost = "api.anthropic.com"

// ClaudeExits 返回规则里 Claude 实际走的出口（去重；回退到主代理时就是主代理）。
func ClaudeExits(cfg *rules.Config, exitUp func(string) bool) []string {
	if cfg == nil {
		return []string{rules.ExitMain}
	}
	env := rules.Env{State: rules.Enabled, GateOpen: true, ExitUp: exitUp}
	dest := rules.Dest{Net: "tcp", Addr: netip.AddrPortFrom(netip.MustParseAddr("198.18.0.10"), 443), FakeDomain: claudeHost}
	seen := map[string]bool{}
	var out []string
	for _, a := range claudeApps {
		dec := cfg.Decide(a.who, dest, env)
		if dec.Action.Kind != rules.Proxy {
			continue
		}
		x := dec.Action.Exit
		if x == "" {
			x = rules.ExitMain
		}
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	if len(out) == 0 {
		out = []string{rules.ExitMain}
	}
	return out
}

// ReachDeps 是 10 的依赖：经数据通道按出口开流（同 exitcheck，不经 utun、不经闸门）。
type ReachDeps struct {
	Open    func(ctx context.Context, h wire.Header) (net.Conn, error)
	Exits   func() []string
	RootCAs *x509.CertPool // nil 用系统根证书（测试注入假 CA）
	Timeout time.Duration  // 每个目标的时限，默认 10 秒
}

type reachResult int

const (
	reachOK reachResult = iota
	reachBad
	reachUnknown
)

// reachOne 经出口 exit 对 host 做一次 TLS 握手（证书照常校验）和最简单的 HTTP 请求（通用浏览器标识，不带任何凭据）。
func reachOne(ctx context.Context, d ReachDeps, exit, host string) reachResult {
	to := d.Timeout
	if to <= 0 {
		to = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	c, err := d.Open(ctx, wire.Header{V: wire.Version, Exit: exit, Net: wire.TCP, Host: host, Port: 443})
	if err != nil {
		return reachUnknown
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	tc := tls.Client(c, &tls.Config{ServerName: host, RootCAs: d.RootCAs, MinVersion: tls.VersionTLS12})
	if err := tc.HandshakeContext(ctx); err != nil {
		var ua x509.UnknownAuthorityError
		var he x509.HostnameError
		var ci x509.CertificateInvalidError
		var ae tls.AlertError
		var cv *tls.CertificateVerificationError
		if errors.As(err, &ua) || errors.As(err, &he) || errors.As(err, &ci) || errors.As(err, &ae) || errors.As(err, &cv) {
			return reachBad
		}
		return reachUnknown
	}
	req := "HEAD / HTTP/1.1\r\nHost: " + host + "\r\nUser-Agent: " + exitpolicy.BrowserUA + "\r\nAccept: */*\r\nConnection: close\r\n\r\n"
	if _, err := io.WriteString(tc, req); err != nil {
		return reachUnknown
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		return reachUnknown
	}
	resp.Body.Close()
	return reachOK // 任意状态码都算到达
}

// ClaudeReach 是 10 Claude 可达性（硬性，要发网络探测）：规则里 Claude 走的每个出口，对 claude.ai 和 api.anthropic.com
// 各做一次。握手失败或证书不对是不通过；超时、连接失败是无法判断。
func ClaudeReach(d ReachDeps) Item {
	return Item{ID: "10", Name: "Claude 可达性", Level: Hard, Network: true, Run: func(ctx context.Context) Outcome {
		unknown := false
		for _, x := range d.Exits() {
			for _, h := range []string{claudeHost, apiHost} {
				switch reachOne(ctx, d, x, h) {
				case reachBad:
					return fail("经出口 "+x+" 访问 Claude 时 TLS 握手失败或证书不对（可能被劫持）", "换一个节点；如果所有节点都这样，检查本机有没有装来路不明的根证书。")
				case reachUnknown:
					unknown = true
				}
			}
		}
		if unknown {
			return unk("无法判断：访问 Claude 超时或连不上")
		}
		return pass("经 Claude 实际走的出口能访问 Claude")
	}}
}
