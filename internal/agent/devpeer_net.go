//go:build abroaddev

package agent

import (
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"

	"golang.org/x/net/dns/dnsmessage"
)

// 开发用对端（M4-7 的虚拟机端到端）里的两个小服务：带标记的回显、会计数的 DNS。只在开发标记打开时用。

// tagEcho 在一个地址上起 TCP、UDP 回显：回 “<标记>:” 加原样数据，用来判断流量从哪条隧道到达。
type tagEcho struct {
	tag string
	ln  net.Listener
	pc  net.PacketConn
	n   atomic.Int64
}

func listenTagEcho(addr netip.AddrPort, tag string) (*tagEcho, error) {
	ln, err := net.Listen("tcp4", addr.String())
	if err != nil {
		return nil, err
	}
	pc, err := net.ListenPacket("udp4", addr.String())
	if err != nil {
		ln.Close()
		return nil, err
	}
	e := &tagEcho{tag: tag, ln: ln, pc: pc}
	go e.serveTCP()
	go e.serveUDP()
	return e, nil
}

func (e *tagEcho) serveTCP() {
	for {
		c, err := e.ln.Accept()
		if err != nil {
			return
		}
		e.n.Add(1)
		go func() {
			defer c.Close()
			if _, err := io.WriteString(c, e.tag+":"); err != nil {
				return
			}
			_, _ = io.Copy(c, c)
		}()
	}
}

func (e *tagEcho) serveUDP() {
	b := make([]byte, 2048)
	for {
		n, a, err := e.pc.ReadFrom(b)
		if err != nil {
			return
		}
		e.n.Add(1)
		_, _ = e.pc.WriteTo(append([]byte(e.tag+":"), b[:n]...), a)
	}
}

func (e *tagEcho) Close() { e.ln.Close(); e.pc.Close() }

// countDNS 在一个地址上答 A 查询：任何名字都答成自己监听的地址（这样经哪条隧道解析，就连到那条隧道的回显）。
type countDNS struct {
	addr netip.Addr
	pc   net.PacketConn
	n    atomic.Int64
}

func listenCountDNS(addr netip.AddrPort) (*countDNS, error) {
	pc, err := net.ListenPacket("udp4", addr.String())
	if err != nil {
		return nil, err
	}
	d := &countDNS{addr: addr.Addr(), pc: pc}
	go d.serve()
	return d, nil
}

func (d *countDNS) serve() {
	b := make([]byte, 1500)
	for {
		n, a, err := d.pc.ReadFrom(b)
		if err != nil {
			return
		}
		d.n.Add(1)
		if resp, err := dnsAnswerA(b[:n], d.addr); err == nil {
			_, _ = d.pc.WriteTo(resp, a)
		}
	}
}

func (d *countDNS) Close() { d.pc.Close() }

// dnsAnswerA 对 A 查询回一条 A 记录（TTL 5）；其他类型回空答案。
func dnsAnswerA(q []byte, a netip.Addr) ([]byte, error) {
	var p dnsmessage.Parser
	h, err := p.Start(q)
	if err != nil {
		return nil, err
	}
	qs, err := p.AllQuestions()
	if err != nil || len(qs) != 1 {
		return nil, errors.New("只答单个问题")
	}
	bld := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, Authoritative: true, RecursionDesired: h.RecursionDesired, RecursionAvailable: true})
	bld.EnableCompression()
	if err := bld.StartQuestions(); err != nil {
		return nil, err
	}
	if err := bld.Question(qs[0]); err != nil {
		return nil, err
	}
	if err := bld.StartAnswers(); err != nil {
		return nil, err
	}
	if qs[0].Type == dnsmessage.TypeA && a.Is4() {
		if err := bld.AResource(dnsmessage.ResourceHeader{Name: qs[0].Name, Class: dnsmessage.ClassINET, TTL: 5},
			dnsmessage.AResource{A: a.As4()}); err != nil {
			return nil, err
		}
	}
	return bld.Finish()
}

// devPeerServices 是对端命名空间里起的全部小服务（按地址）。
type devPeerServices struct {
	mu    sync.Mutex
	echo  map[string]*tagEcho  // 标记 → 回显
	dns   map[string]*countDNS // 标记 → DNS
	stops []func()
}

// addAt 在指定端口上起一个带标记的回显（只用来计数到达的连接，如冒充 DoH 的 1.1.1.1:443）。
func (s *devPeerServices) addAt(tag string, ap netip.AddrPort) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.echo == nil {
		s.echo, s.dns = map[string]*tagEcho{}, map[string]*countDNS{}
	}
	e, err := listenTagEcho(ap, tag)
	if err != nil {
		return err
	}
	s.echo[tag] = e
	s.stops = append(s.stops, e.Close)
	return nil
}

func (s *devPeerServices) add(tag string, ip netip.Addr, withDNS bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.echo == nil {
		s.echo, s.dns = map[string]*tagEcho{}, map[string]*countDNS{}
	}
	e, err := listenTagEcho(netip.AddrPortFrom(ip, echoPort), tag)
	if err != nil {
		return err
	}
	s.echo[tag] = e
	s.stops = append(s.stops, e.Close)
	if withDNS {
		d, err := listenCountDNS(netip.AddrPortFrom(ip, 53))
		if err != nil {
			return err
		}
		s.dns[tag] = d
		s.stops = append(s.stops, d.Close)
	}
	return nil
}

func (s *devPeerServices) counts() (echo, dns map[string]int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	echo, dns = map[string]int64{}, map[string]int64{}
	for t, e := range s.echo {
		echo[t] = e.n.Load()
	}
	for t, d := range s.dns {
		dns[t] = d.n.Load()
	}
	return echo, dns
}

func (s *devPeerServices) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.stops {
		f()
	}
	s.stops, s.echo, s.dns = nil, nil, nil
}
