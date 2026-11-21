package fakedns

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"claude-abroad/internal/guestabi"
	"claude-abroad/internal/wire"
)

// DoHUpstream 的常量（设计 4.3，M2-5d）。
const (
	// DoHIP 是 DoH 服务器的地址：与虚拟机里 mihomo 的 nameserver（https://1.1.1.1/dns-query#CAGE）一致。
	DoHIP = "1.1.1.1"
	// DoHURL 是请求地址（主机是 IP 字面量，本机不解析任何域名）。
	DoHURL = "https://" + DoHIP + "/dns-query"
	// DoHMaxBody 是应答体的上限。
	DoHMaxBody = 64 << 10
	// DoHIdle 是空闲连接保留多久。
	DoHIdle = 30 * time.Second
	// DoHMaxConns 是同时打开的连接上限（并发查询复用这几条连接）。
	DoHMaxConns = 4
	// DoHTimeout 是调用方没给期限时单次请求的上限。注意它管不到拨号和 TLS 握手：http.Transport 传给
	// 拨号函数的 ctx 不带请求的期限，那一段由 DoHDialTimeout 单独管。
	DoHTimeout = 10 * time.Second
	// DoHDialTimeout 是开流加 TLS 握手的上限。
	DoHDialTimeout = 5 * time.Second
)

// StreamOpener 开一条到虚拟机数据面的流（*wire.Dialer 满足；与 stack.Opener 同签名，本包不引用 stack）。
type StreamOpener interface {
	Open(ctx context.Context, h wire.Header) (net.Conn, error)
}

// Gate 是闸门（与 stack.Gate 同签名）：Open 必须不阻塞。
type Gate interface{ Open() bool }

// ErrGateClosed 是闸门关闭时的查询（Server 答 SERVFAIL）。
var ErrGateClosed = errors.New("闸门关闭，不经主代理查询")

// dohHeader 是 DoH 连接的帧头：出口 main，TCP，目标 1.1.1.1:443，没有域名。
var dohHeader = wire.Header{V: wire.Version, Exit: guestabi.ExitMain, Net: wire.TCP, IP: DoHIP, Port: 443}

// DoHUpstream 是 fake-IP DNS 的上游：经数据通道、出口 main 连 1.1.1.1:443 的 DoH（RFC 8484），
// 把查询交给主代理解析（设计 4.3）。
//   - 每条 TCP 连接都经 Opener 开流，帧头固定（dohHeader），传入的拨号地址一律忽略；本机不解析任何域名；
//   - TLS 校验证书，ServerName 为 1.1.1.1（走证书的 IP SAN），不允许跳过校验；
//   - 不读环境变量里的代理，不跟随重定向；只接受 200、Content-Type 为 application/dns-message、
//     不超过 DoHMaxBody、问题段与请求一致的应答；
//   - 闸门关闭时直接返回 ErrGateClosed，不开流；
//   - 错误信息里不带查询的域名。
type DoHUpstream struct {
	Opener StreamOpener
	Gate   Gate
	// RootCAs 为 nil 时用系统根证书（测试注入自签 CA）。
	RootCAs *x509.CertPool
	// dialTimeout 为零时用 DoHDialTimeout（测试缩短）。
	dialTimeout time.Duration

	once   sync.Once
	tr     *http.Transport
	client *http.Client
}

func (d *DoHUpstream) init() {
	d.once.Do(func() {
		tlsCfg := &tls.Config{ServerName: DoHIP, RootCAs: d.RootCAs, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}}
		d.tr = &http.Transport{
			Proxy: nil, // 不读 HTTPS_PROXY、ALL_PROXY 等环境变量
			DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				// http.Transport 传进来的 ctx 不带请求的取消和期限（Go 1.26 transport.go 的 dialConnFor），
				// 不加这一层的话，对端不回 TLS 时握手会永远挂着，DoHMaxConns 个名额被占满后 DoH 永久不可用。
				timeout := d.dialTimeout
				if timeout <= 0 {
					timeout = DoHDialTimeout
				}
				ctx, cancel := context.WithTimeout(ctx, timeout)
				defer cancel()
				if d.Opener == nil {
					return nil, errors.New("没有配置数据通道")
				}
				// 排队等名额的拨号可能在闸门关闭之后才开始：再判一次，关闸门后不开流。
				if d.Gate == nil || !d.Gate.Open() {
					return nil, ErrGateClosed
				}
				c, err := d.Opener.Open(ctx, dohHeader) // 忽略传入的地址，永远是 1.1.1.1:443
				if err != nil {
					return nil, fmt.Errorf("经主代理连接 DoH：%w", err)
				}
				tc := tls.Client(c, tlsCfg.Clone())
				if err := tc.HandshakeContext(ctx); err != nil {
					c.Close()
					return nil, fmt.Errorf("DoH 的 TLS 握手：%w", err)
				}
				return tc, nil
			},
			MaxIdleConns:        DoHMaxConns,
			MaxIdleConnsPerHost: DoHMaxConns,
			MaxConnsPerHost:     DoHMaxConns,
			IdleConnTimeout:     DoHIdle,
			ForceAttemptHTTP2:   false,
		}
		d.client = &http.Client{Transport: d.tr,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	})
}

// Exchange 实现 Upstream。
func (d *DoHUpstream) Exchange(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
	if d.Gate == nil || !d.Gate.Open() {
		return nil, ErrGateClosed
	}
	d.init()
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DoHTimeout)
		defer cancel()
	}
	m := q.Copy()
	m.Id = 0 // RFC 8484 建议 ID 为 0（利于缓存）
	body, err := m.Pack()
	if err != nil {
		return nil, errors.New("DoH 查询打包失败")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, DoHURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("DoH 请求失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DoH 返回 HTTP %d", resp.StatusCode)
	}
	if mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || mt != "application/dns-message" {
		return nil, errors.New("DoH 应答的 Content-Type 不对")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, DoHMaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("读 DoH 应答：%w", err)
	}
	if len(data) > DoHMaxBody {
		return nil, errors.New("DoH 应答超过上限")
	}
	r := new(dns.Msg)
	if err := r.Unpack(data); err != nil {
		return nil, errors.New("DoH 应答格式不对")
	}
	if !sameQuestion(q.Question, r.Question) {
		return nil, errors.New("DoH 应答的问题段与请求不一致")
	}
	r.Id = q.Id
	return r, nil
}

func sameQuestion(a, b []dns.Question) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i].Name, b[i].Name) || a[i].Qtype != b[i].Qtype || a[i].Qclass != b[i].Qclass {
			return false
		}
	}
	return true
}

// Close 关掉空闲连接（停用第 ② 步在 DropAll 之后调用；正在用的连接由 wire.Dialer.Close 兜底）。
func (d *DoHUpstream) Close() {
	d.init()
	d.tr.CloseIdleConnections()
}
