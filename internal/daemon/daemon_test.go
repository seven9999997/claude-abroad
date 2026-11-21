package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"claude-abroad/internal/pf"
	"claude-abroad/internal/preflight"
	"claude-abroad/internal/state"
	"claude-abroad/internal/sysnet"
)

var bg = context.Background()

// env 是一台“机器”：一套假系统和一个状态目录，上面可以先后起多个 daemon 实例。
type env struct {
	t    *testing.T
	sys  *FakeSystem
	dir  string
	view *state.Store
	init sysnet.FakeState

	mu         sync.Mutex
	violations []string
	noCheck    atomic.Bool
	d          *Daemon
	cur        atomic.Pointer[Daemon]             // check 读它（闸门回调可能来自后台的复查循环）
	system     *System                            // 非 nil 时用它（NewRealSystem），否则用 sys.System()
	ifaces     atomic.Pointer[[]sysnet.IfaceInfo] // 非 nil 时真实系统的网卡列表用它（测试里换网络）
}

func newEnv(t *testing.T) *env {
	// 状态目录放在短路径下：unix socket 的路径不能超过 104 字节，t.TempDir() 带着测试名会超长。
	short, err := os.MkdirTemp("", "ab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	e := &env{t: t, sys: NewFakeSystem(), dir: filepath.Join(short, "st"), init: FakeInitialNet()}
	e.view = state.NewStore(e.dir)
	e.sys.Net.OnChange = func(op string) { e.check("net " + op) }
	e.sys.OnTun = func(ev string) { e.check("tun " + ev) }
	t.Cleanup(func() {
		if e.d != nil {
			e.d.abandon()
		}
	})
	return e
}

func (e *env) opts() Options {
	sys := e.sys.System()
	if e.system != nil {
		sys = *e.system
	}
	return Options{StateDir: e.dir, Sys: sys, SuperviseInterval: time.Hour, LockTimeout: 300 * time.Millisecond, LockRetry: 20 * time.Millisecond}
}

func (e *env) start(o Options) *Daemon {
	e.t.Helper()
	d, err := Start(bg, o)
	if err != nil {
		e.t.Fatal(err)
	}
	e.d = d
	e.cur.Store(d)
	d.gate.SetOnChange(func(bool) { e.check("gate") })
	return d
}

var public = []pf.Packet{
	{If: "en0", User: "alice", Proto: "tcp", DstPort: 443, Dst: netip.MustParseAddr("160.79.104.10")},
	{If: "en0", User: "alice", Proto: "udp", DstPort: 443, Dst: netip.MustParseAddr("160.79.104.10")},
}

func (e *env) blocked() bool {
	for _, p := range public {
		if ok, err := e.sys.PF.Eval(pf.DefaultAnchor, p); err != nil || ok {
			return false
		}
	}
	return true
}

// check 是 7b 的不变量在 daemon 里的版本：I1、I2、泄漏断言（每次假网络、闸门、utun 改动后）。
func (e *env) check(where string) {
	if e.noCheck.Load() {
		return
	}
	f, err := e.view.Load()
	if err != nil {
		e.violate(where, err.Error())
		return
	}
	active := e.sys.PF.Active(pf.DefaultAnchor)
	if d := e.cur.Load(); d != nil && d.gate.IsOpen() && (f.State != state.Enabled || !active) {
		e.violate(where, fmt.Sprintf("I1：闸门开，状态 %s，pf 生效=%v", f.State, active))
	}
	st := e.sys.Net.Snapshot()
	traces := len(st.Routes) > 0 || st.DNS.IsValid()
	for svc, s := range st.IPv6 {
		if s.Mode == sysnet.IPv6Off && e.init.IPv6[svc].Mode != sysnet.IPv6Off {
			traces = true
		}
	}
	if traces && !active {
		e.violate(where, "I2：有接管痕迹但 pf 没生效")
	}
	if (f.State != state.Restored || traces) && !e.blocked() {
		e.violate(where, "泄漏：en0 上普通用户到公网被放行")
	}
}

func (e *env) violate(where, what string) {
	e.mu.Lock()
	e.violations = append(e.violations, where+"："+what)
	e.mu.Unlock()
}

func (e *env) noViolations() {
	e.t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.violations) > 0 {
		e.t.Fatalf("不变量被破坏：\n%s", strings.Join(e.violations, "\n"))
	}
}

