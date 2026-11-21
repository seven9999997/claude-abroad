package envcheck

import (
	"context"
	"errors"
	"net"
	"strconv"

	"claude-abroad/internal/wire"
)

// ProbeDial 是本机发起探测的唯一出口（m4-plan v2.7 决定 1）：用系统默认的方式拨号——不绑网卡、不设代理，
// 流量和普通应用一样经 utun 进我们的网络栈、按 abroadd 自己的身份走（闸门关着时靠探测窗口）。
// envcheck 里只有这个文件可以直接拨号（源码检查）；它只会在同一轮 01 通过之后被调用（要发探测的项都标了 Network）。
func ProbeDial(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// ProbeOpener 让 exitcheck 的检查器经 ProbeDial 发请求（02、03、13、14 复用它的解析器、大陆段判断和位置缓存）。
// 帧头里的出口不起作用：本机的探测走哪个出口由分流规则决定（abroadd 自己的探测在窗口期间固定走主代理）。
// UDP 不支持（exitcheck 的 UDP 探测只记录，不参与判定）。
type ProbeOpener struct {
	Dial func(ctx context.Context, network, addr string) (net.Conn, error) // nil 用 ProbeDial（测试注入假服务）
}

// Open 实现 stack.Opener。
func (o ProbeOpener) Open(ctx context.Context, h wire.Header) (net.Conn, error) {
	if h.Net != wire.TCP {
		return nil, errors.New("本机探测不经这里发 UDP")
	}
	host := h.Host
	if host == "" {
		host = h.IP
	}
	dial := o.Dial
	if dial == nil {
		dial = ProbeDial
	}
	return dial(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(h.Port)))
}
