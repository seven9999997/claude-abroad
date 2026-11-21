package directdial

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// 解析的约束（m3-plan v2.5 补充 2）。
const (
	queryTimeout = 3 * time.Second
	maxCNAME     = 8
	maxAddrs     = 16
	maxDNSMsg    = 64 << 10
)

// BoundDialer 是直连唯一的出口：一律绑物理网卡（usernet.Dialer.DialDirect）。解析查询也经它发出。
type BoundDialer interface {
	DialDirect(ctx context.Context, network string, dst netip.AddrPort) (net.Conn, error)
}

// resolver 用启用前记下的本机 DNS 服务器解析 A 记录，经 BoundDialer 发出。不缓存。
type resolver struct {
	dial    BoundDialer
	servers []netip.Addr
	newID   func() uint16
}

var errNoAnswer = errors.New("没有可用的 A 记录")

// lookupA 依次问每个服务器，返回第一个成功的结果（最多 16 个地址），以及别名链上出现过的名字
// （不含最初的名字；服务端要逐个判置顶）。
func (r *resolver) lookupA(ctx context.Context, host string) ([]netip.Addr, []string, error) {
	if len(r.servers) == 0 {
		return nil, nil, errors.New("没有记下的本机 DNS 服务器")
	}
	var last error = errNoAnswer
	for _, s := range r.servers {
		if err := checkDNSServer(s); err != nil {
			return nil, nil, err
		}
	}
	for _, s := range r.servers {
		addrs, chain, err := r.query(ctx, s, host)
		if err == nil {
			return addrs, chain, nil
		}
		last = err
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
	}
	return nil, nil, last
}

func (r *resolver) query(ctx context.Context, server netip.Addr, host string) ([]netip.Addr, []string, error) {
	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(host), dns.TypeA)
	q.RecursionDesired = true
	if r.newID != nil {
		q.Id = r.newID()
	}
	resp, err := r.exchange(ctx, "udp", server, q)
	if err == nil && resp.Truncated {
		resp, err = r.exchange(ctx, "tcp", server, q) // 截断：改用 TCP 重查
	}
	if err != nil {
		return nil, nil, err
	}
	return answerA(q, resp)
}

// exchange 发一次查询，校验事务号和问题段，不符的应答丢弃（UDP 继续等，直到超时）。
func (r *resolver) exchange(ctx context.Context, network string, server netip.Addr, q *dns.Msg) (*dns.Msg, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	c, err := r.dial.DialDirect(ctx, network, netip.AddrPortFrom(server, 53))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if d, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(d)
	}
	out, err := q.Pack()
	if err != nil {
		return nil, err
	}
	if network == "tcp" {
		if _, err := c.Write(binary.BigEndian.AppendUint16(nil, uint16(len(out)))); err != nil {
			return nil, err
		}
	}
	if _, err := c.Write(out); err != nil {
		return nil, err
	}
	for {
		var buf []byte
		if network == "tcp" {
			var l [2]byte
			if _, err := io.ReadFull(c, l[:]); err != nil {
				return nil, err
			}
			buf = make([]byte, binary.BigEndian.Uint16(l[:]))
			if _, err := io.ReadFull(c, buf); err != nil {
				return nil, err
			}
		} else {
			buf = make([]byte, maxDNSMsg)
			n, err := c.Read(buf)
			if err != nil {
				return nil, err
			}
			buf = buf[:n]
		}
		m := new(dns.Msg)
		if err := m.Unpack(buf); err != nil || !matches(q, m) {
			if network == "tcp" {
				return nil, errors.New("DNS 应答与请求不符")
			}
			continue // UDP：丢掉不符的应答，继续等
		}
		return m, nil
	}
}

// matches 校验事务号、应答标志和问题段。
func matches(q, m *dns.Msg) bool {
	if m.Id != q.Id || !m.Response || len(m.Question) != 1 {
		return false
	}
	a, b := m.Question[0], q.Question[0]
	return strings.EqualFold(a.Name, b.Name) && a.Qtype == b.Qtype && a.Qclass == b.Qclass
}

// answerA 从应答里取 A 记录：从问题名开始，跟着同一应答里的 CNAME 走，最多 8 级；最多 16 个地址。
// 同时返回别名链上出现过的名字（不含问题名，去掉末尾的点）。
func answerA(q, m *dns.Msg) ([]netip.Addr, []string, error) {
	if m.Rcode != dns.RcodeSuccess {
		return nil, nil, errors.New("DNS 应答出错")
	}
	name := strings.ToLower(q.Question[0].Name)
	var chain []string
	for hop := 0; hop <= maxCNAME; hop++ {
		var addrs []netip.Addr
		next := ""
		for _, rr := range m.Answer {
			h := rr.Header()
			if !strings.EqualFold(h.Name, name) || h.Class != dns.ClassINET {
				continue
			}
			switch v := rr.(type) {
			case *dns.A:
				if a, ok := netip.AddrFromSlice(v.A.To4()); ok && len(addrs) < maxAddrs {
					addrs = append(addrs, a)
				}
			case *dns.CNAME:
				next = strings.ToLower(v.Target)
			}
		}
		if len(addrs) > 0 {
			return addrs, chain, nil
		}
		if next == "" {
			return nil, chain, errNoAnswer
		}
		name = next
		chain = append(chain, strings.TrimSuffix(next, "."))
	}
	return nil, chain, errors.New("别名链太长")
}
