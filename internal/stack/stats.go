package stack

import (
	"errors"
	"sync"
	"sync/atomic"

	"claude-abroad/internal/directdial"
	"claude-abroad/internal/rules"
	"claude-abroad/internal/wire"
)

type proxyStats struct {
	noMapping, badDst, gateClosed                                  atomic.Int64
	unknownExit, exitDown, dialFailed, udpUnsupported, openOther   atomic.Int64
	zeroEOF, dnsTCPRejected, dnsUDPDropped, tcpProxied, udpProxied atomic.Int64
	dropped, directDropped, directOK, udpPendingDropped, rechecked atomic.Int64
	probeProxied, probeRefused                                     atomic.Int64 // 探测窗口（M4-6a）

	reasons    sync.Map // rules.Reason → *atomic.Int64
	directFail sync.Map // 直连失败的应答码 → *atomic.Int64
	sniffs     sync.Map // 首包识别结果 → *atomic.Int64
}

// ProxyStats 是流处理器的计数（不含域名、地址、应用）。
type ProxyStats struct {
	NoMapping      int64 `json:"no_mapping"`
	BadDst         int64 `json:"bad_dst"`
	GateClosed     int64 `json:"gate_closed"`
	UnknownExit    int64 `json:"unknown_exit"`
	ExitDown       int64 `json:"exit_down"`
	DialFailed     int64 `json:"dial_failed"`
	UDPUnsupported int64 `json:"udp_unsupported"`
	OpenOther      int64 `json:"open_failed_other"`
	ZeroEOF        int64 `json:"zero_eof"` // 走代理时 ok 之后上游一个字节都没给就结束（只用于诊断，不参与任何判定）
	DNSTCPRejected int64 `json:"dns_tcp_rejected"`
	DNSUDPDropped  int64 `json:"dns_udp_dropped"`
	TCPProxied     int64 `json:"tcp_proxied"`
	ProbeProxied   int64 `json:"probe_proxied"` // 探测窗口放行的（abroadd 自己的探测，固定走主代理）
	ProbeRefused   int64 `json:"probe_refused"` // 窗口打开时识别不到或不是自己、照常处理的（只计数）
	UDPProxied     int64 `json:"udp_proxied"`
	Dropped        int64 `json:"dropped"` // DropAll 断开的流

	DirectOK          int64            `json:"direct_ok"`
	DirectFail        map[string]int64 `json:"direct_fail"` // 按直连拨号模块的应答码（other 为连接、身份等其他错误）
	DirectDropped     int64            `json:"direct_dropped"`
	Reasons           map[string]int64 `json:"reasons"` // 按 rules.Reason
	Sniff             map[string]int64 `json:"sniff"`   // 首包识别的各种结果
	UDPPendingDropped int64            `json:"udp_pending_dropped"`
	Rechecked         int64            `json:"rechecked_dropped"` // 状态变化或规则修改时重判后断开的流
}

func bump(m *sync.Map, k string) {
	v, _ := m.LoadOrStore(k, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}

func snapshot(m *sync.Map) map[string]int64 {
	out := map[string]int64{}
	m.Range(func(k, v any) bool {
		out[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	return out
}

// Stats 返回计数快照。
func (p *Proxy) Stats() ProxyStats {
	s := &p.st
	return ProxyStats{NoMapping: s.noMapping.Load(), BadDst: s.badDst.Load(), GateClosed: s.gateClosed.Load(),
		UnknownExit: s.unknownExit.Load(), ExitDown: s.exitDown.Load(), DialFailed: s.dialFailed.Load(),
		UDPUnsupported: s.udpUnsupported.Load(), OpenOther: s.openOther.Load(), ZeroEOF: s.zeroEOF.Load(),
		DNSTCPRejected: s.dnsTCPRejected.Load(), DNSUDPDropped: s.dnsUDPDropped.Load(),
		TCPProxied: s.tcpProxied.Load(), UDPProxied: s.udpProxied.Load(), Dropped: s.dropped.Load(),
		ProbeProxied: s.probeProxied.Load(), ProbeRefused: s.probeRefused.Load(),
		DirectOK: s.directOK.Load(), DirectFail: snapshot(&s.directFail), DirectDropped: s.directDropped.Load(),
		Reasons: snapshot(&s.reasons), Sniff: snapshot(&s.sniffs), UDPPendingDropped: s.udpPendingDropped.Load(),
		Rechecked: s.rechecked.Load()}
}

func (p *Proxy) countReason(r rules.Reason) { bump(&p.st.reasons, string(r)) }

var sniffNames = map[rules.SniffKind]string{
	rules.NotTLS: "not_tls", rules.TLSName: "tls_name", rules.TLSNoName: "tls_noname", rules.TLSECH: "tls_ech",
	rules.QUICName: "quic_name", rules.QUICNoName: "quic_noname", rules.HTTPHost: "http_host", rules.Timeout: "timeout",
}

func (p *Proxy) countSniff(k rules.SniffKind) {
	n, ok := sniffNames[k]
	if !ok {
		n = "other"
	}
	bump(&p.st.sniffs, n)
}

func (p *Proxy) countDirectFail(err error) {
	var ce *directdial.CodeError
	if errors.As(err, &ce) {
		bump(&p.st.directFail, ce.Code)
		return
	}
	bump(&p.st.directFail, "other")
}

// countOpenErr 按错误码计数。
func (p *Proxy) countOpenErr(err error) {
	var re *wire.ReplyError
	if errors.As(err, &re) {
		switch re.Code {
		case wire.CodeUnknownExit:
			p.st.unknownExit.Add(1)
			return
		case wire.CodeExitDown:
			p.st.exitDown.Add(1)
			return
		case wire.CodeDialFailed:
			p.st.dialFailed.Add(1)
			return
		case wire.CodeUDPUnsupported:
			p.st.udpUnsupported.Add(1)
			return
		}
	}
	p.st.openOther.Add(1)
}
