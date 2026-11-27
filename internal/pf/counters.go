package pf

import (
	"bytes"
	"context"
	"regexp"
	"strconv"
	"strings"
)

// RuleCounter 是锚点里一条规则和它的匹配计数（pfctl -a <锚点> -v -s rules 的输出；只读）。
// Rule 是 pfctl 打印的原文，调用方写进事件、诊断包之前要脱敏（DHCP、局域网那几行有网段）。
type RuleCounter struct {
	Rule        string `json:"rule"`
	Evaluations int64  `json:"evaluations"`
	Packets     int64  `json:"packets"`
	Bytes       int64  `json:"bytes"`
	States      int64  `json:"states"`
}

// Counters 是现场快照要的 pf 部分（2026-11-27 真机启用失败后加）：pf 是否启用、主规则集是否还引用锚点、
// 锚点里每条规则的匹配计数。
type Counters struct {
	Enabled    bool          `json:"enabled"`
	Referenced bool          `json:"referenced"`
	Rules      []RuleCounter `json:"rules"`
}

// VerboseRulesArgs 是读锚点规则和计数的参数（等价于 pfctl -a <锚点> -vsr）。
func VerboseRulesArgs(anchor string) []string { return []string{"-a", anchor, "-v", "-s", "rules"} }

// Counters 只读地取 pf 的启用状态和锚点里每条规则的匹配计数。
func (c *Controller) Counters(ctx context.Context) (Counters, error) {
	a, err := c.anchor()
	if err != nil {
		return Counters{}, err
	}
	var out Counters
	info, se, err := c.run(ctx, nil, "-s", "info")
	if err != nil {
		return out, cmdErr("读状态", se, err)
	}
	first, _, _ := bytes.Cut(info, []byte("\n"))
	out.Enabled = bytes.Contains(first, []byte("Status: Enabled"))
	main, se, err := c.run(ctx, nil, "-s", "rules")
	if err != nil {
		return out, cmdErr("读主规则集", se, err)
	}
	out.Referenced = referencesAnchor(main)
	vr, se, err := c.run(ctx, nil, VerboseRulesArgs(a)...)
	if err != nil {
		return out, cmdErr("读锚点规则的计数", se, err)
	}
	out.Rules = ParseVerboseRules(vr)
	return out, nil
}

var counterRe = regexp.MustCompile(`(Evaluations|Packets|Bytes|States)\s*:\s*(\d+)`)

// ParseVerboseRules 解析 pfctl -v -s rules 的输出：规则行后面跟一行 “[ Evaluations: … Packets: … Bytes: … States: … ]”
// （-vv 时规则行前面有 “@序号 ”，一并去掉）。没有计数行的规则计数为 0。
func ParseVerboseRules(out []byte) []RuleCounter {
	var rules []RuleCounter
	for _, l := range strings.Split(string(out), "\n") {
		t := strings.TrimSpace(l)
		switch {
		case t == "":
		case strings.HasPrefix(t, "["):
			if len(rules) == 0 {
				continue
			}
			r := &rules[len(rules)-1]
			for _, m := range counterRe.FindAllStringSubmatch(t, -1) {
				n, _ := strconv.ParseInt(m[2], 10, 64)
				switch m[1] {
				case "Evaluations":
					r.Evaluations = n
				case "Packets":
					r.Packets = n
				case "Bytes":
					r.Bytes = n
				case "States":
					r.States = n
				}
			}
		default:
			if strings.HasPrefix(t, "@") {
				if _, rest, ok := strings.Cut(t, " "); ok {
					t = strings.TrimSpace(rest)
				}
			}
			rules = append(rules, RuleCounter{Rule: t})
		}
	}
	return rules
}
