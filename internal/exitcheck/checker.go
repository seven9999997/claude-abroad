package exitcheck

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"claude-abroad/internal/exitpolicy"
	"claude-abroad/internal/stack"
)

// 时限（m4-plan v2.1 决定 2）：每项探测独立计时，一轮总共不超过 TotalTimeout。超时一律“无法判断”。
const (
	TotalTimeout  = 20 * time.Second
	HTTPSTimeout  = 8 * time.Second
	IPTimeout     = 5 * time.Second // 每个取 IP 的源
	GeoTimeout    = 6 * time.Second // 每个位置源
	UDPTimeout    = 3 * time.Second
	DoHTimeout    = 6 * time.Second
	GeoCacheTTL   = 24 * time.Hour
	DefaultHTTPS  = "https://www.gstatic.com/generate_204"
	DefaultDoH    = "https://cloudflare-dns.com/dns-query"
	DefaultProbeN = "example.com"
)

// Item 是一轮检查里的一项。Detail 不含地址和位置。
type Item struct {
	Name   string `json:"name"` // https、ip、mainland、rule、geo、udp、doh
	Result Result `json:"result"`
	Cause  Cause  `json:"cause"`
	Detail string `json:"detail,omitempty"`
}

// Report 是一轮检查的结果。IP、Geo 是敏感字段：只在内存里；String、JSON、fmt 的输出都只有摘要。
type Report struct {
	Exit     string
	Verdict  Result // https、ip、mainland、rule、doh 的合成；UDP 只记录，不参与（主代理的 UDP 不通过只记录）
	Items    []Item
	At       time.Time
	IP       netip.Addr      // 敏感
	Country  string          // 国家代码
	Geo      *exitpolicy.Geo // 敏感；这一轮查过或缓存命中时才有
	Timezone string          // 来自位置（缓存）；没有时为空
	Lookups  int             // 这一轮实际发出的位置查询次数（测试和诊断用）
}

// Summary 是可以进日志、status 的部分：国家、时区和各项的结果，不含 IP、城市、运营商。
type Summary struct {
	Exit     string `json:"exit"`
	Verdict  Result `json:"verdict"`
	Country  string `json:"country,omitempty"`
	Timezone string `json:"timezone,omitempty"`
	Items    []Item `json:"items"`
}

// Summary 返回摘要。
func (r Report) Summary() Summary {
	return Summary{Exit: r.Exit, Verdict: r.Verdict, Country: r.Country, Timezone: r.Timezone, Items: r.Items}
}

// String 只输出摘要（防止 %v 把 IP 打进日志）。
func (r Report) String() string {
	b, _ := json.Marshal(r.Summary())
	return string(b)
}

// Format 让 fmt 的所有动词（%v、%+v、%#v、%s）都只输出摘要。
func (r Report) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(r.String())) }

// MarshalJSON 只输出摘要。
func (r Report) MarshalJSON() ([]byte, error) { return json.Marshal(r.Summary()) }

// Item 按名字取一项。
func (r Report) Item(name string) (Item, bool) {
	for _, it := range r.Items {
		if it.Name == name {
			return it, true
		}
	}
	return Item{}, false
}

// Checker 做出口检查。零值不可用：至少要有 Opener。
type Checker struct {
	Opener     stack.Opener // 按出口开流（不经闸门）
	IPSources  []IPSource   // 默认 DefaultIPSources
	GeoSources []GeoSource  // 默认 DefaultGeoSources
	HTTPSURL   string
	DoHURL     string
	ProbeName  string
	UDPTarget  netip.AddrPort
	RootCAs    *x509.CertPool // 测试用；nil 用系统根证书
	Now        func() time.Time
	Logf       func(format string, a ...any) // 只收到摘要
	// OnReport 在每一轮检查结束后收到完整的报告（补齐二：abroadd 记下主代理的 UDP 结果）；探测本身不变。可为 nil。
	// 要在开始检查之前设好（检查过程中不改）。
	OnReport func(Report)

	// 时限（测试可调小）；0 用默认值。
	Total, HTTPS, IP, Geo, UDP, DoH time.Duration

	mu    sync.Mutex
	cache map[netip.Addr]geoEntry
}

