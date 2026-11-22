package api

import (
	"slices"

	"claude-abroad/internal/rules"
)

// RulesLevel 是改规则的按内容分级（m5-plan v2 第 3.2 节、v2.1 决定 3）。原则一句话：**新规则下，如果存在某条连接
// 比旧规则下更可能走本机网络，或某项保护被关掉，就是敏感**。具体（任一条成立即敏感，返回全部理由）：
//   - 某个应用的规则从“不是直连”变成“直连”，或默认动作变成直连；
//   - 往 local_domains 里加了模式（启用状态下本地域名不看应用规则、一律直连）；
//   - 从 pinned_extra 里删了模式（删掉后到它的直连不再被收紧）；
//   - 往 offline_allow 里加了应用（停用后它可以直连）；
//   - 改动了 Claude 应用的规则（任何方向）；
//   - block_udp_443 从开变成关；
//   - 解除了某个应用的拦截规则，而新规则里有本地域名，或这个应用在停用名单里（拦截优先于本地域名和停用名单，
//     解除之后它到本地域名、或停用后，会直连；性质测试找出的情况）。
//
// 反方向（从 local_domains 删、往 pinned_extra 加、从 offline_allow 删、打开 block_udp_443、把直连改成别的）是收紧，
// 加删 proxy/block 规则、改默认出口为分流出口都是普通。old 为 nil 时按默认配置比较。
func RulesLevel(old, next *rules.Config) (Level, []string) {
	if old == nil {
		old = rules.DefaultConfig()
	}
	if next == nil {
		next = rules.DefaultConfig()
	}
	var why []string
	ruleOf := func(c *rules.Config, app rules.AppID) (rules.Action, bool) {
		for _, r := range c.Rules {
			if r.App == app {
				return r.Action, true
			}
		}
		return rules.Action{}, false
	}
	// 某个应用的有效动作（有规则用规则，没有用默认）。
	eff := func(c *rules.Config, app rules.AppID) rules.Action {
		if a, ok := ruleOf(c, app); ok {
			return a
		}
		return c.Default
	}
	if next.Default.Kind == rules.Direct && old.Default.Kind != rules.Direct {
		why = append(why, "默认动作改成了本机网络")
	}
	apps := map[rules.AppID]bool{}
	for _, r := range old.Rules {
		apps[r.App] = true
	}
	for _, r := range next.Rules {
		apps[r.App] = true
	}
	toDirect, claude, unblock := false, false, false
	for app := range apps {
		oa0, _ := ruleOf(old, app)
		na0, nh0 := ruleOf(next, app)
		if oa0.Kind == rules.Block && !(nh0 && na0.Kind == rules.Block) &&
			(len(next.LocalDomains) > 0 || slices.Contains(next.OfflineAllow, app)) {
			unblock = true
		}
		o, n := eff(old, app), eff(next, app)
		if n.Kind == rules.Direct && o.Kind != rules.Direct {
			toDirect = true
		}
		oa, oh := ruleOf(old, app)
		na, nh := ruleOf(next, app)
		if rules.IsClaudeApp(app) && (oh != nh || oa != na) {
			claude = true
		}
	}
	if toDirect {
		why = append(why, "有应用改成了本机网络")
	}
	if claude {
		why = append(why, "改动了 Claude 应用的规则")
	}
	if unblock {
		why = append(why, "解除了拦截，这个应用可能经本地域名或停用名单直连")
	}
	for _, p := range next.LocalDomains {
		if !slices.Contains(old.LocalDomains, p) {
			why = append(why, "本地域名名单里加了模式")
			break
		}
	}
	for _, p := range old.PinnedExtra {
		if !slices.Contains(next.PinnedExtra, p) {
			why = append(why, "置顶域名的追加名单里删了模式")
			break
		}
	}
	for _, a := range next.OfflineAllow {
		if !slices.Contains(old.OfflineAllow, a) {
			why = append(why, "停用后仍可直连的名单里加了应用")
			break
		}
	}
	if old.BlockUDP443 && !next.BlockUDP443 {
		why = append(why, "关掉了拦截 UDP 443")
	}
	if len(why) > 0 {
		return Sensitive, why
	}
	return Normal, nil
}
