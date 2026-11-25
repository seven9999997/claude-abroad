package daemon

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"claude-abroad/internal/claudecfg"
	"claude-abroad/internal/preflight"
	"claude-abroad/internal/rules"
	"claude-abroad/internal/stack"
	"claude-abroad/internal/state"
)

// 本机代理端口（设计 10.2 第四层，M6-3b，docs/fallback-plan.md v1.1 第 3.2 节）：Claude Code 被托管设置指向
// 127.0.0.1 上的固定端口。只接受 CONNECT 和带完整地址的明文 HTTP（不带请求体），不做任何解密、不记请求内容。
//   - 未启用的各状态（DISABLED、ENABLING、RESTORING、RESTORED）一律回 403，带一句说明，不向外拨任何连接；
//   - 已启用：交给网络栈的 ServeProxyFlow——和接管流量同一个判定入口、同一个拨号入口、同一张连接表
//     （闸门由开变关时一起断开）；识别不出连接方进程的按 Claude 处理；
//   - abroadd 自己不往外拨：走代理经数据通道、直连经直连拨号模块（都在网络栈的 dispatch 里）。
const (
	cpMaxHeader = 16 << 10         // 请求头上限
	cpHandshake = 10 * time.Second // 读完请求头的时限
	cpMaxConns  = 64               // 同时连接数上限
	cpRetry     = 5 * time.Second  // 没监听上（端口被占用）时重试的间隔
	cpAcceptMax = time.Second      // Accept 持续出错时退避的上限
)

// ClaudeProxyStatus 是第四层的状态（状态接口用；不含路径和地址）。
type ClaudeProxyStatus struct {
	Settings  *claudecfg.Status `json:"settings,omitempty"` // 托管设置的自检（接了才有）
	Listening bool              `json:"listening"`          // 本机代理端口在听
	Port      int               `json:"port,omitempty"`
	ListenErr string            `json:"listen_error,omitempty"` // 没在听的原因（端口被占用等）
	Effective bool              `json:"effective"`              // 托管设置生效且端口在听
}

type claudeProxy struct {
	d         *Daemon
	addr      string
	retry     time.Duration // 0 用 cpRetry
	handshake time.Duration // 0 用 cpHandshake
	listen    func(addr string) (net.Listener, error)

	lmu  sync.Mutex // 保护 ln、port、err（重试协程会改）
	ln   net.Listener
	port int
	err  string

	active  atomic.Int64
	refused atomic.Int64 // 未启用时回 403 的次数
	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	wg      sync.WaitGroup
	ctx     context.Context
	cancel  context.CancelFunc
}

// startClaudeProxy 在 Options.ClaudeProxyAddr 上监听（必须是回环地址）。监听失败（端口被占用）时 abroadd 照常启动，
// 状态里标出“第四层未生效”，冲突检查里按“必须处理”列出。
func (d *Daemon) startClaudeProxy() {
	addr := d.opts.ClaudeProxyAddr
	if addr == "" {
		return
	}
	cp := &claudeProxy{d: d, addr: addr, retry: d.opts.ClaudeProxyRetry, handshake: d.opts.ClaudeProxyHandshake,
		conns: map[net.Conn]struct{}{}, listen: listenLoopbackTCP}
	cp.ctx, cp.cancel = context.WithCancel(context.Background())
	d.cproxy = cp
	ap, err := netip.ParseAddrPort(addr)
	if err != nil || !ap.Addr().IsLoopback() {
		cp.err = "本机代理端口只能监听回环地址"
		d.events.Add("claude_proxy", "listen_failed", cp.err)
		return // 配置错误：不重试
	}
	if !cp.tryListen() {
		d.events.Add("claude_proxy", "listen_failed", cp.err)
		cp.wg.Add(1)
		go cp.retryLoop()
	}
}

// listenLoopbackTCP 是唯一的监听：地址在 startClaudeProxy 里已校验是回环（源码检查要求本文件有 IsLoopback 校验）。
func listenLoopbackTCP(addr string) (net.Listener, error) { return net.Listen("tcp", addr) }

// tryListen 监听一次；成功时开始接受连接。
func (cp *claudeProxy) tryListen() bool {
	ln, err := cp.listen(cp.addr)
	cp.lmu.Lock()
	defer cp.lmu.Unlock()
	if err != nil {
		cp.err = "端口被别的程序占着或不能监听"
		return false
	}
	cp.ln, cp.err = ln, ""
	if ta, ok := ln.Addr().(*net.TCPAddr); ok {
		cp.port = ta.Port
	}
	cp.wg.Add(1)
	go cp.acceptLoop(ln)
	return true
}

// retryLoop：端口被占用时按固定间隔重试，监听上之后记一条恢复事件并停止（冲突检查读当前状态，自动解除）。
func (cp *claudeProxy) retryLoop() {
	defer cp.wg.Done()
	iv := cp.retry
	if iv <= 0 {
		iv = cpRetry
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-cp.ctx.Done():
			return
		case <-t.C:
		}
		if cp.tryListen() {
			cp.d.events.Add("claude_proxy", "listen_recovered", "")
			return
		}
	}
}

