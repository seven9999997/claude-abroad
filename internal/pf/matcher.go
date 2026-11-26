package pf

import (
	"net/netip"
	"regexp"
	"slices"
	"strings"
)

// PrintMatcher 是默认的 Matcher：把 pf 打印的每一行（关闭优化器时的形式，M2-6b 实测样本）还原成 Render 的写法，
// 再和期望文本（去掉 table 行）逐行比对，必须完全相等。只做四种还原：
//   - 去掉行尾的 " flags S/SA keep state" 或 " keep state"；
//   - "port = N" → "port N"；
//   - "user = <UID 或用户名>" → "user <User>"（UID 不对的保持原样，于是判不同）；
//   - "inet from any to X" → "inet to X"（没有源端口条件时 pf 会补上 from any；inet6 同样，M6-11a 回国拦截那一组）。
//
// 还原后仍不相等的一律判不同（包括 __automatic 表、多行、少行、顺序不同），不做任何容错。
// 表内容另行比对：-t abroad_lan -T show 的结果与期望的网段集合相同。
type PrintMatcher struct {
	User string // 放行用户名（Render 的 Config.User）
	UID  string // 它的 UID（十进制；由调用方注入，本包不查 os/user）
}

var userEqRe = regexp.MustCompile(`user = (\S+)`)

func (m PrintMatcher) normalize(line string) string {
	l := strings.Join(strings.Fields(line), " ")
	for _, suf := range []string{" flags S/SA keep state", " keep state"} {
		if s, ok := strings.CutSuffix(l, suf); ok {
			l = s
			break
		}
	}
	l = strings.ReplaceAll(l, "port = ", "port ")
	l = userEqRe.ReplaceAllStringFunc(l, func(s string) string {
		v := strings.TrimPrefix(s, "user = ")
		if v == m.User || (m.UID != "" && v == m.UID) {
			return "user " + m.User
		}
		return s
	})
	l = strings.Replace(l, " inet from any to ", " inet to ", 1)
	return strings.Replace(l, " inet6 from any to ", " inet6 to ", 1)
}

// ruleLines 取出规则行（去掉空行和 table 行）。
func ruleLines(text []byte) []string {
	var out []string
	for _, l := range strings.Split(string(text), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "table ") {
			continue
		}
		out = append(out, l)
	}
	return out
}

// Present 实现 Matcher：锚点里有没有规则。
func (m PrintMatcher) Present(st Status) bool { return len(ruleLines(st.AnchorRules)) > 0 }

// Same 实现 Matcher。
func (m PrintMatcher) Same(expected []byte, st Status) bool {
	want := ruleLines(expected)
	got := ruleLines(st.AnchorRules)
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if m.normalize(got[i]) != want[i] {
			return false
		}
	}
	wantTable := expectedTable(expected)
	if wantTable == nil {
		// 期望里没有 <abroad_lan>（“允许局域网”关闭）：旧表可能残留在锚点里，但没有规则引用它，不比。
		return true
	}
	return slices.Equal(wantTable, tableEntries(st.Table))
}

// expectedTable 从 Render 的输出里取 <abroad_lan> 的网段（已排序）。
func expectedTable(rules []byte) []netip.Prefix {
	for _, l := range strings.Split(string(rules), "\n") {
		if rest, ok := strings.CutPrefix(l, "table <abroad_lan> { "); ok {
			body, _ := strings.CutSuffix(rest, " }")
			var out []netip.Prefix
			for _, p := range strings.Split(body, ", ") {
				if pp, err := netip.ParsePrefix(p); err == nil {
					out = append(out, pp.Masked())
				}
			}
			return sortPrefixes(out)
		}
	}
	return nil
}

// tableEntries 解析 -T show 的输出（每行一个地址或网段；单个地址按 /32）。解析不了的行让结果失配。
func tableEntries(show []byte) []netip.Prefix {
	var out []netip.Prefix
	for _, l := range strings.Split(string(show), "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if p, err := netip.ParsePrefix(l); err == nil {
			out = append(out, p.Masked())
		} else if a, err := netip.ParseAddr(l); err == nil {
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
		} else {
			out = append(out, netip.Prefix{}) // 不认识：保证不相等
		}
	}
	return sortPrefixes(out)
}
