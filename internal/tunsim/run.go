package tunsim

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"

	"claude-abroad/internal/fakedns"
	"claude-abroad/internal/redact"
	"claude-abroad/internal/stack"
	"claude-abroad/internal/tun"
	"claude-abroad/internal/wire"
)

// Config 是一次模拟运行的参数。
type Config struct {
	DataSock string         // abroad-vm 的 data.sock（Opener 为 nil 时必填）
	Name     string         // 默认 tunsim.test
	UDPPort  int            // 默认 4500
	RealIP   netip.AddrPort // 默认 203.0.113.9:80
	Timeout  time.Duration  // 单步上限，默认 30 秒
	// UDPTarget 不为零值时，步骤 c（和 f 里关闸门后的 UDP 检查）发往这个真实 IP，步骤名为 c_udp_realip；
	// 零值时发往 假地址:UDPPort（c_udp_fake）。主代理答不了 DoH 时（例如开发上游），mihomo 解析不了
	// UDP 的域名目标，包会被丢（design 7.2），这时用真实 IP。
	UDPTarget netip.AddrPort
	// DoHName 非空时，在 e 之后、f 之前加一步 h_doh：经 198.18.0.2:53 查它的 TXT，要求 NOERROR 且有应答
	// （fake-IP DNS 经主代理的 DoH 解析，M2-5d）；f 里再查一次，要求关闸门后得到 SERVFAIL。
	DoHName string
	// Upstream 测试注入；为 nil 且 DoHName 非空时用经 Opener 的 fakedns.DoHUpstream（系统根证书）。
	// 不论哪种，Run 都在外面包一层闸门检查。
	Upstream fakedns.Upstream
	// Opener 测试注入；为 nil 时用只连 unix DataSock 的 wire.Dialer（Run 结束时由 Run 关闭）。
	Opener stack.Opener
}

// Step 是一步的结果。Err 已脱敏。
type Step struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	Err  string `json:"err,omitempty"`
}

// Report 是一次运行的报告（一行 JSON；只有步骤结果和计数，没有出口 IP 之类的敏感内容）。
type Report struct {
	OK              bool             `json:"ok"`
	Steps           []Step           `json:"steps"`
	OpensBeforeGate int64            `json:"opens_before_gate"` // 关闸门前 Opener 的调用次数
	OpensAfterGate  int64            `json:"opens_after_gate"`  // 步骤 f 结束时的调用次数（应与关闸门前相同）
	Stack           stack.Stats      `json:"stack"`
	Proxy           stack.ProxyStats `json:"proxy"`
}

type gate struct{ open atomic.Bool }

func (g *gate) Open() bool { return g.open.Load() }

// gatedUpstream 在闸门关闭时直接返回错误（fake-IP DNS 答 SERVFAIL），不调用内层上游。
type gatedUpstream struct {
	inner fakedns.Upstream
	g     *gate
}

func (u gatedUpstream) Exchange(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
	if !u.g.Open() {
		return nil, fakedns.ErrGateClosed
	}
	return u.inner.Exchange(ctx, q)
}

// countingOpener 记录 Opener 的调用次数。
type countingOpener struct {
	inner stack.Opener
	n     atomic.Int64
}

func (o *countingOpener) Open(ctx context.Context, h wire.Header) (net.Conn, error) {
	o.n.Add(1)
	return o.inner.Open(ctx, h)
}

// fakeNet、reservedNet 与 stack.Proxy 的判定一致。
var (
	fakeNet     = netip.MustParsePrefix("198.18.0.0/15")
	reservedNet = netip.MustParsePrefix("198.18.0.0/24")
)

func (c Config) withDefaults() Config {
	if c.Name == "" {
		c.Name = "tunsim.test"
	}
	if c.UDPPort <= 0 {
		c.UDPPort = 4500
	}
	if !c.RealIP.IsValid() {
		c.RealIP = netip.MustParseAddrPort("203.0.113.9:80")
	}
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
	return c
}

