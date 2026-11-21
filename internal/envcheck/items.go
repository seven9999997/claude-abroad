package envcheck

import (
	"context"
	"net/netip"
	"sort"
	"strings"

	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/preflight"
	"claude-abroad/internal/rules"
	"claude-abroad/internal/sysnet"
)

var (
	pass = func(d string) Outcome { return Outcome{Result: exitcheck.Pass, Detail: d} }
	fail = func(d, fix string) Outcome { return Outcome{Result: exitcheck.Fail, Detail: d, Fix: fix} }
	unk  = func(d string) Outcome {
		return Outcome{Result: exitcheck.Unknown, Detail: d, Fix: "检查本身没能完成，稍后会重试。"}
	}
)

// ---- 01 接管就绪 ----

// TakeoverView 是监护的核对（daemon 组装）：pf 保护有效、utun 活着、路由经我们的 utun 且没有冲突、DNS 在。
// 返回不通过的原因（空表示通过）；err 非 nil 表示查不了（无法判断）。原因里不含地址。
type TakeoverView func(ctx context.Context) (problems []string, err error)

// Takeover 是 01 接管就绪（硬性，只看本机状态）。
func Takeover(v TakeoverView) Item {
	return Item{ID: "01", Name: "接管就绪", Level: Hard, Run: func(ctx context.Context) Outcome {
		probs, err := v(ctx)
		switch {
		case err != nil:
			return unk("无法判断：读不出接管的状态")
		case len(probs) > 0:
			return fail("接管没有就绪："+strings.Join(probs, "；"), "稍等监护修复（一般几秒内）；持续不通过请执行“恢复本机网络”后重新启用。")
		}
		return pass("断网保护有效，路由和 DNS 都经 Claude 出国")
	}}
}

// ---- 06 IPv6 ----

// IPv6Deps 是 06 的依赖：各网络服务的 IPv6 快照（sysnet），和“连一个只有 IPv6 的地址”（必须失败）。
type IPv6Deps struct {
	Snapshot func(ctx context.Context) (map[string]sysnet.IPv6Setting, error)
	DialV6   func(ctx context.Context) error // 返回 nil 表示连上了（泄漏）
}

// IPv6 是 06（硬性，要发网络探测）。原值无法还原、我们没动的服务（unknown）单独列出，不算不通过。
func IPv6(d IPv6Deps) Item {
	return Item{ID: "06", Name: "IPv6", Level: Hard, Network: true, Run: func(ctx context.Context) Outcome {
		snap, err := d.Snapshot(ctx)
		if err != nil {
			return unk("无法判断：读不出网络服务的 IPv6 设置")
		}
		var on, untouched []string
		for svc, st := range snap {
			switch st.Mode {
			case sysnet.IPv6Off:
			case sysnet.IPv6Unknown:
				untouched = append(untouched, svc)
			default:
				on = append(on, svc)
			}
		}
		sort.Strings(on)
		sort.Strings(untouched)
		if len(on) > 0 {
			return fail("这些网络服务的 IPv6 没有关闭："+strings.Join(on, "、"), "稍等监护修复；持续不通过请执行“恢复本机网络”后重新启用。")
		}
		if d.DialV6(ctx) == nil {
			return fail("本机能连上只有 IPv6 的地址（IPv6 会绕过 Claude 出国）", "请执行“恢复本机网络”后重新启用；仍不行请检查有没有别的软件打开了 IPv6。")
		}
		detail := "各网络服务的 IPv6 都已关闭，只有 IPv6 的地址连不上"
		if len(untouched) > 0 {
			detail += "；这些网络服务的 IPv6 设置没法原样还原，没有改动（断网规则照样拦 IPv6）：" + strings.Join(untouched, "、")
		}
		return pass(detail)
	}}
}

// ---- 07 DNS ----

// ProbeName 是 07 解析的测试域名（由我们自己的 fake-IP DNS 应答，不对外查询）。
const ProbeName = "envcheck.claude-abroad.invalid"

var fakeNet = netip.MustParsePrefix("198.18.0.0/15")

// DNS 是 07（硬性，要发网络探测）：经系统解析器解析测试域名，结果必须在 fake 段内；再经我们的 DNS 查回显解析器地址的记录，
// 解析器的出口不能在大陆（6b；echo 为 nil 时只做前一半）。
func DNS(resolve func(ctx context.Context, name string) ([]netip.Addr, error), echo ...EchoResolver) Item {
	var ec EchoResolver
	if len(echo) > 0 {
		ec = echo[0]
	}
	return Item{ID: "07", Name: "DNS", Level: Hard, Network: true, Run: func(ctx context.Context) Outcome {
		addrs, err := resolve(ctx, ProbeName)
		if err != nil || len(addrs) == 0 {
			return unk("无法判断：系统解析器没有应答")
		}
		for _, a := range addrs {
			if !fakeNet.Contains(a.Unmap()) {
				return fail("系统的域名解析没有经过 Claude 出国", "检查有没有别的软件改了 DNS；稍等监护修复，持续不通过请重新启用。")
			}
		}
		bad, known := resolverMainland(ctx, ec)
		switch {
		case bad:
			return fail("域名解析的上游在中国大陆", "主代理的 DNS 出口在大陆：换一个节点。")
		case !known:
			return unk("无法判断：查不到解析器的位置（系统的解析已经经过 Claude 出国）")
		}
		return pass("系统的域名解析经过 Claude 出国，上游不在大陆")
	}}
}

// ---- 11 系统代理与其他接管 ----

// Conflicts 是 11（硬性，只看本机状态）：复用冲突检查里系统代理、PAC、自动发现、路由冲突这几项（带上我们自己的 utun 名）。
func Conflicts(check func(ctx context.Context) []preflight.Finding) Item {
	return Item{ID: "11", Name: "系统代理与其他接管", Level: Hard, Run: func(ctx context.Context) Outcome {
		fs := check(ctx)
		var titles, fixes []string
		undecided := false
		for _, f := range fs {
			if f.Undecidable {
				undecided = true
				continue
			}
			titles = append(titles, f.Title)
			fixes = append(fixes, f.Fix)
		}
		switch {
		case len(titles) > 0:
			return fail(strings.Join(titles, "；"), strings.Join(fixes, " "))
		case undecided:
			return unk("无法判断：读不出系统代理或路由的设置")
		}
		return pass("没有系统代理，也没有别的软件抢路由")
	}}
}

// ---- 09 Claude 去向 ----

// RouteDeps 是 09 的依赖：当前规则、出口是否可用。只调用规则引擎，不发任何网络请求。
type RouteDeps struct {
	Config func() *rules.Config
	ExitUp func(exit string) bool
	// Decide 是判定函数（nil 用 Config.Decide；测试注入来模拟规则引擎出错）。
	Decide func(c *rules.Config, w rules.Who, d rules.Dest, e rules.Env) rules.Decision
}

// ClaudeRoute 是 09（硬性，只调用规则引擎）。
func ClaudeRoute(d RouteDeps) Item {
	return Item{ID: "09", Name: "Claude 去向", Level: Hard, Run: func(ctx context.Context) Outcome {
		return claudeRoute(d)
	}}
}
