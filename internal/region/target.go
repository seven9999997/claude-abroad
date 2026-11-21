// Package region 是地区（design 9，m4-plan M4-5 v2.4）：目标地区的确定与校验、国家到语言的映射、
// Mac 一侧的命令拼装与解析（经 sysnet.Runner，测试用命令模拟器）。改 Mac 设置的命令只在真实模式下执行。
package region

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // 内置时区数据库：校验时区名不依赖本机的 zoneinfo

	"claude-abroad/internal/sysnet"
)

// 目标的来源。
const (
	SourceExit   = "exit"   // 跟随出口（默认）
	SourceManual = "manual" // 手动指定
)

// Target 是锁定的目标地区。Lang 是首选语言（AppleLanguages 的第一项，如 en-US）；Locale 是地区格式（AppleLocale，如 en_US）。
type Target struct {
	Timezone string `json:"timezone"`
	Lang     string `json:"lang"`
	Locale   string `json:"locale"`
	Country  string `json:"country,omitempty"`
	Source   string `json:"source"`
}

// 错误。
var (
	ErrTimezone = errors.New("时区不对")
	ErrLang     = errors.New("语言不对")
	ErrCountry  = errors.New("国家不对")
)

var (
	tzRe     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+\-]*(/[A-Za-z0-9][A-Za-z0-9_+\-]*){0,2}$`)
	langRe   = regexp.MustCompile(`^[a-z]{2,3}(-[A-Z][a-z]{3})?(-([A-Z]{2}|[0-9]{3}))?$`)
	localeRe = regexp.MustCompile(`^[a-z]{2,3}(-[A-Z][a-z]{3})?(_([A-Z]{2}|[0-9]{3}))?$`)
	// rawRe 是从 Mac 读回来的原值（还原时原样写回）：只允许不会被命令行误解的字符。
	rawRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_@=;.+\-]{0,63}$`)
)

// ValidTimezone 报告 tz 是不是内置时区数据库里存在的名字（不接受 Local、空串、路径）。
func ValidTimezone(tz string) error {
	if len(tz) > 64 || !tzRe.MatchString(tz) || tz == "Local" || strings.Contains(tz, "..") {
		return fmt.Errorf("%w：%q", ErrTimezone, tz)
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return fmt.Errorf("%w：%q 不在时区数据库里", ErrTimezone, tz)
	}
	return nil
}

// ValidLang 报告 l 是不是语言标签（语言[-文字][-地区]，如 en-US、zh-Hant-TW）。
func ValidLang(l string) error {
	if !langRe.MatchString(l) {
		return fmt.Errorf("%w：%q", ErrLang, l)
	}
	return nil
}

// ValidLocale 报告 l 是不是地区格式（语言[-文字][_地区]，如 en_US、zh-Hans_CN）。
func ValidLocale(l string) error {
	if !localeRe.MatchString(l) {
		return fmt.Errorf("%w：地区格式 %q", ErrLang, l)
	}
	return nil
}

// ValidRaw 校验从 Mac 读回的原值（语言列表的项、地区格式）：还原时原样写回命令行。
func ValidRaw(v string) error {
	if !rawRe.MatchString(v) {
		return fmt.Errorf("读回的值含不认识的字符：%q", v)
	}
	return nil
}

// LocaleOf 由语言标签得出地区格式：最后一段地区用下划线连接（en-US → en_US，zh-Hant-TW → zh-Hant_TW）。
func LocaleOf(lang string) string {
	i := strings.LastIndex(lang, "-")
	if i < 0 {
		return lang
	}
	last := lang[i+1:]
	if len(last) == 2 || len(last) == 3 && last[0] >= '0' && last[0] <= '9' {
		return lang[:i] + "_" + last
	}
	return lang
}

// countryLang 是国家 → 首选语言（design 9.1）。没有列出的国家用 en-US。
var countryLang = map[string]string{
	"US": "en-US", "CA": "en-CA", "GB": "en-GB", "IE": "en-IE", "AU": "en-AU", "NZ": "en-NZ", "SG": "en-SG",
	"IN": "en-IN", "ZA": "en-ZA", "PH": "en-PH", "MY": "en-MY",
	"JP": "ja-JP", "KR": "ko-KR", "TW": "zh-Hant-TW", "HK": "zh-Hant-HK", "MO": "zh-Hant-MO",
	"DE": "de-DE", "AT": "de-AT", "CH": "de-CH", "FR": "fr-FR", "BE": "fr-BE", "LU": "fr-LU", "NL": "nl-NL",
	"ES": "es-ES", "MX": "es-MX", "AR": "es-AR", "CL": "es-CL", "CO": "es-CO", "PE": "es-PE",
	"PT": "pt-PT", "BR": "pt-BR", "IT": "it-IT", "SE": "sv-SE", "NO": "nb-NO", "DK": "da-DK", "FI": "fi-FI",
	"PL": "pl-PL", "CZ": "cs-CZ", "HU": "hu-HU", "RO": "ro-RO", "GR": "el-GR", "TR": "tr-TR", "IL": "he-IL",
	"AE": "ar-AE", "SA": "ar-SA", "TH": "th-TH", "VN": "vi-VN", "ID": "id-ID", "UA": "uk-UA",
}

