package region

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"claude-abroad/internal/srccheck"
	"claude-abroad/internal/sysnet"
)

var bg = context.Background()

// a. 国家到语言的映射，表驱动。
func TestLangFor(t *testing.T) {
	for c, want := range map[string]string{"US": "en-US", "JP": "ja-JP", "TW": "zh-Hant-TW", "HK": "zh-Hant-HK", "DE": "de-DE",
		"SG": "en-SG", "BR": "pt-BR", "KE": "en-US" /* 没列出的用 en-US */} {
		if got, err := LangFor(c); err != nil || got != want {
			t.Errorf("%s → %q %v，应为 %q", c, got, err, want)
		}
	}
	for _, c := range []string{"CN", "us", "USA", "", "1A"} {
		if _, err := LangFor(c); err == nil {
			t.Errorf("%q 应拒绝", c)
		}
	}
	for l, want := range map[string]string{"en-US": "en_US", "zh-Hant-TW": "zh-Hant_TW", "es-419": "es_419", "en": "en"} {
		if got := LocaleOf(l); got != want {
			t.Errorf("LocaleOf(%q)=%q", l, got)
		}
	}
}

// a. 时区和语言的校验；两种来源都能得到目标。
func TestTargets(t *testing.T) {
	tg, err := FromExit("JP", "Asia/Tokyo")
	if err != nil || tg != (Target{Timezone: "Asia/Tokyo", Lang: "ja-JP", Locale: "ja_JP", Country: "JP", Source: SourceExit}) || tg.Validate() != nil {
		t.Fatalf("%+v %v", tg, err)
	}
	m, err := Manual("America/Argentina/Buenos_Aires", "es-AR")
	if err != nil || m.Source != SourceManual || m.Locale != "es_AR" || m.Validate() != nil {
		t.Fatalf("%+v %v", m, err)
	}
	for _, tz := range []string{"Mars/Olympus", "America/New York", "../etc/passwd", "/etc/localtime", "Local", "", "Asia/Tokyo;rm", "-settimezone"} {
		if _, err := FromExit("US", tz); !errors.Is(err, ErrTimezone) {
			t.Errorf("时区 %q 应拒绝：%v", tz, err)
		}
		if _, err := Manual(tz, "en-US"); err == nil {
			t.Errorf("手动时区 %q 应拒绝", tz)
		}
	}
	for _, l := range []string{"english", "en_US", "EN-us", "en-US;x", "-array", ""} {
		if _, err := Manual("Asia/Tokyo", l); !errors.Is(err, ErrLang) {
			t.Errorf("语言 %q 应拒绝：%v", l, err)
		}
	}
}

func TestMergeAndDesired(t *testing.T) {
	if got := MergeLangs("en-US", []string{"zh-Hans-CN", "en-US", "ja-JP", "zh-Hans-CN"}); !slices.Equal(got, []string{"en-US", "zh-Hans-CN", "ja-JP"}) {
		t.Fatalf("%v", got)
	}
	base := sysnet.MacLocale{Timezone: "Europe/Berlin", AutoTZ: sysnet.Some("1"), User: "alice",
		UserLangs: sysnet.SomeList("zh-Hans-CN", "en-US"), SysLangs: sysnet.OptList{}}
	tg, _ := FromExit("JP", "Asia/Tokyo")
	d := Desired(base, tg)
	if d.Timezone != "Asia/Tokyo" || d.AutoTZ != sysnet.Some("0") || !slices.Equal(d.UserLangs.V, []string{"ja-JP", "zh-Hans-CN", "en-US"}) ||
		d.UserLocale != sysnet.Some("ja_JP") || !slices.Equal(d.SysLangs.V, []string{"ja-JP"}) {
		t.Fatalf("%+v", d)
	}
	if !Changes(base, tg) || Changes(d, tg) {
		t.Fatal("Changes 不对")
	}
	nobody := base
	nobody.User = ""
	if Changes(sysnet.MacLocale{Timezone: "Asia/Tokyo"}, tg) || !Changes(nobody, tg) {
		t.Fatal("没人登录时只看时区")
	}
	if d := Desired(nobody, tg); !d.UserLangs.Equal(nobody.UserLangs) || !d.UserLocale.Equal(nobody.UserLocale) {
		t.Fatal("没人登录时用户级不动")
	}
}

// ---- b. 命令拼装与解析 ----

func sim(t *testing.T, l sysnet.MacLocale, console string) (*sysnet.Fake, *sysnet.SimRunner, Mac) {
	t.Helper()
	f := sysnet.NewFake(sysnet.FakeState{Locale: l, Console: console})
	s := sysnet.NewSimRunner(f)
	m := Mac{Run: s, Console: func() (string, int, error) {
		c := f.Snapshot().Console
		if c == "" {
			return "", 0, sysnet.ErrNoConsoleUser
		}
		return c, 501, nil
	}}
	return f, s, m
}

const (
	sudoU = "/bin/launchctl asuser 501 /usr/bin/sudo -u alice /usr/bin/defaults "
)

