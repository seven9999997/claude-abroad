package usernet

import (
	"context"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
)

// MaxGatewayAnswers 是网关 DNS 一次应答里最多的 A 记录数。
const MaxGatewayAnswers = 16

// GatewayDNS 是收紧模式下虚拟机网关（192.168.127.1:53）的 DNS（设计 4.5，M2-1a）：
//   - 只处理单问题、IN 类的标准查询，其余 REFUSED；
//   - 只应答登记过的第一跳域名：A 查询经 Resolve 解析，只留公网 IPv4（最多 MaxGatewayAnswers 条），
//     全被滤掉或解析失败回 SERVFAIL；答出的地址交给 Outbound.Learn 放行；其他类型回空的 NOERROR；
//   - 没登记的域名一律 REFUSED。
//
// 计数不含域名。
type GatewayDNS struct {
	Out     *Outbound
	Resolve func(ctx context.Context, name string) ([]netip.Addr, time.Duration, error)
	// OnFailed 在登记过的域名的 A 查询回 SERVFAIL 时调用（解析失败或没有可用的公网地址；诊断用，可为 nil）。
	OnFailed func(name string)

	answered, refused, failed, hopPrivate atomic.Int64
}

// DNSCounters 是网关 DNS 的计数。
type DNSCounters struct {
	Answered int64 `json:"dns_answered"`
	Refused  int64 `json:"dns_refused"`
	// Failed 是登记域名的 A 查询回 SERVFAIL 的次数（解析失败或没有公网地址）。
	Failed int64 `json:"dns_failed"`
	// HopPrivate 是第一跳域名的答案全是私有地址、因此被拒的次数（M5-4：给用户“局域网里的代理请直接填 IP 地址”的提示）。
	HopPrivate int64 `json:"dns_hop_private"`
}

// Counters 返回计数快照。
func (g *GatewayDNS) Counters() DNSCounters {
	return DNSCounters{Answered: g.answered.Load(), Refused: g.refused.Load(), Failed: g.failed.Load(), HopPrivate: g.hopPrivate.Load()}
}

func reply(q *dns.Msg, rcode int) *dns.Msg {
	m := new(dns.Msg)
	m.SetRcode(q, rcode)
	m.RecursionAvailable = false
	return m
}

// Exchange 满足 gvisor-tap-vsock 的 DNSExchange。
func (g *GatewayDNS) Exchange(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
	if q.Opcode != dns.OpcodeQuery || len(q.Question) != 1 || q.Question[0].Qclass != dns.ClassINET {
		g.refused.Add(1)
		return reply(q, dns.RcodeRefused), nil
	}
	qs := q.Question[0]
	name := strings.ToLower(strings.TrimSuffix(qs.Name, "."))
	if g.Out == nil || !g.Out.Registered(name) {
		g.refused.Add(1)
		return reply(q, dns.RcodeRefused), nil
	}
	if qs.Qtype != dns.TypeA {
		return reply(q, dns.RcodeSuccess), nil
	}
	if g.Resolve == nil {
		g.fail(name)
		return reply(q, dns.RcodeServerFailure), nil
	}
	addrs, ttl, err := g.Resolve(ctx, name)
	if err != nil {
		g.fail(name)
		return reply(q, dns.RcodeServerFailure), nil
	}
	var keep []netip.Addr
	for _, a := range addrs {
		if g.Out.Keep(name, a) && len(keep) < MaxGatewayAnswers {
			keep = append(keep, a.Unmap())
		}
	}
	if len(keep) == 0 {
		if len(addrs) > 0 && allPrivate(addrs) && g.Out.IsMainHopDomain(name) {
			g.hopPrivate.Add(1)
		}
		g.fail(name)
		return reply(q, dns.RcodeServerFailure), nil
	}
	g.Out.Learn(name, keep)
	m := reply(q, dns.RcodeSuccess)
	secs := uint32(max(ttl, time.Second) / time.Second)
	for _, a := range keep {
		m.Answer = append(m.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: qs.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: secs},
			A:   a.AsSlice(),
		})
	}
	g.answered.Add(1)
	return m, nil
}

// fail 计一次 SERVFAIL 并调用 OnFailed。
func (g *GatewayDNS) fail(name string) {
	g.failed.Add(1)
	if g.OnFailed != nil {
		g.OnFailed(name)
	}
}

// allPrivate 报告 addrs 是否全是局域网 IPv4（10/8、172.16/12、192.168/16、100.64/10）。
func allPrivate(addrs []netip.Addr) bool {
	for _, a := range addrs {
		a = a.Unmap()
		in := false
		for _, p := range mainLANPrefixes {
			if p.Contains(a) {
				in = true
			}
		}
		if !in {
			return false
		}
	}
	return true
}
