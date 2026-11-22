package stack

import (
	"sort"

	"claude-abroad/internal/rules"
)

// AppFlows 是一个应用当前的在途连接汇总（M5-5，m5-plan v2.3 决定 2）：不含目的地址、域名。
type AppFlows struct {
	App    rules.AppID `json:"app"`
	Conns  int         `json:"conns"`
	Exits  []string    `json:"exits"`  // 正在使用的去向：main、分流出口的标签、direct、block
	Reason string      `json:"reason"` // 最近一次判定的理由类别（rules.Reason）
}

// Apps 按应用汇总已经判定的在途流（还在判定的不算），按连接数从多到少，最多 max 个；truncated 表示还有更多。
// 只读登记表，不另外扫描系统。
func (p *Proxy) Apps(max int) (out []AppFlows, truncated bool) {
	type agg struct {
		n     int
		exits map[string]bool
		last  rules.Reason
	}
	m := map[rules.AppID]*agg{}
	p.mu.Lock()
	for e := range p.flows {
		if !e.decided {
			continue
		}
		a := m[e.who.App]
		if a == nil {
			a = &agg{exits: map[string]bool{}}
			m[e.who.App] = a
		}
		a.n++
		a.exits[exitName(e.action)] = true
		a.last = e.reason
	}
	p.mu.Unlock()
	for app, a := range m {
		var ex []string
		for k := range a.exits {
			ex = append(ex, k)
		}
		sort.Strings(ex)
		out = append(out, AppFlows{App: app, Conns: a.n, Exits: ex, Reason: string(a.last)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Conns != out[j].Conns {
			return out[i].Conns > out[j].Conns
		}
		return out[i].App < out[j].App
	})
	if max > 0 && len(out) > max {
		return out[:max], true
	}
	return out, false
}

func exitName(a rules.Action) string {
	switch a.Kind {
	case rules.Direct:
		return "direct"
	case rules.Block:
		return "block"
	case rules.Proxy:
		return a.Exit
	}
	return "unknown"
}
