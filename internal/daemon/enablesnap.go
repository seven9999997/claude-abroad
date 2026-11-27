package daemon

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"time"

	"claude-abroad/internal/pf"
	"claude-abroad/internal/redact"
	"claude-abroad/internal/state"
	"claude-abroad/internal/sysnet"
	"claude-abroad/internal/usernet"
)

// 启用失败的现场快照（2026-11-27 真机：启用两次都在出口检查失败、回滚，虚拟机对第一跳没有一次成功的拨号，
// 而回滚之后 pf 规则、路由都已经撤掉，事后看不到当时的情况）。启用在出口检查没通过、回滚之前留一份：
//   - pf：是否启用、主规则集是否还引用锚点、我们锚点里每条规则的匹配计数（pfctl -a <锚点> -v -s rules，经 internal/pf）；
//   - 路由：去第一跳走哪个接口（第一跳是 IP 时；是域名时不解析——不经被接管的系统 DNS）、两条 /1 各走哪个接口、
//     占着默认或公网路由的别的隧道接口；
//   - 虚拟机：拨号绑定的接口、拨号计数（host.sock 的 netstats）。
//
// 只读：pfctl 只用 -s 一类的查询，路由只用 route -n get、netstat -rn。只记接口名和计数，不记地址：
// 规则原文经 redact（DHCP、局域网那几行的网段换掉），netstats 只留数字和接口名、出站模式。

// EnableSnapshot 是启用失败时的现场快照（事件里是摘要，诊断包里是全文）。
type EnableSnapshot struct {
	Time time.Time    `json:"time"`
	PF   *pf.Counters `json:"pf,omitempty"`
	// OurUtun 是我们的 utun（空表示当时没有）。
	OurUtun string `json:"our_utun"`
	// FirstHopKind：ip、domain、none（读不出主代理）。
	FirstHopKind string `json:"first_hop_kind"`
	// FirstHopRouteIface 是去第一跳的路由走的接口（不绑网卡时的路由；第一跳是 IP 时才查）。
	FirstHopRouteIface string `json:"first_hop_route_iface,omitempty"`
	// HalfRouteIfaces 是两条 /1（各取一个地址探）走的接口：启用中应是我们的 utun。
	HalfRouteIfaces map[string]string `json:"half_route_ifaces,omitempty"`
	// ForeignTunnels 是占着默认路由或公网路由的别的隧道接口（同冲突检查的判定，不含我们的 utun）。
	ForeignTunnels []string `json:"foreign_tunnels"`
	// DialBoundIface 是虚拟机出站拨号绑定的接口（abroad-vm 最近一次查到的物理网卡）。
	DialBoundIface string `json:"dial_bound_iface,omitempty"`
	// VMNetStats 是 abroad-vm 的网络计数（只有数字和出站模式）。
	VMNetStats map[string]any `json:"vm_netstats,omitempty"`
	// Errors 是哪一部分没取到（脱敏后的原因）。
	Errors []string `json:"errors,omitempty"`
}

// netStatsReader 是能读 abroad-vm 网络计数的虚拟机（ExternalVM 实现）。
type netStatsReader interface {
	NetStats(ctx context.Context) (map[string]any, error)
}

// NetStats 读 host.sock 的 GET /v1/netstats（原样的 JSON 对象；调用方过滤）。
func (v ExternalVM) NetStats(ctx context.Context) (map[string]any, error) {
	var st map[string]any
	if err := getJSON(ctx, v.hostSock(), "http://abroad-vm/v1/netstats", &st); err != nil {
		return nil, ErrVMNotReady
	}
	return st, nil
}

// snapRoutes 是快照要的路由查询（sysnet.Routes 满足）。
type snapRoutes interface {
	Via(ctx context.Context, addr netip.Addr) (string, error)
	Conflicts(ctx context.Context, ours string) ([]string, error)
}

// SnapshotDeps 是取快照的依赖（都可为 nil：缺哪样就在 Errors 里记一笔）。
type SnapshotDeps struct {
	PF       *pf.Controller
	Routes   snapRoutes
	OurUtun  func() string
	FirstHop func() (string, error)
	VM       netStatsReader
	Now      func() time.Time
}

// SnapshotTimeout 是取一次快照的总时限（不能让回滚等太久）。
const SnapshotTimeout = 5 * time.Second

