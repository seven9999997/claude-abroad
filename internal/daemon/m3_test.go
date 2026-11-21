package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"claude-abroad/internal/directdial"
	"claude-abroad/internal/rules"
	"claude-abroad/internal/tun"
	"claude-abroad/internal/tunsim"
	"claude-abroad/internal/wire"
)

// m3Opener 是假的数据通道：出口检查（exit.test）回 204；其余按出口记帧头、原样回显；可按出口回“出口不存在”。
type m3Opener struct {
	mu      sync.Mutex
	headers []wire.Header
	missing map[string]bool
}

func (o *m3Opener) Open(_ context.Context, h wire.Header) (net.Conn, error) {
	if h.Host == "exit.test" {
		a, b := net.Pipe()
		go func() {
			buf := make([]byte, 4096)
			_, _ = b.Read(buf)
			_, _ = io.WriteString(b, "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")
			b.Close()
		}()
		return a, nil
	}
	o.mu.Lock()
	o.headers = append(o.headers, h)
	missing := o.missing[h.Exit]
	o.mu.Unlock()
	if missing {
		return nil, &wire.ReplyError{Code: wire.CodeUnknownExit}
	}
	a, b := net.Pipe()
	go func() { _, _ = io.Copy(b, b); b.Close() }()
	return a, nil
}

func (o *m3Opener) Close() error { return nil }

func (o *m3Opener) exits() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []string
	for _, h := range o.headers {
		out = append(out, h.Exit)
	}
	return out
}

// m3Direct 是假的直连拨号模块：记下下发的配置和拨号，回显；可注入配置失败。
type m3Direct struct {
	mu        sync.Mutex
	dials     []string
	dns       []netip.Addr
	pinned    []string
	configs   int
	failCfg   atomic.Bool
	pinnedSet map[string]bool
	source    func() directdial.Config
	okGen     uint64
}

func (d *m3Direct) SetConfigSource(f func() directdial.Config) {
	d.mu.Lock()
	d.source = f
	d.mu.Unlock()
}

func (d *m3Direct) ConfiguredGen() (uint64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.okGen, d.configs > 0
}

func (d *m3Direct) Dial(_ context.Context, network, host string, ip netip.Addr, port uint16) (net.Conn, error) {
	t := host
	if t == "" {
		t = ip.String()
	}
	d.mu.Lock()
	d.dials = append(d.dials, network+" "+t)
	p := d.pinnedSet[t]
	d.mu.Unlock()
	if p {
		return nil, directdial.ErrPinned
	}
	a, b := net.Pipe()
	go func() { _, _ = io.Copy(b, b); b.Close() }()
	return a, nil
}

func (d *m3Direct) Configure(_ context.Context, cfg directdial.Config) error {
	if d.failCfg.Load() {
		return errors.New("注入的下发失败")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dns, d.pinned = append([]netip.Addr(nil), cfg.DNS...), append([]string(nil), cfg.PinnedExtra...)
	d.okGen = cfg.Gen
	d.configs++
	return nil
}

func (d *m3Direct) snapshot() (dials []string, pinned []string, configs int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.dials...), append([]string(nil), d.pinned...), d.configs
}

// m3Ident 按源端口段区分应用（同 stack 的测试约定）。
type m3Ident struct{}

func (m3Ident) Identify(_ context.Context, _ string, src, _ netip.AddrPort) (rules.AppID, bool) {
	switch p := src.Port(); {
	case p >= 20000 && p < 21000:
		return "com.app.a", false
	case p >= 21000 && p < 22000:
		return "com.app.b", false
	case p >= 22000 && p < 23000:
		return "com.app.c", false
	}
	return "com.app.other", false
}

type m3Env struct {
	*dataEnv
	op       *m3Opener
	direct   *m3Direct
	d        *Daemon
	cl       *tunsim.Client
	logBuf   *syncBuf
	clock    atomic.Int64
	clMu     sync.Mutex
	lazyCl   *tunsim.Client
	lazyPeer tun.Device
}

const m3Rules = `
rules:
  - {app: com.app.a, action: "proxy:wg-a"}
  - {app: com.app.b, action: "proxy:clash-b"}
  - {app: com.app.c, action: direct}
local_domains: ["*.corp.example"]
offline_allow: [com.app.c]
`

