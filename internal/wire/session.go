package wire

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// SessionConfig 是 yamux 会话的可调参数（R6 性能测试时要调）。零值字段用默认值。
type SessionConfig struct {
	MaxStreamWindow   uint32        // 每条流的接收窗口，默认 1 MiB
	KeepAliveInterval time.Duration // 心跳间隔，默认 15 秒；负数表示关闭心跳
	WriteTimeout      time.Duration // 单次写的超时，默认 10 秒
	AcceptBacklog     int           // 待接受的流上限，默认 256
	OpenTimeout       time.Duration // 交换帧头和回应的超时（只管握手），默认 10 秒
	// StreamOpenTimeout 是 yamux 等对方确认新流的时间。注意：到期时 yamux 会关掉
	// 整个会话（所有流一起断），所以默认沿用 yamux 的 75 秒，不要和 OpenTimeout 绑在一起。
	StreamOpenTimeout time.Duration
	// StreamCloseTimeout 是本端 Close（半关闭）之后，等对方也关闭的最长时间（绝对时间，
	// 不是空闲时间），到期强制关流。默认 5 分钟：半关闭后另一方向最多再传 5 分钟。
	StreamCloseTimeout time.Duration
}

// DefaultSessionConfig 返回默认参数。
func DefaultSessionConfig() SessionConfig {
	return SessionConfig{
		MaxStreamWindow:    1 << 20,
		KeepAliveInterval:  15 * time.Second,
		WriteTimeout:       10 * time.Second,
		AcceptBacklog:      256,
		OpenTimeout:        10 * time.Second,
		StreamOpenTimeout:  75 * time.Second,
		StreamCloseTimeout: 5 * time.Minute,
	}
}

func (c SessionConfig) withDefaults() SessionConfig {
	d := DefaultSessionConfig()
	if c.MaxStreamWindow == 0 {
		c.MaxStreamWindow = d.MaxStreamWindow
	}
	if c.KeepAliveInterval == 0 {
		c.KeepAliveInterval = d.KeepAliveInterval
	}
	if c.WriteTimeout == 0 {
		c.WriteTimeout = d.WriteTimeout
	}
	if c.AcceptBacklog == 0 {
		c.AcceptBacklog = d.AcceptBacklog
	}
	if c.OpenTimeout == 0 {
		c.OpenTimeout = d.OpenTimeout
	}
	if c.StreamOpenTimeout == 0 {
		c.StreamOpenTimeout = d.StreamOpenTimeout
	}
	if c.StreamCloseTimeout == 0 {
		c.StreamCloseTimeout = d.StreamCloseTimeout
	}
	return c
}

func (c SessionConfig) yamux() *yamux.Config {
	c = c.withDefaults()
	y := yamux.DefaultConfig()
	y.MaxStreamWindowSize = c.MaxStreamWindow
	y.EnableKeepAlive = c.KeepAliveInterval > 0
	if y.EnableKeepAlive {
		y.KeepAliveInterval = c.KeepAliveInterval
	}
	y.ConnectionWriteTimeout = c.WriteTimeout
	y.StreamOpenTimeout = c.StreamOpenTimeout
	y.StreamCloseTimeout = c.StreamCloseTimeout
	y.AcceptBacklog = c.AcceptBacklog
	y.LogOutput = io.Discard // 日志由调用方按需记录，不让 yamux 直接写 stderr
	return y
}

// yamux 必须不低于 v0.1.2：从这个版本起 Stream.Close 是半关闭（只关本端写方向，
// 之后仍能读到对方的数据），数据面靠它实现 TCP 的 CloseWrite 语义。

// Server 在 agent 一侧（vsock 1080 上接受的连接）建立 yamux 会话。
func Server(conn net.Conn, cfg SessionConfig) (*yamux.Session, error) {
	return yamux.Server(conn, cfg.yamux())
}

// Client 在本机一侧建立 yamux 会话。
func Client(conn net.Conn, cfg SessionConfig) (*yamux.Session, error) {
	return yamux.Client(conn, cfg.yamux())
}

// Dialer 是本机一侧的数据面入口：按需连接 data.sock、建立会话，会话断开后
// 下一次 Open 自动重连。并发安全。
type Dialer struct {
	// Dial 建立到 agent 数据面的底层连接（通常是连 data.sock）。
	Dial   func(ctx context.Context) (net.Conn, error)
	Config SessionConfig

	mu   sync.Mutex
	sess *yamux.Session
}

func (d *Dialer) session(ctx context.Context) (*yamux.Session, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sess != nil && !d.sess.IsClosed() {
		return d.sess, nil
	}
	conn, err := d.Dial(ctx)
	if err != nil {
		return nil, err
	}
	s, err := Client(conn, d.Config)
	if err != nil {
		conn.Close()
		return nil, err
	}
	d.sess = s
	return s, nil
}

// drop 丢弃一个已经失效的会话（只在它仍是当前会话时）。
func (d *Dialer) drop(s *yamux.Session) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sess == s {
		_ = s.Close()
		d.sess = nil
	}
}

// Open 开一条数据流：写帧头、等回应。agent 拒绝时返回 *ReplyError。
// 会话已断开时自动重连一次。返回的连接上，TCP 直接读写字节流；UDP 用 NewDatagramConn 包装。
func (d *Dialer) Open(ctx context.Context, h Header) (net.Conn, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		s, err := d.session(ctx)
		if err != nil {
			return nil, err
		}
		st, err := s.OpenStream()
		if err != nil {
			d.drop(s)
			if attempt == 0 && isSessionGone(err) {
				continue
			}
			return nil, err
		}
		c, err := handshake(ctx, st, h, d.Config.withDefaults().OpenTimeout)
		if err != nil && attempt == 0 && s.IsClosed() {
			d.drop(s)
			continue
		}
		return c, err
	}
}

func isSessionGone(err error) bool {
	return errors.Is(err, yamux.ErrSessionShutdown) || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed)
}

func handshake(ctx context.Context, st *yamux.Stream, h Header, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = st.SetDeadline(deadline)
	// ctx 被取消时把 deadline 设到过去，打断阻塞的读写。
	cancelled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(cancelled)
		_ = st.SetDeadline(time.Unix(1, 0))
	})
	fail := func(err error) (net.Conn, error) {
		stop()
		st.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if err := WriteHeader(st, h); err != nil {
		return fail(err)
	}
	rep, err := ReadReply(st)
	if err != nil {
		return fail(err)
	}
	if err := rep.Err(); err != nil {
		return fail(err)
	}
	// 成功路径：必须确认取消回调没有跑、也不会再跑，才能清掉 deadline；
	// 否则回调可能在清除之后又把 deadline 设到过去，返回一条永远超时的流。
	if !stop() {
		<-cancelled
		st.Close()
		return nil, ctx.Err()
	}
	_ = st.SetDeadline(time.Time{})
	return st, nil
}

// Close 关闭当前会话（之后的 Open 会重新连接）。
func (d *Dialer) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sess == nil {
		return nil
	}
	err := d.sess.Close()
	d.sess = nil
	return err
}