// halfProbes 是两条 /1 各取的探测地址（同冲突检查的 RouteProbes）。
var halfProbes = []struct{ name, addr string }{{"0/1", "1.1.1.1"}, {"128/1", "129.0.0.1"}}

var ifNameRe = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)

// safeIf 只放行像接口名的文字（防止解析出错时把别的内容带进来）。
func safeIf(s string) string {
	if ifNameRe.MatchString(s) {
		return s
	}
	if s == "" {
		return ""
	}
	return "（不认识的接口名）"
}

// TakeEnableSnapshot 取一份快照（只读）。
func TakeEnableSnapshot(ctx context.Context, d SnapshotDeps) EnableSnapshot {
	ctx, cancel := context.WithTimeout(ctx, SnapshotTimeout)
	defer cancel()
	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	s := EnableSnapshot{Time: now().UTC().Truncate(time.Second), ForeignTunnels: []string{}}
	fail := func(part string, err error) {
		s.Errors = append(s.Errors, part+"："+redact.String(err.Error()))
	}
	if d.PF != nil {
		if c, err := d.PF.Counters(ctx); err != nil {
			fail("pf", err)
		} else {
			for i := range c.Rules {
				c.Rules[i].Rule = redact.String(c.Rules[i].Rule)
			}
			s.PF = &c
		}
	}
	if d.OurUtun != nil {
		s.OurUtun = safeIf(d.OurUtun())
	}
	s.FirstHopKind = "none"
	var hop netip.Addr
	if d.FirstHop != nil {
		if h, err := d.FirstHop(); err == nil {
			if a, perr := netip.ParseAddr(strings.Trim(h, "[]")); perr == nil {
				s.FirstHopKind, hop = "ip", a.Unmap()
			} else {
				s.FirstHopKind = "domain" // 不解析：系统 DNS 这时已被接管
			}
		}
	}
	if d.Routes != nil {
		if hop.IsValid() && hop.Is4() {
			if ifn, err := d.Routes.Via(ctx, hop); err != nil {
				fail("去第一跳的路由", err)
			} else {
				s.FirstHopRouteIface = safeIf(ifn)
			}
		}
		s.HalfRouteIfaces = map[string]string{}
		for _, p := range halfProbes {
			if ifn, err := d.Routes.Via(ctx, netip.MustParseAddr(p.addr)); err != nil {
				fail(p.name+" 的路由", err)
			} else {
				s.HalfRouteIfaces[p.name] = safeIf(ifn)
			}
		}
		if t, err := d.Routes.Conflicts(ctx, s.OurUtun); err != nil {
			fail("路由表", err)
		} else {
			for _, x := range t {
				s.ForeignTunnels = append(s.ForeignTunnels, safeIf(x))
			}
		}
	}
	if d.VM != nil {
		if st, err := d.VM.NetStats(ctx); err != nil {
			fail("虚拟机的网络计数", err)
		} else {
			s.VMNetStats = numericStats(st)
			if v, ok := st["interface"].(string); ok {
				s.DialBoundIface = safeIf(v)
			}
		}
	}
	return s
}

// numericStats 只留数字（以及数字组成的一层对象，例如 dial_fail）和出站模式。
func numericStats(in map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range in {
		switch x := v.(type) {
		case float64:
			out[k] = x
		case map[string]any:
			m := map[string]any{}
			for k2, v2 := range x {
				if f, ok := v2.(float64); ok {
					m[k2] = f
				}
			}
			out[k] = m
		case string:
			if k == "outbound_mode" && (x == "first_hop" || x == "open") {
				out[k] = x
			}
		}
	}
	return out
}