func newM3Daemon(t *testing.T, initialRules string) *m3Env {
	t.Helper()
	fake := &m3Direct{pinnedSet: map[string]bool{}}
	e := newM3DaemonWith(t, initialRules, fake)
	e.direct = fake
	return e
}

// newM3DaemonWith 同 newM3Daemon，直连拨号模块的客户端由调用方给（端到端测试用真实的 directdial.Client）。
func newM3DaemonWith(t *testing.T, initialRules string, direct DirectConfigurer, mods ...func(*Options)) *m3Env {
	return newM3DaemonCore(t, initialRules, direct, func(_ *m3Env, o *Options) {
		for _, m := range mods {
			m(o)
		}
	})
}

// newM3DaemonCore：before 在启动之前调用（拿到 m3Env，可以改 Options）。客户端按需建（getClient），
// 启用过程中（例如环境检测的探测）也能用同一个客户端。
func newM3DaemonCore(t *testing.T, initialRules string, direct DirectConfigurer, before func(*m3Env, *Options)) *m3Env {
	t.Helper()
	op := &m3Opener{missing: map[string]bool{}}
	de := newDataEnv(t, op, nil)
	e := &m3Env{dataEnv: de, op: op, logBuf: &syncBuf{}}
	e.clock.Store(time.Unix(1_900_000_000, 0).UnixNano())
	de.dp.now = func() time.Time { return time.Unix(0, e.clock.Load()) }
	de.dp.SetDirect(direct)
	de.dp.SetIdent(m3Ident{})
	if initialRules != "" {
		if err := os.MkdirAll(de.dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(de.dir, RulesFile), []byte(initialRules), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	o := de.opts()
	o.Log = log.New(e.logBuf, "", 0)
	// M3 的多出口是开发用的（指向本机应答的开发上游，M4-8 起由构造时的 DevExits 登记）。
	o.DevExits = []string{"wg-a", "clash-b"}
	if before != nil {
		before(e, &o)
	}
	e.d = de.start(o)
	post(t, e.d, "/v1/enable", 200)
	cl, err := e.getClient()
	if err != nil {
		t.Fatal(err)
	}
	e.cl = cl
	return e
}

// getClient 在最新的管道上建（或复用）客户端。
func (e *m3Env) getClient() (*tunsim.Client, error) {
	e.clMu.Lock()
	defer e.clMu.Unlock()
	e.mu.Lock()
	peer := e.peers[len(e.peers)-1]
	e.mu.Unlock()
	if e.lazyCl != nil && e.lazyPeer == peer {
		return e.lazyCl, nil
	}
	cl, err := tunsim.NewClient(peer, netip.Addr{}) // utun 重建过（恢复后再启用）：换到新的管道
	if err != nil {
		return nil, err
	}
	e.t.Cleanup(func() { cl.Close() })
	e.lazyCl, e.lazyPeer = cl, peer
	return cl, nil
}

func (e *m3Env) lookup(t *testing.T, name string) netip.Addr {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, err := e.cl.LookupA(ctx, name, netip.MustParseAddrPort("198.18.0.2:53"))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (e *m3Env) echo(t *testing.T, port uint16, dst netip.AddrPort) error {
	t.Helper()
	c, err := e.dial(port, dst)
	if err != nil {
		return err
	}
	defer c.Close()
	return echoConn(c)
}

func (e *m3Env) dial(port uint16, dst netip.AddrPort) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return e.cl.DialTCPFrom(ctx, port, dst)
}

func echoConn(c net.Conn) error {
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte("hi")); err != nil {
		return err
	}
	b := make([]byte, 2)
	if _, err := io.ReadFull(c, b); err != nil {
		return err
	}
	if string(b) != "hi" {
		return errors.New("回显不对")
	}
	return nil
}

func putRules(t *testing.T, d *Daemon, y string, drop bool) (int, string) {
	t.Helper()
	q := "?drop_affected=0"
	if drop {
		q = "?drop_affected=1"
	}
	code, b := call(t, d, "PUT", "/v1/rules"+q, []byte(y))
	for i := 0; code == 409 && i < 100; i++ { // 监护每轮会短暂占住忙标记
		time.Sleep(10 * time.Millisecond)
		code, b = call(t, d, "PUT", "/v1/rules"+q, []byte(y))
	}
	return code, string(b)
}

func pinnedDomain() string { return "claude.ai" }

// d. 规则接口：GET、PUT、持久化、校验失败、注入失败、drop_affected、409、损坏的文件。
func TestRulesAPI(t *testing.T) {
	e := newM3Daemon(t, "")
	code, b := call(t, e.d, "GET", "/v1/rules", nil)
	if code != 200 || !strings.Contains(string(b), "default: proxy:main") {
		t.Fatalf("默认规则：%d %s", code, b)
	}
	if code, msg := putRules(t, e.d, m3Rules, false); code != 200 {
		t.Fatalf("PUT 合法规则：%d %s", code, msg)
	}
	_, b = call(t, e.d, "GET", "/v1/rules", nil)
	want, _ := mustParseRules(t, m3Rules).Marshal()
	if string(b) != string(want) {
		t.Fatalf("GET 应返回新规则：%s", b)
	}
	fi, err := os.Stat(filepath.Join(e.dir, RulesFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("规则文件应是 0600：%v %v", fi, err)
	}
	if fb, _ := os.ReadFile(filepath.Join(e.dir, RulesFile)); string(fb) != string(want) {
		t.Fatal("文件内容应等于新规则")
	}
	// 新连接按新规则走。
	plain := netip.AddrPortFrom(e.lookup(t, "plain.example.org"), 443)
	if err := e.echo(t, 20001, plain); err != nil {
		t.Fatal(err)
	}
	if ex := e.op.exits(); len(ex) == 0 || ex[len(ex)-1] != "wg-a" {
		t.Fatalf("应走出口 a：%v", ex)
	}
	// 不合法：422，指出哪一项；内存和文件都不变。
	code, msg := putRules(t, e.d, "rules: [{app: com.anthropic.claudefordesktop, action: direct}]\n", false)
	if code != 422 || !strings.Contains(msg, "rules 第 1 条") {
		t.Fatalf("不合法的规则应回 422 并指出哪一项：%d %s", code, msg)
	}
	if _, b = call(t, e.d, "GET", "/v1/rules", nil); string(b) != string(want) {
		t.Fatal("校验失败时内存不变")
	}
	// 两处注入失败：都回错误，内存、文件、fakedns 的本地名单、直连配置（含代数）都是旧的。
	dc0 := e.dp.directConfig()
	other := strings.Replace(m3Rules, "*.corp.example", "*.intra.example", 1) + "pinned_extra: [x.special.example]\n"
	check := func(step string) {
		t.Helper()
		if _, b := call(t, e.d, "GET", "/v1/rules", nil); string(b) != string(want) {
			t.Fatalf("%s 失败后内存应是旧的", step)
		}
		if fb, _ := os.ReadFile(filepath.Join(e.dir, RulesFile)); string(fb) != string(want) {
			t.Fatalf("%s 失败后文件应是旧的", step)
		}
		if !e.dp.pool.IsLocalName("git.corp.example") || e.dp.pool.IsLocalName("wiki.intra.example") {
			t.Fatalf("%s 失败后 fakedns 的本地名单应是旧的", step)
		}
		if dc := e.dp.directConfig(); dc.Gen != dc0.Gen || !slices.Equal(dc.PinnedExtra, dc0.PinnedExtra) {
			t.Fatalf("%s 失败后直连配置应是旧的：%+v %+v", step, dc, dc0)
		}
	}
	e.d.writeRules = func(string, []byte) error { return errors.New("注入的写文件失败") }
	if code, _ := putRules(t, e.d, other, false); code != 500 {
		t.Fatalf("写文件失败应回错误：%d", code)
	}
	check("写文件")
	e.d.writeRules = nil
	e.dp.syncFail = func() error { return errors.New("注入的同步失败") }
	if code, _ := putRules(t, e.d, other, false); code != 500 {
		t.Fatalf("同步失败应回错误：%d", code)
	}
	e.dp.syncFail = nil
	check("同步 fakedns")
	// 直连拨号模块连不上（下发会失败）不妨碍改规则（m3-plan v2.9）：不再同步下发，只更新代数。
	e.direct.failCfg.Store(true)
	if code, _ := putRules(t, e.d, other, false); code != 200 {
		t.Fatalf("直连拨号模块连不上时改规则应成功：%d", code)
	}
	if dc := e.dp.directConfig(); dc.Gen != dc0.Gen+1 || !slices.Equal(dc.PinnedExtra, []string{"x.special.example"}) {
		t.Fatalf("置顶追加名单变了代数应加一：%+v", dc)
	}
	e.direct.failCfg.Store(false)
	if code, _ := putRules(t, e.d, m3Rules, false); code != 200 {
		t.Fatal("改回原规则失败")
	}
	// drop_affected：=0 时长连接保持，=1 时受影响的被断。
	c, err := e.dial(20002, plain)
	if err != nil || echoConn(c) != nil {
		t.Fatalf("长连接：%v", err)
	}
	defer c.Close()
	toB := strings.Replace(m3Rules, `"proxy:wg-a"`, `"proxy:clash-b"`, 1)
	if code, _ := putRules(t, e.d, toB, false); code != 200 || echoConn(c) != nil {
		t.Fatal("drop_affected=0 时长连接应保持")
	}
	if code, _ := putRules(t, e.d, m3Rules, true); code != 200 {
		t.Fatal("PUT 失败")
	}
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("drop_affected=1 时受影响的长连接应被断")
	}
	// 启用、停用、恢复进行中：409。
	held := make(chan struct{})
	release := make(chan struct{})
	go func() { _ = e.d.m.Exclusive(func() error { close(held); <-release; return nil }) }()
	<-held
	code, _ = call(t, e.d, "PUT", "/v1/rules", []byte(m3Rules))
	close(release)
	if code != 409 {
		t.Fatalf("忙时应回 409：%d", code)
	}
	// 重启 daemon：规则还在。
	_ = e.d.Shutdown()
	d2 := e.dataEnv.start(e.dataEnv.opts())
	if _, b := call(t, d2, "GET", "/v1/rules", nil); string(b) != string(want) {
		t.Fatalf("重启后规则应还在：%s", b)
	}
}

func mustParseRules(t *testing.T, y string) *rules.Config {
	t.Helper()
	c, err := rules.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// d. 启动时规则文件损坏：用默认配置，有事件，进程不退出。
func TestRulesFileCorrupt(t *testing.T) {
	e := newM3Daemon(t, "rules: [ this is not yaml")
	s := status(t, e.d)
	if s.Rules == nil || s.Rules.Default != "proxy:main" || s.Rules.Direct != 0 {
		t.Fatalf("应用默认配置：%+v", s.Rules)
	}
	found := false
	for _, ev := range s.Events {
		found = found || ev.Kind == "rules_invalid"
	}
	if !found {
		t.Fatal("应有“规则文件不合法”的事件")
	}
}

// e. 多出口多应用；把走出口 A 的应用改成直连后访问置顶域名经 main；出口 A 不可用时回退 main、status 列出它、30 秒后恢复。
func TestMultiExitMultiApp(t *testing.T) {
	e := newM3Daemon(t, m3Rules)
	plain := netip.AddrPortFrom(e.lookup(t, "plain.example.org"), 443)
	var wg sync.WaitGroup
	errs := make(chan error, 30)
	for i := 0; i < 10; i++ {
		for _, base := range []uint16{20000, 21000, 22000} {
			wg.Add(1)
			go func(port uint16) { defer wg.Done(); errs <- e.echo(t, port, plain) }(base + uint16(i) + 10)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	count := map[string]int{}
	for _, x := range e.op.exits() {
		count[x]++
	}
	dials, _, _ := e.direct.snapshot()
	if count["wg-a"] != 10 || count["clash-b"] != 10 || len(dials) != 10 {
		t.Fatalf("出口 a %d、出口 b %d、直连 %d", count["wg-a"], count["clash-b"], len(dials))
	}
	// A 改成直连后访问置顶域名：经 main，直连拨号数不变。
	aDirect := strings.Replace(m3Rules, `"proxy:wg-a"`, `direct`, 1)
	if code, msg := putRules(t, e.d, aDirect, false); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	pin := netip.AddrPortFrom(e.lookup(t, pinnedDomain()), 443)
	n0, d0 := len(e.op.exits()), len(dials)
	if err := e.echo(t, 20100, pin); err != nil {
		t.Fatal(err)
	}
	if ex := e.op.exits(); len(ex) != n0+1 || ex[n0] != "main" {
		t.Fatalf("应经 main：%v", ex[n0:])
	}
	if dials, _, _ := e.direct.snapshot(); len(dials) != d0 {
		t.Fatal("直连拨号数不应变")
	}
	// 出口 A 不可用：回 unknown_exit 后，新连接走 main，status 列出它；30 秒后重新走出口 A。
	if code, _ := putRules(t, e.d, m3Rules, false); code != 200 {
		t.Fatal("PUT 失败")
	}
	e.op.mu.Lock()
	e.op.missing["wg-a"] = true
	e.op.mu.Unlock()
	_ = e.echo(t, 20200, plain) // 这一条失败，标记出口 a 不可用
	e.op.mu.Lock()
	e.op.missing["wg-a"] = false
	e.op.mu.Unlock()
	if s := status(t, e.d); exitUpIn(s, "wg-a") {
		t.Fatalf("status 应列出不可用的出口：%v", s.Exits)
	}
	n1 := len(e.op.exits())
	if err := e.echo(t, 20201, plain); err != nil {
		t.Fatal(err)
	}
	if ex := e.op.exits(); ex[n1] != "main" {
		t.Fatalf("出口 a 不可用时应回退 main：%v", ex[n1:])
	}
	e.clock.Add(int64(31 * time.Second))
	n2 := len(e.op.exits())
	if err := e.echo(t, 20202, plain); err != nil {
		t.Fatal(err)
	}
	if ex := e.op.exits(); ex[n2] != "wg-a" {
		t.Fatalf("30 秒后应重新走出口 a：%v", ex[n2:])
	}
	if s := status(t, e.d); !exitUpIn(s, "wg-a") {
		t.Fatalf("过期后 status 里不应再标为不可用：%v", s.Exits)
	}
}

// f. 停用：只有 offline_allow 里的应用能直连，置顶域名不可达；恢复后数据面拆除，没有残留。
func TestDisableAndRestoreFlow(t *testing.T) {
	e := newM3Daemon(t, m3Rules)
	plain := netip.AddrPortFrom(e.lookup(t, "plain.example.org"), 443)
	pin := netip.AddrPortFrom(e.lookup(t, pinnedDomain()), 443)
	post(t, e.d, "/v1/disable", 200)
	if err := e.echo(t, 22001, plain); err != nil {
		t.Fatalf("offline_allow 里的应用停用时应能直连：%v", err)
	}
	if err := e.echo(t, 20001, plain); err == nil {
		t.Fatal("名单外的应用停用时应被拦截")
	}
	if err := e.echo(t, 22002, pin); err == nil {
		t.Fatal("停用时置顶域名应不可达")
	}
	post(t, e.d, "/v1/enable", 200)
	post(t, e.d, "/v1/restore", 200)
	if e.dp.StackAlive() {
		t.Fatal("恢复后数据面应拆除")
	}
	if n := e.dp.proxy.FlowRecords(); n != 0 {
		t.Fatalf("恢复后不应有在途流：%d", n)
	}
}

// h. status 和 daemon 日志里搜不到测试用的域名、目的地址、应用 ID。
func TestNoSecretsInStatusAndLog(t *testing.T) {
	e := newM3Daemon(t, m3Rules)
	plain := netip.AddrPortFrom(e.lookup(t, "secret-host.example.org"), 443)
	for _, p := range []uint16{20001, 21001, 22001} {
		_ = e.echo(t, p, plain)
	}
	_ = e.echo(t, 22002, netip.MustParseAddrPort("203.0.113.77:5222"))
	b, _ := json.Marshal(status(t, e.d))
	all := string(b) + e.logBuf.String()
	for _, s := range []string{"secret-host", "203.0.113", "com.app", "corp.example", "198.18.1", "198.19"} {
		if strings.Contains(all, s) {
			t.Fatalf("status 或日志里出现了 %q", s)
		}
	}
	if !bytes.Contains(b, []byte(`"rules"`)) || !bytes.Contains(b, []byte(`"direct"`)) || !bytes.Contains(b, []byte(`"stack"`)) {
		t.Fatalf("status 应有规则摘要、直连和网络栈计数：%s", b)
	}
}

// exitUpIn 从 status 里取一个出口的可用状态（没有这个出口时 false）。
func exitUpIn(s Status, tag string) bool {
	for _, x := range s.Exits {
		if x.Tag == tag {
			return x.Avail.Up
		}
	}
	return false
}
