package rules

import "net/netip"

// SniffKind 是首包识别的结果（M3-2 的 sniff 包产出）。
type SniffKind int

const (
	SniffNone  SniffKind = iota // 没有嗅探
	NotTLS                      // 明显不是 TLS、QUIC、HTTP
	TLSName                     // TLS，带明文 SNI
	TLSNoName                   // TLS，没有 SNI，或解析失败
	TLSECH                      // TLS，带加密的 ClientHello
	QUICName                    // QUIC Initial，解出了 SNI
	QUICNoName                  // QUIC，但没有 SNI 或解不开
	HTTPHost                    // 明文 HTTP，带 Host
	Timeout                     // 时限内首包没到
)

// State 是规则引擎关心的状态。
type State int

const (
	Enabled State = iota + 1
	Disabled
)

// Dest 是一条连接的去向信息，由网络栈收集。
type Dest struct {
	Net        string         // "tcp" 或 "udp"
	Addr       netip.AddrPort // 原始目的地址（假地址或真实地址）
	FakeDomain string         // fake-IP 映射出的域名；空表示没有映射
	Sniff      SniffKind
	SniffName  string // TLSName、QUICName、HTTPHost 时的域名
	Reverse    string // 反查表的结果，只用来变严格
	Local      bool   // fake 映射带“本地域名”标记
	Refused    bool   // 直连拨号模块回了 pinned（解析结果落在 Anthropic 地址段）
}

// Who 是连接属于谁。
type Who struct {
	App    AppID
	Claude bool // 属于 Claude 进程（含子进程）
}

// Env 是判定时的外部状态。函数为 nil 时按最严格处理：分流出口都不可用，UDP 都不支持。
type Env struct {
	State    State
	GateOpen bool
	ExitUp   func(exit string) bool
	UDPOK    func(exit string) bool
}

func (e Env) exitUp(x string) bool { return e.ExitUp != nil && e.ExitUp(x) }
func (e Env) udpOK(x string) bool  { return e.UDPOK != nil && e.UDPOK(x) }

// Reason 是判定理由，只用于计数和调试，不带域名和地址。
type Reason string

const (
	RRuleBlock      Reason = "rule_block"
	RLocalDomain    Reason = "local_domain"
	ROffline        Reason = "offline"
	RRule           Reason = "rule"
	RDefault        Reason = "default"
	RClaudeProcess  Reason = "claude_process"
	RPinned         Reason = "pinned"
	RPinnedResolved Reason = "pinned_resolved"
	RAnthropicNet   Reason = "anthropic_net"
	ROpaqueTLS      Reason = "opaque_tls"
	RPort443Timeout Reason = "port443_timeout"
	RNoSniff        Reason = "no_sniff"
	RExitFallback   Reason = "exit_fallback"
	RUDPFallback    Reason = "udp_fallback"
	RUDPUnsupported Reason = "udp_unsupported"
	RBlockUDP443    Reason = "block_udp443"
	RGateClosed     Reason = "gate_closed"
)

// Decision 是判定结果。Row 是 m3-plan 优先级表里最后一个改变结果的行号，便于测试和排查。
type Decision struct {
	Action   Action
	Reason   Reason
	Fallback bool // 规则指向的出口不可用而回退到主代理
	Row      string
}

// Decide 按 m3-plan v2.1 的优先级表判定，是纯函数。
func (c *Config) Decide(w Who, d Dest, e Env) Decision {
	if c == nil {
		c = DefaultConfig()
	}
	disabled := e.State != Enabled // 不认识的状态按停用处理

	// 阶段 1：候选动作
	var dec Decision
	rule, hasRule := c.rule(w.App)
	switch {
	case hasRule && rule.Kind == Block:
		return Decision{Action: ActBlock, Reason: RRuleBlock, Row: "1.1"}
	case d.Local && !disabled:
		dec = Decision{Action: ActDirect, Reason: RLocalDomain, Row: "1.2"}
	case disabled && c.offline(w.App):
		dec = Decision{Action: ActDirect, Reason: ROffline, Row: "1.3"}
	case disabled:
		return Decision{Action: ActBlock, Reason: ROffline, Row: "1.4"}
	case hasRule:
		dec = Decision{Action: rule, Reason: RRule, Row: "1.5"}
	default:
		if c.Default.Kind == Block {
			return Decision{Action: ActBlock, Reason: RDefault, Row: "1.6"}
		}
		dec = Decision{Action: ActMain, Reason: RDefault, Row: "1.6"}
	}

	// 阶段 2：直连的收紧
	if dec.Action.Kind == Direct {
		if r, row := c.tighten(w, d); row != "" {
			dec = Decision{Action: ActMain, Reason: r, Row: row}
		}
	}

	// 阶段 3：出口回退
	udp := d.Net != "tcp"
	if dec.Action.Kind == Proxy {
		x := dec.Action.Exit
		switch {
		case x != ExitMain && !e.exitUp(x):
			dec = Decision{Action: ActMain, Reason: RExitFallback, Fallback: true, Row: "3.1"}
		case udp && x != ExitMain && !e.udpOK(x):
			dec = Decision{Action: ActMain, Reason: RUDPFallback, Row: "3.2"}
		}
		// 3.3 只管此时的出口是 main 的情况（规则本来指向 main，或经 3.1、3.2 回退而来）；
		// 走分流出口且它支持 UDP 时，不因主代理不支持 UDP 而拦截。
		if udp && dec.Action.Exit == ExitMain && !e.udpOK(ExitMain) {
			return Decision{Action: ActBlock, Reason: RUDPUnsupported, Row: "3.3"}
		}
	}

	// 阶段 4：拦截 UDP 443
	if c.BlockUDP443 && udp && d.Addr.Port() == 443 {
		return Decision{Action: ActBlock, Reason: RBlockUDP443, Row: "4.1"}
	}

	// 阶段 5：闸门与状态
	if dec.Action.Kind == Proxy && (!e.GateOpen || disabled) {
		return Decision{Action: ActBlock, Reason: RGateClosed, Row: "5.1"}
	}
	return dec
}