func TestMacCommands(t *testing.T) {
	init := sysnet.MacLocale{Timezone: "Europe/Berlin", AutoTZ: sysnet.Some("1"),
		UserLangs: sysnet.SomeList("zh-Hans-CN", "en-US"), UserLocale: sysnet.Some("zh-Hans_CN"), SysLangs: sysnet.OptList{}}
	f, s, m := sim(t, init, "alice")
	cur, err := m.Read(bg)
	want := init
	want.User = "alice"
	if err != nil || !cur.Equal(want) {
		t.Fatalf("读：%+v %v", cur, err)
	}
	tg, _ := FromExit("US", "America/Los_Angeles")
	d := Desired(cur, tg)
	if err := m.SetTimezone(bg, d.Timezone); err != nil {
		t.Fatal(err)
	}
	if err := m.SetAutoTZ(bg, d.AutoTZ); err != nil {
		t.Fatal(err)
	}
	if err := m.SetUser(bg, "alice", d.UserLangs, d.UserLocale); err != nil {
		t.Fatal(err)
	}
	if err := m.SetSystem(bg, d.SysLangs); err != nil {
		t.Fatal(err)
	}
	// 还原：系统级原来没有 → delete；其余写回原值。
	if err := m.SetSystem(bg, cur.SysLangs); err != nil {
		t.Fatal(err)
	}
	if err := m.SetUser(bg, "alice", cur.UserLangs, cur.UserLocale); err != nil {
		t.Fatal(err)
	}
	if err := m.SetAutoTZ(bg, cur.AutoTZ); err != nil {
		t.Fatal(err)
	}
	if err := m.SetTimezone(bg, cur.Timezone); err != nil {
		t.Fatal(err)
	}
	wantCalls := []string{
		"/usr/sbin/systemsetup -gettimezone",
		"/usr/bin/defaults read /Library/Preferences/com.apple.timezone.auto Active",
		"/usr/bin/defaults read /Library/Preferences/.GlobalPreferences AppleLanguages",
		sudoU + "read -g AppleLanguages",
		sudoU + "read -g AppleLocale",
		"/usr/sbin/systemsetup -settimezone America/Los_Angeles",
		"/usr/bin/defaults write /Library/Preferences/com.apple.timezone.auto Active -bool false",
		sudoU + "write -g AppleLanguages -array en-US zh-Hans-CN",
		sudoU + "write -g AppleLocale -string en_US",
		"/usr/bin/defaults write /Library/Preferences/.GlobalPreferences AppleLanguages -array en-US",
		"/usr/bin/defaults delete /Library/Preferences/.GlobalPreferences AppleLanguages",
		sudoU + "write -g AppleLanguages -array zh-Hans-CN en-US",
		sudoU + "write -g AppleLocale -string zh-Hans_CN",
		"/usr/bin/defaults write /Library/Preferences/com.apple.timezone.auto Active -bool true",
		"/usr/sbin/systemsetup -settimezone Europe/Berlin",
	}
	if got := s.Calls(); !slices.Equal(got, wantCalls) {
		t.Fatalf("命令：\n%s", strings.Join(got, "\n"))
	}
	if got := f.Snapshot().Locale; !got.Equal(init) {
		t.Fatalf("还原后应等于初始值：%+v", got)
	}
	if u := s.Unknown(); len(u) != 0 {
		t.Fatalf("模拟器不认识：%v", u)
	}
}

// 原来没有键：读出“不存在”，还原用 delete；本来就没有时 delete 也算成功。
func TestMacMissingKeys(t *testing.T) {
	f, s, m := sim(t, sysnet.MacLocale{Timezone: "Europe/Berlin"}, "alice")
	cur, err := m.Read(bg)
	if err != nil || cur.AutoTZ.Set || cur.UserLangs.Set || cur.UserLocale.Set || cur.SysLangs.Set {
		t.Fatalf("%+v %v", cur, err)
	}
	tg, _ := Manual("Asia/Tokyo", "ja-JP")
	d := Desired(cur, tg)
	_ = m.SetAutoTZ(bg, d.AutoTZ)
	_ = m.SetUser(bg, "alice", d.UserLangs, d.UserLocale)
	before := len(s.Calls())
	if err := m.SetUser(bg, "alice", cur.UserLangs, cur.UserLocale); err != nil {
		t.Fatal(err)
	}
	if err := m.SetAutoTZ(bg, cur.AutoTZ); err != nil {
		t.Fatal(err)
	}
	if err := m.SetAutoTZ(bg, cur.AutoTZ); err != nil { // 再删一次：本来就没有
		t.Fatal(err)
	}
	got := s.Calls()[before:]
	want := []string{sudoU + "delete -g AppleLanguages", sudoU + "delete -g AppleLocale",
		"/usr/bin/defaults delete /Library/Preferences/com.apple.timezone.auto Active",
		"/usr/bin/defaults delete /Library/Preferences/com.apple.timezone.auto Active"}
	if !slices.Equal(got, want) {
		t.Fatalf("%v", got)
	}
	if l := f.Snapshot().Locale; l.AutoTZ.Set || l.UserLangs.Set || l.UserLocale.Set {
		t.Fatalf("%+v", l)
	}
}

