package exitpolicy

// 查询源的解析（m4-plan M4-2 v2.1 决定 1）：取出口 IP 的轻量源（Cloudflare trace、ipwho.is、IP.SB 不带 IP 参数时
// 回报调用方自己的地址和国家），以及按 IP 查位置的源（ipwho.is、IP.SB）。这里只做解析，HTTP 请求在 internal/exitcheck，
// 经指定出口发出。解析失败一律返回 ErrFormat（调用方按“无法判断”处理），字段缺失只影响依赖它的检查项。

import (
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"time"
	_ "time/tzdata"
)

// BrowserUA 是查询时用的通用浏览器标识（不带任何项目名，m4-plan v2.1 决定 5）。
const BrowserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36"

// ErrFormat 表示应答不是预期的格式（网页、字段缺失、IP 不合法）。
var ErrFormat = errors.New("查询源的应答格式不对")

// IPInfo 是取出口 IP 的结果：IP 必有；Country 可能为空（源没给）。
type IPInfo struct {
	IP      netip.Addr
	Country string // 两个大写字母
}

// Geo 是位置查询的结果。缺的字段为空。
type Geo struct {
	IP                       netip.Addr
	Country, CountryName     string
	Region, RegionCode, City string
	Timezone                 string // IANA 名，已用 tzdata 校验；不合法时为空
	ASN                      int
	Org                      string
}

func validCountry(c string) string {
	c = strings.ToUpper(strings.TrimSpace(c))
	if len(c) != 2 || c[0] < 'A' || c[0] > 'Z' || c[1] < 'A' || c[1] > 'Z' || c == "ZZ" || c == "XX" {
		return ""
	}
	return c
}

func validIP(s string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil || !a.Is4() || !a.IsGlobalUnicast() || a.IsPrivate() {
		return netip.Addr{}, false
	}
	return a, true
}

func validTZ(tz string) string {
	tz = strings.TrimSpace(tz)
	if tz == "" || tz == "Local" {
		return ""
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return ""
	}
	return tz
}

// ParseCloudflareTrace 解析 /cdn-cgi/trace 的 key=value 文本：ip、loc。
func ParseCloudflareTrace(b []byte) (IPInfo, error) {
	var info IPInfo
	ok := false
	for _, l := range strings.Split(string(b), "\n") {
		k, v, found := strings.Cut(strings.TrimSpace(l), "=")
		if !found {
			continue
		}
		switch k {
		case "ip":
			info.IP, ok = validIP(v)
		case "loc":
			info.Country = validCountry(v)
		}
	}
	if !ok {
		return IPInfo{}, ErrFormat
	}
	return info, nil
}

type ipwhoBody struct {
	Success     *bool  `json:"success"`
	IP          string `json:"ip"`
	Country     string `json:"country"`
	CountryCode string `json:"country_code"`
	Region      string `json:"region"`
	RegionCode  string `json:"region_code"`
	City        string `json:"city"`
	Timezone    *struct {
		ID string `json:"id"`
	} `json:"timezone"`
	Connection *struct {
		ASN int    `json:"asn"`
		Org string `json:"org"`
		ISP string `json:"isp"`
	} `json:"connection"`
}

// ParseIPWhoIs 解析 ipwho.is 的应答。want 有效时 IP 必须等于它（按 IP 查位置）；无效时是“查自己”。
func ParseIPWhoIs(b []byte, want netip.Addr) (Geo, error) {
	var v ipwhoBody
	if json.Unmarshal(b, &v) != nil || v.Success == nil || !*v.Success {
		return Geo{}, ErrFormat
	}
	a, ok := validIP(v.IP)
	if !ok || want.IsValid() && a != want {
		return Geo{}, ErrFormat
	}
	g := Geo{IP: a, Country: validCountry(v.CountryCode), CountryName: v.Country, Region: v.Region, RegionCode: v.RegionCode, City: v.City}
	if v.Timezone != nil {
		g.Timezone = validTZ(v.Timezone.ID)
	}
	if v.Connection != nil {
		g.ASN, g.Org = v.Connection.ASN, v.Connection.Org
		if g.Org == "" {
			g.Org = v.Connection.ISP
		}
	}
	return g, nil
}

type ipsbBody struct {
	IP              string `json:"ip"`
	Country         string `json:"country"`
	CountryCode     string `json:"country_code"`
	Region          string `json:"region"`
	RegionCode      string `json:"region_code"`
	City            string `json:"city"`
	Timezone        string `json:"timezone"`
	ASN             int    `json:"asn"`
	ASNOrganization string `json:"asn_organization"`
	Organization    string `json:"organization"`
}

// ParseIPSB 解析 api.ip.sb/geoip 的应答（规则同 ParseIPWhoIs）。
func ParseIPSB(b []byte, want netip.Addr) (Geo, error) {
	var v ipsbBody
	if json.Unmarshal(b, &v) != nil {
		return Geo{}, ErrFormat
	}
	a, ok := validIP(v.IP)
	if !ok || want.IsValid() && a != want {
		return Geo{}, ErrFormat
	}
	g := Geo{IP: a, Country: validCountry(v.CountryCode), CountryName: v.Country, Region: v.Region, RegionCode: v.RegionCode,
		City: v.City, Timezone: validTZ(v.Timezone), ASN: v.ASN, Org: v.ASNOrganization}
	if g.Org == "" {
		g.Org = v.Organization
	}
	return g, nil
}

// ---- 出口变化规则（design 8 的四条）----

// Rule 是出口变化规则。
type Rule string

const (
	RuleFixedIP     Rule = "fixed_ip"     // 出口 IP 必须和基准完全相同
	RuleSameCity    Rule = "same_city"    // 国家、州省、城市要相同
	RuleSameCountry Rule = "same_country" // 国家要相同（默认）
	RuleAny         Rule = "any"          // 只要求不在大陆
)

// Valid 报告规则名是否认识。
func (r Rule) Valid() bool {
	switch r {
	case RuleFixedIP, RuleSameCity, RuleSameCountry, RuleAny:
		return true
	}
	return false
}

// NeedsGeo 报告这条规则的比对是否需要位置查询（城市、州省）。同一国家和不限只用国家代码和本地大陆段。
func (r Rule) NeedsGeo() bool { return r == RuleSameCity }

// PolicyFor 把规则和基准映射到 Policy（沿用 Cage 的 static/dynamic + scope 模型）。
func PolicyFor(r Rule, base Geo) Policy {
	switch r {
	case RuleFixedIP:
		return Policy{Mode: "static", IP: base.IP.String()}.Normalized()
	case RuleSameCity:
		return Policy{Mode: "dynamic", Scope: "city", CountryCode: base.Country, Region: base.Region, City: base.City}.Normalized()
	case RuleAny:
		return Policy{Mode: "dynamic", Scope: "any"}.Normalized()
	}
	return Policy{Mode: "dynamic", Scope: "country", CountryCode: base.Country}.Normalized()
}
