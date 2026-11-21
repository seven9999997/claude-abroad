package agent

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

const testPW = "0123456789abcdef0123456789abcdef"

func TestSingBoxConfigInvariants(t *testing.T) {
	for name, exits := range map[string][]SingBoxExit{
		"只有 main": {{Tag: "main", Port: 7890}},
		"多个出口":    {{Tag: "main", Port: 7890}, {Tag: "wg-us", Port: 7891}},
	} {
		t.Run(name, func(t *testing.T) {
			b, err := SingBoxConfig(exits, testPW)
			if err != nil {
				t.Fatal(err)
			}
			var raw map[string]any
			if err := json.Unmarshal(b, &raw); err != nil {
				t.Fatal(err)
			}
			var keys []string
			for k := range raw {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if strings.Join(keys, ",") != "inbounds,log,outbounds,route" {
				t.Fatalf("顶层键只能有 log、inbounds、outbounds、route：%v", keys)
			}
			text := string(b)
			for _, bad := range []string{"bypass", "sniff", "domain_strategy", "auto_detect_interface",
				"default_interface", "bind_interface", "direct", "block", "\"dns\"", "routing_mark"} {
				if strings.Contains(text, bad) {
					t.Errorf("配置里不应出现 %q", bad)
				}
			}

			var c sbConfig
			if err := json.Unmarshal(b, &c); err != nil {
				t.Fatal(err)
			}
			tags := map[string]bool{}
			if len(c.Outbounds) == 0 {
				t.Fatal("outbounds 至少一个")
			}
			for _, o := range c.Outbounds {
				if o.Type != "socks" || o.Server != "127.0.0.1" || o.Version != "5" {
					t.Errorf("出站只能是指向 127.0.0.1 的 socks5：%+v", o)
				}
				tags[o.Tag] = true
			}
			if c.Route.Final == "" || !tags[c.Route.Final] {
				t.Errorf("route.final 必须是已有出站：%q", c.Route.Final)
			}
			rules := c.Route.Rules
			last := rules[len(rules)-1]
			if last.Action != "reject" || len(last.AuthUser) != 0 || strings.Join(last.Network, ",") != "tcp,udp" {
				t.Errorf("最后一条应是无条件的 reject：%+v", last)
			}
			ruleUsers := map[string]bool{}
			for _, r := range rules[:len(rules)-1] {
				if len(r.AuthUser) == 0 || r.Action != "route" || !tags[r.Outbound] {
					t.Errorf("reject 之前的规则都要带 auth_user 并指向已有出站：%+v", r)
				}
				for _, u := range r.AuthUser {
					ruleUsers[u] = true
				}
			}
			if len(c.Inbounds) != 1 || c.Inbounds[0].Listen != "127.0.0.1" || c.Inbounds[0].Type != "socks" {
				t.Fatalf("入站只能是一个只听 127.0.0.1 的 socks：%+v", c.Inbounds)
			}
			users := map[string]bool{}
			for _, u := range c.Inbounds[0].Users {
				users[u.Username] = true
				if !ruleUsers[u.Username] || u.Password != testPW {
					t.Errorf("用户 %s 没有对应规则或口令不对", u.Username)
				}
			}
			for u := range ruleUsers {
				if !users[u] {
					t.Errorf("规则里的 %s 不是入站用户", u)
				}
			}
		})
	}
}

func TestSingBoxConfigRejectsBadInput(t *testing.T) {
	for name, c := range map[string]struct {
		exits []SingBoxExit
		pw    string
	}{
		"出口表为空":   {nil, testPW},
		"没有 main": {[]SingBoxExit{{Tag: "wg-us", Port: 7891}}, testPW},
		"标签非法":    {[]SingBoxExit{{Tag: "Main!", Port: 7890}}, testPW},
		"标签重复":    {[]SingBoxExit{{Tag: "main", Port: 7890}, {Tag: "main", Port: 7891}}, testPW},
		"端口越界":    {[]SingBoxExit{{Tag: "main", Port: 70000}}, testPW},
		"端口冲突":    {[]SingBoxExit{{Tag: "main", Port: 1090}}, testPW},
		"口令太短":    {[]SingBoxExit{{Tag: "main", Port: 7890}}, "short"},
	} {
		if _, err := SingBoxConfig(c.exits, c.pw); err == nil {
			t.Errorf("%s：应报错", name)
		}
	}
}