// UnixOpener 返回只连 unix socket path 的 wire.Dialer。
func UnixOpener(path string) *wire.Dialer {
	return &wire.Dialer{Config: wire.DefaultSessionConfig(), Dial: func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}}
}

// get204 在 conn 上发 GET /generate_204，要求 204 并读到 EOF。
func get204(conn net.Conn, host string, timeout time.Duration) error {
	_ = conn.SetDeadline(time.Now().Add(timeout))
	req := "GET /generate_204 HTTP/1.1\r\nHost: " + host + "\r\nConnection: close\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		return fmt.Errorf("写请求：%w", err)
	}
	body, err := io.ReadAll(conn)
	if err != nil {
		return fmt.Errorf("读应答（应读到 EOF）：%w", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(body)), nil)
	if err != nil {
		return fmt.Errorf("解析应答：%w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("状态码 %d，应为 204", resp.StatusCode)
	}
	return nil
}

// holdDropProblem 判断“保持的连接在 DropAll 后是否被断开”：读到 EOF 或连接错误算断开（返回空串）；
// 读成功或读超时说明没有被断开。
func holdDropProblem(err error) string {
	if err == nil {
		return "已有的 TCP 没有被断开（还能读到数据）"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "已有的 TCP 没有被断开（读超时）"
	}
	return ""
}

func refused(err error) bool { return err != nil && strings.Contains(err.Error(), "refused") }

