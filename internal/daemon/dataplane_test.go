package daemon

import (
	"bytes"
	"claude-abroad/internal/rules"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"claude-abroad/internal/agent"
	"claude-abroad/internal/devupstream"
	"claude-abroad/internal/pf"
	"claude-abroad/internal/stack"
	"claude-abroad/internal/tun"
	"claude-abroad/internal/tunsim"
	"claude-abroad/internal/wire"
)

// ---- 进程内的数据通道对端：真实的 agent.DataPlane → devupstream（当 sing-box）----

type exits struct{}

func (exits) Lookup(tag string) (bool, bool) { return tag == "main", true }

type pipeLn struct {
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func (l *pipeLn) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *pipeLn) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *pipeLn) Addr() net.Addr { return &net.UnixAddr{Name: "pipe", Net: "unix"} }
func (l *pipeLn) Dial(ctx context.Context) (net.Conn, error) {
	a, b := net.Pipe()
	select {
	case l.ch <- b:
		return a, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func agentChain(t *testing.T) (*wire.Dialer, *syncBuf) {
	t.Helper()
	l, u, err := devupstream.Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	log := &syncBuf{}
	ctx, cancel := context.WithCancel(context.Background())
	upDone := make(chan struct{})
	go func() { (&devupstream.Server{Log: log}).Serve(ctx, l, u); close(upDone) }()
	adp := &agent.DataPlane{Exits: exits{},
		Timing: agent.DataTiming{DialTimeout: 5 * time.Second, UDPIdle: 5 * time.Second},
		Socks: func(e string) agent.SocksDialer {
			return agent.SocksDialer{Addr: l.Addr().String(), User: e, Password: "0123456789abcdef0123456789abcdef"}
		}}
	pl := &pipeLn{ch: make(chan net.Conn), closed: make(chan struct{})}
	dpDone := make(chan struct{})
	go func() { adp.Serve(ctx, pl); close(dpDone) }()
	d := &wire.Dialer{Dial: pl.Dial, Config: wire.DefaultSessionConfig()}
	t.Cleanup(func() {
		d.Close()
		cancel()
		pl.Close()
		<-dpDone
		<-upDone
	})
	return d, log
}

// txtUp 是测试用的 DNS 上游：对任何查询答一条 TXT（证明“需要上游的查询”在闸门开时能通）。
type txtUp struct{ n atomic.Int64 }

func (u *txtUp) Exchange(_ context.Context, q *dns.Msg) (*dns.Msg, error) {
	u.n.Add(1)
	m := new(dns.Msg)
	m.SetReply(q)
	m.Answer = append(m.Answer, &dns.TXT{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 1}, Txt: []string{"ok"}})
	return m, nil
}

// rejectAll 是“全部拒绝”的数据通道对端。
type rejectAll struct{}

func (rejectAll) Open(context.Context, wire.Header) (net.Conn, error) {
	return nil, errors.New("拒绝（测试）")
}
func (rejectAll) Close() error { return nil }

// dataEnv 是带真实数据面的真实系统：系统层是模拟器 + FakePF，utun 是管道，数据通道接进程内的 agent。
type dataEnv struct {
	*env
	k     *realKit
	dp    *DataPlane
	up    *txtUp
	mu    sync.Mutex
	peers []tun.Device
}

func newDataEnv(t *testing.T, op StreamOpener, exit func(*DataPlane) ExitCheck) *dataEnv {
	e := newEnv(t)
	de := &dataEnv{env: e, k: &realKit{log: &cmdLog{}}, up: &txtUp{}}
	if op == nil {
		d, _ := agentChain(t)
		op = d
	}
	de.dp = NewDataPlane(op, de.up)
	p := e.realParams(de.k)
	p.OpenTun = func() (tun.Device, error) {
		a, b := tun.Pipe(1500)
		de.mu.Lock()
		de.peers = append(de.peers, b)
		n := len(de.peers)
		de.mu.Unlock()
		return namedDev{Device: a, name: fmt.Sprintf("utun%d", 30+n)}, nil
	}
	p.Data = de.dp
	ec := ExitCheck{Opener: de.dp.Streams(), Host: "exit.test", Timeout: 5 * time.Second}
	if exit != nil {
		ec = exit(de.dp)
	}
	p.Exit = ec
	sys, err := NewRealSystem(p)
	if err != nil {
		t.Fatal(err)
	}
	e.system = &sys
	return de
}

// client 在最新的管道另一头建一个客户端（扮演本机应用）。
func (de *dataEnv) client(t *testing.T) *tunsim.Client {
	t.Helper()
	de.mu.Lock()
	peer := de.peers[len(de.peers)-1]
	de.mu.Unlock()
	cl, err := tunsim.NewClient(peer, netip.Addr{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	return cl
}

var probeCfg = tunsim.Config{Name: "e2e.test", UDPPort: 4500, RealIP: netip.MustParseAddrPort("203.0.113.9:80"), Timeout: 5 * time.Second}

func txt(t *testing.T, cl *tunsim.Client) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	m, err := cl.Query(ctx, "txt.e2e.test", dns.TypeTXT, stack.DefaultDNSAddr)
	if err != nil {
		t.Fatal(err)
	}
	return m.Rcode
}

// b. 进程内数据面端到端。
func TestDataPlaneEndToEnd(t *testing.T) {
	de := newDataEnv(t, nil, nil)
	d := de.start(de.opts())
	base := runtime.NumGoroutine()
	post(t, d, "/v1/enable", 200)
	cl := de.client(t)
	r := tunsim.Probe(bg, cl, probeCfg, netip.Addr{})
	if !r.OK() {
		t.Fatalf("启用后探测应全部通过：%+v", r.Steps)
	}
	fake := r.Fake
	if txt(t, cl) != dns.RcodeSuccess {
		t.Fatal("闸门开时需要上游的查询应有应答")
	}
	// 保持一条 TCP，停用后应在 1 秒内被断开。
	hold, err := cl.DialTCP(bg, netip.AddrPortFrom(fake, 80))
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	time.Sleep(100 * time.Millisecond)
	opensBefore := de.dp.Opens()
	post(t, d, "/v1/disable", 200)
	_ = hold.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := hold.Read(make([]byte, 1)); err == nil {
		t.Fatal("已有的 TCP 应被断开")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("已有的 TCP 1 秒内没有被断开")
	}
	opensAfterDisable := de.dp.Opens()
	r2 := tunsim.Probe(bg, cl, tunsim.Config{Name: "e2e.test", UDPPort: 4500, RealIP: probeCfg.RealIP, Timeout: time.Second}, fake)
	if !r2.Failed() {
		t.Fatalf("停用后探测应全部失败：%+v", r2.Steps)
	}
	if de.dp.Opens() != opensAfterDisable {
		t.Fatalf("停用后不应再开流：%d → %d", opensAfterDisable, de.dp.Opens())
	}
	if txt(t, cl) != dns.RcodeServerFailure {
		t.Fatal("闸门关时需要上游的查询应得到 SERVFAIL")
	}
	_ = opensBefore
	// 再启用：同一个域名解析到同一个假地址，TCP 重新可用（拨号器关闭后能重新拨号）。
	post(t, d, "/v1/enable", 200)
	r3 := tunsim.Probe(bg, cl, probeCfg, netip.Addr{})
	if !r3.OK() || r3.Fake != fake {
		t.Fatalf("再启用后：%+v，假地址 %v（原来 %v）", r3.Steps, r3.Fake, fake)
	}
	// 恢复：管道关闭，没有残留的 goroutine。
	t0 := time.Now()
	post(t, d, "/v1/restore", 200)
	t.Logf("恢复用时 %v", time.Since(t0))
	de.mu.Lock()
	peer := de.peers[len(de.peers)-1]
	de.mu.Unlock()
	if _, err := peer.Write(make([]byte, 20)); err == nil {
		t.Fatal("恢复后管道应已关闭")
	}
	cl.Close()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > base+2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base+2 {
		buf := make([]byte, 1<<20)
		t.Fatalf("残留 goroutine：%d → %d\n%s", base, n, buf[:runtime.Stack(buf, true)])
	}
	if !de.sys.Net.Snapshot().Equal(de.init) {
		t.Fatal("恢复后应与初始值相等")
	}
	de.noViolations()
}

func (de *dataEnv) curDev(d *Daemon) *trackedDev {
	rt := d.tun.(*realTun)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.dev
}

// c. utun 重建后映射还在：不重新解析，直接连同一个假地址。
func TestDataPlaneRebuildKeepsMapping(t *testing.T) {
	de := newDataEnv(t, nil, nil)
	o := de.opts()
	o.SuperviseInterval = 50 * time.Millisecond
	d := de.start(o)
	post(t, d, "/v1/enable", 200)
	r := tunsim.Probe(bg, de.client(t), probeCfg, netip.Addr{})
	if !r.OK() {
		t.Fatalf("%+v", r.Steps)
	}
	old := status(t, d).Utun
	_ = de.curDev(d).Close() // 设备意外消失
	deadline := time.Now().Add(2*o.SuperviseInterval + 2*time.Second)
	for time.Now().Before(deadline) {
		if s := status(t, d); s.Utun != "" && s.Utun != old && s.GateOpen {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s := status(t, d); s.Utun == old || !s.GateOpen {
		t.Fatalf("没有重建：%+v", s)
	}
	r2 := tunsim.Probe(bg, de.client(t), probeCfg, r.Fake)
	if !r2.OK() {
		t.Fatalf("新设备上直接连原来的假地址应通过：%+v", r2.Steps)
	}
	de.noViolations()
}

// d. 网络栈意外退出：Alive 变假，监护被 Kick（间隔很长），期间闸门关，重建后重开。
func TestDataPlaneStackDiesKicks(t *testing.T) {
	de := newDataEnv(t, nil, nil)
	o := de.opts()
	o.SuperviseInterval = time.Hour // 只能靠 Kick
	d := de.start(o)
	var mu sync.Mutex
	var gates []bool
	d.gate.SetOnChange(func(open bool) { mu.Lock(); gates = append(gates, open); mu.Unlock(); de.check("gate") })
	post(t, d, "/v1/enable", 200)
	old := status(t, d).Utun
	de.mu.Lock()
	peer := de.peers[len(de.peers)-1]
	de.mu.Unlock()
	_ = peer.Close() // 管道另一头关掉：网络栈的搬运协程读出错退出
	// 网络栈退出后 Alive 变假、立刻 Kick 监护：间隔是 1 小时，能在几秒内重建就只可能是 Kick 触发的。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s := status(t, d); s.Utun != "" && s.Utun != old && s.GateOpen {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s := status(t, d); s.Utun == old || !s.GateOpen {
		t.Fatalf("没有被 Kick 重建：%+v", s)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gates) < 2 || gates[len(gates)-2] != false || gates[len(gates)-1] != true {
		t.Fatalf("期间闸门应先关后开：%v", gates)
	}
	de.noViolations()
}

// e. 断开顺序：每次闸门由开变关，恰好 DropAll、DoH Close、拨号器 Close 各一次。
func TestTeardownOrder(t *testing.T) {
	de := newDataEnv(t, nil, nil)
	o := de.opts()
	o.SuperviseInterval = 30 * time.Millisecond
	var mu sync.Mutex
	var tr []string
	de.dp.trace = func(s string) { mu.Lock(); tr = append(tr, s); mu.Unlock() }
	take := func() []string { mu.Lock(); defer mu.Unlock(); x := tr; tr = nil; return x }
	want := []string{"drop_all", "doh_close", "dialer_close"}
	d := de.start(o)
	post(t, d, "/v1/enable", 200)
	take()
	post(t, d, "/v1/disable", 200)
	if got := take(); !slices.Equal(got, want) {
		t.Fatalf("停用：%v", got)
	}
	post(t, d, "/v1/enable", 200)
	take()
	de.noCheck.Store(true)
	de.sys.PF.DropMainRef() // 监护因保护失效关闸门
	deadline := time.Now().Add(3 * time.Second)
	for status(t, d).GateOpen && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := take(); !slices.Equal(got, want) {
		t.Fatalf("监护关闸门：%v", got)
	}
	de.sys.PF.RestoreMainRef()
	deadline = time.Now().Add(3 * time.Second)
	for !status(t, d).GateOpen && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	de.noCheck.Store(false)
	take()
	post(t, d, "/v1/restore", 200)
	if got := take(); !slices.Equal(got, want) {
		t.Fatalf("恢复：%v", got)
	}
}

// ---- f. 外部虚拟机 ----

func serveUnix(t *testing.T, path string, h http.HandlerFunc) {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
}

func shortDir(t *testing.T) string {
	d, err := os.MkdirTemp("", "vm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestExternalVM(t *testing.T) {
	vm := func(t *testing.T, healthOK bool, mode string) ExternalVM {
		dir := shortDir(t)
		serveUnix(t, filepath.Join(dir, "ctl.sock"), func(w http.ResponseWriter, r *http.Request) {
			if !healthOK {
				http.Error(w, "no", 503)
				return
			}
			io.WriteString(w, `{"engines":{}}`)
		})
		serveUnix(t, filepath.Join(dir, "host.sock"), func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"outbound_mode":%q}`, mode)
		})
		return ExternalVM{Dir: dir, Timeout: 300 * time.Millisecond, Poll: 20 * time.Millisecond}
	}
	if err := vm(t, true, "first_hop").Start(bg); err != nil {
		t.Fatalf("应通过：%v", err)
	}
	t0 := time.Now()
	if err := vm(t, false, "first_hop").Start(bg); !errors.Is(err, ErrVMNotReady) || time.Since(t0) < 250*time.Millisecond {
		t.Fatalf("health 不通应到时限失败：%v", err)
	}
	if err := vm(t, true, "open").Start(bg); !errors.Is(err, ErrVMOpen) || !strings.Contains(err.Error(), "没有收紧") {
		t.Fatalf("%v", err)
	}
	if err := (ExternalVM{Dir: shortDir(t), Timeout: 100 * time.Millisecond, Poll: 20 * time.Millisecond}).Start(bg); !errors.Is(err, ErrVMNotRunning) || !strings.Contains(err.Error(), "没有启动") {
		t.Fatalf("%v", err)
	}
	if err := (ExternalVM{Dir: "/nonexistent"}).Stop(bg); err != nil {
		t.Fatal(err)
	}
}

// f. health 一直不通：启用到时限失败，回滚干净。
func TestExternalVMEnableRollsBack(t *testing.T) {
	de := newDataEnv(t, nil, nil)
	sys := *de.system
	sys.VM = ExternalVM{Dir: shortDir(t), Timeout: 200 * time.Millisecond, Poll: 20 * time.Millisecond}
	de.system = &sys
	d := de.start(de.opts())
	code, b := callOp(t, d, "/v1/enable")
	if code == 200 || !strings.Contains(string(b), "虚拟机没有启动") {
		t.Fatalf("%d %s", code, b)
	}
	if s := status(t, d); s.State != "RESTORED" || s.GateOpen {
		t.Fatalf("%+v", s)
	}
	if !de.sys.Net.Snapshot().Equal(de.init) || de.sys.PF.Active(pf.DefaultAnchor) {
		t.Fatal("应回滚干净")
	}
	de.noViolations()
}

// ---- g. 出口检查 ----

type scriptedOpener struct {
	reply       string // 对端回的内容；"" 表示不回（超时）
	fail        bool
	n           atomic.Int64
	gate        func() bool
	sawGateOpen atomic.Bool
}

func (o *scriptedOpener) Open(ctx context.Context, h wire.Header) (net.Conn, error) {
	o.n.Add(1)
	if o.gate != nil && o.gate() {
		o.sawGateOpen.Store(true)
	}
	if o.fail {
		return nil, errors.New("连不上（测试）")
	}
	a, b := net.Pipe()
	go func() {
		defer b.Close()
		buf := make([]byte, 4096)
		_, _ = b.Read(buf)
		if o.reply == "" {
			time.Sleep(2 * time.Second)
			return
		}
		io.WriteString(b, o.reply)
	}()
	return a, nil
}
func (o *scriptedOpener) Close() error { return nil }

func TestExitCheck(t *testing.T) {
	cases := map[string]*scriptedOpener{
		"204":  {reply: "HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n"},
		"200":  {reply: "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"},
		"超时":   {reply: ""},
		"连接失败": {fail: true},
	}
	for name, op := range cases {
		err := ExitCheck{Opener: op, Host: "exit.test", Timeout: 300 * time.Millisecond}.Check(bg)
		if (name == "204") != (err == nil) {
			t.Errorf("%s：%v", name, err)
		}
		if op.n.Load() != 1 {
			t.Errorf("%s：应开一条流，实际 %d", name, op.n.Load())
		}
	}
}

// g. 出口检查失败时启用回滚，闸门从未打开；检查时闸门是关的，确实经数据通道开了一条流。
func TestExitCheckInEnable(t *testing.T) {
	for _, name := range []string{"200", "超时", "连接失败"} {
		t.Run(name, func(t *testing.T) {
			op := map[string]*scriptedOpener{
				"200": {reply: "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"}, "超时": {reply: ""}, "连接失败": {fail: true},
			}[name]
			de := newDataEnv(t, nil, func(dp *DataPlane) ExitCheck {
				return ExitCheck{Opener: op, Host: "exit.test", Timeout: 300 * time.Millisecond}
			})
			d := de.start(de.opts())
			var openedN atomic.Int64
			d.gate.SetOnChange(func(open bool) {
				if open {
					openedN.Add(1)
				}
			})
			op.gate = d.gate.IsOpen
			code, body := callOp(t, d, "/v1/enable")
			if code == 200 {
				t.Fatal("出口检查失败时启用应失败")
			}
			if opened := openedN.Load(); opened != 0 || op.sawGateOpen.Load() || op.n.Load() != 1 {
				t.Fatalf("闸门开过 %d 次，检查时闸门开着=%v，开流 %d；启用的应答 %d %s", opened, op.sawGateOpen.Load(), op.n.Load(), code, body)
			}
			if s := status(t, d); s.State != "RESTORED" || !de.sys.Net.Snapshot().Equal(de.init) {
				t.Fatalf("应回滚干净：%+v", s)
			}
			de.noViolations()
		})
	}
	// 通过时：经数据面的计数开流加一，开流时闸门关着。
	de := newDataEnv(t, nil, nil)
	d := de.start(de.opts())
	sawOpen := false
	de.dp.trace = nil
	before := de.dp.Opens()
	orig := de.system.Exit.(ExitCheck)
	wrapped := gateSpy{inner: orig.Opener, gate: d.gate.IsOpen, saw: &sawOpen}
	sys := *de.system
	sys.Exit = ExitCheck{Opener: wrapped, Host: orig.Host, Timeout: orig.Timeout}
	de.system = &sys
	d.m.D.Check = sys.Exit
	post(t, d, "/v1/enable", 200)
	if de.dp.Opens() != before+1 || sawOpen {
		t.Fatalf("开流 %d → %d，检查时闸门开着=%v", before, de.dp.Opens(), sawOpen)
	}
}

type gateSpy struct {
	inner stack.Opener
	gate  func() bool
	saw   *bool
}

func (g gateSpy) Open(ctx context.Context, h wire.Header) (net.Conn, error) {
	if g.gate() {
		*g.saw = true
	}
	return g.inner.Open(ctx, h)
}

// ---- h. 没有直连 ----

// 数据通道的对端全部拒绝：所有流都失败，没有别的出口（DNS 的假地址照常分配，那是本地的）。
func TestNoOtherExit(t *testing.T) {
	de := newDataEnv(t, rejectAll{}, func(dp *DataPlane) ExitCheck {
		return ExitCheck{Opener: &scriptedOpener{reply: "HTTP/1.1 204 No Content\r\n\r\n"}, Host: "exit.test", Timeout: time.Second}
	})
	d := de.start(de.opts())
	post(t, d, "/v1/enable", 200)
	r := tunsim.Probe(bg, de.client(t), tunsim.Config{Name: "e2e.test", Timeout: time.Second, RealIP: probeCfg.RealIP}, netip.Addr{})
	for _, s := range r.Steps {
		if s.Name != "dns" && s.OK {
			t.Fatalf("%s 不应通过（数据通道全部拒绝）：%+v", s.Name, r.Steps)
		}
	}
	if de.dp.Opens() == 0 {
		t.Fatal("流应经数据通道（被拒绝）")
	}
}

// AST：internal/daemon 的非测试文件里，拨号和监听的网络类型只能是 "unix"；不用 net/http 的默认客户端。
func TestNoDirectDialSource(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	dialers := map[string]int{"Dial": 0, "DialTimeout": 0, "Listen": 0, "ListenPacket": 0, "ListenUnix": 0, "DialContext": 1}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				sel, ok := x.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				idx, isDial := dialers[sel.Sel.Name]
				if !isDial || len(x.Args) <= idx {
					return true
				}
				lit, ok := x.Args[idx].(*ast.BasicLit)
				if !ok || lit.Value != strconv.Quote("unix") {
					t.Errorf("%s：%s 的网络类型不是字面量 \"unix\"", f, sel.Sel.Name)
				}
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && id.Name == "http" {
					switch x.Sel.Name {
					case "Get", "Post", "Head", "PostForm", "DefaultClient", "DefaultTransport":
						t.Errorf("%s 用了 http.%s", f, x.Sel.Name)
					}
				}
			}
			return true
		})
	}
}

// M3-6：数据面是状态机的“状态接收方”：初始停用；启用后启用；停用、恢复后停用。
func TestDataPlaneFollowsState(t *testing.T) {
	de := newDataEnv(t, nil, nil)
	d := de.start(de.opts())
	if de.dp.Enabled() {
		t.Fatal("初始应按停用判定")
	}
	post(t, d, "/v1/enable", 200)
	if !de.dp.Enabled() {
		t.Fatal("启用后数据面应按启用判定")
	}
	post(t, d, "/v1/disable", 200)
	if de.dp.Enabled() {
		t.Fatal("停用后数据面应按停用判定")
	}
	post(t, d, "/v1/enable", 200)
	post(t, d, "/v1/restore", 200)
	if de.dp.Enabled() {
		t.Fatal("恢复后数据面应按停用判定")
	}
	// 换规则：本地名单同步；同一份配置再换一次不报错。
	c, err := rules.Parse([]byte("local_domains: [\"*.corp.example\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := de.dp.SetRules(c, true); err != nil {
		t.Fatal(err)
	}
	if !de.dp.pool.IsLocalName("git.corp.example") {
		t.Fatal("换规则后 fake-IP 的本地名单应同步")
	}
	de.noViolations()
}
