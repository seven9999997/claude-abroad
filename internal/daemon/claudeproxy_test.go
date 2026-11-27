package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"claude-abroad/internal/gatesm"
	"claude-abroad/internal/rules"
	"claude-abroad/internal/state"
)

// ---- M6-3b 第四层的本机代理端口：daemon 里的端到端（假数据通道、假直连拨号模块、自己写的客户端；不启动 Claude Code）----

// cpIdent：在 m3Ident 之外，23xxx 端口识别不出（Unknown），24xxx 是 Claude。
type cpIdent struct{}

func (cpIdent) Identify(ctx context.Context, n string, src, dst netip.AddrPort) (rules.AppID, bool) {
	switch p := src.Port(); {
	case p >= 23000 && p < 24000:
		return rules.Unknown, false
	case p >= 24000 && p < 25000:
		return "com.anthropic.claudecode", true
	}
	return m3Ident{}.Identify(ctx, n, src, dst)
}

// armVM 包住虚拟机：armed 时 Start、Stop 卡住，直到放行（让状态停在启用中、恢复中）。
type armVM struct {
	inner        state.VM
	armStart     atomic.Bool
	armStop      atomic.Bool
	entered, rel chan struct{}
	enterOnce    sync.Once
}

func newArmVM(inner state.VM) *armVM {
	return &armVM{inner: inner, entered: make(chan struct{}), rel: make(chan struct{})}
}

func (v *armVM) block() {
	v.enterOnce.Do(func() { close(v.entered) })
	<-v.rel
}

func (v *armVM) Start(ctx context.Context) error {
	if v.armStart.Load() {
		v.block()
	}
	return v.inner.Start(ctx)
}

func (v *armVM) Stop(ctx context.Context) error {
	if v.armStop.Load() {
		v.block()
	}
	return v.inner.Stop(ctx)
}

// postRetryBusy 发请求，遇到 409 busy（监护循环正好占着忙标记）就重试。
func postRetryBusy(t *testing.T, d *Daemon, path string) int {
	var body []byte
	for i := 0; i < 200; i++ {
		code, b := call(t, d, "POST", path, body)
		if code == 409 && strings.Contains(string(b), `"needs_confirm"`) { // 两步确认：带上确认码再发
			var cn ConfirmNeeded
			if json.Unmarshal(b, &cn) == nil && cn.Confirm != "" {
				body, _ = json.Marshal(ConfirmRequest{Confirm: cn.Confirm})
				continue
			}
		}
		if code != 409 || !strings.Contains(string(b), `"busy"`) {
			if code != 200 {
				t.Logf("%s：%d %s", path, code, b)
			}
			return code
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 409
}

// waitEntered 等虚拟机的启停卡住；请求先结束了（没走到这一步）就失败。
func waitEntered(t *testing.T, v *armVM, done chan struct{}, what string) {
	t.Helper()
	select {
	case <-v.entered:
	case <-done:
		t.Fatalf("请求结束了，没有走到%s", what)
	case <-time.After(10 * time.Second):
		t.Fatalf("10 秒内没有走到%s", what)
	}
}

type cpEnv struct {
	*m3Env
	vm   *armVM
	port atomic.Int32 // 下一个客户端源端口的偏移
}

func newCPEnv(t *testing.T) *cpEnv {
	t.Helper()
	c := &cpEnv{}
	fake := &m3Direct{pinnedSet: map[string]bool{}}
	c.m3Env = newM3DaemonCore(t, m3Rules, fake, func(e *m3Env, o *Options) {
		o.ClaudeProxyAddr = "127.0.0.1:0"
		presetClaudeProxyOn(t, o.StateDir) // 第四道打开着（v0.68：默认关）
		e.dp.SetIdent(cpIdent{})
		c.vm = newArmVM(o.Sys.VM)
		o.Sys.VM = c.vm
	})
	c.direct = fake
	if s := waitGateOpen(t, c.d); !s.GateOpen {
		t.Fatalf("启用后应打开：%+v", s.Gate)
	}
	return c
}

func (c *cpEnv) addr() string {
	_, port, _ := c.d.cproxy.listening()
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// dial 从指定源端口段连代理端口（段里的端口逐次递增，免得撞上 TIME_WAIT）。
func (c *cpEnv) dial(t *testing.T, base int) net.Conn {
	t.Helper()
	for i := 0; i < 50; i++ {
		p := base + int(c.port.Add(1))%900
		d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: p}, Timeout: 3 * time.Second}
		conn, err := d.Dial("tcp", c.addr())
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			return conn
		}
	}
	t.Fatal("连不上代理端口")
	return nil
}

