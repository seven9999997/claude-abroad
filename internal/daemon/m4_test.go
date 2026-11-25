package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/exitpolicy"
	"claude-abroad/internal/gatesm"
	"claude-abroad/internal/pf"
)

// fakeProber 返回预设的检查结果（建立基准用）。
type fakeProber struct {
	mu  sync.Mutex
	rep exitcheck.Report
	n   int
}

func (p *fakeProber) Check(context.Context, string, exitcheck.Options) exitcheck.Report {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n++
	return p.rep
}

func passReport() exitcheck.Report {
	g := &exitpolicy.Geo{IP: netip.MustParseAddr("203.0.113.7"), Country: "US", Region: "Example State", City: "Exampleville", Timezone: "America/Los_Angeles"}
	return exitcheck.Report{Exit: "main", At: time.Now(), Verdict: exitcheck.Unknown /* 没有基准时规则一项无法判断 */, IP: g.IP, Country: "US", Geo: g, Timezone: g.Timezone,
		Items: []exitcheck.Item{
			{Name: "https", Result: exitcheck.Pass}, {Name: "ip", Result: exitcheck.Pass}, {Name: "mainland", Result: exitcheck.Pass},
			{Name: "geo", Result: exitcheck.Pass}, {Name: "rule", Result: exitcheck.Unknown, Cause: exitcheck.CauseNoBaseline},
			{Name: "doh", Result: exitcheck.Pass},
		}}
}

// e、i. 没有基准时启用被拒、系统没有改动；建立基准（属主调用）后可以启用；基准文件 0600；应答、status、日志里没有出口 IP。
func TestBaselineRequiredAndAPI(t *testing.T) {
	e := newEnv(t)
	pr := &fakeProber{rep: passReport()}
	sys := e.sys.System()
	sys.Baseline = NewBaselineService(pr, e.dir)
	e.system = &sys
	d := e.start(e.opts())
	code, b := call(t, d, "POST", "/v1/enable", nil)
	if code == 200 || !strings.Contains(string(b), "出口基准") {
		t.Fatalf("没有基准时启用应被拒：%d %s", code, b)
	}
	if f, _ := e.view.Load(); f.State != "RESTORED" || !e.sys.Net.Snapshot().Equal(e.init) || e.sys.PF.Active(pf.DefaultAnchor) {
		t.Fatalf("被拒时系统不应有改动：%s", f.State)
	}
	// 出口在大陆、结果不明确：拒绝建立。
	for name, mut := range map[string]func(*exitcheck.Report){
		"大陆": func(r *exitcheck.Report) {
			r.Items[2] = exitcheck.Item{Name: "mainland", Result: exitcheck.Fail, Cause: exitcheck.CauseMainland}
		},
		"HTTPS 无法判断": func(r *exitcheck.Report) {
			r.Items[0] = exitcheck.Item{Name: "https", Result: exitcheck.Unknown, Cause: exitcheck.CauseTimeout}
		},
	} {
		r := passReport()
		mut(&r)
		pr.mu.Lock()
		pr.rep = r
		pr.mu.Unlock()
		if code, b := call(t, d, "POST", "/v1/main-proxy/apply", nil); code != http.StatusUnprocessableEntity {
			t.Fatalf("%s：应拒绝建立：%d %s", name, code, b)
		}
		if _, err := os.Stat(filepath.Join(e.dir, BaselineFile)); !os.IsNotExist(err) {
			t.Fatalf("%s：不应写出基准文件", name)
		}
	}
	pr.mu.Lock()
	pr.rep = passReport()
	pr.mu.Unlock()
	code, b = call(t, d, "POST", "/v1/main-proxy/apply?rule=same_city", nil)
	var res MainProxyResult
	sum := &res.Baseline
	if code != 200 || json.Unmarshal(b, &res) != nil || sum.Rule != exitpolicy.RuleSameCity || sum.Country != "US" ||
		res.Region == nil || res.Region.Timezone != "America/Los_Angeles" || res.Synced {
		t.Fatalf("建立基准：%d %s", code, b)
	}
	fi, err := os.Stat(filepath.Join(e.dir, BaselineFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("基准文件应是 0600：%v %v", fi, err)
	}
	if code, _ := call(t, d, "POST", "/v1/main-proxy/apply?rule=bogus", nil); code != http.StatusUnprocessableEntity {
		t.Fatal("规则不认识应拒绝")
	}
	post(t, d, "/v1/enable", 200)
	s := waitGateOpen(t, d)
	if !s.GateOpen {
		t.Fatalf("有基准后应能启用：%+v", s.Gate)
	}
	_, sb := call(t, d, "GET", "/v1/status", nil)
	all := string(b) + string(sb)
	for _, x := range []string{"203.0.113.7", "Exampleville", "Example State"} {
		if strings.Contains(all, x) {
			t.Fatalf("应答或 status 里出现了敏感字段 %q", x)
		}
	}
}

// h. 主代理的复查不通过：所有走代理的连接都被拒（包括规则指向分流出口的），直连不受影响；
// 复查通过后重新打开、走回去。status 有闸门原因、没有 exits_down。
func TestMainExitFailClosesAllProxied(t *testing.T) {
	e := newM3Daemon(t, m3Rules)
	s := waitGateOpen(t, e.d)
	if !s.GateOpen {
		t.Fatal("启用后应打开")
	}
	plain := netip.AddrPortFrom(e.lookup(t, "plain.example.org"), 443)
	if err := e.echo(t, 20001, plain); err != nil {
		t.Fatalf("出口 A：%v", err)
	}
	held, err := e.dial(20004, plain) // 关闭前的走代理长连接
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := echoConn(held); err != nil {
		t.Fatal(err)
	}
	var res gatesm.Result = gatesm.Fail
	var mu sync.Mutex
	e.d.re.Check = func(context.Context) (gatesm.Result, string) {
		mu.Lock()
		defer mu.Unlock()
		return res, "测试注入"
	}
	e.d.re.RunOnce(context.Background())
	st := status(t, e.d)
	if st.GateOpen || len(st.Gate.Reasons) != 1 || st.Gate.Reasons[0] != string(gatesm.ReasonExitCheck) {
		t.Fatalf("主代理不通过应关闸门，原因 exit_check：%+v", st.Gate)
	}
	mustDropped(t, held, "加原因关闭")
	for _, port := range []uint16{20002, 21002} { // 规则指向出口 A、出口 B 的应用
		if err := e.echo(t, port, plain); err == nil {
			t.Fatalf("端口 %d：主代理不通过时走代理的连接应被拒", port)
		}
	}
	mu.Lock()
	res = gatesm.Pass
	mu.Unlock()
	e.d.re.RunOnce(context.Background())
	if err := e.echo(t, 20003, plain); err != nil {
		t.Fatalf("复查通过后应走回出口 A：%v", err)
	}
	_, raw := call(t, e.d, "GET", "/v1/status", nil)
	if strings.Contains(string(raw), "exits_down") || !strings.Contains(string(raw), `"gate":{"open":true`) {
		t.Fatalf("status 应有闸门状态、没有 exits_down：%s", raw)
	}
}

// ---- 关闭时已有连接被断开（验收 b、g 的端到端证明：闸门 → daemon 钩子 → 数据面断开顺序 → 连接真的断了）----

// stepClock 是注入给闸门和复查的时钟：Advance 按到期顺序同步触发定时器。
type stepClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*stepTimer
}