// Run 按顺序跑 a–g（给了 DoHName 时在 e、f 之间多一步 h_doh）（设计见 docs/m2-plan.md 的 M2-5c）。某一步失败后继续跑后面的步骤；g 总是执行。
func Run(ctx context.Context, cfg Config) (rep Report) {
	cfg = cfg.withDefaults()
	add := func(name string, err error) bool {
		st := Step{Name: name, OK: err == nil}
		if err != nil {
			st.Err = redact.String(err.Error())
		}
		rep.Steps = append(rep.Steps, st)
		return err == nil
	}
	defer func() {
		rep.OK = len(rep.Steps) > 0
		for _, s := range rep.Steps {
			rep.OK = rep.OK && s.OK
		}
	}()

	var own *wire.Dialer
	inner := cfg.Opener
	if inner == nil {
		own = UnixOpener(cfg.DataSock)
		inner = own
	}
	op := &countingOpener{inner: inner}
	g := &gate{}
	g.open.Store(true)
	pool := fakedns.DefaultPool()
	srv := &fakedns.Server{Pool: pool, Bypass: fakedns.NewBypass(), Reverse: fakedns.NewReverse()}
	var doh *fakedns.DoHUpstream
	switch {
	case cfg.Upstream != nil:
		srv.Upstream = gatedUpstream{inner: cfg.Upstream, g: g}
	case cfg.DoHName != "":
		doh = &fakedns.DoHUpstream{Opener: op, Gate: g} // 与代理流同一个 countingOpener
		srv.Upstream = gatedUpstream{inner: doh, g: g}
	}
	proxy := &stack.Proxy{Pool: pool, DNS: srv, Gate: g, Opener: op}
	proxy.SetEnabled(true) // tunsim 模拟的是启用状态的数据面（没设状态时网络栈按停用处理）
	a, b := tun.Pipe(1500)
	st, err := stack.New(a, proxy, stack.Options{})
	if err != nil {
		add("setup", err)
		return rep
	}
	cl, err := NewClient(b, netip.Addr{})
	if err != nil {
		st.Close()
		add("setup", err)
		return rep
	}
	defer cl.Close()
	step := func() (context.Context, context.CancelFunc) { return context.WithTimeout(ctx, cfg.Timeout) }
	dnsAddr := stack.DefaultDNSAddr
	notA := errors.New("步骤 a 未通过")

	// a. DNS 查 A：拿到 fake 段（不在保留 /24 里）的地址。
	var fake netip.Addr
	aOK := func() bool {
		c, cancel := step()
		defer cancel()
		addr, err := cl.LookupA(c, cfg.Name, dnsAddr)
		if err == nil && (!fakeNet.Contains(addr) || reservedNet.Contains(addr)) {
			err = fmt.Errorf("拿到的地址不在 fake 段的可分配范围")
		}
		fake = addr
		return add("a_dns", err)
	}()

	// b. TCP 连 假地址:80，GET /generate_204 → 204。
	func() {
		if !aOK {
			add("b_tcp_fake", notA)
			return
		}
		c, cancel := step()
		defer cancel()
		conn, err := cl.DialTCP(c, netip.AddrPortFrom(fake, 80))
		if err != nil {
			add("b_tcp_fake", fmt.Errorf("连接：%w", err))
			return
		}
		defer conn.Close()
		add("b_tcp_fake", get204(conn, cfg.Name, cfg.Timeout))
	}()

	// c. UDP 发 32 字节随机数据 → 原样回显，回包源地址等于发送的目的地址。
	// 目的地址：UDPTarget（真实 IP）或 假地址:UDPPort。
	udpDst := func() netip.AddrPort {
		if cfg.UDPTarget.IsValid() {
			return cfg.UDPTarget
		}
		return netip.AddrPortFrom(fake, uint16(cfg.UDPPort))
	}
	cName := "c_udp_fake"
	if cfg.UDPTarget.IsValid() {
		cName = "c_udp_realip"
	}
	func() {
		if !aOK && !cfg.UDPTarget.IsValid() {
			add(cName, notA)
			return
		}
		u, err := cl.DialUDP()
		if err != nil {
			add(cName, err)
			return
		}
		defer u.Close()
		dst := udpDst()
		msg := make([]byte, 32)
		_, _ = rand.Read(msg)
		_ = u.SetReadDeadline(time.Now().Add(cfg.Timeout))
		if _, err := u.WriteTo(msg, net.UDPAddrFromAddrPort(dst)); err != nil {
			add(cName, err)
			return
		}
		buf := make([]byte, 2048)
		n, from, err := u.ReadFrom(buf)
		switch {
		case err != nil:
			err = fmt.Errorf("没有回显：%w", err)
		case !bytes.Equal(buf[:n], msg):
			err = errors.New("回显内容不对")
		case from.(*net.UDPAddr).AddrPort() != dst:
			err = errors.New("回包源地址不是原目的地址")
		}
		add(cName, err)
	}()

	// d. TCP 连真实 IP，GET → 204。
	func() {
		c, cancel := step()
		defer cancel()
		conn, err := cl.DialTCP(c, cfg.RealIP)
		if err != nil {
			add("d_tcp_realip", fmt.Errorf("连接：%w", err))
			return
		}
		defer conn.Close()
		add("d_tcp_realip", get204(conn, cfg.RealIP.Addr().String(), cfg.Timeout))
	}()

	// e. 连没有映射的假地址 → connection refused。
	func() {
		noMap := netip.MustParseAddr("198.19.255.250")
		if noMap == fake {
			noMap = noMap.Next()
		}
		c, cancel := step()
		defer cancel()
		conn, err := cl.DialTCP(c, netip.AddrPortFrom(noMap, 443))
		if err == nil {
			conn.Close()
			add("e_no_mapping", errors.New("连接没有被拒"))
			return
		}
		if !refused(err) {
			add("e_no_mapping", fmt.Errorf("应为 connection refused：%w", err))
			return
		}
		add("e_no_mapping", nil)
	}()

	// h. 经主代理的 DoH 查 DoHName 的 TXT：NOERROR 且有应答。
	if cfg.DoHName != "" {
		func() {
			c, cancel := step()
			defer cancel()
			m, err := cl.Query(c, cfg.DoHName, dns.TypeTXT, dnsAddr)
			switch {
			case err != nil:
			case m.Rcode != dns.RcodeSuccess:
				err = errors.New("DNS 应答 " + dns.RcodeToString[m.Rcode])
			case len(m.Answer) == 0:
				err = errors.New("TXT 查询没有应答记录")
			}
			add("h_doh", err)
		}()
	}

	// f. 关闸门 + DropAll：已有的流断开；新 TCP 被拒；UDP 无回显；DNS 仍应答；Opener 调用次数不变。
	func() {
		if !aOK {
			add("f_gate", notA)
			return
		}
		c, cancel := step()
		defer cancel()
		tcp0 := proxy.Stats().TCPProxied
		hold, err := cl.DialTCP(c, netip.AddrPortFrom(fake, 80))
		if err != nil {
			add("f_gate", fmt.Errorf("建保持的连接：%w", err))
			return
		}
		defer hold.Close()
		// 等这条流真的交出去（开流成功）再关闸门。
		for deadline := time.Now().Add(cfg.Timeout); proxy.Stats().TCPProxied <= tcp0 && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
		if proxy.Stats().TCPProxied <= tcp0 {
			add("f_gate", errors.New("保持的连接没有交给虚拟机"))
			return
		}
		before := op.n.Load()
		rep.OpensBeforeGate = before
		ps0 := proxy.Stats()
		g.open.Store(false)
		proxy.DropAll()
		var problems []string
		// 读期限单独设短：读超时说明没有被断开，不能让它耗掉整步的期限、让后面的检查报出误导的原因。
		_ = hold.SetReadDeadline(time.Now().Add(min(cfg.Timeout, 5*time.Second)))
		_, rerr := hold.Read(make([]byte, 1))
		if p := holdDropProblem(rerr); p != "" {
			problems = append(problems, p)
		}
		if conn, err := cl.DialTCP(c, netip.AddrPortFrom(fake, 80)); err == nil {
			conn.Close()
			problems = append(problems, "关闸门后新 TCP 没有被拒")
		} else if !refused(err) {
			problems = append(problems, "关闸门后新 TCP 不是 connection refused")
		}
		if u, err := cl.DialUDP(); err != nil {
			problems = append(problems, "关闸门后的 UDP 检查没能执行（建 UDP 端点失败）")
		} else {
			_, _ = u.WriteTo([]byte("gate-closed"), net.UDPAddrFromAddrPort(udpDst()))
			_ = u.SetReadDeadline(time.Now().Add(time.Second))
			if _, _, err := u.ReadFrom(make([]byte, 64)); err == nil {
				problems = append(problems, "关闸门后 UDP 仍有回显")
			}
			u.Close()
		}
		if _, err := cl.LookupA(c, cfg.Name, dnsAddr); err != nil {
			problems = append(problems, "关闸门后 DNS 没有应答")
		}
		if cfg.DoHName != "" {
			if m, err := cl.Query(c, cfg.DoHName, dns.TypeTXT, dnsAddr); err != nil || m.Rcode != dns.RcodeServerFailure {
				problems = append(problems, "关闸门后经 DoH 的查询应得到 SERVFAIL")
			}
		}
		rep.OpensAfterGate = op.n.Load()
		ps1 := proxy.Stats()
		if rep.OpensAfterGate != before {
			problems = append(problems, "关闸门后 Opener 又被调用了")
		}
		if ps1.GateClosed < ps0.GateClosed+2 { // 新 TCP、UDP 各被闸门挡一次
			problems = append(problems, "gate_closed 增长不足 2（新 TCP 和 UDP 应各被闸门挡一次）")
		}
		if ps1.Dropped <= ps0.Dropped {
			problems = append(problems, "dropped 没有增长")
		}
		if len(problems) > 0 {
			add("f_gate", errors.New(strings.Join(problems, "；")))
			return
		}
		add("f_gate", nil)
	}()

	// g. 按“停用第 ② 步”的顺序收尾：关闸门 → DropAll → 关 Run 自己建的 Dialer → Stack.Close（10 秒内返回）。
	func() {
		g.open.Store(false)
		proxy.DropAll()
		if doh != nil {
			doh.Close()
		}
		if own != nil {
			_ = own.Close()
		}
		done := make(chan struct{})
		go func() { st.Close(); close(done) }()
		select {
		case <-done:
			add("g_close", nil)
		case <-time.After(10 * time.Second):
			add("g_close", errors.New("Stack.Close 10 秒内没有返回"))
		}
	}()
	rep.Stack, rep.Proxy = st.Stats(), proxy.Stats()
	return rep
}