// tighten 是阶段 2：任一行命中就改走主代理，返回第一条命中的理由和行号。
func (c *Config) tighten(w Who, d Dest) (Reason, string) {
	if w.Claude || IsClaudeApp(w.App) {
		return RClaudeProcess, "2.1"
	}
	for _, n := range []string{d.FakeDomain, d.SniffName, d.Reverse} {
		if n == "" {
			continue
		}
		if p, _ := c.isPinned(n); p {
			return RPinned, "2.2"
		}
	}
	if d.Refused {
		return RPinnedResolved, "2.3"
	}
	if d.Addr.Addr().IsValid() && InAnthropicNet(d.Addr.Addr()) {
		return RAnthropicNet, "2.4"
	}
	// 映射或首包里的名字不合法：说不清去向，按看不清处理（不管有没有 fake 映射）。
	// 反查表只能让结果更严格，它的名字不合法就忽略。
	if bad(d.FakeDomain) || bad(d.SniffName) {
		return ROpaqueTLS, "2.5"
	}
	if d.FakeDomain == "" {
		switch d.Sniff {
		case TLSNoName, TLSECH, QUICNoName:
			return ROpaqueTLS, "2.5"
		case TLSName, QUICName, HTTPHost:
			if d.SniffName == "" {
				return ROpaqueTLS, "2.5"
			}
		case Timeout:
			if d.Addr.Port() == 443 {
				return RPort443Timeout, "2.6"
			}
		case SniffNone:
			return RNoSniff, "2.7"
		case NotTLS:
		default:
			return RNoSniff, "2.7" // 不认识的首包结果按没看首包处理
		}
	}
	return "", ""
}

func bad(n string) bool {
	if n == "" {
		return false
	}
	_, err := NormalizeName(n)
	return err != nil
}

// NeedSniff 判断要不要读首包：只有没有 fake 映射、还没有首包结果、而且首包结果可能让它得到直连时才需要。
// 与 Decide 的一致性由测试保证：凡是某个首包结果能得到直连的输入，这里都为真。
func (c *Config) NeedSniff(w Who, d Dest, e Env) bool {
	if d.FakeDomain != "" || d.Sniff != SniffNone {
		return false
	}
	probe := d
	probe.Sniff, probe.SniffName = NotTLS, "" // 最宽松的首包结果
	return c.Decide(w, probe, e).Action.Kind == Direct
}

func (c *Config) rule(app AppID) (Action, bool) {
	app = NormalizeAppID(app)
	if app == Unknown {
		return Action{}, false
	}
	for _, r := range c.Rules {
		if r.App == app {
			return r.Action, true
		}
	}
	return Action{}, false
}

func (c *Config) offline(app AppID) bool {
	app = NormalizeAppID(app)
	if app == Unknown {
		return false
	}
	for _, a := range c.OfflineAllow {
		if a == app {
			return true
		}
	}
	return false
}

// MayDirect 判断在当前配置和状态下是否可能出现直连（m3-plan v2.6 补充 6）：网络栈据此决定闸门关闭时
// 是在 SYN 就拒（不可能直连），还是先接受再判（可能直连）。偏宽：返回真只说明“可能”。
func (c *Config) MayDirect(s State) bool {
	if c == nil {
		return false
	}
	if s != Enabled {
		return len(c.OfflineAllow) > 0
	}
	if len(c.LocalDomains) > 0 {
		return true
	}
	for _, r := range c.Rules {
		if r.Action.Kind == Direct {
			return true
		}
	}
	return len(c.OfflineAllow) > 0 // 启用状态下 offline_allow 不带来直连，但保持偏宽，状态切换的瞬间也安全
}

// NeedApp 判断判定时是否需要识别应用（m3-plan v2.6 补充 4）：没有任何应用规则、offline_allow 为空、
// 也没有 local_domains 时，应用是谁不影响结果，不调用识别器。
// （有 local_domains 时仍要识别：本地域名会带来直连，而 Claude 进程访问本地域名要改走主代理。）
func (c *Config) NeedApp() bool {
	return c != nil && (len(c.Rules) > 0 || len(c.OfflineAllow) > 0 || len(c.LocalDomains) > 0)
}
