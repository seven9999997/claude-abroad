package usernet

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// DoH 解析器的常量（M2-1a）。
const (
	DoHTimeout   = 5 * time.Second
	DoHMaxAnswer = 4096
	DoHMinTTL    = 30 * time.Second
	DoHMaxTTL    = 10 * time.Minute
	// DoHStaleFor：解析失败时，这么久以内的旧结果仍可回答（TTL 用 DoHStaleTTL）。
	DoHStaleFor = time.Hour
	DoHStaleTTL = 30 * time.Second
	// DoHStaleWait：有可用的旧结果时，新结果最多等这么久，超时先回旧结果，解析在后台继续并更新缓存
	// （引擎的 DNS 超时约 5 秒，等满 DoHTimeout 再回旧结果就晚了）。
	DoHStaleWait = 1500 * time.Millisecond
)

// DefaultDoHEndpoints 是 abroad-vm 自己解析第一跳域名用的 DoH（与引擎原来用的同一组，IP 字面量）。
var DefaultDoHEndpoints = []netip.AddrPort{
	netip.MustParseAddrPort("1.1.1.1:443"),
	netip.MustParseAddrPort("223.5.5.5:443"),
}

// ErrNoAddress 是域名没有 A 记录。
var ErrNoAddress = errors.New("域名没有 IPv4 地址")

// DoH 是 abroad-vm 自己的 DoH 解析器（RFC 8484），只解析第一跳域名的 A 记录：
//   - 同时问所有 Endpoints，取先成功的；
//   - 拨号只经 Dial（abroad-vm 里是 Dialer.DialResolver：只连 Endpoints、绑物理网卡），不读系统代理；
//   - 证书按 IP 校验（URL 的主机就是 IP 字面量）；
//   - 单次 DoHTimeout，应答上限 DoHMaxAnswer 字节，TTL 夹在 [DoHMinTTL, DoHMaxTTL]；
//   - 同一域名的并发查询合并成一次；失败时 DoHStaleFor 以内的旧结果仍可用，
//     而且有旧结果时最多等 DoHStaleWait 就先回旧结果。
type DoH struct {
	Endpoints []netip.AddrPort
	Dial      func(ctx context.Context, network, addr string) (net.Conn, error)
	TLS       *tls.Config // 为 nil 时用系统根证书（测试注入）
	Now       func() time.Time

	once   sync.Once
	client *http.Client

	mu       sync.Mutex
	cache    map[string]dohEntry
	inflight map[string]*dohCall
}

type dohEntry struct {
	addrs []netip.Addr
	ttl   time.Duration
	at    time.Time
}

type dohCall struct {
	done  chan struct{}
	addrs []netip.Addr
	ttl   time.Duration
	err   error
}