// connect 发 CONNECT，返回状态码和（成功时）可用的连接。
func (c *cpEnv) connect(t *testing.T, base int, target string) (int, net.Conn) {
	t.Helper()
	conn := c.dial(t, base)
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return 0, nil
	}
	if resp.StatusCode != 200 {
		conn.Close()
		return resp.StatusCode, nil
	}
	return 200, &bufConn{Conn: conn, r: br}
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func echoOK(conn net.Conn) bool {
	if _, err := conn.Write([]byte("ping")); err != nil {
		return false
	}
	buf := make([]byte, 4)
	_, err := io.ReadFull(conn, buf)
	return err == nil && string(buf) == "ping"
}

// 去向的观察：开代理流的出口（数据通道的帧头）、直连拨号。
func (c *cpEnv) counts() (opens []string, dials []string) {
	c.op.mu.Lock()
	for _, h := range c.op.headers {
		opens = append(opens, h.Exit+" "+h.Host+h.IP)
	}
	c.op.mu.Unlock()
	dials, _, _ = c.direct.snapshot()
	return
}

// c. 状态表：未启用的各状态一律 403、没有任何向外的拨号或开流；已启用且闸门开时按规则走。
func TestClaudeProxyStateTable(t *testing.T) {
	c := newCPEnv(t)
	o0, d0 := c.counts()
	// 已启用、闸门开：Claude 走主代理
	code, conn := c.connect(t, 24000, "api.anthropic.com:443")
	if code != 200 || !echoOK(conn) {
		t.Fatalf("已启用：%d", code)
	}
	conn.Close()
	expect403 := func(name string) {
		t.Helper()
		o1, d1 := c.counts()
		for _, base := range []int{24000, 22000, 23000} { // Claude、设成直连的应用、识别不出
			code, conn := c.connect(t, base, "api.anthropic.com:443")
			if conn != nil {
				conn.Close()
			}
			if code != 403 {
				t.Fatalf("%s：源端口段 %d 应 403，得到 %d", name, base, code)
			}
		}
		o2, d2 := c.counts()
		if len(o2) != len(o1) || len(d2) != len(d1) {
			t.Fatalf("%s：不应有任何向外的开流或拨号（%v → %v，%v → %v）", name, o1, o2, d1, d2)
		}
	}
	// 已启用、闸门关：Claude 拒绝（403），没有开流
	c.d.gate.AddReason(gatesm.ReasonExitCheck, "测试")
	o1, _ := c.counts()
	code, conn = c.connect(t, 24000, "api.anthropic.com:443")
	if conn != nil {
		conn.Close()
	}
	if code != 403 {
		t.Fatalf("闸门关时 Claude 应 403：%d", code)
	}
	if o2, _ := c.counts(); len(o2) != len(o1) {
		t.Fatal("闸门关时不应开流")
	}
	c.d.gate.ClearReason(gatesm.ReasonExitCheck)
	c.d.re.Kick()
	waitGateOpen(t, c.d)
	// 停用
	post(t, c.d, "/v1/disable", 200)
	expect403("停用")
	// 启用中：卡在启用第 ④ 步
	c.vm.armStart.Store(true)
	done := make(chan struct{})
	go func() { postRetryBusy(t, c.d, "/v1/enable"); close(done) }()
	waitEntered(t, c.vm, done, "启用第 ④ 步")
	if st := status(t, c.d).State; st != "ENABLING" {
		t.Fatalf("应停在启用中：%s", st)
	}
	expect403("启用中")
	c.vm.armStart.Store(false)
	close(c.vm.rel)
	<-done
	waitGateOpen(t, c.d)
	// 恢复中：卡在回国第 ② 步
	c.vm.entered, c.vm.rel, c.vm.enterOnce = make(chan struct{}), make(chan struct{}), sync.Once{}
	c.vm.armStop.Store(true)
	done = make(chan struct{})
	go func() { postRetryBusy(t, c.d, "/v1/restore"); close(done) }()
	waitEntered(t, c.vm, done, "回国第 ② 步")
	if st := status(t, c.d).State; st != "RESTORING" {
		t.Fatalf("应停在恢复中：%s", st)
	}
	expect403("恢复中")
	c.vm.armStop.Store(false)
	close(c.vm.rel)
	<-done
	expect403("回国")
	// 403 带说明
	conn = c.dial(t, 24000)
	fmt.Fprintf(conn, "CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != 403 || resp.Header.Get("X-Claude-Abroad") != "not_enabled" {
		t.Fatalf("403 应带纯 ASCII 的短代码：%v %v", resp, err)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "不是已启用状态") {
		t.Fatalf("正文应有中文说明：%q", body)
	}
	conn.Close()
	_ = o0
	_ = d0
}

