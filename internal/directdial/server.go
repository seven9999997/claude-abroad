package directdial

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"claude-abroad/internal/fdpass"
	"claude-abroad/internal/rules"
)

// 默认值（m3-plan M3-4）。
const (
	DefaultMaxInflight = 256
	DefaultTimeout     = 8 * time.Second
	maxTries           = 3
	readTimeout        = 5 * time.Second
	ackTimeout         = 5 * time.Second
)

// Server 是直连拨号模块的服务端。
type Server struct {
	Dialer BoundDialer
	// DNSServers 返回启用前记下的本机 DNS 服务器（每个请求取一次）。
	DNSServers func() []netip.Addr
	// AllowUID 判定对端 uid：生产只收 root（abroadd），开发模式收当前用户。nil 时一律拒绝。
	AllowUID    func(uid uint32) bool
	PinnedExtra []string // 用户追加的置顶域名（内置的永远生效）
	MaxInflight int
	Timeout     time.Duration

	inflight atomic.Int64
	rt       atomic.Pointer[runtimeConfig] // abroadd 下发的运行时配置（m3-plan v2.8 决定 1）
	counts   sync.Map                      // 应答码 → *atomic.Int64（"ok" 表示成功）
	newID    func() uint16
}

// Stats 返回各种结果的计数（只有计数，不带目标），给诊断用。
func (s *Server) Stats() map[string]int64 {
	out := map[string]int64{"ok": 0}
	for _, c := range codes {
		out[c] = 0
	}
	s.counts.Range(func(k, v any) bool {
		out[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	if _, ok := out["uid_rejected"]; !ok {
		out["uid_rejected"] = 0
	}
	return out
}

func (s *Server) count(code string) {
	v, _ := s.counts.LoadOrStore(code, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}

// Serve 在 ln 上提供服务，直到 ctx 结束或 ln 关闭。
func (s *Server) Serve(ctx context.Context, ln *net.UnixListener) error {
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			time.Sleep(10 * time.Millisecond) // 临时错误（fd 用尽等）：稍等再接
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer c.Close()
			s.handle(ctx, c)
		}()
	}
}

func (s *Server) reply(c *net.UnixConn, code string, f *os.File) {
	r := Reply{OK: code == "", Code: code}
	b, _ := json.Marshal(r)
	_ = fdpass.Send(c, b, f)
	if code == "" {
		s.count("ok")
	} else {
		s.count(code)
	}
}

func (s *Server) handle(ctx context.Context, c *net.UnixConn) {
	uid, err := fdpass.PeerUID(c)
	if err != nil || s.AllowUID == nil || !s.AllowUID(uid) {
		s.count("uid_rejected") // 不回应答，直接关
		return
	}
	_ = c.SetReadDeadline(time.Now().Add(readTimeout))
	msg, f, err := fdpass.Recv(c, MaxConfigRequest)
	if f != nil {
		f.Close() // 请求不应带 fd
		err = errBad
	}
	if err != nil {
		s.reply(c, CodeBadRequest, nil)
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	req, err := parseRequest(msg)
	if err == nil && req.Op == "config" {
		rc, cerr := decodeConfig(req)
		if cerr != nil {
			s.reply(c, CodeBadRequest, nil) // 旧配置保持
			return
		}
		s.rt.Store(rc)
		s.count("config")
		s.reply(c, "", nil)
		return
	}
	t, err := decodeRequest(msg)
	if err != nil {
		s.reply(c, CodeBadRequest, nil)
		return
	}
	// 代数检查（m3-plan v2.9）：没有配置或代数不一致时不解析、不拨号（按 IP 的请求同样检查，
	// 置顶追加名单不会用旧的）。这次请求全程只用这一份配置。
	rc, ok := s.configFor(t.gen)
	if !ok {
		s.reply(c, CodeStaleConfig, nil)
		return
	}
	max := s.MaxInflight
	if max <= 0 {
		max = DefaultMaxInflight
	}
	if s.inflight.Add(1) > int64(max) {
		s.inflight.Add(-1)
		s.reply(c, CodeBusy, nil) // 不排队
		return
	}
	defer s.inflight.Add(-1)
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, code := s.serve(ctx, rc, t)
	if code != "" {
		s.reply(c, code, nil)
		return
	}
	defer conn.Close()
	fc, ok := conn.(interface{ File() (*os.File, error) })
	if !ok {
		s.reply(c, CodeDialFailed, nil)
		return
	}
	file, err := fc.File()
	if err != nil {
		s.reply(c, CodeDialFailed, nil)
		return
	}
	defer file.Close()
	s.reply(c, "", file)
	// 发出 fd 之后，持有自己这份副本，直到客户端收下（它收到后关闭 unix 连接，这里读到 EOF）。
	// 原因（M3-4 实测）：fd 处在“已发出、未收到”的窗口时，如果发送方已不持有副本，
	// 内核回收在途 fd 时偶尔会把这个套接字冲掉，接收方随后读到 EOF。
	_ = c.SetReadDeadline(time.Now().Add(ackTimeout))
	_, _ = io.Copy(io.Discard, io.LimitReader(c, 64))
}

// serve 处理一个已校验的请求：判定目标、（必要时）解析、逐个再判、拨号。
// 解析和拨号的出口各只有一处：s.lookup 和 s.dialOne。
func (s *Server) serve(ctx context.Context, rc *runtimeConfig, t target) (net.Conn, string) {
	var addrs []netip.Addr
	pinned := s.pinnedExtra(rc)
	if t.host != "" {
		if p, _ := rules.IsPinned(t.host, pinned); p {
			return nil, CodePinned // 置顶域名：不解析、不拨号
		}
		res, chain, err := s.lookup(ctx, rc, t.host)
		// 别名链上出现的任何一个名字是置顶域名：整个请求拒绝（普通域名别名到 x.claude.ai，
		// 而那个子域托管在 Anthropic 地址段之外时，只判最初的名字和最终的地址会漏掉）。
		for _, n := range chain {
			if p, ok := rules.IsPinned(n, pinned); p || !ok {
				return nil, CodePinned
			}
		}
		if err != nil {
			if timedOut(ctx) {
				return nil, CodeTimeout
			}
			return nil, CodeResolveFailed
		}
		// 解析结果逐个再判：任何一个在 Anthropic 段，整个请求拒绝；其余被禁止的剔掉。
		for _, a := range res {
			switch checkDst(a) {
			case CodePinned:
				return nil, CodePinned
			case "":
				addrs = append(addrs, a)
			}
		}
		if len(addrs) == 0 {
			return nil, CodeForbiddenDst
		}
	} else {
		if code := checkDst(t.ip); code != "" {
			return nil, code
		}
		addrs = []netip.Addr{t.ip}
	}
	for i, a := range addrs {
		if i >= maxTries {
			break
		}
		if code := checkDst(a); code != "" { // 拨号前再判一次
			return nil, code
		}
		c, err := s.dialOne(ctx, t.net, netip.AddrPortFrom(a, t.port))
		if err == nil {
			return c, ""
		}
		if timedOut(ctx) {
			return nil, CodeTimeout
		}
	}
	return nil, CodeDialFailed
}

// lookup 是解析的唯一出口。返回地址和别名链上的名字。
func (s *Server) lookup(ctx context.Context, rc *runtimeConfig, host string) ([]netip.Addr, []string, error) {
	var servers []netip.Addr
	if rc != nil {
		servers = rc.dns // 下发的配置优先；收到配置之前只有单独运行时的 --dns（DNSServers）可用
	} else if s.DNSServers != nil {
		servers = s.DNSServers()
	}
	r := &resolver{dial: s.Dialer, servers: servers, newID: s.newID}
	return r.lookupA(ctx, host)
}

// dialOne 是拨号的唯一出口（绑物理网卡）。
func (s *Server) dialOne(ctx context.Context, network string, dst netip.AddrPort) (net.Conn, error) {
	if s.Dialer == nil {
		return nil, errors.New("没有配置拨号器")
	}
	return s.Dialer.DialDirect(ctx, network, dst)
}

// timedOut：ctx 已结束，或它的期限已过（单次查询的读期限可能比 ctx 的计时器先触发一点）。
func timedOut(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	d, ok := ctx.Deadline()
	return ok && !time.Now().Before(d)
}

// pinnedExtra 是置顶追加名单：服务端自己的（PinnedExtra）加下发的。内置名单在 rules.IsPinned 里永远生效。
func (s *Server) pinnedExtra(rc *runtimeConfig) []string {
	out := append([]string(nil), s.PinnedExtra...)
	if rc != nil {
		out = append(out, rc.pinned...)
	}
	return out
}

// configFor 按请求里的代数取配置：收到过配置时代数要一致；还没收到配置时，只有单独运行
// （带 --dns，DNSServers 非空）按代数 0 处理，其余一律不一致。
func (s *Server) configFor(gen uint64) (*runtimeConfig, bool) {
	if rc := s.rt.Load(); rc != nil {
		return rc, rc.gen == gen
	}
	return nil, s.DNSServers != nil && gen == 0
}

// Configured 报告是否收到过下发的配置。
func (s *Server) Configured() bool { return s.rt.Load() != nil }