func (r *DoH) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *DoH) httpClient() *http.Client {
	r.once.Do(func() {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if r.TLS != nil {
			cfg = r.TLS.Clone()
		}
		tr := &http.Transport{
			Proxy:               nil, // 不读 HTTPS_PROXY 等环境变量，也不走系统代理
			DialContext:         r.Dial,
			TLSClientConfig:     cfg,
			ForceAttemptHTTP2:   true,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     90 * time.Second,
		}
		r.client = &http.Client{Transport: tr, Timeout: DoHTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	})
	return r.client
}

// Lookup 返回 name 的 IPv4 地址和应答 TTL。name 是小写、不带末尾点的主机名。
func (r *DoH) Lookup(ctx context.Context, name string) ([]netip.Addr, time.Duration, error) {
	now := r.now()
	r.mu.Lock()
	if r.cache == nil {
		r.cache, r.inflight = map[string]dohEntry{}, map[string]*dohCall{}
	}
	if e, ok := r.cache[name]; ok && now.Sub(e.at) < e.ttl {
		r.mu.Unlock()
		return e.addrs, e.ttl - now.Sub(e.at), nil
	}
	c := r.inflight[name]
	if c == nil {
		c = &dohCall{done: make(chan struct{})}
		r.inflight[name] = c
		go r.resolve(name, c)
	}
	stale, hasStale := r.cache[name]
	hasStale = hasStale && now.Sub(stale.at) <= DoHStaleFor
	r.mu.Unlock()
	var staleWait <-chan time.Time
	if hasStale {
		t := time.NewTimer(DoHStaleWait)
		defer t.Stop()
		staleWait = t.C
	}
	select {
	case <-c.done:
	case <-staleWait:
		return stale.addrs, DoHStaleTTL, nil
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
	if c.err == nil {
		return c.addrs, c.ttl, nil
	}
	r.mu.Lock()
	e, ok := r.cache[name]
	r.mu.Unlock()
	if ok && r.now().Sub(e.at) <= DoHStaleFor {
		return e.addrs, DoHStaleTTL, nil
	}
	return nil, 0, c.err
}

// resolve 不跟调用方的 ctx 走（被合并的其他调用方还在等），自带超时。
func (r *DoH) resolve(name string, c *dohCall) {
	ctx, cancel := context.WithTimeout(context.Background(), DoHTimeout)
	defer cancel()
	c.addrs, c.ttl, c.err = r.query(ctx, name)
	r.mu.Lock()
	if c.err == nil {
		r.cache[name] = dohEntry{addrs: c.addrs, ttl: c.ttl, at: r.now()}
	}
	delete(r.inflight, name)
	r.mu.Unlock()
	close(c.done)
}

func (r *DoH) query(ctx context.Context, name string) ([]netip.Addr, time.Duration, error) {
	if len(r.Endpoints) == 0 || r.Dial == nil {
		return nil, 0, errors.New("没有配置 DoH")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		addrs []netip.Addr
		ttl   time.Duration
		err   error
	}
	ch := make(chan result, len(r.Endpoints))
	for _, ep := range r.Endpoints {
		go func() {
			a, t, err := r.ask(ctx, ep, name)
			ch <- result{a, t, err}
		}()
	}
	var firstErr error
	for range r.Endpoints {
		res := <-ch
		if res.err == nil {
			return res.addrs, res.ttl, nil
		}
		if firstErr == nil || errors.Is(firstErr, context.Canceled) {
			firstErr = res.err
		}
	}
	return nil, 0, firstErr
}

func (r *DoH) ask(ctx context.Context, ep netip.AddrPort, name string) ([]netip.Addr, time.Duration, error) {
	q := new(dns.Msg)
	q.SetQuestion(dns.Fqdn(name), dns.TypeA)
	q.Id = 0 // RFC 8484 建议 ID 为 0
	wire, err := q.Pack()
	if err != nil {
		return nil, 0, err
	}
	url := "https://" + ep.String() + "/dns-query"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(wire))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("DoH 请求失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("DoH 返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, DoHMaxAnswer+1))
	if err != nil {
		return nil, 0, fmt.Errorf("读 DoH 应答：%w", err)
	}
	if len(body) > DoHMaxAnswer {
		return nil, 0, errors.New("DoH 应答超过上限")
	}
	m := new(dns.Msg)
	if err := m.Unpack(body); err != nil {
		return nil, 0, errors.New("DoH 应答格式不对")
	}
	if m.Rcode != dns.RcodeSuccess {
		return nil, 0, fmt.Errorf("DoH 应答 %s", dns.RcodeToString[m.Rcode])
	}
	var addrs []netip.Addr
	ttl := DoHMaxTTL
	for _, rr := range m.Answer {
		a, ok := rr.(*dns.A)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(a.A)
		if !ok {
			continue
		}
		addrs = append(addrs, ip.Unmap())
		ttl = min(ttl, time.Duration(a.Hdr.Ttl)*time.Second)
	}
	if len(addrs) == 0 {
		return nil, 0, ErrNoAddress
	}
	return addrs, max(ttl, DoHMinTTL), nil
}