// d. 已启用时的规则：和接管流量同一个判定入口。
func TestClaudeProxyRules(t *testing.T) {
	c := newCPEnv(t)
	type want struct {
		base   int
		target string
		kind   string // main、exit:<名>、direct、block
	}
	cases := []struct {
		name string
		w    want
	}{
		{"Claude 访问任意域名：主代理", want{24000, "www.example.org:443", "main"}},
		{"置顶域名不论谁访问：主代理", want{22000, "claude.ai:443", "main"}},
		{"设成本机网络的应用：本机网络", want{22000, "www.example.org:443", "direct"}},
		{"指向分流出口的：那个出口", want{20000, "www.example.org:443", "exit:wg-a"}},
		{"本地域名名单里的：本机网络", want{21000, "intranet.corp.example:443", "direct"}},
		{"识别不出对端：按 Claude 处理（主代理）", want{23000, "intranet.corp.example:443", "main"}},
	}
	for _, cs := range cases {
		o1, d1 := c.counts()
		code, conn := c.connect(t, cs.w.base, cs.w.target)
		if code != 200 || !echoOK(conn) {
			t.Fatalf("%s：%d", cs.name, code)
		}
		conn.Close()
		o2, d2 := c.counts()
		host := strings.Split(cs.w.target, ":")[0]
		switch {
		case cs.w.kind == "direct":
			if len(d2) != len(d1)+1 || !strings.Contains(d2[len(d2)-1], host) || len(o2) != len(o1) {
				t.Fatalf("%s：应直连 %s（开流 %v，拨号 %v）", cs.name, host, o2[len(o1):], d2[len(d1):])
			}
		default:
			exit := "main"
			if strings.HasPrefix(cs.w.kind, "exit:") {
				exit = strings.TrimPrefix(cs.w.kind, "exit:")
			}
			if len(o2) != len(o1)+1 || o2[len(o2)-1] != exit+" "+host || len(d2) != len(d1) {
				t.Fatalf("%s：应经 %s 开流到 %s（开流 %v，拨号 %v）", cs.name, exit, host, o2[len(o1):], d2[len(d1):])
			}
		}
	}
	// 设成阻止的应用：拒绝
	if code, raw := call(t, c.d, "PUT", "/v1/rules?drop_affected=0", []byte(strings.Replace(m3Rules, `{app: com.app.c, action: direct}`, `{app: com.app.c, action: block}`, 1))); code != 200 {
		t.Fatalf("改规则：%d %s", code, raw)
	}
	code, conn := c.connect(t, 22000, "www.example.org:443")
	if conn != nil {
		conn.Close()
	}
	if code != 403 {
		t.Fatalf("设成阻止的应 403：%d", code)
	}
	// 和接管流量同一个判定：同一组输入（应用、域名），接管路径的去向和代理端口一致
	// （上面每一格的期望就是 m3 测试里接管流量对同一规则的去向；这里再直接对一格做两条路径的对比）
	o1, _ := c.counts()
	if err := c.echo(t, 20005, netip.AddrPortFrom(c.lookup(t, "plain.example.org"), 443)); err != nil {
		t.Fatalf("接管路径：%v", err)
	}
	code, conn = c.connect(t, 20000, "plain.example.org:443")
	if code != 200 || !echoOK(conn) {
		t.Fatalf("代理路径：%d", code)
	}
	conn.Close()
	o2, _ := c.counts()
	if len(o2) != len(o1)+2 || o2[len(o2)-1] != o2[len(o2)-2] {
		t.Fatalf("两条路径的去向应相同：%v", o2[len(o1):])
	}
}