type stepTimer struct {
	c       *stepClock
	at      time.Time
	f       func()
	stopped bool
}

func (t *stepTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := !t.stopped
	t.stopped = true
	return was
}

func (c *stepClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *stepClock) AfterFunc(d time.Duration, f func()) gatesm.Stopper {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &stepTimer{c: c, at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

func (c *stepClock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if !t.stopped {
			n++
		}
	}
	return n
}

func (c *stepClock) Advance(d time.Duration) {
	c.mu.Lock()
	end := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var next *stepTimer
		for _, t := range c.timers {
			if !t.stopped && !t.at.After(end) && (next == nil || t.at.Before(next.at)) {
				next = t
			}
		}
		if next == nil {
			c.now = end
			c.mu.Unlock()
			return
		}
		next.stopped = true
		if next.at.After(c.now) {
			c.now = next.at
		}
		c.mu.Unlock()
		next.f()
	}
}

// leaseEnv 是带注入时钟的 M3 环境：启用后闸门已开、复查循环停在“等下一轮”（放行期定时器 + 循环定时器）。
type leaseEnv struct {
	*m3Env
	clk   *stepClock
	plain netip.AddrPort
	mu    sync.Mutex
	tr    []string
}

func newLeaseEnv(t *testing.T) *leaseEnv {
	t.Helper()
	clk := &stepClock{now: time.Now()}
	fake := &m3Direct{pinnedSet: map[string]bool{}}
	m := newM3DaemonWith(t, m3Rules, fake, func(o *Options) { o.Clock = clk })
	m.direct = fake
	le := &leaseEnv{m3Env: m, clk: clk}
	if s := waitGateOpen(t, m.d); !s.GateOpen {
		t.Fatalf("启用后应打开：%+v", s.Gate)
	}
	le.settle(t)
	le.plain = netip.AddrPortFrom(m.lookup(t, "plain.example.org"), 443)
	m.dp.mu.Lock()
	m.dp.trace = func(s string) { le.mu.Lock(); le.tr = append(le.tr, s); le.mu.Unlock() }
	m.dp.mu.Unlock()
	return le
}

// settle 等复查循环跑完这一轮并挂上下一轮的定时器（放行期一个、循环一个、分流出口的下发和检查循环一个（M4-8）；
// 接了地区时还有待还原重试的一个）。
func (le *leaseEnv) settle(t *testing.T) {
	t.Helper()
	want := 3
	if le.d.regionOn() {
		want++
	}
	for i := 0; i < 500 && le.clk.pending() != want; i++ {
		time.Sleep(2 * time.Millisecond)
	}
	if n := le.clk.pending(); n != want {
		t.Fatalf("复查循环没有停在等下一轮：定时器 %d 个", n)
	}
}

