package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"claude-abroad/internal/rules"
)

// 规则里的四项（补齐一，architect 2026-11-27）：本地域名名单、追加的置顶域名、离线放行名单、拦截 UDP 443。
// 都是“先读后写”：读当前规则，在读到的文档上只改这一处，整份提交；放松还是收紧由后台按内容判定（放松要 sudo）。
// 域名的规范化和合法性由后台负责：这里只为“重复添加、删除不存在的项不产生改动”做最基本的比较，不另写一套校验。

// listKind 是一个名单。
type listKind struct {
	name  string // 子命令里的名字
	title string // 输出里的名字
	what  string // 用法里一个元素叫什么
	get   func(c *rules.Config) []string
	set   func(c *rules.Config, xs []string)
	key   func(s string) string // 比较用的规范形式（失败时原样，交后台判定）
}

func domainKey(s string) string {
	s = strings.TrimSpace(s)
	wild := strings.HasPrefix(s, "*.")
	base := strings.TrimPrefix(s, "*.")
	n, err := rules.NormalizeName(base)
	if err != nil {
		return s
	}
	if wild {
		return "*." + n
	}
	return n
}

var ruleLists = map[string]listKind{
	"local": {
		name: "local", title: "本地域名名单（走本机网络，不经代理）", what: "域名或 *.域名",
		get: func(c *rules.Config) []string { return c.LocalDomains },
		set: func(c *rules.Config, xs []string) { c.LocalDomains = xs },
		key: domainKey,
	},
	"pinned": {
		name: "pinned", title: "追加的置顶域名（永远走主代理；不进解析锁）", what: "域名或 *.域名",
		get: func(c *rules.Config) []string { return c.PinnedExtra },
		set: func(c *rules.Config, xs []string) { c.PinnedExtra = xs },
		key: domainKey,
	},
	"nofake": {
		name: "nofake", title: "不分配假地址的名单（经主代理的 DoH 解析出真实地址，连接仍按规则走；内置的几项删不掉）", what: "域名或 *.域名",
		get: func(c *rules.Config) []string { return c.BypassExtra },
		set: func(c *rules.Config, xs []string) { c.BypassExtra = xs },
		key: domainKey,
	},
	"offline": {
		name: "offline", title: "离线放行名单（停用或闸门关着时仍可直连的应用）", what: "应用",
		get: func(c *rules.Config) []string {
			out := make([]string, 0, len(c.OfflineAllow))
			for _, a := range c.OfflineAllow {
				out = append(out, string(a))
			}
			return out
		},
		set: func(c *rules.Config, xs []string) {
			c.OfflineAllow = nil
			for _, x := range xs {
				c.OfflineAllow = append(c.OfflineAllow, rules.AppID(x))
			}
		},
		key: func(s string) string { return string(rules.NormalizeAppID(rules.AppID(strings.TrimSpace(s)))) },
	},
}

func cmdRulesList(kind string) func(a *app, args []string) int {
	return func(a *app, args []string) int {
		k := ruleLists[kind]
		c, code := a.currentRules()
		if code != ExitOK {
			return code
		}
		items := k.get(c)
		var builtin []string
		if kind == "pinned" {
			if builtin, code = a.builtinPinned(); code != ExitOK {
				return code
			}
		}
		if a.json {
			out := map[string]any{kind: items}
			if kind == "pinned" {
				out["builtin"] = builtin
			}
			b, _ := json.Marshal(out)
			fmt.Fprintln(a.stdout, string(b))
			return ExitOK
		}
		fmt.Fprintln(a.stdout, k.title+"：")
		if kind == "pinned" {
			fmt.Fprintln(a.stdout, "  内置（删不掉，含各自的子域名）："+strings.Join(builtin, "、"))
			fmt.Fprintln(a.stdout, "  你追加的：")
		}
		if len(items) == 0 {
			fmt.Fprintln(a.stdout, "  （没有）")
		}
		for _, x := range items {
			fmt.Fprintln(a.stdout, "  "+x)
		}
		return ExitOK
	}
}

func cmdRulesAdd(kind string) func(a *app, args []string) int {
	return func(a *app, args []string) int {
		k := ruleLists[kind]
		if len(args) == 0 {
			return usageErr(a, fmt.Sprintf("rules %s add <%s>…", kind, k.what))
		}
		var added []string
		_, changed, code := a.modifyRules(func(c *rules.Config) bool {
			cur := k.get(c)
			have := map[string]bool{}
			for _, x := range cur {
				have[k.key(x)] = true
			}
			added = added[:0]
			for _, x := range args {
				kx := k.key(x)
				if have[kx] {
					continue
				}
				have[kx] = true
				cur = append(cur, strings.TrimSpace(x))
				added = append(added, kx)
			}
			if len(added) == 0 {
				return false
			}
			k.set(c, cur)
			return true
		})
		if code != ExitOK {
			return code
		}
		if !changed {
			fmt.Fprintf(a.stdout, "已经在%s里，没有改动\n", k.title)
			return ExitOK
		}
		fmt.Fprintf(a.stdout, "已加进%s：%s\n", k.title, strings.Join(added, "、"))
		return ExitOK
	}
}