// e. 闸门由开变关：经端口走代理的连接被断开、新连接 403；经本机网络的不受影响（和接管流量一致）。
func TestClaudeProxyGateClose(t *testing.T) {
	c := newCPEnv(t)
	_, proxied := c.connect(t, 24000, "api.anthropic.com:443")
	_, direct := c.connect(t, 22000, "www.example.org:443")
	if proxied == nil || direct == nil || !echoOK(proxied) || !echoOK(direct) {
		t.Fatal("建立连接失败")
	}
	c.d.gate.AddReason(gatesm.ReasonExitCheck, "测试")
	// 只读不写：断开是在别的协程里异步做的，先写再等回显会和断开赛跑（-race 下偶发读到回显）。
	_ = proxied.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := proxied.Read(make([]byte, 1)); err == nil {
		t.Fatal("闸门关：走代理的已有连接应被断开")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("闸门关：走代理的已有连接 2 秒内没有被断开")
	}
	_ = direct.SetDeadline(time.Now().Add(2 * time.Second))
	if !echoOK(direct) {
		t.Fatal("经本机网络的连接不应受影响")
	}
	code, conn := c.connect(t, 24000, "api.anthropic.com:443")
	if conn != nil {
		conn.Close()
	}
	if code != 403 {
		t.Fatalf("闸门关后新连接应 403：%d", code)
	}
	proxied.Close()
	direct.Close()
}

// f. 协议的健壮性：超大请求头、慢速握手、不认识的方法、目标是端口自己或回环、连接数超限都拒绝并释放资源；没有协程泄漏。
func TestClaudeProxyRobust(t *testing.T) {
	c := newCPEnv(t)
	base := runtime.NumGoroutine()
	statusOf := func(raw string) int {
		conn := c.dial(t, 24000)
		defer conn.Close()
		_, _ = io.WriteString(conn, raw)
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			return 0
		}
		return resp.StatusCode
	}
	if s := statusOf("CONNECT a:443 HTTP/1.1\r\nX: " + strings.Repeat("a", 20<<10) + "\r\n\r\n"); s != 431 && s != 400 {
		t.Fatalf("超大请求头：%d", s)
	}
	if s := statusOf("DELETE https://x/ HTTP/1.1\r\nHost: x\r\n\r\n"); s != 405 {
		t.Fatalf("不认识的方法：%d", s)
	}
	if s := statusOf("GET /relative HTTP/1.1\r\nHost: x\r\n\r\n"); s != 405 {
		t.Fatalf("相对地址：%d", s)
	}
	for _, tgt := range []string{c.addr(), "127.0.0.1:22", "localhost:443", "[::1]:443", "0.0.0.0:443", "169.254.169.254:80", "x:0"} {
		if s := statusOf("CONNECT " + tgt + " HTTP/1.1\r\nHost: " + tgt + "\r\n\r\n"); s != 403 {
			t.Fatalf("目标 %s 应拒绝：%d", tgt, s)
		}
	}
	if s := statusOf("POST http://www.example.org/ HTTP/1.1\r\nHost: www.example.org\r\nContent-Length: 3\r\n\r\nabc"); s != 501 {
		t.Fatalf("带请求体的明文 HTTP：%d", s)
	}
	// 明文 HTTP（不带请求体）：改写成相对地址发给目的地（假上游原样回显请求）
	conn := c.dial(t, 24000)
	_, _ = io.WriteString(conn, "GET http://www.example.org/a?b=1 HTTP/1.1\r\nHost: www.example.org\r\nProxy-Connection: keep-alive\r\n\r\n")
	buf := make([]byte, 512)
	n, _ := io.ReadAtLeast(conn, buf, 20)
	if got := string(buf[:n]); !strings.HasPrefix(got, "GET /a?b=1 HTTP/1.1\r\nHost: www.example.org\r\n") || strings.Contains(got, "Proxy-Connection") {
		t.Fatalf("明文 HTTP 改写：%q", got)
	}
	conn.Close()
	// 慢速握手：只发半行，到时限被关（缩短时限不方便注入：这里只核对连接被登记、关闭后释放；时限由 cpHandshake 守着）
	// 连接数超限：占满后新连接 503
	var held []net.Conn
	for i := 0; i < cpMaxConns; i++ {
		conn := c.dial(t, 24000)
		_, _ = io.WriteString(conn, "CON") // 半个请求，占着
		held = append(held, conn)
	}
	time.Sleep(50 * time.Millisecond)
	conn = c.dial(t, 24000)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != 503 {
		t.Fatalf("超限应 503：%v %v", resp, err)
	}
	conn.Close()
	for _, h := range held {
		h.Close()
	}
	// 资源释放：连接数回落、协程不涨
	deadline := time.Now().Add(5 * time.Second)
	for c.d.cproxy.active.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := c.d.cproxy.active.Load(); n != 0 {
		t.Fatalf("还有 %d 个连接没释放", n)
	}
	for runtime.NumGoroutine() > base+5 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if g := runtime.NumGoroutine(); g > base+5 {
		t.Fatalf("协程从 %d 涨到 %d", base, g)
	}
}