type geoEntry struct {
	geo exitpolicy.Geo
	at  time.Time
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func dur(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

func (c *Checker) probeName() string {
	if c.ProbeName != "" {
		return c.ProbeName
	}
	return DefaultProbeN
}

func (c *Checker) dohURL() string {
	if c.DoHURL != "" {
		return c.DoHURL
	}
	return DefaultDoH
}

func (c *Checker) tlsConf() *tls.Config {
	return &tls.Config{RootCAs: c.RootCAs, MinVersion: tls.VersionTLS12}
}

// cachedGeo 取缓存里这个 IP 的位置（24 小时内）。
func (c *Checker) cachedGeo(ip netip.Addr) (exitpolicy.Geo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[ip]
	if !ok || c.now().Sub(e.at) >= GeoCacheTTL {
		return exitpolicy.Geo{}, false
	}
	return e.geo, true
}

func (c *Checker) putGeo(g exitpolicy.Geo, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		c.cache = map[netip.Addr]geoEntry{}
	}
	c.cache[g.IP] = geoEntry{geo: g, at: at}
}

// Options 是一轮检查的参数。
type Options struct {
	Base    *Baseline // nil：没有基准（规则一项是“无法判断”，要重新应用主代理）
	WantGeo bool      // 应用主代理（建立基准、锁定地区）时要位置，不管规则
}

// Check 对出口 exit 做一轮检查。总时限 Total（默认 20 秒）；ctx 取消时及时返回，返回前所有 goroutine 都已结束。
func (c *Checker) Check(ctx context.Context, exit string, o Options) Report {
	ctx, cancel := context.WithTimeout(ctx, dur(c.Total, TotalTimeout))
	defer cancel()
	rep := Report{Exit: exit, At: c.now()}
	if o.Base != nil {
		// 基准里的位置也是缓存（m4-plan v2.1 决定 1：缓存只在内存和基准文件里）。
		if g := o.Base.geo(); g.IP.IsValid() {
			if _, ok := c.cachedGeo(g.IP); !ok && c.now().Sub(o.Base.At) < GeoCacheTTL {
				c.putGeo(g, o.Base.At)
			}
		}
	}
	tc := c.tlsConf()
	var wg sync.WaitGroup
	var https, udp, doh error
	wg.Add(3)
	go func() {
		defer wg.Done()
		pctx, pc := context.WithTimeout(ctx, dur(c.HTTPS, HTTPSTimeout))
		defer pc()
		u := c.HTTPSURL
		if u == "" {
			u = DefaultHTTPS
		}
		https = probeHTTPS(pctx, httpClient(ctx, c.Opener, exit, tc, dur(c.HTTPS, HTTPSTimeout)), u)
	}()
	go func() {
		defer wg.Done()
		pctx, pc := context.WithTimeout(ctx, dur(c.UDP, UDPTimeout))
		defer pc()
		udp = c.probeUDP(pctx, exit)
	}()
	go func() {
		defer wg.Done()
		pctx, pc := context.WithTimeout(ctx, dur(c.DoH, DoHTimeout))
		defer pc()
		doh = c.probeDoH(pctx, httpClient(ctx, c.Opener, exit, tc, dur(c.DoH, DoHTimeout)))
	}()
	ipItems := c.ipAndRule(ctx, exit, o, tc, &rep)
	wg.Wait()

	add := func(name string, err error, okDetail string) {
		cause := causeOf(err)
		it := Item{Name: name, Result: ResultOf(cause), Cause: cause, Detail: okDetail}
		if err != nil {
			it.Detail = firstLine(err)
		}
		rep.Items = append(rep.Items, it)
	}
	add("https", https, "收到预期的 204")
	rep.Items = append(rep.Items, ipItems...)
	add("doh", doh, "DoH 解析正常")
	add("udp", udp, "收到合法的 DNS 应答")
	rep.Verdict = Pass
	for _, it := range rep.Items {
		if it.Name == "udp" || it.Name == "geo" {
			continue // UDP 只记录；位置查询失败只影响依赖它的检查项（规则一项）
		}
		switch it.Result {
		case Fail:
			rep.Verdict = Fail
		case Unknown:
			if rep.Verdict != Fail {
				rep.Verdict = Unknown
			}
		}
	}
	if c.Logf != nil {
		c.Logf("出口检查：%s", rep.String())
	}
	if c.OnReport != nil {
		c.OnReport(rep)
	}
	return rep
}

// UDP 是否可用（补齐二，给状态和界面用；探测和判定不变）。
const (
	UDPAvailable   = "available"
	UDPUnavailable = "unavailable"
	UDPUnknown     = "unknown"
)

// UDPAvailability 从一轮报告得出出口的 UDP 是否可用：探测通过（收到合法的 DNS 应答）→ 可用；明确不通过（收到的不是
// 预期的应答）→ 不可用；探测超时、而同一出口的 HTTPS 通过（出口本身是通的，只是 UDP 没有应答——回应帧显示成功但没有
// 应答也落在这里，设计 4.2 的备注）→ 不可用；其余（没检查、出口本身不通、连不上）→ 还不知道。
func (r Report) UDPAvailability() string {
	it, ok := r.Item("udp")
	if !ok {
		return UDPUnknown
	}
	switch it.Result {
	case Pass:
		return UDPAvailable
	case Fail:
		return UDPUnavailable
	}
	if it.Cause == CauseTimeout {
		if h, ok := r.Item("https"); ok && h.Result == Pass {
			return UDPUnavailable
		}
	}
	return UDPUnknown
}

func firstLine(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

// ipAndRule 取出口 IP（换源）、本地判断大陆段、需要时查位置（缓存）、按规则比对。返回 ip、mainland、geo、rule 四项。
func (c *Checker) ipAndRule(ctx context.Context, exit string, o Options, tc *tls.Config, rep *Report) []Item {
	srcs := c.IPSources
	if srcs == nil {
		srcs = DefaultIPSources()
	}
	var info exitpolicy.IPInfo
	var lastErr error = fail(CauseBadFormat, "没有取出口 IP 的源")
	got := false
	for _, s := range srcs {
		if ctx.Err() != nil {
			lastErr = context.DeadlineExceeded
			break
		}
		pctx, pc := context.WithTimeout(ctx, dur(c.IP, IPTimeout))
		b, err := fetch(pctx, httpClient(ctx, c.Opener, exit, tc, dur(c.IP, IPTimeout)), s.URL())
		if err == nil {
			info, err = s.Parse(b)
			if err != nil {
				err = fail(CauseBadFormat, s.Name()+"：应答格式不对")
			}
		}
		pc()
		if err == nil {
			got = true
			break
		}
		lastErr = err
	}
	unknownRest := func(cause Cause, why string) []Item {
		r := ResultOf(cause)
		return []Item{
			{Name: "ip", Result: r, Cause: cause, Detail: why},
			{Name: "mainland", Result: Unknown, Cause: cause, Detail: "没有出口 IP，判断不了"},
			{Name: "rule", Result: Unknown, Cause: cause, Detail: "没有出口 IP，判断不了"},
		}
	}
	if !got {
		return unknownRest(causeOf(lastErr), "所有查询源都失败："+firstLine(lastErr))
	}
	rep.IP, rep.Country = info.IP, info.Country
	items := []Item{{Name: "ip", Result: Pass, Cause: CauseOK, Detail: "取得出口 IP"}}
	// 大陆段：每次本地判断，不依赖任何查询。
	in, lerr := exitpolicy.InMainland(info.IP)
	switch {
	case lerr != nil:
		items = append(items, Item{Name: "mainland", Result: Unknown, Cause: CauseBadFormat, Detail: "内置的大陆网段不可用"})
	case in || info.Country == "CN":
		items = append(items, Item{Name: "mainland", Result: Fail, Cause: CauseMainland, Detail: "出口在中国大陆"})
	default:
		items = append(items, Item{Name: "mainland", Result: Pass, Cause: CauseOK, Detail: "出口不在大陆网段"})
	}
	// 位置：规则需要（同一城市）、国家缺失、或调用方要位置时才用；先看缓存，IP 变了才查。
	var rule exitpolicy.Rule
	if o.Base != nil {
		rule = o.Base.Rule
	}
	needGeo := o.WantGeo || rule.NeedsGeo() || info.Country == ""
	var geo *exitpolicy.Geo
	if needGeo {
		if g, ok := c.cachedGeo(info.IP); ok {
			geo = &g
			items = append(items, Item{Name: "geo", Result: Pass, Cause: CauseOK, Detail: "位置来自缓存"})
		} else {
			g, err := c.lookupGeo(ctx, exit, tc, info.IP, rep)
			if err != nil {
				cause := causeOf(err)
				items = append(items, Item{Name: "geo", Result: ResultOf(cause), Cause: cause, Detail: firstLine(err)})
			} else {
				geo = &g
				c.putGeo(g, c.now())
				items = append(items, Item{Name: "geo", Result: Pass, Cause: CauseOK, Detail: "查到位置"})
			}
		}
	} else {
		items = append(items, Item{Name: "geo", Result: Pass, Cause: CauseNotNeeded, Detail: "这条规则不需要位置查询"})
		if g, ok := c.cachedGeo(info.IP); ok {
			geo = &g
		}
	}
	if geo != nil {
		rep.Geo, rep.Timezone = geo, geo.Timezone
		if rep.Country == "" {
			rep.Country = geo.Country
		}
		if geo.Country == "CN" && items[1].Result != Fail {
			items[1] = Item{Name: "mainland", Result: Fail, Cause: CauseMainland, Detail: "位置查询显示出口在中国大陆"}
		}
	}
	items = append(items, c.compare(o.Base, info, geo, items))
	return items
}

// Locate 查一个 IP 的位置（经出口 exit；按 IP 缓存 24 小时）。环境检测第 08 项用它看 STUN 回报地址的国家（M4-6b）。
func (c *Checker) Locate(ctx context.Context, exit string, ip netip.Addr) (exitpolicy.Geo, error) {
	if g, ok := c.cachedGeo(ip); ok {
		return g, nil
	}
	var rep Report
	g, err := c.lookupGeo(ctx, exit, c.tlsConf(), ip, &rep)
	if err == nil {
		c.putGeo(g, c.now())
	}
	return g, err
}

func (c *Checker) lookupGeo(ctx context.Context, exit string, tc *tls.Config, ip netip.Addr, rep *Report) (exitpolicy.Geo, error) {
	srcs := c.GeoSources
	if srcs == nil {
		srcs = DefaultGeoSources()
	}
	var lastErr error = fail(CauseBadFormat, "没有位置查询源")
	for _, s := range srcs {
		if ctx.Err() != nil {
			return exitpolicy.Geo{}, context.DeadlineExceeded
		}
		rep.Lookups++
		pctx, pc := context.WithTimeout(ctx, dur(c.Geo, GeoTimeout))
		b, err := fetch(pctx, httpClient(ctx, c.Opener, exit, tc, dur(c.Geo, GeoTimeout)), s.URL(ip))
		pc()
		if err == nil {
			g, perr := s.Parse(b, ip)
			if perr == nil {
				return g, nil
			}
			err = fail(CauseBadFormat, s.Name()+"：应答格式不对")
		}
		lastErr = err
	}
	return exitpolicy.Geo{}, lastErr
}

// compare 按出口变化规则与基准比对（大陆段已经单独判过）。
func (c *Checker) compare(base *Baseline, info exitpolicy.IPInfo, geo *exitpolicy.Geo, items []Item) Item {
	if base == nil {
		return Item{Name: "rule", Result: Unknown, Cause: CauseNoBaseline, Detail: "没有出口基准，请重新应用主代理"}
	}
	if items[1].Result == Fail {
		return Item{Name: "rule", Result: Fail, Cause: CauseMainland, Detail: "出口在中国大陆"}
	}
	loc := exitpolicy.Location{IP: info.IP.String(), CountryCode: info.Country}
	if geo != nil {
		if loc.CountryCode == "" {
			loc.CountryCode = geo.Country
		}
		loc.Region, loc.City = geo.Region, geo.City
	}
	if base.Rule.NeedsGeo() && geo == nil {
		return Item{Name: "rule", Result: Unknown, Cause: CauseBadFormat, Detail: "这条规则要比对城市，但位置查询没有结果"}
	}
	st, _ := exitpolicy.PolicyFor(base.Rule, base.geo()).Evaluate(info.IP.String(), &loc)
	switch st {
	case exitpolicy.Passed:
		return Item{Name: "rule", Result: Pass, Cause: CauseOK, Detail: "符合出口变化规则"}
	case exitpolicy.Failed:
		return Item{Name: "rule", Result: Fail, Cause: CauseRuleViolation, Detail: "违反出口变化规则（" + string(base.Rule) + "）"}
	}
	return Item{Name: "rule", Result: Unknown, Cause: CauseBadFormat, Detail: "位置信息不全，比对不了"}
}

// SplitVerdict 是分流出口的判定（M4-8，m4-plan v2.12 决定 4）：只看 https、ip、mainland、doh 四项；
// 不做基准比对（rule）、不看位置查询（geo），UDP 单独由 UDPResult 决定“只回退 UDP”。主代理的判定（Verdict）不变。
// 四项一项都没有时是无法判断（不能当作通过）。
func (r Report) SplitVerdict() Result {
	v, seen := Pass, 0
	for _, it := range r.Items {
		switch it.Name {
		case "https", "ip", "mainland", "doh":
		default:
			continue
		}
		seen++
		switch it.Result {
		case Fail:
			v = Fail
		case Unknown:
			if v != Fail {
				v = Unknown
			}
		}
	}
	if seen == 0 {
		return Unknown
	}
	return v
}

// UDPResult 是 UDP 探测的结果；没有这一项时是无法判断。
func (r Report) UDPResult() Result {
	if it, ok := r.Item("udp"); ok {
		return it.Result
	}
	return Unknown
}