// listening 报告端口在听；状态和冲突检查读它。
func (cp *claudeProxy) listening() (bool, int, string) {
	cp.lmu.Lock()
	defer cp.lmu.Unlock()
	return cp.ln != nil, cp.port, cp.err
}

func (cp *claudeProxy) stop() {
	if cp == nil {
		return
	}
	cp.cancel()
	cp.lmu.Lock()
	if cp.ln != nil {
		cp.ln.Close()
	}
	cp.lmu.Unlock()
	cp.mu.Lock()
	for c := range cp.conns {
		c.Close()
	}
	cp.mu.Unlock()
	cp.wg.Wait()
}

func (cp *claudeProxy) acceptLoop(ln net.Listener) {
	defer cp.wg.Done()
	var backoff time.Duration
	for {
		c, err := ln.Accept()
		if err != nil {
			if cp.ctx.Err() != nil {
				return
			}
			// 持续性错误（文件描述符用尽等）：退避，不空转占满一个核
			if backoff == 0 {
				backoff = 5 * time.Millisecond
			} else if backoff *= 2; backoff > cpAcceptMax {
				backoff = cpAcceptMax
			}
			select {
			case <-cp.ctx.Done():
				return
			case <-time.After(backoff):
			}
			continue
		}
		backoff = 0
		if cp.active.Add(1) > cpMaxConns {
			cp.active.Add(-1)
			writeProxyError(c, http.StatusServiceUnavailable, "too_many_connections", "同时连接数已到上限")
			c.Close()
			continue
		}
		cp.mu.Lock()
		cp.conns[c] = struct{}{}
		cp.mu.Unlock()
		cp.wg.Add(1)
		go func() {
			defer cp.wg.Done()
			defer cp.active.Add(-1)
			defer func() {
				cp.mu.Lock()
				delete(cp.conns, c)
				cp.mu.Unlock()
				c.Close()
			}()
			cp.handle(c)
		}()
	}
}

// writeProxyError 回一个错误应答：响应头 X-Claude-Abroad 是纯 ASCII 的短代码（各家 HTTP 客户端对头里的非 ASCII
// 容忍度不一，不能让“解析响应失败”盖过真正的原因），中文说明在正文里。
func writeProxyError(c net.Conn, code int, short, why string) {
	_ = c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	body := "Claude 出国：" + why + "\n"
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\nX-Claude-Abroad: %s\r\n\r\n%s",
		code, http.StatusText(code), len(body), short, body)
}

// 拒绝的说明（未启用时）。
const cpNotEnabled = "现在不是已启用状态（回国、停用或正在切换），Claude 出国不允许 Claude Code 联网"

// target 解析出的目的地。
type cpTarget struct {
	host string
	ip   netip.Addr
	port uint16
}

