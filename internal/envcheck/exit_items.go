package envcheck

import (
	"context"
	"strings"
	"sync"
	"time"

	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/exitpolicy"
	"claude-abroad/internal/region"
)

// ExitProbe 是 02、03、13、14 共用的“从本机看出口”：经 ProbeOpener 跑一轮 exitcheck（带位置），同一轮里复用结果。
type ExitProbe struct {
	Checker  *exitcheck.Checker           // Opener 是 ProbeOpener
	Baseline func() *exitcheck.Baseline   // 当前的出口基准（没有时 nil）
	Target   func() (region.Target, bool) // 锁定的目标地区（地区同步没接时 false）
	Now      func() time.Time
	TTL      time.Duration // 同一轮复用多久，默认 30 秒
	// Run、LocateFn 是测试注入（nil 时用 Checker）。
	Run      func(ctx context.Context, base *exitcheck.Baseline) exitcheck.Report
	LocateFn func(ctx context.Context, ip netipAddr) (string, error)

	mu  sync.Mutex
	rep *exitcheck.Report
	at  time.Time
}

func (e *ExitProbe) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Report 返回这一轮的结果（TTL 内复用）。
func (e *ExitProbe) Report(ctx context.Context) exitcheck.Report {
	e.mu.Lock()
	defer e.mu.Unlock()
	ttl := e.TTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if e.rep != nil && e.now().Sub(e.at) < ttl {
		return *e.rep
	}
	var base *exitcheck.Baseline
	if e.Baseline != nil {
		base = e.Baseline()
	}
	var rep exitcheck.Report
	if e.Run != nil {
		rep = e.Run(ctx, base)
	} else {
		rep = e.Checker.Check(ctx, "main", exitcheck.Options{Base: base, WantGeo: true})
	}
	e.rep, e.at = &rep, e.now()
	return rep
}

// Locate 查一个 IP 的国家（经位置缓存；08 用）。
func (e *ExitProbe) Locate(ctx context.Context, ip netipAddr) (string, error) {
	if e.LocateFn != nil {
		return e.LocateFn(ctx, ip)
	}
	g, err := e.Checker.Locate(ctx, "main", ip)
	if err != nil {
		return "", err
	}
	return g.Country, nil
}

func itemOf(rep exitcheck.Report, name string) exitcheck.Item {
	it, _ := rep.Item(name)
	return it
}

// ExitIP 是 02 出口 IP（硬性，要发网络探测）：取 IP 的源经本机发起，结果按基准和出口变化规则比对。
func ExitIP(p *ExitProbe) Item {
	return Item{ID: "02", Name: "出口 IP", Level: Hard, Network: true, Run: func(ctx context.Context) Outcome {
		rep := p.Report(ctx)
		ip, rule := itemOf(rep, "ip"), itemOf(rep, "rule")
		switch {
		case ip.Result != exitcheck.Pass:
			return unk("无法判断：取不到出口 IP")
		case rule.Result == exitcheck.Fail:
			return fail("出口变了，不符合出口变化规则", "换回原来的节点，或重新“应用主代理”建立新的出口基准。")
		case rule.Result != exitcheck.Pass:
			return unk("无法判断：没法和出口基准比对（" + exitcheck.CauseText(rule.Cause) + "）")
		}
		return pass("出口符合出口变化规则")
	}}
}

// ExitRegion 是 03 出口地区（硬性，要发网络探测）：不在大陆段；国家等于锁定目标的国家（地区同步没接时只判大陆段）。
func ExitRegion(p *ExitProbe) Item {
	return Item{ID: "03", Name: "出口地区", Level: Hard, Network: true, Run: func(ctx context.Context) Outcome {
		rep := p.Report(ctx)
		ml := itemOf(rep, "mainland")
		switch ml.Result {
		case exitcheck.Fail:
			return fail("出口在中国大陆", "换一个不在大陆的节点。")
		case exitcheck.Pass:
		default:
			return unk("无法判断：取不到出口 IP")
		}
		t, ok := (region.Target{}), false
		if p.Target != nil {
			t, ok = p.Target()
		}
		if !ok || t.Country == "" {
			return pass("出口不在大陆")
		}
		if rep.Country == "" {
			return unk("无法判断：查不到出口的国家")
		}
		if rep.Country != t.Country {
			return fail("出口的国家和锁定的目标地区不同", "换一个目标国家的节点，或重新“应用主代理”、切换地区。")
		}
		return pass("出口不在大陆，国家和目标地区一致")
	}}
}

// IPStability 是 13 IP 稳定性（警告，要发网络探测）：48 小时内出口变化 3 次以上给警告。
func IPStability(p *ExitProbe, h *History) Item {
	return Item{ID: "13", Name: "IP 稳定性", Level: Warn, Network: true, Run: func(ctx context.Context) Outcome {
		rep := p.Report(ctx)
		if !rep.IP.IsValid() {
			return unk("无法判断：取不到出口 IP")
		}
		n, err := h.Record(rep.IP, p.now())
		if err != nil {
			return unk("无法判断：出口变化记录写不进去")
		}
		if n >= HistoryWarnAt {
			return fail("48 小时内出口变了 "+itoa(n)+" 次", "出口频繁变化容易触发风控：换一个稳定的节点，或固定使用同一个节点。")
		}
		return pass("48 小时内出口变化 " + itoa(n) + " 次")
	}}
}

// datacenterWords 是疑似机房的运营商关键词（小写）。只是参考，不等于住宅认证。
var datacenterWords = []string{"hosting", "host", "datacenter", "data center", "cloud", "server", "vps", "colo",
	"amazon", "aws", "google", "microsoft", "azure", "digitalocean", "linode", "akamai", "vultr", "choopa", "ovh",
	"hetzner", "leaseweb", "m247", "oracle", "alibaba", "tencent", "contabo", "scaleway", "dedicated"}

// NetworkType 是 14 网络类型（警告，要发网络探测）：按出口的运营商和组织名判断是否疑似机房。
func NetworkType(p *ExitProbe) Item {
	return Item{ID: "14", Name: "网络类型", Level: Warn, Network: true, Run: func(ctx context.Context) Outcome {
		rep := p.Report(ctx)
		if rep.Geo == nil || strings.TrimSpace(rep.Geo.Org) == "" {
			return unk("无法判断：查不到出口的运营商")
		}
		org := strings.ToLower(rep.Geo.Org)
		for _, w := range datacenterWords {
			if strings.Contains(org, w) {
				return fail("出口疑似机房网络（按运营商名判断，只是参考）", "可以换住宅宽带的节点；这一项只是提示，不影响使用。")
			}
		}
		return pass("出口不像机房网络（只是参考）")
	}}
}

func itoa(n int) string { return strconvItoa(n) }

var _ = exitpolicy.Geo{}