func client(sock string) *http.Client {
	return &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{
		DisableKeepAlives: true, // 每次请求新建客户端，不留空闲连接（否则查 goroutine 残留时会算上它们）
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
}

// call 发一个请求，返回状态码和响应体。
func call(t *testing.T, d *Daemon, method, path string, body []byte) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, "http://abroadd"+path, bytes.NewReader(body))
	resp, err := client(d.SocketPath()).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// waitGateOpen 等复查循环放行（Recover 到 ENABLED 后由它异步开闸门，m4-plan v2.2 决定 4）。
func waitGateOpen(t *testing.T, d *Daemon) Status {
	t.Helper()
	var s Status
	for i := 0; i < 300; i++ {
		if s = status(t, d); s.GateOpen {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	return s
}

func status(t *testing.T, d *Daemon) Status {
	t.Helper()
	code, b := call(t, d, "GET", "/v1/status", nil)
	if code != 200 {
		t.Fatalf("status %d %s", code, b)
	}
	var s Status
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// post 发一个操作请求。监护每轮会短暂拿住状态机的忙标记，这时得到 409 是设计的行为：调用方稍后重试
// （TestBusy409 直接用 call 测 409 本身）。
// confirmCall 发一个操作请求；要两步确认的（恢复，地区会变时的启用，m4-plan v2.3 决定 5、v2.4）：
// 第一次拿到带确认码的 409 后，带确认码再调一次。
func confirmCall(t *testing.T, d *Daemon, path string) (int, []byte) {
	t.Helper()
	code, b := call(t, d, "POST", path, nil)
	if code != http.StatusConflict {
		return code, b
	}
	var cn ConfirmNeeded
	if json.Unmarshal(b, &cn) != nil || cn.Confirm == "" {
		return code, b // 别的 409（忙）
	}
	body, _ := json.Marshal(ConfirmRequest{Confirm: cn.Confirm})
	return call(t, d, "POST", path, body)
}

func post(t *testing.T, d *Daemon, path string, want int) []byte {
	t.Helper()
	code, b := confirmCall(t, d, path)
	for i := 0; code == http.StatusConflict && want != http.StatusConflict && i < 100; i++ {
		time.Sleep(10 * time.Millisecond)
		code, b = confirmCall(t, d, path)
	}
	if code != want {
		t.Fatalf("%s：%d（期望 %d）%s", path, code, want, b)
	}
	return b
}

// callOp 发一个操作请求，返回状态码和应答；遇到监护短暂占用的 409 时重试（同 post），
// 给期望操作失败、要检查具体状态码的用例用，避免把 409 误当成被测的失败。
func callOp(t *testing.T, d *Daemon, path string) (int, []byte) {
	t.Helper()
	code, b := confirmCall(t, d, path)
	for i := 0; code == http.StatusConflict && i < 100; i++ {
		time.Sleep(10 * time.Millisecond)
		code, b = confirmCall(t, d, path)
	}
	return code, b
}

// d. 进程内端到端：启用 → 状态 → 停用 → 再启用 → 恢复。
func TestEndToEnd(t *testing.T) { bothSystems(t, testEndToEnd) }

func testEndToEnd(t *testing.T, e *env) {
	d := e.start(e.opts())
	if s := status(t, d); s.State != "RESTORED" || s.GateOpen {
		t.Fatalf("%+v", s)
	}
	post(t, d, "/v1/enable", 200)
	s := status(t, d)
	if s.State != "ENABLED" || !s.GateOpen || !s.Protected || s.Utun == "" {
		t.Fatalf("%+v", s)
	}
	post(t, d, "/v1/disable", 200)
	if s := status(t, d); s.State != "DISABLED" || s.GateOpen || !s.Protected {
		t.Fatalf("%+v", s)
	}
	post(t, d, "/v1/enable", 200)
	post(t, d, "/v1/restore", 200)
	s = status(t, d)
	if s.State != "RESTORED" || s.GateOpen || s.Utun != "" {
		t.Fatalf("%+v", s)
	}
	if !e.sys.Net.Snapshot().Equal(e.init) || e.sys.PF.Active(pf.DefaultAnchor) || len(e.sys.PF.Tokens()) != 0 {
		t.Fatal("没有回国干净")
	}
	if len(s.Events) == 0 {
		t.Fatal("应有事件")
	}
	e.noViolations()
}

type blockingPre struct {
	entered, release chan struct{}
	once             sync.Once
}

func (b *blockingPre) Check(context.Context) error {
	b.once.Do(func() { close(b.entered); <-b.release })
	return nil
}

// d. 启用中再发一个启用得到 409。
func TestBusy409(t *testing.T) {
	e := newEnv(t)
	bp := &blockingPre{entered: make(chan struct{}), release: make(chan struct{})}
	e.sys.Pre = bp
	d := e.start(e.opts())
	done := make(chan int, 1)
	go func() { c, _ := call(t, d, "POST", "/v1/enable", nil); done <- c }()
	<-bp.entered
	code, b := call(t, d, "POST", "/v1/enable", nil)
	if code != http.StatusConflict || !strings.Contains(string(b), "失败：") {
		t.Fatalf("%d %s", code, b)
	}
	close(bp.release)
	if c := <-done; c != 200 {
		t.Fatalf("第一个启用 %d", c)
	}
	e.noViolations()
}

// sampleRunner 用 preflight 的 testdata 应答（PAC 开着）。
type sampleRunner struct{ t *testing.T }

func (r sampleRunner) Run(_ context.Context, p string, args []string, _ []byte) ([]byte, []byte, error) {
	files := map[string]string{
		"netstat -rn -f inet": "netstat-clean.constructed.txt", "scutil --proxy": "proxy-pac.constructed.txt",
		"scutil --nwi": "nwi-v4.txt", "ps -axo comm": "ps-clash.txt", "route -n get default": "route-default-en0.txt",
		"route -n get 1.1.1.1": "route-en0.constructed.txt", "route -n get 129.0.0.1": "route-en0.constructed.txt",
		"route -n get 198.18.0.2": "route-en0.constructed.txt",
	}
	key := filepath.Base(p) + " " + strings.Join(args, " ")
	b, err := os.ReadFile(filepath.Join("..", "preflight", "testdata", files[key]))
	if err != nil {
		r.t.Fatalf("%s：%v", key, err)
	}
	return b, nil, nil
}

// d. 冲突检查阻断得到 422，带处理办法，状态文件未写入；提示项在 status 里。
func TestPreflight422(t *testing.T) {
	e := newEnv(t)
	e.sys.Pre = PreflightHints{C: &preflight.Checker{R: sampleRunner{t}}}
	d := e.start(e.opts())
	code, b := callOp(t, d, "/v1/enable")
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(b), "自动代理配置") || !strings.Contains(string(b), "请按下面处理") {
		t.Fatalf("%d %s", code, b)
	}
	if _, err := os.Stat(filepath.Join(e.dir, state.FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("状态文件不应写入")
	}
	s := status(t, d)
	if len(s.Hints) != 1 || s.Hints[0].ID != "proxy_app" {
		t.Fatalf("提示项：%+v", s.Hints)
	}
	if !e.sys.Net.Snapshot().Equal(e.init) || e.sys.PF.Active(pf.DefaultAnchor) {
		t.Fatal("不应有改动")
	}
	e.noViolations()
}

// d. 结束 Claude 失败时恢复得到 500，带三段文字，系统没有改动。
func TestRestore500(t *testing.T) {
	e := newEnv(t)
	d := e.start(e.opts())
	post(t, d, "/v1/enable", 200)
	before := e.sys.Net.Snapshot()
	e.sys.Kill.SetFail(ErrFakeKill)
	code, b := callOp(t, d, "/v1/restore")
	var eb errBody
	_ = json.Unmarshal(b, &eb)
	if code != 500 || !strings.HasPrefix(eb.Error, "结束 Claude 进程失败：") || !strings.Contains(eb.Error, "本机网络没有改动") {
		t.Fatalf("%d %s", code, b)
	}
	if !e.sys.Net.Snapshot().Equal(before) || status(t, d).State != "ENABLED" || !status(t, d).GateOpen {
		t.Fatal("系统不应有改动")
	}
	e.noViolations()
}

// e. 重启恢复：走退出流程、直接丢弃（被杀）都能恢复；状态文件损坏时进程不退出。
func TestRestart(t *testing.T) { bothSystems(t, testRestart) }

func testRestart(t *testing.T, e *env) {
	d := e.start(e.opts())
	post(t, d, "/v1/enable", 200)
	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	e.d = nil
	e.cur.Store(nil)
	f, _ := e.view.Load()
	if f.State != state.Enabled || !e.sys.PF.Active(pf.DefaultAnchor) || !e.blocked() {
		t.Fatalf("退出后应保持断网：%s", f.State)
	}
	if _, err := os.Stat(d.SocketPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("socket 应已删除")
	}
	d2 := e.start(e.opts())
	if s := waitGateOpen(t, d2); s.State != "ENABLED" || !s.GateOpen || !s.Protected {
		t.Fatalf("重启后：%+v", s)
	}
	// 被杀：不走退出流程。
	d2.abandon()
	e.d = nil
	e.cur.Store(nil)
	d3 := e.start(e.opts())
	if s := waitGateOpen(t, d3); s.State != "ENABLED" || !s.GateOpen {
		t.Fatalf("被杀后重启：%+v", s)
	}
	post(t, d3, "/v1/restore", 200)
	if !e.sys.Net.Snapshot().Equal(e.init) {
		t.Fatal("恢复后应与初始值相等")
	}
	e.noViolations()
}

func TestCorruptStart(t *testing.T) {
	e := newEnv(t)
	e.noCheck.Store(true) // 状态文件读不出来，不变量检查做不了
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, state.FileName), []byte("{坏"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := e.start(e.opts())
	s := status(t, d)
	if !s.Corrupt || s.State != "UNKNOWN" {
		t.Fatalf("%+v", s)
	}
	code, b := callOp(t, d, "/v1/enable")
	if code != http.StatusServiceUnavailable || !strings.Contains(string(b), "失败：") {
		t.Fatalf("%d %s", code, b)
	}
	if !e.sys.PF.Active(pf.DefaultAnchor) || !e.blocked() {
		t.Fatal("损坏时 pf 应生效")
	}
}

// f. 锁：第二个实例在第一个持锁时启动，时限内报错退出且没有改动；第一个退出后第二个能启动。
func TestLock(t *testing.T) {
	e := newEnv(t)
	d := e.start(e.opts())
	post(t, d, "/v1/enable", 200)
	before, pfCalls := e.sys.Net.Snapshot(), len(e.sys.PF.Calls())
	t0 := time.Now()
	_, err := Start(bg, e.opts())
	if !errors.Is(err, state.ErrLocked) || time.Since(t0) > 2*time.Second {
		t.Fatalf("%v（用时 %v）", err, time.Since(t0))
	}
	if !e.sys.Net.Snapshot().Equal(before) || len(e.sys.PF.Calls()) != pfCalls {
		t.Fatal("拿不到锁的实例不应有任何改动")
	}
	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	d2 := e.start(e.opts())
	if s := status(t, d2); s.State != "ENABLED" {
		t.Fatalf("%+v", s)
	}
	e.noViolations()
}

// g. 接口安全。
func TestSocketSecurity(t *testing.T) {
	e := newEnv(t)
	var rejected atomic.Int32
	o := e.opts()
	o.PeerUID = func(*net.UnixConn) (int, error) { return os.Getuid() + 1, nil }
	o.OnReject = func() { rejected.Add(1) }
	d := e.start(o)
	for _, p := range []string{e.dir, d.SocketPath()} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o600)
		if p == e.dir {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s 权限 %o", p, info.Mode().Perm())
		}
	}
	req, _ := http.NewRequest("POST", "http://abroadd/v1/enable", nil)
	if _, err := client(d.SocketPath()).Do(req); err == nil {
		t.Fatal("对端 uid 不对时连接应被断开")
	}
	if rejected.Load() == 0 || e.sys.Net.Snapshot().Equal(e.init) == false || e.sys.PF.Active(pf.DefaultAnchor) {
		t.Fatal("应断开且没有触达处理函数")
	}
	if f, _ := e.view.Load(); f.State != state.Restored {
		t.Fatal("不应有改动")
	}
	d.abandon()
	e.d = nil
	e.cur.Store(nil)

	// 正常实例：上限、404、405、status 不含敏感内容。
	d = e.start(e.opts())
	// 服务端看到 Content-Length 超限就回 413 并关连接；客户端可能还在写请求体，先拿到 broken pipe。两种都是被拒绝。
	big, _ := http.NewRequest("POST", "http://abroadd/v1/enable", bytes.NewReader(bytes.Repeat([]byte("x"), MaxBody+1)))
	if resp, err := client(d.SocketPath()).Do(big); err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("超长请求体：%d", resp.StatusCode)
		}
	}
	if f, _ := e.view.Load(); f.State != state.Restored {
		t.Fatal("超长请求体不应触发启用")
	}
	if code, _ := call(t, d, "GET", "/v1/nope", nil); code != 404 {
		t.Fatalf("未知路径：%d", code)
	}
	if code, _ := call(t, d, "GET", "/v1/enable", nil); code != 405 {
		t.Fatalf("方法不对：%d", code)
	}
	if code, _ := call(t, d, "POST", "/v1/status", nil); code != 405 {
		t.Fatalf("方法不对：%d", code)
	}
	post(t, d, "/v1/enable", 200)
	f, _ := e.view.Load()
	if len(f.PFTokens) == 0 {
		t.Fatal("前提：应有 token")
	}
	_, b := call(t, d, "GET", "/v1/status", nil)
	for _, secret := range append([]string{"192.168.1.0", "fd00::5", "fd00::1", "bridge100"}, f.PFTokens...) {
		if strings.Contains(string(b), `"`+secret+`"`) || strings.Contains(string(b), secret+"/") || (len(secret) > 4 && strings.Contains(string(b), secret)) {
			t.Fatalf("status 里出现了 %q：%s", secret, b)
		}
	}
	for _, tok := range f.PFTokens {
		if strings.Contains(string(b), tok) {
			t.Fatalf("status 里出现了 token %s：%s", tok, b)
		}
	}
	d.abandon()
	e.d = nil
	e.cur.Store(nil)

	// socket 路径是符号链接：拒绝启动。
	sock := filepath.Join(e.dir, SocketName)
	_ = os.Remove(sock)
	if err := os.Symlink(filepath.Join(t.TempDir(), "target"), sock); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(bg, e.opts()); !errors.Is(err, ErrSocketSymlink) {
		t.Fatalf("%v", err)
	}
	if fi, _ := os.Lstat(sock); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("符号链接不应被动")
	}
}