func parseTarget(hostport string, defPort uint16) (cpTarget, bool) {
	h, p, err := net.SplitHostPort(hostport)
	if err != nil {
		h, p = hostport, ""
	}
	t := cpTarget{port: defPort}
	if p != "" {
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil || n == 0 {
			return t, false
		}
		t.port = uint16(n)
	}
	if t.port == 0 {
		return t, false
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if a, err := netip.ParseAddr(h); err == nil {
		a = a.Unmap()
		// 回环、未指定、组播、链路本地：不经这个端口（也挡住指向端口自己的转圈）
		if a.IsLoopback() || a.IsUnspecified() || a.IsMulticast() || a.IsLinkLocalUnicast() || a.Zone() != "" {
			return t, false
		}
		t.ip = a
		return t, true
	}
	n, err := rules.NormalizeName(h)
	if err != nil || n == "localhost" || strings.HasSuffix(n, ".localhost") {
		return t, false
	}
	t.host = n
	return t, true
}

// prefixConn 让网络栈先读到已经缓冲的字节（请求头之后客户端早发的、或改写后的明文 HTTP 请求），再读连接本身。
type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixConn) Read(b []byte) (int, error) { return c.r.Read(b) }
func (c *prefixConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (cp *claudeProxy) handle(c net.Conn) {
	hs := cp.handshake
	if hs <= 0 {
		hs = cpHandshake
	}
	_ = c.SetReadDeadline(time.Now().Add(hs))
	lr := &io.LimitedReader{R: c, N: cpMaxHeader}
	br := bufio.NewReaderSize(lr, 4096)
	req, err := http.ReadRequest(br)
	if err != nil {
		if lr.N <= 0 {
			writeProxyError(c, http.StatusRequestHeaderFieldsTooLarge, "header_too_large", "请求头太大")
		} else {
			writeProxyError(c, http.StatusBadRequest, "bad_request", "请求不合法或超时")
		}
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	lr.N = math.MaxInt64

	var t cpTarget
	var ok bool
	var first []byte // 明文 HTTP：改写成相对地址后的请求头
	switch {
	case req.Method == http.MethodConnect:
		t, ok = parseTarget(req.Host, 0)
	case req.URL != nil && req.URL.Scheme == "http" && req.URL.Host != "":
		if req.ContentLength != 0 || len(req.TransferEncoding) > 0 {
			writeProxyError(c, http.StatusNotImplemented, "body_unsupported", "明文 HTTP 不支持带请求体")
			return
		}
		t, ok = parseTarget(req.URL.Host, 80)
		first = rewritePlain(req)
	default:
		writeProxyError(c, http.StatusMethodNotAllowed, "method_unsupported", "只支持 CONNECT 和带完整地址的明文 HTTP")
		return
	}
	if !ok {
		writeProxyError(c, http.StatusForbidden, "bad_target", "目的地不合法（不经这个端口访问本机和局域网链路地址）")
		return
	}
	if cp.d.fileState() != state.Enabled {
		cp.refused.Add(1)
		writeProxyError(c, http.StatusForbidden, "not_enabled", cpNotEnabled)
		return
	}
	dp := cp.d.opts.Sys.Data
	if dp == nil || dp.proxy == nil {
		writeProxyError(c, http.StatusServiceUnavailable, "no_dataplane", "数据面没有接上")
		return
	}
	src, _ := netip.ParseAddrPort(c.RemoteAddr().String())
	local, _ := netip.ParseAddrPort(c.LocalAddr().String())
	var rd io.Reader = br
	if first != nil {
		rd = io.MultiReader(bytes.NewReader(first), br)
	}
	pc := &prefixConn{Conn: c, r: rd}
	isConnect := req.Method == http.MethodConnect
	dp.proxy.ServeProxyFlow(cp.ctx, stack.ProxyFlow{Src: src, Local: local, Host: t.host, IP: t.ip, Port: t.port}, pc, func(ok bool) error {
		if !ok {
			writeProxyError(c, http.StatusForbidden, "denied", "按规则拒绝，或闸门关着（Claude 只能走主代理）")
			return nil
		}
		if isConnect {
			_, err := io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
			return err
		}
		return nil
	})
}

// rewritePlain 把带完整地址的明文 HTTP 请求改写成发给目的地的形式（相对地址、去掉逐跳的代理头、一次一个请求）。
func rewritePlain(req *http.Request) []byte {
	var b bytes.Buffer
	uri := req.URL.RequestURI()
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\nHost: %s\r\n", req.Method, uri, req.URL.Host)
	h := req.Header.Clone()
	for _, k := range []string{"Proxy-Connection", "Proxy-Authorization", "Connection", "Keep-Alive", "Content-Length", "Transfer-Encoding", "Upgrade"} {
		h.Del(k)
	}
	_ = h.Write(&b)
	b.WriteString("Connection: close\r\n\r\n")
	return b.Bytes()
}

// claudeCodeStatus 是第四层的状态：托管设置的自检加本机代理端口。
func (d *Daemon) claudeCodeStatus() *ClaudeProxyStatus {
	s := d.opts.Sys.ClaudeSettings
	cp := d.cproxy
	if s == nil && cp == nil {
		return nil
	}
	st := &ClaudeProxyStatus{}
	if s != nil {
		cs := s.Check()
		st.Settings = &cs
	}
	if cp != nil {
		st.Listening, st.Port, st.ListenErr = cp.listening()
	}
	st.Effective = st.Settings != nil && st.Settings.Effective && st.Listening
	return st
}

// claudeProxyDown 报告本机代理端口没在听（冲突检查的“必须处理”项用）。没配置端口时为 false。
func (d *Daemon) claudeProxyDown() bool {
	if d.cproxy == nil {
		return false
	}
	ok, _, _ := d.cproxy.listening()
	return !ok
}

// preflightChecker 取出系统依赖里的冲突检查器（真实系统和测试用的 PreflightHints）；没有时为 nil。
func preflightChecker(p state.Preflight) *preflight.Checker {
	switch v := p.(type) {
	case realPreflight:
		return v.C
	case PreflightHints:
		return v.C
	case *PreflightHints:
		return v.C
	}
	return nil
}

// checkIntegrity 启动时做一次完整性校验（M6-7）：不通过时记事件；状态是已启用的，转成停用（只保留断网规则、不接管）。
func (d *Daemon) checkIntegrity(ctx context.Context) {
	c := d.opts.Sys.Integrity
	if c == nil {
		return
	}
	r := c.Check()
	d.integrity = &r
	if r.OK {
		return
	}
	d.events.Add("integrity", "failed", r.Summary())
	if f, err := d.store.Load(); err == nil && f.State == state.Enabled {
		if err := d.m.Disable(ctx, false); err != nil {
			d.events.Add("integrity", "disable_failed", "完整性校验不通过，转成停用失败（闸门保持关闭）")
		}
	}
}
