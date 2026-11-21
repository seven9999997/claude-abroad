package rules

import (
	"errors"
	"strings"

	"golang.org/x/net/idna"
)

var errBadName = errors.New("不是合法的域名")

// NormalizeName 把域名归一成小写 ASCII（国际化域名转成 punycode），去掉末尾的点。
// 不合法的名字返回错误；调用方要把它当成“看不清去向”，不能当成“不是置顶”。
func NormalizeName(s string) (string, error) {
	s = strings.TrimSuffix(s, ".")
	if s == "" || len(s) > 253*4 {
		return "", errBadName
	}
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if !ascii {
		a, err := idna.Lookup.ToASCII(s)
		if err != nil {
			return "", errBadName
		}
		s = strings.TrimSuffix(a, ".")
	}
	s = strings.ToLower(s)
	if len(s) == 0 || len(s) > 253 {
		return "", errBadName
	}
	for _, l := range strings.Split(s, ".") {
		if !validLabel(l) {
			return "", errBadName
		}
		if strings.HasPrefix(l, "xn--") {
			// punycode 标签要能解开：解不开的按不合法处理，不猜它指什么。
			if _, err := idna.Lookup.ToUnicode(l); err != nil {
				return "", errBadName
			}
		}
	}
	return s, nil
}

func validLabel(l string) bool {
	if len(l) == 0 || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// pattern 是归一后的域名模式：Wild 为真时匹配 Base 的所有子域（不含 Base 本身）。
type pattern struct {
	Wild bool
	Base string
}

func (p pattern) String() string {
	if p.Wild {
		return "*." + p.Base
	}
	return p.Base
}

// parsePattern 只收 "example.com" 和 "*.example.com" 两种形式，不收单独的 "*"。
func parsePattern(s string) (pattern, error) {
	wild := strings.HasPrefix(s, "*.")
	if wild {
		s = s[2:]
	}
	if strings.Contains(s, "*") {
		return pattern{}, errors.New("通配符只能写在最前面，形如 *.example.com")
	}
	n, err := NormalizeName(s)
	if err != nil {
		return pattern{}, err
	}
	return pattern{Wild: wild, Base: n}, nil
}

// isSub 判断 name 是不是 base 的真子域。
func isSub(name, base string) bool {
	return len(name) > len(base) && strings.HasSuffix(name, base) && name[len(name)-len(base)-1] == '.'
}

// match 判断归一后的名字是否命中模式。
func (p pattern) match(name string) bool {
	if p.Wild {
		return isSub(name, p.Base)
	}
	return name == p.Base
}

// overlap 判断两个模式是否存在同时命中的名字。
func overlap(a, b pattern) bool {
	switch {
	case !a.Wild && !b.Wild:
		return a.Base == b.Base
	case !a.Wild:
		return b.match(a.Base)
	case !b.Wild:
		return a.match(b.Base)
	default:
		return a.Base == b.Base || isSub(a.Base, b.Base) || isSub(b.Base, a.Base)
	}
}
