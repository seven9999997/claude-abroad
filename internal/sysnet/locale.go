package sysnet

import (
	"context"
	"errors"
	"slices"
)

// Mac 的地区设置（design 9.2，m4-plan M4-5）：时区、自动时区、当前登录用户的语言和地区格式、系统级的语言。
// 真实实现是 region.Mac（经 Runner 执行 systemsetup、defaults、launchctl asuser）；Fake 在内存里。

// OptStr 是“可能不存在”的值（defaults 的键可能不存在：还原时要删除，不能写入一个值）。
type OptStr struct {
	Set bool   `json:"set"`
	V   string `json:"v,omitempty"`
}

// OptList 是“可能不存在”的列表。
type OptList struct {
	Set bool     `json:"set"`
	V   []string `json:"v,omitempty"`
}

// Some、SomeList 构造有值的 OptStr、OptList。
func Some(v string) OptStr           { return OptStr{Set: true, V: v} }
func SomeList(v ...string) OptList   { return OptList{Set: true, V: slices.Clone(v)} }
func (o OptStr) Equal(p OptStr) bool { return o.Set == p.Set && (!o.Set || o.V == p.V) }
func (o OptList) Equal(p OptList) bool {
	return o.Set == p.Set && (!o.Set || slices.Equal(o.V, p.V))
}

// MacLocale 是 Mac 的地区设置。User 是读用户级设置时的登录用户（空表示没人登录，用户级的两项没有意义）。
// AutoTZ 是 /Library/Preferences/com.apple.timezone.auto 的 Active（"1" 或 "0"）。
type MacLocale struct {
	Timezone   string  `json:"timezone"`
	AutoTZ     OptStr  `json:"auto_tz"`
	User       string  `json:"user,omitempty"`
	UserLangs  OptList `json:"user_languages"`
	UserLocale OptStr  `json:"user_locale"`
	SysLangs   OptList `json:"sys_languages"`
}

// Equal 逐项比较。
func (m MacLocale) Equal(o MacLocale) bool {
	return m.Timezone == o.Timezone && m.AutoTZ.Equal(o.AutoTZ) && m.User == o.User && m.UserLangs.Equal(o.UserLangs) &&
		m.UserLocale.Equal(o.UserLocale) && m.SysLangs.Equal(o.SysLangs)
}

// MacRegion 读写 Mac 的地区设置。各个写方法幂等。
type MacRegion interface {
	Read(ctx context.Context) (MacLocale, error)
	SetTimezone(ctx context.Context, tz string) error
	SetAutoTZ(ctx context.Context, v OptStr) error
	// SetUser 改当前登录用户（/dev/console 的属主）的语言和地区格式。user 只用来核对“还是不是这个人”：
	// 没有人登录时返回 ErrNoConsoleUser，登录的换了人时返回 ErrConsoleChanged，都不执行命令。
	SetUser(ctx context.Context, user string, langs OptList, locale OptStr) error
	SetSystem(ctx context.Context, langs OptList) error
}

// 错误。
var (
	ErrNoConsoleUser  = errors.New("没有人登录")
	ErrConsoleChanged = errors.New("登录的用户换了")
)

// 地区设置所在的 plist（不是可执行的程序，所以不在 exec.go 的白名单里）。
const (
	AutoTZDomain = "/Library/Preferences/com.apple.timezone.auto"
	GlobalDomain = "/Library/Preferences/.GlobalPreferences"
)

// 地区的操作名（Fake 的 FailNext 和 OnChange 用）。
const (
	OpLocaleRead = "locale.read"
	OpLocaleTZ   = "locale.tz"
	OpLocaleAuto = "locale.autotz"
	OpLocaleUser = "locale.user"
	OpLocaleSys  = "locale.sys"
)

// Locale 返回 Fake 的地区视图。
func (f *Fake) Locale() MacRegion { return fakeLocale{f} }

// SetConsoleUser 模拟登录、注销（不算我们的改动，不回调）。
func (f *Fake) SetConsoleUser(u string) { f.mu.Lock(); f.st.Console = u; f.mu.Unlock() }

// SetLocaleForTest 模拟用户自己改了设置（不算我们的改动，不回调）。
func (f *Fake) SetLocaleForTest(l MacLocale) { f.mu.Lock(); f.st.Locale = l.clone(); f.mu.Unlock() }

func (m MacLocale) clone() MacLocale {
	m.UserLangs.V = slices.Clone(m.UserLangs.V)
	m.SysLangs.V = slices.Clone(m.SysLangs.V)
	return m
}

type fakeLocale struct{ f *Fake }

func (l fakeLocale) Read(context.Context) (MacLocale, error) {
	var out MacLocale
	err := l.f.do(OpLocaleRead, "", false, func() error {
		out = l.f.st.Locale.clone()
		out.User = l.f.st.Console
		if out.User == "" {
			out.UserLangs, out.UserLocale = OptList{}, OptStr{}
		}
		return nil
	})
	return out, err
}

func (l fakeLocale) SetTimezone(_ context.Context, tz string) error {
	return l.f.do(OpLocaleTZ, tz, true, func() error {
		if tz == "" {
			return errors.New("时区为空")
		}
		l.f.st.Locale.Timezone = tz
		return nil
	})
}

func (l fakeLocale) SetAutoTZ(_ context.Context, v OptStr) error {
	return l.f.do(OpLocaleAuto, v.V, true, func() error { l.f.st.Locale.AutoTZ = v; return nil })
}

func (l fakeLocale) SetUser(_ context.Context, user string, langs OptList, locale OptStr) error {
	l.f.mu.Lock()
	cur := l.f.st.Console
	l.f.mu.Unlock()
	switch {
	case cur == "":
		return ErrNoConsoleUser
	case cur != user:
		return ErrConsoleChanged
	}
	return l.f.do(OpLocaleUser, user, true, func() error {
		l.f.st.Locale.UserLangs = OptList{Set: langs.Set, V: slices.Clone(langs.V)}
		l.f.st.Locale.UserLocale = locale
		return nil
	})
}

func (l fakeLocale) SetSystem(_ context.Context, langs OptList) error {
	return l.f.do(OpLocaleSys, "", true, func() error {
		l.f.st.Locale.SysLangs = OptList{Set: langs.Set, V: slices.Clone(langs.V)}
		return nil
	})
}