func (le *leaseEnv) trace() []string {
	le.mu.Lock()
	defer le.mu.Unlock()
	return append([]string(nil), le.tr...)
}

// hold 建一条长连接并回显一次。
func (le *leaseEnv) hold(t *testing.T, port uint16) net.Conn {
	t.Helper()
	c, err := le.dial(port, le.plain)
	if err != nil {
		t.Fatalf("端口 %d：%v", port, err)
	}
	t.Cleanup(func() { c.Close() })
	if err := echoConn(c); err != nil {
		t.Fatalf("端口 %d：%v", port, err)
	}
	return c
}

// mustDropped：1 秒内读到错误（不是超时）。
func mustDropped(t *testing.T, c net.Conn, what string) {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	_, _ = c.Write([]byte("hi"))
	_, err := io.ReadFull(c, make([]byte, 2))
	if err == nil {
		t.Fatalf("%s：走代理的已有连接应被断开", what)
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("%s：走代理的已有连接 1 秒内没有被断开", what)
	}
}

func hasEvent(d *Daemon, source, kind string) bool {
	for _, r := range d.events.List() {
		if r.Source == source && r.Kind == kind {
			return true
		}
	}
	return false
}

var teardownOrder = []string{"drop_all", "doh_close", "dialer_close"}

// b、g. 放行期到期没有续期（复查卡住 / 复查协程不再运行）：闸门自己关，走代理的已有连接 1 秒内断开、直连不受影响；
// 断开顺序恰好一次；有 lease_expired；status 显示关、没有任何原因。
func TestLeaseExpiryDropsProxied(t *testing.T) {
	for _, mode := range []string{"检查卡住", "复查协程不再运行"} {
		t.Run(mode, func(t *testing.T) {
			le := newLeaseEnv(t)
			d := le.d
			if mode == "检查卡住" {
				release := make(chan struct{})
				t.Cleanup(func() { close(release) }) // 先于 abandon（后注册的先执行）
				// 闸门已开（Grant 在锁里，晚于上一轮读 Check），下一轮要等时钟推进：这里改 Check 没有竞争。
				d.re.Check = func(ctx context.Context) (gatesm.Result, string) {
					select {
					case <-release:
					case <-ctx.Done():
					}
					return gatesm.Unknown, "卡住"
				}
			} else {
				d.supStop()
				<-d.reDone
			}
			proxied := le.hold(t, 20001)
			direct := le.hold(t, 22001)
			le.clk.Advance(gatesm.DefaultInterval) // 60 秒：该复查了，但没有结果
			if !status(t, d).GateOpen {
				t.Fatal("放行期内应仍开着")
			}
			le.clk.Advance(gatesm.DefaultLease - gatesm.DefaultInterval + time.Second) // 91 秒：到期
			mustDropped(t, proxied, mode)
			if err := echoConn(direct); err != nil {
				t.Fatalf("直连不应受影响：%v", err)
			}
			if got := le.trace(); !slices.Equal(got, teardownOrder) {
				t.Fatalf("断开顺序应恰好一次：%v", got)
			}
			if !hasEvent(d, "gate", gatesm.EvLeaseExpired) {
				t.Fatal("应有 lease_expired 事件")
			}
			st := status(t, d)
			if st.GateOpen || len(st.Gate.Reasons) != 0 {
				t.Fatalf("到期关闭不是原因：%+v", st.Gate)
			}
			if err := le.echo(t, 20002, le.plain); err == nil {
				t.Fatal("到期后新的走代理连接应被拒")
			}
		})
	}
}

// 续期及时到达：60 秒一轮推进 5 轮，每轮都通过，长连接全程不断，断开 0 次。
func TestLeaseRenewedKeepsConnections(t *testing.T) {
	le := newLeaseEnv(t)
	d := le.d
	var calls atomic.Int32
	d.re.Check = func(context.Context) (gatesm.Result, string) { calls.Add(1); return gatesm.Pass, "" }
	proxied := le.hold(t, 20001)
	for i := 1; i <= 5; i++ {
		le.clk.Advance(gatesm.DefaultInterval)
		for k := 0; k < 500 && (calls.Load() < int32(i) || d.gate.Status().LeaseLeftMs != gatesm.DefaultLease.Milliseconds()); k++ {
			time.Sleep(2 * time.Millisecond)
		}
		if d.gate.Status().LeaseLeftMs != gatesm.DefaultLease.Milliseconds() {
			t.Fatalf("第 %d 轮没有续期：%+v", i, d.gate.Status())
		}
		le.settle(t)
		if err := echoConn(proxied); err != nil {
			t.Fatalf("第 %d 轮：长连接不应断：%v", i, err)
		}
	}
	if got := le.trace(); len(got) != 0 || hasEvent(d, "gate", gatesm.EvLeaseExpired) {
		t.Fatalf("及时续期时不应断开：%v", got)
	}
}
