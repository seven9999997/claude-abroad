package directdial

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"claude-abroad/internal/fdpass"
)

// ErrWrongServer 表示 socket 对端不是配置的专用用户：可能有别的本地用户抢先放了一个假的 socket，
// 把直连流量骗走。这时不发请求。
var ErrWrongServer = errors.New("直连拨号模块的身份不对")

// Client 是 abroadd 一侧的客户端。每次拨号建一条 unix 连接。
type Client struct {
	Path      string
	ServerUID uint32 // 直连拨号模块运行的用户（生产是 _abroad，开发模式是当前用户）

	mu      sync.Mutex
	source  func() Config // 当前配置（abroadd 的数据面维护）；nil 时拨号带代数 0、不补发
	push    *pushCall     // 正在进行的补发（并发的 stale_config 共用这一次）
	pushSeq uint64        // 已完成的补发次数
	lastOK  bool          // 最近一次补发是否成功
	lastGen uint64        // 最近一次补发的代数
	okGen   atomic.Uint64 // 最近一次下发成功的代数 + 1（0 表示还没成功过）
}

// Config 是下发给直连拨号模块的运行时配置（m3-plan v2.8 决定 1、v2.9）。
type Config struct {
	Gen         uint64       // 代数：内容每变一次加一
	DNS         []netip.Addr // 本机原 DNS 服务器，最多 4 个
	PinnedExtra []string     // 置顶追加名单
}

type pushCall struct {
	done chan struct{}
	err  error
}

// pushTimeout 是按需补发配置的时限。
const pushTimeout = 5 * time.Second

// SetConfigSource 设置当前配置的来源：拨号请求带上它的代数，服务端回 stale_config 时下发它。
func (c *Client) SetConfigSource(f func() Config) {
	c.mu.Lock()
	c.source = f
	c.mu.Unlock()
}

func (c *Client) current() (Config, bool) {
	c.mu.Lock()
	f := c.source
	c.mu.Unlock()
	if f == nil {
		return Config{}, false
	}
	return f(), true
}

// ConfiguredGen 返回最近一次下发成功的代数；ok 为假表示还没成功下发过。
func (c *Client) ConfiguredGen() (gen uint64, ok bool) {
	v := c.okGen.Load()
	return v - 1, v != 0
}

