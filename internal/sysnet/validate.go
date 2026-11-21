package sysnet

import (
	"errors"
	"regexp"
	"strings"
)

// ErrBadArg 是参数在调用 Runner 之前就被判为不合法。
var ErrBadArg = errors.New("参数不合法")

// ValidService 校验网络服务名（进 networksetup 参数之前）：非空、不含换行和其他控制字符、不以 - 开头。
func ValidService(name string) error {
	if name == "" || strings.HasPrefix(name, "-") || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return ErrBadArg
	}
	return nil
}

var utunRe = regexp.MustCompile(`^utun[0-9]{1,4}$`)

// ValidUtun 校验 utun 接口名：utun<数字>。
func ValidUtun(name string) error {
	if !utunRe.MatchString(name) {
		return ErrBadArg
	}
	return nil
}
