package exitcheck

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"claude-abroad/internal/stack"
	"claude-abroad/internal/wire"
)

// 这是 exitcheck 里唯一构造 HTTP 客户端的文件（源码检查的白名单只放这里）：拨号一律经 Opener 在指定出口上开流。

// openTCP 经出口开一条 TCP 流。开流失败但不是出口的回应错误时，标成“数据通道不可用”。
func openTCP(ctx context.Context, op stack.Opener, exit, addr string) (net.Conn, error) {
	host, ps, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(ps)
	if err != nil {
		return nil, err
	}
	h := wire.Header{V: wire.Version, Exit: exit, Net: wire.TCP, Port: port}
	if a, err := netip.ParseAddr(host); err == nil {
		h.IP = a.String()
	} else {
		h.Host = host
	}
	c, err := op.Open(ctx, h)
	if err != nil {
		var re *wire.ReplyError
		if errors.As(err, &re) {
			return nil, err
		}
		return nil, fmt.Errorf("%w：%v", errChannel, err)
	}
	return c, nil
}

// client 是其他文件用的 HTTP 客户端类型（构造只在这里）。
type client = http.Client

// httpClient 返回经出口 exit 访问的 HTTP 客户端：不复用连接、不走代理环境变量、只用 HTTP/1.1。
// round 是一轮检查的 ctx：http.Transport 的拨号不一定随请求取消（它可能用分离的 ctx 拨号），
// 这里让每次开流也随 round 结束，Check 返回后不会留下卡在开流里的 goroutine。
func httpClient(round context.Context, op stack.Opener, exit string, tlsConf *tls.Config, timeout time.Duration) *client {
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			dctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			stop := context.AfterFunc(round, cancel)
			defer stop()
			return openTCP(dctx, op, exit, addr)
		},
		TLSClientConfig:        tlsConf,
		DisableKeepAlives:      true,
		ForceAttemptHTTP2:      false,
		TLSHandshakeTimeout:    timeout,
		MaxResponseHeaderBytes: 64 << 10,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // 不跟随重定向：被劫持到别的页面时如实拿到 3xx
		},
	}
}