// Configure 下发运行时配置：本机原 DNS 服务器（最多 4 个）、置顶追加名单和代数。
// 同样先核对服务端身份。服务端校验不过时返回 ErrBadRequest，旧配置保持。
func (c *Client) Configure(ctx context.Context, cfg Config) error {
	req := Request{V: Version, Op: "config", PinnedExtra: cfg.PinnedExtra, Gen: cfg.Gen}
	for _, a := range cfg.DNS {
		req.DNS = append(req.DNS, a.String())
	}
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if len(b) > MaxConfigRequest {
		return ErrBadRequest
	}
	uc, err := c.connect(ctx)
	if err != nil {
		return err
	}
	defer uc.Close()
	stop := context.AfterFunc(ctx, func() { _ = uc.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		_ = uc.SetDeadline(dl)
	}
	if err := fdpass.Send(uc, b, nil); err != nil {
		return ctxErr(ctx, fmt.Errorf("发送配置：%w", err))
	}
	msg, f, err := fdpass.Recv(uc, fdpass.MaxMsg)
	if f != nil {
		f.Close()
	}
	if err != nil {
		return ctxErr(ctx, fmt.Errorf("接收应答：%w", err))
	}
	var r Reply
	if err := json.Unmarshal(msg, &r); err != nil {
		return errors.New("应答格式不对")
	}
	if !r.OK {
		return &CodeError{Code: r.Code}
	}
	c.okGen.Store(cfg.Gen + 1)
	return nil
}

// ensurePushed 在拨号遇到 stale_config 后补发当前配置。seen 是发出拨号请求前的补发次数：
// 这之后已经有一次成功的补发、代数也还是当前的，就不再下发（它的 stale_config 发生在那次补发之前）；
// 正在补发时等它完成。并发的多个拨号因此只下发一次。
func (c *Client) ensurePushed(ctx context.Context, seen uint64) error {
	c.mu.Lock()
	if p := c.push; p != nil {
		c.mu.Unlock()
		select {
		case <-p.done:
			return p.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	src := c.source
	if src == nil {
		c.mu.Unlock()
		return ErrStaleConfig
	}
	cfg := src()
	if c.pushSeq != seen && c.lastOK && c.lastGen == cfg.Gen {
		c.mu.Unlock()
		return nil
	}
	p := &pushCall{done: make(chan struct{})}
	c.push = p
	c.mu.Unlock()
	// 补发不跟随某一个拨号的 ctx：它被取消时，等着同一次补发的其他拨号不受影响。
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pushTimeout)
	p.err = c.Configure(pctx, cfg)
	cancel()
	c.mu.Lock()
	c.push = nil
	c.pushSeq++
	c.lastOK, c.lastGen = p.err == nil, cfg.Gen
	c.mu.Unlock()
	close(p.done)
	return p.err
}

// connect 连上服务端并核对它的身份（不符就不发请求）。
func (c *Client) connect(ctx context.Context) (*net.UnixConn, error) {
	var d net.Dialer
	raw, err := d.DialContext(ctx, "unix", c.Path)
	if err != nil {
		return nil, fmt.Errorf("连不上直连拨号模块：%w", err)
	}
	uc := raw.(*net.UnixConn)
	if uid, err := fdpass.PeerUID(uc); err != nil || uid != c.ServerUID {
		uc.Close()
		return nil, ErrWrongServer
	}
	return uc, nil
}

// Dial 请求直连拨号：host 和 ip 二选一（host 非空时用 host）。成功时返回拨好的连接
// （TCP 是 *net.TCPConn，UDP 是已连接的 *net.UDPConn）。失败时返回 *CodeError（可用 errors.Is 比较，
// 例如 ErrPinned），或者连接、身份方面的错误。
//
// 服务端回 stale_config（没有配置或代数不一致，例如它刚重启过）时，下发当前配置并重试这一次，只重试一次；
// 补发失败或重试仍不行时返回带 ErrStaleConfig 的错误（m3-plan v2.9）。
func (c *Client) Dial(ctx context.Context, network, host string, ip netip.Addr, port uint16) (net.Conn, error) {
	c.mu.Lock()
	seen := c.pushSeq
	c.mu.Unlock()
	cfg, _ := c.current()
	conn, err := c.dialOnce(ctx, network, host, ip, port, cfg.Gen)
	if !errors.Is(err, ErrStaleConfig) {
		return conn, err
	}
	if perr := c.ensurePushed(ctx, seen); perr != nil {
		if errors.Is(perr, ErrStaleConfig) {
			return nil, perr
		}
		return nil, errors.Join(ErrStaleConfig, fmt.Errorf("补发配置：%w", perr))
	}
	cfg, _ = c.current()
	return c.dialOnce(ctx, network, host, ip, port, cfg.Gen)
}

func (c *Client) dialOnce(ctx context.Context, network, host string, ip netip.Addr, port uint16, gen uint64) (net.Conn, error) {
	req := Request{V: Version, Net: network, Host: host, Port: port, Gen: gen}
	if host == "" {
		req.IP = ip.String()
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	uc, err := c.connect(ctx) // 先核对服务端身份，不符就不发请求
	if err != nil {
		return nil, err
	}
	defer uc.Close()
	stop := context.AfterFunc(ctx, func() { _ = uc.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		_ = uc.SetDeadline(dl)
	}
	if err := fdpass.Send(uc, b, nil); err != nil {
		return nil, ctxErr(ctx, fmt.Errorf("发送请求：%w", err))
	}
	msg, f, err := fdpass.Recv(uc, fdpass.MaxMsg)
	if err != nil {
		return nil, ctxErr(ctx, fmt.Errorf("接收应答：%w", err))
	}
	var r Reply
	if err := json.Unmarshal(msg, &r); err != nil {
		if f != nil {
			f.Close()
		}
		return nil, errors.New("应答格式不对")
	}
	if !r.OK {
		if f != nil {
			f.Close()
		}
		return nil, &CodeError{Code: r.Code}
	}
	if f == nil {
		return nil, errors.New("应答成功但没有带连接")
	}
	conn, err := net.FileConn(f)
	f.Close()
	if err != nil {
		return nil, err
	}
	// 已经拿到自己的副本：关闭 unix 连接，通知服务端可以放掉它那份（见 Server.handle）。
	uc.Close()
	return conn, nil
}

func ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
