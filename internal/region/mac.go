package region

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"claude-abroad/internal/sysnet"
)

// Console 返回当前登录用户（/dev/console 的属主）。没有人登录（属主是 root）时返回 sysnet.ErrNoConsoleUser。
type Console func() (user string, uid int, err error)

// Mac 是 sysnet.MacRegion 的真实实现：只拼命令、解析输出，命令经 Run 执行（真实模式是 sysnet.ExecRunner，
// 测试是命令模拟器）。用户名和 uid 只来自 Console，不接受外部传入（m4-plan v2.4 决定 7）。
type Mac struct {
	Run     sysnet.Runner
	Console Console
}

var userRe = regexp.MustCompile(`^[a-z_][a-z0-9_.\-]{0,31}$`)

// console 取并校验登录用户。
func (m Mac) console() (string, int, error) {
	if m.Console == nil {
		return "", 0, sysnet.ErrNoConsoleUser
	}
	u, uid, err := m.Console()
	if err != nil {
		return "", 0, err
	}
	if uid <= 0 || !userRe.MatchString(u) {
		return "", 0, fmt.Errorf("登录用户不对：uid %d", uid)
	}
	return u, uid, nil
}

func (m Mac) run(ctx context.Context, path string, args ...string) (string, string, error) {
	so, se, err := m.Run.Run(ctx, path, args, nil)
	return string(so), string(se), err
}

// missing 报告 defaults read 的失败是不是“键不存在”。
func missing(stderr string) bool {
	return strings.Contains(stderr, "Could not find key") || strings.Contains(stderr, "does not exist")
}

// ParseArray 解析 defaults read 打印的数组（"(\n    "en-US",\n    "zh-Hans-CN"\n)"）。
func ParseArray(out string) ([]string, error) {
	s := strings.TrimSpace(out)
	if !strings.HasPrefix(s, "(") || !strings.HasSuffix(s, ")") {
		return nil, errors.New("不是数组")
	}
	var v []string
	for _, line := range strings.Split(strings.TrimSpace(s[1:len(s)-1]), "\n") {
		item := strings.TrimSuffix(strings.TrimSpace(line), ",")
		if item == "" {
			continue
		}
		if len(item) >= 2 && item[0] == '"' && item[len(item)-1] == '"' {
			item = item[1 : len(item)-1]
		}
		if err := ValidRaw(item); err != nil {
			return nil, err
		}
		v = append(v, item)
	}
	return v, nil
}

// ParseTimezone 解析 systemsetup -gettimezone（"Time Zone: America/Los_Angeles"）。非 root 时它退出码是 0，
// 只打一句“You need administrator access”，按失败处理。
func ParseTimezone(out string) (string, error) {
	s := strings.TrimSpace(out)
	tz, ok := strings.CutPrefix(s, "Time Zone: ")
	if !ok {
		return "", errors.New("读不出时区（systemsetup 的输出不认识，可能没有管理员权限）")
	}
	if err := ValidTimezone(tz); err != nil {
		return "", err
	}
	return tz, nil
}

func (m Mac) userArgs(user string, uid int, defaultsArgs ...string) []string {
	return append([]string{"asuser", strconv.Itoa(uid), sysnet.PathSudo, "-u", user, sysnet.PathDefaults}, defaultsArgs...)
}

// readStr 读一个字符串或布尔键（不存在时 Set 为 false）。
func readStr(so, se string, err error) (sysnet.OptStr, error) {
	if err != nil {
		if missing(se) {
			return sysnet.OptStr{}, nil
		}
		return sysnet.OptStr{}, errors.New("读设置失败")
	}
	v := strings.TrimSpace(so)
	if err := ValidRaw(v); err != nil {
		return sysnet.OptStr{}, err
	}
	return sysnet.Some(v), nil
}

func readList(so, se string, err error) (sysnet.OptList, error) {
	if err != nil {
		if missing(se) {
			return sysnet.OptList{}, nil
		}
		return sysnet.OptList{}, errors.New("读设置失败")
	}
	v, perr := ParseArray(so)
	if perr != nil {
		return sysnet.OptList{}, perr
	}
	return sysnet.SomeList(v...), nil
}