var countryRe = regexp.MustCompile(`^[A-Z]{2}$`)

// LangFor 返回国家的首选语言。大陆（CN）不能作为目标。
func LangFor(country string) (string, error) {
	if !countryRe.MatchString(country) {
		return "", fmt.Errorf("%w：%q", ErrCountry, country)
	}
	if country == "CN" {
		return "", fmt.Errorf("%w：出口在中国大陆，不能作为目标地区", ErrCountry)
	}
	if l, ok := countryLang[country]; ok {
		return l, nil
	}
	return "en-US", nil
}

// FromExit 按出口的国家和时区（查询源给的 IANA 名）确定目标，来源是“跟随出口”。
func FromExit(country, tz string) (Target, error) {
	lang, err := LangFor(country)
	if err != nil {
		return Target{}, err
	}
	if err := ValidTimezone(tz); err != nil {
		return Target{}, err
	}
	return Target{Timezone: tz, Lang: lang, Locale: LocaleOf(lang), Country: country, Source: SourceExit}, nil
}

// Manual 是手动指定的目标。
func Manual(tz, lang string) (Target, error) {
	if err := ValidTimezone(tz); err != nil {
		return Target{}, err
	}
	if err := ValidLang(lang); err != nil {
		return Target{}, err
	}
	return Target{Timezone: tz, Lang: lang, Locale: LocaleOf(lang), Source: SourceManual}, nil
}

// Validate 校验一个目标（读回来的、要下发的都过一遍）。
func (t Target) Validate() error {
	if t.Source != SourceExit && t.Source != SourceManual {
		return fmt.Errorf("来源不对：%q", t.Source)
	}
	if err := ValidTimezone(t.Timezone); err != nil {
		return err
	}
	if err := ValidLang(t.Lang); err != nil {
		return err
	}
	if err := ValidLocale(t.Locale); err != nil {
		return err
	}
	if t.Country != "" && !countryRe.MatchString(t.Country) {
		return fmt.Errorf("%w：%q", ErrCountry, t.Country)
	}
	return nil
}

// Same 报告两个目标在“地区”上是否相同（时区、语言、地区格式；不看来源和国家）。
func (t Target) Same(o Target) bool {
	return t.Timezone == o.Timezone && t.Lang == o.Lang && t.Locale == o.Locale
}

// String 给确认框和 status 用（地区不是敏感信息）。
func (t Target) String() string {
	return fmt.Sprintf("时区 %s、语言 %s、地区格式 %s", t.Timezone, t.Lang, t.Locale)
}

// MergeLangs 把目标语言放到第一位，原有的其余语言按原顺序保留，不重复（design 9.2）。
func MergeLangs(target string, orig []string) []string {
	out := []string{target}
	for _, l := range orig {
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

// Desired 是同步到 target 之后 Mac 应有的设置：以 base（启用前记下的原值）为底，
// 时区换成目标、关自动时区、语言把目标放第一位、地区格式换成目标。没人登录时用户级的两项不动。
func Desired(base sysnet.MacLocale, t Target) sysnet.MacLocale {
	d := base
	d.Timezone = t.Timezone
	d.AutoTZ = sysnet.Some("0")
	if base.User != "" {
		d.UserLangs = sysnet.SomeList(MergeLangs(t.Lang, base.UserLangs.V)...)
		d.UserLocale = sysnet.Some(t.Locale)
	}
	d.SysLangs = sysnet.SomeList(MergeLangs(t.Lang, base.SysLangs.V)...)
	return d
}

// Changes 报告把 cur 同步到 t 会不会改变地区（m4-plan v2.4 决定 1：时区、首选语言、地区格式任何一项不同）。
// 没人登录时只看时区。
func Changes(cur sysnet.MacLocale, t Target) bool {
	if cur.Timezone != t.Timezone {
		return true
	}
	if cur.User == "" {
		return false
	}
	first := ""
	if len(cur.UserLangs.V) > 0 {
		first = cur.UserLangs.V[0]
	}
	return first != t.Lang || cur.UserLocale.V != t.Locale
}

// Describe 是“此刻的地区”（确认框里的“从 A”）。
func Describe(cur sysnet.MacLocale) string {
	lang, loc := "（未设置）", "（未设置）"
	if len(cur.UserLangs.V) > 0 {
		lang = cur.UserLangs.V[0]
	}
	if cur.UserLocale.Set {
		loc = cur.UserLocale.V
	}
	if cur.User == "" {
		lang, loc = "（没有人登录）", "（没有人登录）"
	}
	return fmt.Sprintf("时区 %s、语言 %s、地区格式 %s", cur.Timezone, lang, loc)
}
