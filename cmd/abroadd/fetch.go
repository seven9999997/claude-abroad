package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"claude-abroad/internal/daemon"
	"claude-abroad/internal/directdial"
	"claude-abroad/internal/guestabi"
	"claude-abroad/internal/stack"
	"claude-abroad/internal/wire"
)

// realFetch 是真实模式抓取订阅（M5-4，m5-plan v2.2 决定 5）。abroadd 自己不拨网络（daemon 包的不拨号检查）：
//   - 启用时经数据通道、按出口 main 开流（走主代理；域名由出口一侧解析）；
//   - 未启用、调用方明确选择经本机网络（viaHost）时，经直连拨号模块（绑物理网卡，域名由它按启用前记下的本机 DNS 解析）。
//
// 证书照常校验；不跟随跳转到别的协议；有大小上限和时限。错误只给类别（daemon.ErrFetch*），底层错误里的地址不往上传。
func realFetch(streams stack.Opener, direct *directdial.Client) func(ctx context.Context, url string, viaHost bool) ([]byte, error) {
	return func(ctx context.Context, url string, viaHost bool) ([]byte, error) {
		dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, ps, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			port, err := strconv.Atoi(ps)
			if err != nil || port <= 0 || port > 65535 {
				return nil, errors.New("端口不对")
			}
			ip, _ := netip.ParseAddr(host)
			if viaHost {
				if direct == nil {
					return nil, errors.New("没有直连拨号模块")
				}
				if ip.IsValid() {
					return direct.Dial(ctx, "tcp", "", ip, uint16(port))
				}
				return direct.Dial(ctx, "tcp", host, netip.Addr{}, uint16(port))
			}
			h := wire.Header{V: wire.Version, Exit: guestabi.ExitMain, Net: wire.TCP, Port: port}
			if ip.IsValid() {
				h.IP = ip.String()
			} else {
				h.Host = host
			}
			return streams.Open(ctx, h)
		}
		client := &http.Client{
			Timeout: daemon.SourceFetchTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 || req.URL.Scheme != via[0].URL.Scheme {
					return daemon.ErrFetchRedirect
				}
				return nil
			},
			Transport: &http.Transport{DialContext: dial, Proxy: nil, TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 20 * time.Second},
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, errors.New("订阅地址不对")
		}
		req.Header.Set("User-Agent", "clash.meta")
		resp, err := client.Do(req)
		if err != nil {
			if errors.Is(err, daemon.ErrFetchRedirect) {
				return nil, daemon.ErrFetchRedirect
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, context.DeadlineExceeded
			}
			return nil, fmt.Errorf("连不上订阅服务器")
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, daemon.ErrFetchStatus
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, daemon.SourceFetchMax+1))
		if err != nil {
			return nil, errors.New("读订阅内容失败")
		}
		if len(b) > daemon.SourceFetchMax {
			return nil, daemon.ErrFetchTooLarge
		}
		return b, nil
	}
}
