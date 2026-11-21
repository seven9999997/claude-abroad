package sysnet

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// 命令模拟器的地区部分（m4-plan v2.4 决定 7）：systemsetup、defaults、launchctl asuser … sudo -u … defaults，
// 按真机的输出格式作答（region/testdata 的样本；systemsetup -gettimezone 的 root 输出是构造样本），改动落到 Fake 上。

func simMissing(key string) ([]byte, []byte, error) {
	return nil, []byte("Error: Could not find key '" + key + "' in domain 'kCFPreferencesAnyApplication'.\n"), errExit1
}

// SimDefaultsArray 按 defaults read 的格式打印数组。
func SimDefaultsArray(v []string) string {
	var b strings.Builder
	b.WriteString("(\n")
	for i, x := range v {
		b.WriteString(`    "` + x + `"`)
		if i < len(v)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString(")\n")
	return b.String()
}

func simErr(err error) ([]byte, []byte, error) { return nil, []byte(err.Error() + "\n"), errExit1 }

func (s *SimRunner) systemsetup(ctx context.Context, args []string) ([]byte, []byte, error) {
	loc := s.F.Locale()
	switch {
	case slices.Equal(args, []string{"-gettimezone"}):
		l, err := loc.Read(ctx)
		if err != nil {
			return simErr(err)
		}
		return []byte("Time Zone: " + l.Timezone + "\n"), nil, nil
	case len(args) == 2 && args[0] == "-settimezone":
		if err := loc.SetTimezone(ctx, args[1]); err != nil {
			return simErr(err)
		}
		return nil, nil, nil
	}
	return nil, nil, fmt.Errorf("%w：systemsetup %v", ErrSimUnknown, args)
}

// defaults 处理 root 执行的（系统级）defaults：自动时区、.GlobalPreferences 的 AppleLanguages。
func (s *SimRunner) defaults(ctx context.Context, args []string) ([]byte, []byte, error) {
	loc := s.F.Locale()
	if len(args) < 3 {
		return nil, nil, fmt.Errorf("%w：defaults %v", ErrSimUnknown, args)
	}
	cur, err := loc.Read(ctx)
	if err != nil {
		return simErr(err)
	}
	op, dom, key := args[0], args[1], args[2]
	switch {
	case dom == AutoTZDomain && key == "Active":
		switch {
		case op == "read" && len(args) == 3:
			if !cur.AutoTZ.Set {
				return simMissing(key)
			}
			return []byte(cur.AutoTZ.V + "\n"), nil, nil
		case op == "write" && len(args) == 5 && args[3] == "-bool" && (args[4] == "true" || args[4] == "false"):
			v := "0"
			if args[4] == "true" {
				v = "1"
			}
			if err := loc.SetAutoTZ(ctx, Some(v)); err != nil {
				return simErr(err)
			}
			return nil, nil, nil
		case op == "delete" && len(args) == 3:
			if err := loc.SetAutoTZ(ctx, OptStr{}); err != nil {
				return simErr(err)
			}
			return nil, nil, nil
		}
	case dom == GlobalDomain && key == "AppleLanguages":
		switch {
		case op == "read" && len(args) == 3:
			if !cur.SysLangs.Set {
				return simMissing(key)
			}
			return []byte(SimDefaultsArray(cur.SysLangs.V)), nil, nil
		case op == "write" && len(args) >= 5 && args[3] == "-array":
			if err := loc.SetSystem(ctx, SomeList(args[4:]...)); err != nil {
				return simErr(err)
			}
			return nil, nil, nil
		case op == "delete" && len(args) == 3:
			if err := loc.SetSystem(ctx, OptList{}); err != nil {
				return simErr(err)
			}
			return nil, nil, nil
		}
	}
	return nil, nil, fmt.Errorf("%w：defaults %v", ErrSimUnknown, args)
}

// launchctl 处理 launchctl asuser <uid> /usr/bin/sudo -u <用户> /usr/bin/defaults <read|write|delete> -g <键> …。
// 用户级的读写一次只改一个键；SetUser 一次改两项，所以这里先读出另一项再写回。
func (s *SimRunner) launchctl(ctx context.Context, args []string) ([]byte, []byte, error) {
	bad := func() ([]byte, []byte, error) { return nil, nil, fmt.Errorf("%w：launchctl %v", ErrSimUnknown, args) }
	if len(args) < 9 || args[0] != "asuser" || args[2] != PathSudo || args[3] != "-u" || args[5] != PathDefaults || args[7] != "-g" {
		return bad()
	}
	if _, err := strconv.Atoi(args[1]); err != nil {
		return bad()
	}
	user, op, key, rest := args[4], args[6], args[8], args[9:]
	loc := s.F.Locale()
	cur, err := loc.Read(ctx)
	if err != nil {
		return simErr(err)
	}
	if cur.User == "" || cur.User != user {
		return nil, []byte("sudo: unknown user or no session\n"), errExit1
	}
	langs, locale := cur.UserLangs, cur.UserLocale
	switch {
	case key == "AppleLanguages" && op == "read" && len(rest) == 0:
		if !langs.Set {
			return simMissing(key)
		}
		return []byte(SimDefaultsArray(langs.V)), nil, nil
	case key == "AppleLocale" && op == "read" && len(rest) == 0:
		if !locale.Set {
			return simMissing(key)
		}
		return []byte(locale.V + "\n"), nil, nil
	case key == "AppleLanguages" && op == "write" && len(rest) >= 2 && rest[0] == "-array":
		langs = SomeList(rest[1:]...)
	case key == "AppleLanguages" && op == "delete" && len(rest) == 0:
		langs = OptList{}
	case key == "AppleLocale" && op == "write" && len(rest) == 2 && rest[0] == "-string":
		locale = Some(rest[1])
	case key == "AppleLocale" && op == "delete" && len(rest) == 0:
		locale = OptStr{}
	default:
		return bad()
	}
	if err := loc.SetUser(ctx, user, langs, locale); err != nil {
		if errors.Is(err, ErrNoConsoleUser) || errors.Is(err, ErrConsoleChanged) {
			return nil, []byte("sudo: unknown user or no session\n"), errExit1
		}
		return simErr(err)
	}
	return nil, nil, nil
}
