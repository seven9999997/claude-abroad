package daemon

import (
	"sync"
	"time"

	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/rules"
)

// 主代理的 UDP 是否可用（补齐二，architect 2026-11-27）：只读出出口检查里已有的 UDP 探测结果（exitcheck.UDPAvailability），
// 记在“当时下发到虚拟机的那个节点”名下；状态给当前节点的，节点列表给每个节点最近一次的。行为一行不动：UDP 怎么处理、
// 回退去哪里，都和以前一样。

type udpRec struct {
	State string
	At    time.Time
}

type mainUDP struct {
	mu     sync.Mutex
	pushed string            // 现在下发到虚拟机的节点 ID（下发成功后设、回滚后改回；启动时取“已下发”记录）
	byNode map[string]udpRec // 每个节点最近一次的结果
}

// setPushedMain 记下现在下发到虚拟机的节点。
func (d *Daemon) setPushedMain(node string) {
	d.mudp.mu.Lock()
	d.mudp.pushed = node
	d.mudp.mu.Unlock()
}

// recordMainReport 收到主代理的一轮检查报告：记在当时下发的节点名下；当前节点由“不是不可用”变成“不可用”时记一条事件。
func (d *Daemon) recordMainReport(rep exitcheck.Report) {
	if rep.Exit != "main" {
		return
	}
	st := rep.UDPAvailability()
	d.mudp.mu.Lock()
	node := d.mudp.pushed
	if node == "" {
		d.mudp.mu.Unlock()
		return
	}
	if d.mudp.byNode == nil {
		d.mudp.byNode = map[string]udpRec{}
	}
	prev := d.mudp.byNode[node].State
	d.mudp.byNode[node] = udpRec{State: st, At: rep.At}
	d.mudp.mu.Unlock()
	if st == exitcheck.UDPUnavailable && prev != exitcheck.UDPUnavailable {
		d.events.Add("main_proxy", "udp_unavailable", MainUDPUnavailableNote)
	}
}

// MainUDPUnavailableNote 是主代理的 UDP 不可用时的说明（事件、状态用；不含节点地址）。
const MainUDPUnavailableNote = "主代理的 UDP 不可用：语音、视频通话和部分游戏会不通；换一个支持 UDP 的节点可以恢复"

// mainUDPState 是当前下发的节点的 UDP 结果（没有下发的节点时为空；没检查过时“还不知道”）。
func (d *Daemon) mainUDPState() string {
	d.mudp.mu.Lock()
	defer d.mudp.mu.Unlock()
	if d.mudp.pushed == "" {
		return ""
	}
	if r, ok := d.mudp.byNode[d.mudp.pushed]; ok {
		return r.State
	}
	return exitcheck.UDPUnknown
}

// nodeUDP 是一个节点最近一次的 UDP 结果（没检查过时“还不知道”）。
func (d *Daemon) nodeUDP(node string) string {
	d.mudp.mu.Lock()
	defer d.mudp.mu.Unlock()
	if r, ok := d.mudp.byNode[node]; ok {
		return r.State
	}
	return exitcheck.UDPUnknown
}

// RuleFallback 是一条正在回退的规则（补齐二）：规则指向的分流出口现在不可用，命中它的流量改走主代理（不是本机网络）。
type RuleFallback struct {
	App    string `json:"app"`
	Exit   string `json:"exit"`
	Reason string `json:"reason"` // 出口为什么不可用（不含地址）
}

// ruleFallbacks 列出正在回退的规则：动作是 proxy:<分流出口> 且这个出口现在不可用。指向主代理、本机网络、阻止的规则永远不在里面。
func (d *Daemon) ruleFallbacks() []RuleFallback {
	var out []RuleFallback
	for _, r := range d.currentRules().Rules {
		if r.Action.Kind != rules.Proxy || r.Action.Exit == "" || r.Action.Exit == "main" {
			continue
		}
		if v := d.exitAvail.View(r.Action.Exit); !v.Up {
			out = append(out, RuleFallback{App: sanitizeApp(string(r.App)), Exit: r.Action.Exit, Reason: v.Reason})
		}
	}
	return out
}

// affectedRules 是指向某个出口的规则（应用 ID）。
func (d *Daemon) affectedRules(exit string) []string {
	var out []string
	for _, r := range d.currentRules().Rules {
		if r.Action.Kind == rules.Proxy && r.Action.Exit == exit {
			out = append(out, sanitizeApp(string(r.App)))
		}
	}
	return out
}
