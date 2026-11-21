package exitcheck

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"

	"claude-abroad/internal/exitpolicy"
	"claude-abroad/internal/wire"
)

// IPSource 是取出口 IP 的源（轻量：每轮复查都用）。
type IPSource interface {
	Name() string
	URL() string
	Parse(body []byte) (exitpolicy.IPInfo, error)
}

// GeoSource 是按 IP 查位置的源（只在出口 IP 与缓存不同时用）。
type GeoSource interface {
	Name() string
	URL(ip netip.Addr) string
	Parse(body []byte, ip netip.Addr) (exitpolicy.Geo, error)
}

type cfTrace struct{ host string }

func (s cfTrace) Name() string { return "cloudflare-trace" }
func (s cfTrace) URL() string  { return "https://" + s.host + "/cdn-cgi/trace" }
func (s cfTrace) Parse(b []byte) (exitpolicy.IPInfo, error) {
	return exitpolicy.ParseCloudflareTrace(b)
}

type ipwhoSelf struct{}

func (ipwhoSelf) Name() string { return "ipwho.is" }
func (ipwhoSelf) URL() string  { return "https://ipwho.is/?fields=success,ip,country_code" }
func (ipwhoSelf) Parse(b []byte) (exitpolicy.IPInfo, error) {
	g, err := exitpolicy.ParseIPWhoIs(b, netip.Addr{})
	return exitpolicy.IPInfo{IP: g.IP, Country: g.Country}, err
}

type ipsbSelf struct{}

func (ipsbSelf) Name() string { return "ip.sb" }
func (ipsbSelf) URL() string  { return "https://api.ip.sb/geoip" }
func (ipsbSelf) Parse(b []byte) (exitpolicy.IPInfo, error) {
	g, err := exitpolicy.ParseIPSB(b, netip.Addr{})
	return exitpolicy.IPInfo{IP: g.IP, Country: g.Country}, err
}

type ipwhoGeo struct{}

func (ipwhoGeo) Name() string { return "ipwho.is" }
func (ipwhoGeo) URL(ip netip.Addr) string {
	return "https://ipwho.is/" + ip.String() + "?fields=success,ip,country,country_code,region,region_code,city,timezone.id,connection.asn,connection.org,connection.isp"
}
func (ipwhoGeo) Parse(b []byte, ip netip.Addr) (exitpolicy.Geo, error) {
	return exitpolicy.ParseIPWhoIs(b, ip)
}

type ipsbGeo struct{}

func (ipsbGeo) Name() string             { return "ip.sb" }
func (ipsbGeo) URL(ip netip.Addr) string { return "https://api.ip.sb/geoip/" + ip.String() }
func (ipsbGeo) Parse(b []byte, ip netip.Addr) (exitpolicy.Geo, error) {
	return exitpolicy.ParseIPSB(b, ip)
}

// DefaultIPSources、DefaultGeoSources 是默认的查询源（按顺序换源）。
func DefaultIPSources() []IPSource {
	return []IPSource{cfTrace{"www.cloudflare.com"}, ipwhoSelf{}, ipsbSelf{}}
}
func DefaultGeoSources() []GeoSource { return []GeoSource{ipwhoGeo{}, ipsbGeo{}} }

// fetch 经客户端 GET 一个查询源，限 64 KiB。429、5xx 是限流（无法判断）；其他非 200 是格式不对（无法判断）。
func fetch(ctx context.Context, hc *client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fail(CauseBadFormat, "查询地址无效")
	}
	req.Header.Set("User-Agent", exitpolicy.BrowserUA)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return nil, fail(CauseRateLimited, "查询源限流或出错（HTTP "+strconv.Itoa(resp.StatusCode)+"）")
	case resp.StatusCode != http.StatusOK:
		return nil, fail(CauseBadFormat, "查询源返回 HTTP "+strconv.Itoa(resp.StatusCode))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	return b, nil
}

// probeHTTPS 以收到预期的 HTTP 应答（204）为准：建连、回应帧 ok 都不算数。
// 回了别的状态（被劫持到网页、重定向）、证书不对 → 收到了不是预期的应答（不通过）；超时、连不上 → 无法判断。
func probeHTTPS(ctx context.Context, hc *client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fail(CauseBadFormat, "探测地址无效")
	}
	req.Header.Set("User-Agent", exitpolicy.BrowserUA)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusNoContent {
		return fail(CauseUnexpectedReply, fmt.Sprintf("探测地址返回 HTTP %d，应为 204", resp.StatusCode))
	}
	return nil
}

// dnsQuery 生成一个 A 查询。
func dnsQuery(name string) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), dns.TypeA)
	m.RecursionDesired = true
	return m
}

// checkDNSReply 核对应答：是应答、事务号相同、问题段相同。
func checkDNSReply(q *dns.Msg, b []byte) error {
	r := new(dns.Msg)
	if err := r.Unpack(b); err != nil {
		return fail(CauseUnexpectedReply, "DNS 应答解析不了")
	}
	if !r.Response || r.Id != q.Id || len(r.Question) != 1 ||
		!strings.EqualFold(r.Question[0].Name, q.Question[0].Name) || r.Question[0].Qtype != q.Question[0].Qtype {
		return fail(CauseUnexpectedReply, "DNS 应答的事务号或问题段不符")
	}
	return nil
}

// probeUDP 经出口发一次 UDP 53 查询，收到合法的 DNS 应答才算通过：回应帧 ok 但没有应答是“无法判断”（超时）。
func (c *Checker) probeUDP(ctx context.Context, exit string) error {
	dst := c.UDPTarget
	if !dst.IsValid() {
		dst = netip.MustParseAddrPort("1.1.1.1:53")
	}
	conn, err := c.Opener.Open(ctx, wire.Header{V: wire.Version, Exit: exit, Net: wire.UDP, IP: dst.Addr().String(), Port: int(dst.Port())})
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	q := dnsQuery(c.probeName())
	pb, _ := q.Pack()
	dc := wire.NewDatagramConn(conn)
	if err := dc.WriteDatagram(pb); err != nil {
		return err
	}
	buf := make([]byte, 4096)
	n, err := dc.ReadDatagram(buf)
	if err != nil {
		if ctx.Err() != nil {
			return context.DeadlineExceeded
		}
		return err
	}
	return checkDNSReply(q, buf[:n])
}

// probeDoH 经出口用 DoH（RFC 8484 GET）解析一次：200 且是合法的 DNS 应答才算通过。
func (c *Checker) probeDoH(ctx context.Context, hc *client) error {
	q := dnsQuery(c.probeName())
	q.Id = 0 // RFC 8484 建议 GET 时事务号为 0，便于缓存
	pb, _ := q.Pack()
	u := c.dohURL() + "?dns=" + base64.RawURLEncoding.EncodeToString(pb)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fail(CauseBadFormat, "DoH 地址无效")
	}
	req.Header.Set("Accept", "application/dns-message")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return fail(CauseRateLimited, "DoH 服务限流或出错（HTTP "+strconv.Itoa(resp.StatusCode)+"）")
	case resp.StatusCode != http.StatusOK:
		return fail(CauseUnexpectedReply, "DoH 返回 HTTP "+strconv.Itoa(resp.StatusCode))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return err
	}
	return checkDNSReply(q, b)
}