// 没有人登录：读时跳过用户级；写用户级返回 ErrNoConsoleUser，不执行命令。登录的换了人：ErrConsoleChanged。
func TestMacNoConsoleUser(t *testing.T) {
	f, s, m := sim(t, sysnet.MacLocale{Timezone: "Asia/Tokyo", UserLangs: sysnet.SomeList("en-US")}, "")
	cur, err := m.Read(bg)
	if err != nil || cur.User != "" || cur.UserLangs.Set {
		t.Fatalf("%+v %v", cur, err)
	}
	n := len(s.Calls())
	if err := m.SetUser(bg, "alice", sysnet.SomeList("ja-JP"), sysnet.Some("ja_JP")); !errors.Is(err, sysnet.ErrNoConsoleUser) || len(s.Calls()) != n {
		t.Fatalf("%v", err)
	}
	f.SetConsoleUser("bob")
	if err := m.SetUser(bg, "alice", sysnet.SomeList("ja-JP"), sysnet.Some("ja_JP")); !errors.Is(err, sysnet.ErrConsoleChanged) || len(s.Calls()) != n {
		t.Fatalf("%v", err)
	}
}

// 非法输入在调用执行器之前报错。
func TestMacRejectsBeforeExec(t *testing.T) {
	_, s, m := sim(t, sysnet.MacLocale{Timezone: "Asia/Tokyo"}, "alice")
	errs := []error{
		m.SetTimezone(bg, "America/New York"),
		m.SetTimezone(bg, "-x"),
		m.SetAutoTZ(bg, sysnet.Some("yes")),
		m.SetUser(bg, "alice", sysnet.SomeList("en-US", "-array"), sysnet.Some("en_US")),
		m.SetUser(bg, "alice", sysnet.SomeList("en US"), sysnet.Some("en_US")),
		m.SetUser(bg, "alice", sysnet.SomeList("en-US"), sysnet.Some("en_US; rm")),
		m.SetSystem(bg, sysnet.OptList{Set: true}),
		m.SetSystem(bg, sysnet.SomeList("$(x)")),
	}
	for i, err := range errs {
		if err == nil {
			t.Errorf("第 %d 个应报错", i)
		}
	}
	if c := s.Calls(); len(c) != 0 {
		t.Fatalf("不应调用执行器：%v", c)
	}
	// 用户名不合法（Console 给的）：不执行。
	bad := Mac{Run: s, Console: func() (string, int, error) { return "Alice Smith", 501, nil }}
	if err := bad.SetUser(bg, "Alice Smith", sysnet.SomeList("en-US"), sysnet.Some("en_US")); err == nil || len(s.Calls()) != 0 {
		t.Fatal("用户名不合法应拒绝")
	}
}

// 解析样本：会话里采到的（region/testdata），加构造的 systemsetup 输出。
func TestParseSamples(t *testing.T) {
	read := func(n string) string {
		b, err := os.ReadFile("testdata/" + n)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if v, err := ParseArray(read("defaults-read-applelanguages.txt")); err != nil || !slices.Equal(v, []string{"zh-Hans-CN", "en-US"}) {
		t.Fatalf("%v %v", v, err)
	}
	if v, err := readStr(read("defaults-read-applelocale.txt"), "", nil); err != nil || v != sysnet.Some("zh-Hans_CN") {
		t.Fatalf("%v %v", v, err)
	}
	if v, err := readStr("", read("defaults-read-missing.stderr"), errors.New("exit status 1")); err != nil || v.Set {
		t.Fatalf("缺键：%v %v", v, err)
	}
	if v, err := readStr(read("defaults-read-timezone-auto.txt"), "", nil); err != nil || v != sysnet.Some("1") {
		t.Fatalf("%v %v", v, err)
	}
	if _, err := ParseTimezone(read("systemsetup-nonroot.txt")); err == nil {
		t.Fatal("非 root 的输出应当成失败")
	}
	if tz, err := ParseTimezone(read("systemsetup-gettimezone.constructed.txt")); err != nil || tz != "America/Los_Angeles" {
		t.Fatalf("%q %v", tz, err)
	}
	if _, err := ParseArray("(\n    \"en-US\",\n    \"$(reboot)\"\n)"); err == nil {
		t.Fatal("读回的值含奇怪字符时应报错")
	}
	if _, err := readStr("", "some other error", errors.New("exit status 1")); err == nil {
		t.Fatal("不是缺键的失败应报错")
	}
}

// i. 源码检查：region 不执行外部命令（只经 sysnet.Runner），不出现以 / 开头的程序路径字面量。
func TestNoExecInRegion(t *testing.T) {
	srccheck.NoExec(t, ".")
}