// g. 端口被占用：abroadd 照常启动，状态“未生效”，冲突检查里有一项（必须处理）。
func TestClaudeProxyPortBusy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	e := newEnv(t)
	o := e.opts()
	o.ClaudeProxyAddr = ln.Addr().String()
	presetClaudeProxyOn(t, o.StateDir)
	d := e.start(o)
	st := status(t, d)
	if st.ClaudeCode == nil || st.ClaudeCode.Listening || st.ClaudeCode.Effective || st.ClaudeCode.ListenErr == "" {
		t.Fatalf("状态 %+v", st.ClaudeCode)
	}
	if !d.claudeProxyDown() {
		t.Fatal("应报端口没在听")
	}
	if !hasEvent(d, "claude_proxy", "listen_failed") {
		t.Fatal("应记事件")
	}
	// 非回环地址：拒绝监听
	e2 := newEnv(t)
	o2 := e2.opts()
	o2.ClaudeProxyAddr = "0.0.0.0:0"
	presetClaudeProxyOn(t, o2.StateDir)
	d2 := e2.start(o2)
	if st := status(t, d2); st.ClaudeCode == nil || st.ClaudeCode.Listening {
		t.Fatalf("非回环地址不应监听：%+v", st.ClaudeCode)
	}
}

// 没配置端口和托管设置（开发工具、假系统模式默认）：不监听、状态里没有这一项。
func TestClaudeProxyNotConfigured(t *testing.T) {
	e := newEnv(t)
	d := e.start(e.opts())
	if d.cproxy != nil || status(t, d).ClaudeCode != nil {
		t.Fatal("没配置时不应监听、不应报告")
	}
}

// 必须改 1：端口被占用时按间隔重试；占用者退出后监听上、状态变成在听、记恢复事件、冲突项消失。
func TestClaudeProxyRecoversAfterPortFreed(t *testing.T) {
	occ, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	o := e.opts()
	o.ClaudeProxyAddr = occ.Addr().String()
	o.ClaudeProxyRetry = 20 * time.Millisecond
	presetClaudeProxyOn(t, o.StateDir)
	d := e.start(o)
	if !d.claudeProxyDown() || status(t, d).ClaudeCode.Listening {
		t.Fatal("被占用时应未生效")
	}
	occ.Close()
	deadline := time.Now().Add(5 * time.Second)
	for d.claudeProxyDown() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	st := status(t, d)
	if d.claudeProxyDown() || !st.ClaudeCode.Listening || st.ClaudeCode.ListenErr != "" {
		t.Fatalf("释放后应监听上：%+v", st.ClaudeCode)
	}
	if !hasEvent(d, "claude_proxy", "listen_recovered") {
		t.Fatal("应记恢复事件")
	}
	// 真的能连上（未启用：403 not_enabled）
	conn, err := net.Dial("tcp", o.ClaudeProxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprintf(conn, "CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil || resp.StatusCode != 403 {
		t.Fatalf("%v %v", resp, err)
	}
}

// errListener：Accept 一直出错（模拟文件描述符用尽），记调用次数。
type errListener struct{ n atomic.Int64 }

func (l *errListener) Accept() (net.Conn, error) {
	l.n.Add(1)
	return nil, fmt.Errorf("too many open files")
}
func (l *errListener) Close() error   { return nil }
func (l *errListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1} }