// h. 监护在 daemon 里跑着。
func TestSupervisorRunning(t *testing.T) { bothSystems(t, testSupervisorRunning) }

func testSupervisorRunning(t *testing.T, e *env) {
	o := e.opts()
	o.SuperviseInterval = 50 * time.Millisecond
	d := e.start(o)
	post(t, d, "/v1/enable", 200)
	e.sys.PF.ClearAnchor()
	deadline := time.Now().Add(2*o.SuperviseInterval + time.Second) // 两个间隔，加上 -race 下的调度余量
	for !e.sys.PF.Active(pf.DefaultAnchor) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !e.sys.PF.Active(pf.DefaultAnchor) {
		t.Fatal("监护没有把锚点恢复")
	}
	if !status(t, d).GateOpen {
		t.Fatal("锚点被清、当轮修好时不应关闸门")
	}
	e.noCheck.Store(true) // 主规则集被换掉期间锚点不被求值，属外部破坏
	e.sys.PF.DropMainRef()
	deadline = time.Now().Add(2 * time.Second)
	for status(t, d).GateOpen && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	s := status(t, d)
	if s.GateOpen || s.Protected {
		t.Fatalf("应关闸门、保护失效：%+v", s)
	}
	found := false
	for _, ev := range s.Events {
		if ev.Kind == string(state.EvGateClosed) {
			found = true
		}
	}
	if !found {
		t.Fatalf("应有关闸门的事件：%+v", s.Events)
	}
	e.sys.PF.RestoreMainRef()
	e.noViolations()
}