// Summary 是写进事件的一行摘要（只有接口名和计数）。
func (s EnableSnapshot) Summary() string {
	var parts []string
	if s.PF == nil {
		parts = append(parts, "pf：没取到")
	} else {
		st := "pf 未启用"
		if s.PF.Enabled {
			st = "pf 已启用"
		}
		if !s.PF.Referenced {
			st += "、主规则集不再引用锚点"
		}
		parts = append(parts, fmt.Sprintf("%s，锚点 %d 条规则", st, len(s.PF.Rules)))
		for _, r := range s.PF.Rules {
			var name string
			switch {
			case strings.Contains(r.Rule, " user ") && strings.Contains(r.Rule, "proto tcp"):
				name = "放行专用用户 tcp"
			case strings.Contains(r.Rule, " user ") && strings.Contains(r.Rule, "proto udp"):
				name = "放行专用用户 udp"
			case strings.HasPrefix(r.Rule, "block drop out quick all"):
				name = "其余全部拦截"
			default:
				continue
			}
			parts = append(parts, fmt.Sprintf("%s：评估 %d、包 %d、状态 %d", name, r.Evaluations, r.Packets, r.States))
		}
	}
	switch s.FirstHopKind {
	case "ip":
		parts = append(parts, "第一跳是 IP，路由走 "+orDash(s.FirstHopRouteIface))
	case "domain":
		parts = append(parts, "第一跳是域名（没有解析）")
	default:
		parts = append(parts, "读不出第一跳")
	}
	if len(s.HalfRouteIfaces) > 0 {
		parts = append(parts, fmt.Sprintf("0/1 走 %s、128/1 走 %s（我们的 utun %s）",
			orDash(s.HalfRouteIfaces["0/1"]), orDash(s.HalfRouteIfaces["128/1"]), orDash(s.OurUtun)))
	}
	if len(s.ForeignTunnels) > 0 {
		parts = append(parts, "别的隧道占着路由："+strings.Join(s.ForeignTunnels, "、"))
	} else {
		parts = append(parts, "没有别的隧道占着路由")
	}
	parts = append(parts, "拨号绑定 "+orDash(s.DialBoundIface))
	if s.VMNetStats != nil {
		parts = append(parts, "虚拟机拨号："+statsLine(s.VMNetStats))
	}
	if len(s.Errors) > 0 {
		parts = append(parts, "没取到："+strings.Join(s.Errors, "；"))
	}
	return "现场快照：" + strings.Join(parts, "；")
}

func orDash(s string) string {
	if s == "" {
		return "（无）"
	}
	return s
}

// statsLine 是拨号相关的几项计数（dial_ok、hop_denied、bind_failed、各类失败）。
func statsLine(m map[string]any) string {
	var parts []string
	for _, k := range []string{"dial_ok", "hop_denied", "policy_denied", "bind_failed", "resolver_dial_ok", "dns_failed"} {
		if v, ok := m[k].(float64); ok {
			parts = append(parts, fmt.Sprintf("%s=%d", k, int64(v)))
		}
	}
	if f, ok := m["dial_fail"].(map[string]any); ok {
		keys := make([]string, 0, len(f))
		for k := range f {
			keys = append(keys, k)
		}
		order := map[string]int{}
		for i, c := range usernet.DialFailClasses {
			order[c] = i
		}
		sort.Slice(keys, func(i, j int) bool { return order[keys[i]] < order[keys[j]] })
		for _, k := range keys {
			if v, ok := f[k].(float64); ok {
				parts = append(parts, fmt.Sprintf("dial_fail_%s=%d", k, int64(v)))
			}
		}
	}
	return strings.Join(parts, " ")
}

// snapCheck 包住启用第 ⑤ 步（以及地区同步后的复查）的出口检查：没通过时先留现场快照，再把错误原样交回
// （状态机随后回滚）。
type snapCheck struct {
	inner state.ExitCheck
	d     *Daemon
}

// enableCheck 包上快照（inner 为 nil 时原样返回 nil）。
func enableCheck(inner state.ExitCheck, d *Daemon) state.ExitCheck {
	if inner == nil {
		return nil
	}
	return snapCheck{inner: inner, d: d}
}

func (c snapCheck) Check(ctx context.Context) error {
	err := c.inner.Check(ctx)
	if err != nil {
		c.d.recordEnableSnapshot(ctx)
	}
	return err
}

// recordEnableSnapshot 取快照、记事件、留给诊断包。
func (d *Daemon) recordEnableSnapshot(ctx context.Context) {
	s := d.opts.Sys
	deps := SnapshotDeps{PF: d.pfctl, OurUtun: d.tun.Name, FirstHop: s.FirstHop}
	if s.Routes != nil {
		deps.Routes = s.Routes
	}
	if r, ok := s.MainVM.(netStatsReader); ok {
		deps.VM = r
	}
	if d.opts.Clock != nil {
		deps.Now = d.opts.Clock.Now
	}
	snap := TakeEnableSnapshot(context.WithoutCancel(ctx), deps)
	d.enableSnap.Store(&snap)
	d.events.Add("enable", "snapshot", snap.Summary())
}

// 确保 sysnet.Routes 满足快照要的路由查询。
var _ snapRoutes = sysnet.Routes(nil)