// 必须改 1：Accept 持续出错时退避，不空转。
func TestClaudeProxyAcceptBackoff(t *testing.T) {
	cp := &claudeProxy{conns: map[net.Conn]struct{}{}}
	cp.ctx, cp.cancel = context.WithCancel(context.Background())
	l := &errListener{}
	cp.wg.Add(1)
	go cp.acceptLoop(l)
	time.Sleep(400 * time.Millisecond)
	cp.cancel()
	cp.wg.Wait()
	// 退避 5、10、20、40、80、160、320 毫秒：400 毫秒里最多八九次；空转会是成千上万次
	if n := l.n.Load(); n > 15 || n < 2 {
		t.Fatalf("400 毫秒里 Accept 调用了 %d 次（应退避）", n)
	}
}

// 握手超时：只连不发，到时限被断开、名额释放。
func TestClaudeProxyHandshakeTimeout(t *testing.T) {
	e := newEnv(t)
	o := e.opts()
	o.ClaudeProxyAddr = "127.0.0.1:0"
	o.ClaudeProxyHandshake = 200 * time.Millisecond
	presetClaudeProxyOn(t, o.StateDir)
	d := e.start(o)
	_, port, _ := d.cproxy.listening()
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	b, _ := io.ReadAll(conn) // 时限到后服务端回 400 并关闭
	if time.Since(start) > 2*time.Second || !strings.Contains(string(b), "bad_request") {
		t.Fatalf("应在时限后被断开：%v %q", time.Since(start), b)
	}
	deadline := time.Now().Add(3 * time.Second)
	for d.cproxy.active.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if d.cproxy.active.Load() != 0 {
		t.Fatal("名额没有释放")
	}
}

// 状态里的计数和最近连接（真机实测 R21–R23 据此核对）：识别出的程序、是否按 Claude 处理、去向；未启用时记 refused。不含地址。
func TestClaudeProxyRecent(t *testing.T) {
	c := newCPEnv(t)
	for _, x := range []struct {
		base   int
		target string
	}{{24000, "claude.ai:443"}, {22000, "www.example.org:443"}, {23000, "intranet.corp.example:443"}} {
		code, conn := c.connect(t, x.base, x.target)
		if code != 200 || !echoOK(conn) {
			t.Fatalf("%s：%d", x.target, code)
		}
		conn.Close()
	}
	st := status(t, c.d).ClaudeCode
	if st == nil || st.Accepted != 3 || st.Refused != 0 || len(st.Recent) != 3 {
		t.Fatalf("计数或记录不对：%+v", st)
	}
	want := []ClaudeProxyConn{
		{App: string(rules.Unknown), Claude: true, Host: "intranet.corp.example", Action: "proxy", Exit: "main", OK: true},
		{App: "com.app.c", Claude: false, Host: "www.example.org", Action: "direct", OK: true},
		{App: "com.anthropic.claudecode", Claude: true, Host: "claude.ai", Action: "proxy", Exit: "main", OK: true},
	}
	for i, w := range want {
		g := st.Recent[i]
		g.At = time.Time{}
		if g != w {
			t.Fatalf("第 %d 条：%+v，应为 %+v", i, g, w)
		}
	}
	post(t, c.d, "/v1/disable", 200)
	code, conn := c.connect(t, 24000, "api.anthropic.com:443")
	if conn != nil {
		conn.Close()
	}
	if code != 403 {
		t.Fatalf("停用后应 403：%d", code)
	}
	st = status(t, c.d).ClaudeCode
	if st.Refused != 1 || st.Recent[0].Action != "refused" || st.Recent[0].Host != "api.anthropic.com" {
		t.Fatalf("停用后应记 refused：%+v", st.Recent[0])
	}
	for i := 0; i < cpRecentMax+5; i++ {
		_, conn := c.connect(t, 24000, "api.anthropic.com:443")
		if conn != nil {
			conn.Close()
		}
	}
	if n := len(status(t, c.d).ClaudeCode.Recent); n != cpRecentMax {
		t.Fatalf("最近连接最多 %d 条：%d", cpRecentMax, n)
	}
}
