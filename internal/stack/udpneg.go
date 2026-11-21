package stack

import (
	"sync"
	"time"

	"claude-abroad/internal/rules"
)

// udpNeg 是“出口不支持 UDP”的负缓存（m3-plan M3-5）：出口回 udp_unsupported 后，TTL 内的新 UDP 流
// 按不支持处理（规则引擎的阶段 3 据此回退主代理或拦截）。本条流不重试、不换出口。
type udpNeg struct {
	mu    sync.Mutex
	until map[string]time.Time
}

func (n *udpNeg) add(exit string, until time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.until == nil {
		n.until = map[string]time.Time{}
	}
	n.until[exit] = until
}

func (n *udpNeg) has(exit string, now time.Time) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	u, ok := n.until[exit]
	if ok && !now.Before(u) {
		delete(n.until, exit)
		return false
	}
	return ok
}

// udpOK 给 rules.Env.UDPOK：没有被负缓存的出口按支持 UDP 处理；分流出口另看 ExitUDP（M4-8）。
func (p *Proxy) udpOK(exit string) bool {
	if p.neg.has(exit, p.now()) {
		return false
	}
	if exit != rules.ExitMain && p.ExitUDP != nil && !p.ExitUDP(exit) {
		return false
	}
	return true
}

func (p *Proxy) markUDPUnsupported(exit string) {
	ttl := p.UDPNegTTL
	if ttl <= 0 {
		ttl = DefaultUDPNegTTL
	}
	p.neg.add(exit, p.now().Add(ttl))
}

// UDPUnsupported 报告出口最近是否明确回过 udp_unsupported（环境检测第 08 项用：主代理不支持 UDP 时 STUN 全部无响应算通过）。
func (p *Proxy) UDPUnsupported(exit string) bool { return p.neg.has(exit, p.now()) }