// Read 读 Mac 当前的地区设置。任何一项读不出来（不是“键不存在”）都返回错误：没有原值就没法保证回国时还原。
// 没有人登录时用户级的两项不读（User 为空）。
func (m Mac) Read(ctx context.Context) (sysnet.MacLocale, error) {
	var l sysnet.MacLocale
	so, _, err := m.run(ctx, sysnet.PathSystemsetup, "-gettimezone")
	if err != nil {
		return l, fmt.Errorf("读时区：%w", err)
	}
	if l.Timezone, err = ParseTimezone(so); err != nil {
		return l, err
	}
	so, se, err := m.run(ctx, sysnet.PathDefaults, "read", sysnet.AutoTZDomain, "Active")
	if l.AutoTZ, err = readStr(so, se, err); err != nil {
		return l, fmt.Errorf("读自动时区：%w", err)
	}
	if l.AutoTZ.Set && l.AutoTZ.V != "0" && l.AutoTZ.V != "1" {
		return l, errors.New("读自动时区：值不认识")
	}
	so, se, err = m.run(ctx, sysnet.PathDefaults, "read", sysnet.GlobalDomain, "AppleLanguages")
	if l.SysLangs, err = readList(so, se, err); err != nil {
		return l, fmt.Errorf("读系统级的语言：%w", err)
	}
	user, uid, err := m.console()
	if errors.Is(err, sysnet.ErrNoConsoleUser) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	l.User = user
	so, se, err = m.run(ctx, sysnet.PathLaunchctl, m.userArgs(user, uid, "read", "-g", "AppleLanguages")...)
	if l.UserLangs, err = readList(so, se, err); err != nil {
		return l, fmt.Errorf("读用户的语言：%w", err)
	}
	so, se, err = m.run(ctx, sysnet.PathLaunchctl, m.userArgs(user, uid, "read", "-g", "AppleLocale")...)
	if l.UserLocale, err = readStr(so, se, err); err != nil {
		return l, fmt.Errorf("读用户的地区格式：%w", err)
	}
	return l, nil
}

func cmdErr(what string, se string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s失败：%w", what, err)
}

// SetTimezone：systemsetup -settimezone <tz>。
func (m Mac) SetTimezone(ctx context.Context, tz string) error {
	if err := ValidTimezone(tz); err != nil {
		return err
	}
	_, se, err := m.run(ctx, sysnet.PathSystemsetup, "-settimezone", tz)
	return cmdErr("改时区", se, err)
}

// SetAutoTZ：defaults write … Active -bool true|false，或 delete（原来没有这个键）。
func (m Mac) SetAutoTZ(ctx context.Context, v sysnet.OptStr) error {
	var args []string
	switch {
	case !v.Set:
		args = []string{"delete", sysnet.AutoTZDomain, "Active"}
	case v.V == "0":
		args = []string{"write", sysnet.AutoTZDomain, "Active", "-bool", "false"}
	case v.V == "1":
		args = []string{"write", sysnet.AutoTZDomain, "Active", "-bool", "true"}
	default:
		return errors.New("自动时区的值不对")
	}
	_, se, err := m.run(ctx, sysnet.PathDefaults, args...)
	if err != nil && !v.Set && missing(se) {
		return nil // 本来就没有：删除是幂等的
	}
	return cmdErr("改自动时区", se, err)
}

func validList(v sysnet.OptList) error {
	if v.Set && len(v.V) == 0 {
		return errors.New("语言列表为空")
	}
	for _, x := range v.V {
		if err := ValidRaw(x); err != nil {
			return err
		}
	}
	return nil
}

// SetUser 改当前登录用户的 AppleLanguages、AppleLocale（launchctl asuser <uid> sudo -u <用户> defaults …）。
func (m Mac) SetUser(ctx context.Context, user string, langs sysnet.OptList, locale sysnet.OptStr) error {
	if err := validList(langs); err != nil {
		return err
	}
	if locale.Set {
		if err := ValidRaw(locale.V); err != nil {
			return err
		}
	}
	cur, uid, err := m.console()
	if err != nil {
		return err
	}
	if cur != user {
		return sysnet.ErrConsoleChanged
	}
	la := []string{"delete", "-g", "AppleLanguages"}
	if langs.Set {
		la = append([]string{"write", "-g", "AppleLanguages", "-array"}, langs.V...)
	}
	if _, se, err := m.run(ctx, sysnet.PathLaunchctl, m.userArgs(cur, uid, la...)...); err != nil && !(!langs.Set && missing(se)) {
		return cmdErr("改用户的语言", se, err)
	}
	lo := []string{"delete", "-g", "AppleLocale"}
	if locale.Set {
		lo = []string{"write", "-g", "AppleLocale", "-string", locale.V}
	}
	if _, se, err := m.run(ctx, sysnet.PathLaunchctl, m.userArgs(cur, uid, lo...)...); err != nil && !(!locale.Set && missing(se)) {
		return cmdErr("改用户的地区格式", se, err)
	}
	return nil
}

// SetSystem 改 /Library/Preferences/.GlobalPreferences 的 AppleLanguages。
func (m Mac) SetSystem(ctx context.Context, langs sysnet.OptList) error {
	if err := validList(langs); err != nil {
		return err
	}
	args := []string{"delete", sysnet.GlobalDomain, "AppleLanguages"}
	if langs.Set {
		args = append([]string{"write", sysnet.GlobalDomain, "AppleLanguages", "-array"}, langs.V...)
	}
	_, se, err := m.run(ctx, sysnet.PathDefaults, args...)
	if err != nil && !langs.Set && missing(se) {
		return nil
	}
	return cmdErr("改系统级的语言", se, err)
}