func cmdRulesRm(kind string) func(a *app, args []string) int {
	return func(a *app, args []string) int {
		k := ruleLists[kind]
		if len(args) == 0 {
			return usageErr(a, fmt.Sprintf("rules %s rm <%s>…", kind, k.what))
		}
		if kind == "pinned" { // 内置的置顶域名不在追加名单里，后台碰不到这个请求：按状态里的内置名单拦下，不发请求
			builtin, code := a.builtinPinned()
			if code != ExitOK {
				return code
			}
			c, code := a.currentRules()
			if code != ExitOK {
				return code
			}
			extra := map[string]bool{}
			for _, x := range c.PinnedExtra {
				extra[domainKey(x)] = true
			}
			for _, x := range args {
				if kx := domainKey(x); !extra[kx] && slices.Contains(builtin, strings.TrimPrefix(kx, "*.")) {
					fmt.Fprintf(a.stderr, "被拒：%s 是内置的置顶域名，删不掉（只能删你追加的）\n", strings.TrimSpace(x))
					return ExitDenied
				}
			}
		}
		var removed []string
		_, changed, code := a.modifyRules(func(c *rules.Config) bool {
			drop := map[string]bool{}
			for _, x := range args {
				drop[k.key(x)] = true
			}
			removed = removed[:0]
			var keep []string
			for _, x := range k.get(c) {
				if drop[k.key(x)] {
					removed = append(removed, x)
					continue
				}
				keep = append(keep, x)
			}
			if len(removed) == 0 {
				return false
			}
			k.set(c, keep)
			return true
		})
		if code != ExitOK {
			return code
		}
		if !changed {
			fmt.Fprintf(a.stdout, "%s里没有这些项，没有改动\n", k.title)
			return ExitOK
		}
		fmt.Fprintf(a.stdout, "已从%s删除：%s\n", k.title, strings.Join(removed, "、"))
		return ExitOK
	}
}

// builtinPinned 取状态里的内置置顶名单（基础域名；和规则引擎、解析锁同一个来源）。
func (a *app) builtinPinned() ([]string, int) {
	resp, code := a.call("GET", "/v1/status", nil)
	if code != ExitOK {
		return nil, code
	}
	var st struct {
		BuiltinPinned []string `json:"builtin_pinned"`
	}
	if json.Unmarshal(resp.Body, &st) != nil {
		fmt.Fprintln(a.stderr, "读不懂后台服务给的状态")
		return nil, ExitFail
	}
	return st.BuiltinPinned, ExitOK
}

func cmdRulesUDP443(a *app, args []string) int {
	if len(args) != 1 || (args[0] != "on" && args[0] != "off" && args[0] != "show") {
		return usageErr(a, "rules udp443 show|on|off")
	}
	if args[0] == "show" {
		c, code := a.currentRules()
		if code != ExitOK {
			return code
		}
		if a.json {
			b, _ := json.Marshal(map[string]bool{"block_udp_443": c.BlockUDP443})
			fmt.Fprintln(a.stdout, string(b))
			return ExitOK
		}
		fmt.Fprintln(a.stdout, udp443Text(c.BlockUDP443))
		return ExitOK
	}
	want := args[0] == "on"
	_, changed, code := a.modifyRules(func(c *rules.Config) bool {
		if c.BlockUDP443 == want {
			return false
		}
		c.BlockUDP443 = want
		return true
	})
	if code != ExitOK {
		return code
	}
	if !changed {
		fmt.Fprintln(a.stdout, udp443Text(want)+"（本来就是，没有改动）")
		return ExitOK
	}
	fmt.Fprintln(a.stdout, udp443Text(want))
	return ExitOK
}

func udp443Text(on bool) string {
	if on {
		return "拦截 UDP 443：开（QUIC 被拦下，浏览器改用 TCP，域名识别更可靠）"
	}
	return "拦截 UDP 443：关"
}

// ---- 补齐三：“允许局域网” ----

func lanText(on bool) string {
	if on {
		return "允许局域网：开（局域网里的打印机、NAS、投屏、别的电脑的共享照常可用）"
	}
	return "允许局域网：关（局域网里的打印机、NAS、投屏、别的电脑的共享都连不上）"
}

func cmdLANShow(a *app, args []string) int {
	resp, code := a.call("GET", "/v1/settings/lan", nil)
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() {
		var v struct {
			AllowLAN bool `json:"allow_lan"`
		}
		_ = json.Unmarshal(resp.Body, &v)
		fmt.Fprintln(a.stdout, lanText(v.AllowLAN))
	})
}

func cmdLANSet(on bool) func(a *app, args []string) int {
	return func(a *app, args []string) int {
		if len(args) != 0 {
			return usageErr(a, "lan on|off 不带参数")
		}
		resp, code := a.call("PUT", "/v1/settings/lan", map[string]bool{"allow_lan": on})
		if code != ExitOK {
			return code
		}
		return a.out(resp, func() { fmt.Fprintln(a.stdout, lanText(on)) })
	}
}
