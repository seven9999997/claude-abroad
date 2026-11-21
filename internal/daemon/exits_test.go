package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/exitconf"
	"claude-abroad/internal/state"
)

const tKey = "YW5vdGhlcjMyYnl0ZXNsb25na2V5Zm9ydGVzdGluZzE="

var tWG = "[Interface]\nPrivateKey = " + tKey + "\nAddress = 10.66.0.2/32\nDNS = 10.66.0.1\n\n[Peer]\nPublicKey = " + tKey +
	"\nEndpoint = vpn-secret-host.example.com:51820\nAllowedIPs = 0.0.0.0/0\n"

// fakeExitVM 是假的虚拟机一侧：记下下发的端点和出口，按设定回 agent 的状态。
type fakeExitVM struct {
	mu        sync.Mutex
	boot      string
	bootErr   error
	pushErr   error
	pushes    [][]string // 每次下发的出口标签
	eps       [][]ExitEndpoint
	agentDown map[string]bool
}

func (f *fakeExitVM) BootID(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.boot, f.bootErr
}

func (f *fakeExitVM) PushEndpoints(_ context.Context, eps []ExitEndpoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eps = append(f.eps, eps)
	return nil
}

func (f *fakeExitVM) status(exits []string) []AgentExit {
	var out []AgentExit
	for _, t := range exits {
		out = append(out, AgentExit{Tag: t, Up: !f.agentDown[t]})
	}
	return out
}

func (f *fakeExitVM) PushExits(_ context.Context, exits []StoredExit) ([]AgentExit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pushErr != nil {
		return nil, f.pushErr
	}
	var tags []string
	for _, x := range exits {
		tags = append(tags, x.Tag)
	}
	f.pushes = append(f.pushes, tags)
	return f.status(tags), nil
}

func (f *fakeExitVM) AgentExits(context.Context) ([]AgentExit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pushes) == 0 {
		return nil, nil
	}
	return f.status(f.pushes[len(f.pushes)-1]), nil
}

func (f *fakeExitVM) set(fn func()) { f.mu.Lock(); defer f.mu.Unlock(); fn() }

func (f *fakeExitVM) nPushes() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.pushes) }

// fakeChecks 是假的按出口检查：按标签回设定的结论（默认通过）。
type fakeChecks struct {
	mu    sync.Mutex
	split map[string]exitcheck.Result
	udp   map[string]exitcheck.Result
	block map[string]chan struct{} // 检查卡住，直到关闭或 ctx 结束
	calls map[string]int
}

func (c *fakeChecks) check(ctx context.Context, tag string) exitcheck.Report {
	c.mu.Lock()
	c.calls[tag]++
	s, ok := c.split[tag]
	if !ok {
		s = exitcheck.Pass
	}
	u, ok := c.udp[tag]
	if !ok {
		u = exitcheck.Pass
	}
	b := c.block[tag]
	c.mu.Unlock()
	if b != nil {
		select {
		case <-b:
		case <-ctx.Done():
			return exitcheck.Report{Exit: tag, Items: []exitcheck.Item{{Name: "https", Result: exitcheck.Unknown}}}
		}
	}
	return exitcheck.Report{Exit: tag, Items: []exitcheck.Item{
		{Name: "https", Result: s}, {Name: "ip", Result: exitcheck.Pass}, {Name: "mainland", Result: exitcheck.Pass},
		{Name: "doh", Result: exitcheck.Pass}, {Name: "udp", Result: u}, {Name: "rule", Result: exitcheck.Unknown}}}
}

func (c *fakeChecks) set(fn func()) { c.mu.Lock(); defer c.mu.Unlock(); fn() }

func (c *fakeChecks) n(tag string) int { c.mu.Lock(); defer c.mu.Unlock(); return c.calls[tag] }

type exitEnv struct {
	*m3Env
	vm *fakeExitVM
	ch *fakeChecks
}

func newExitEnv(t *testing.T, before func(dir string)) *exitEnv {
	vm := &fakeExitVM{boot: "boot-1", agentDown: map[string]bool{}}
	ch := &fakeChecks{split: map[string]exitcheck.Result{}, udp: map[string]exitcheck.Result{}, block: map[string]chan struct{}{}, calls: map[string]int{}}
	m := newM3DaemonWith(t, m3Rules, &m3Direct{}, func(o *Options) {
		o.Sys.ExitVM = vm
		o.Sys.CheckExit = ch.check
		if before != nil {
			before(o.StateDir)
		}
	})
	return &exitEnv{m3Env: m, vm: vm, ch: ch}
}

func putExit(t *testing.T, d *Daemon, tag string, kind exitconf.Kind, conf string) (int, string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"kind": kind, "conf": conf})
	code, out := call(t, d, "PUT", "/v1/exits/"+tag, b)
	return code, string(out)
}

func waitExit(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等不到：%s", what)
}

func (e *exitEnv) up(tag string) bool { return e.d.exitAvail.View(tag).Up }

// b. 接口与存储：增、查、改、删；不合法 422 指出行号且不留文件；文件 0600；应答、status、事件、日志里没有密钥和端点；
// 重启后（预先存在的文件）出口还在。
func TestExitsAPIAndStore(t *testing.T) {
	var pre string
	e := newExitEnv(t, func(dir string) {
		pre = dir
		s := &ExitStore{Dir: filepath.Join(dir, SecretsDir), UID: os.Getuid()}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := s.Put(StoredExit{Tag: "old", Kind: exitconf.WireGuard, Conf: tWG}); err != nil {
			t.Fatal(err)
		}
	})
	if !e.d.exits.Has("old") {
		t.Fatal("启动时应读出已存的出口（重启后出口还在）")
	}
	if code, msg := putExit(t, e.d, "hk", exitconf.WireGuard, tWG); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	fi, err := os.Stat(filepath.Join(pre, SecretsDir, "exit-hk.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("凭据文件应是 0600：%v %v", fi, err)
	}
	if di, _ := os.Stat(filepath.Join(pre, SecretsDir)); di.Mode().Perm() != 0o700 {
		t.Fatal("secrets 应是 0700")
	}
	bad := strings.Replace(tWG, "[Peer]", "PostUp = iptables -F\n[Peer]", 1)
	code, msg := putExit(t, e.d, "bad", exitconf.WireGuard, bad)
	if code != 422 || !strings.Contains(msg, "第 6 行") || strings.Contains(msg, "iptables") {
		t.Fatalf("不合法应 422 并指出行号、不回显：%d %s", code, msg)
	}
	if _, err := os.Stat(filepath.Join(pre, SecretsDir, "exit-bad.json")); !os.IsNotExist(err) {
		t.Fatal("不合法的配置不应留下文件")
	}
	for _, tag := range []string{"main", "Bad", "x_y"} {
		if code, _ := putExit(t, e.d, tag, exitconf.WireGuard, tWG); code != 422 {
			t.Fatalf("%s 应被拒：%d", tag, code)
		}
	}
	if code, _ := call(t, e.d, "PUT", "/v1/exits/zz", []byte(`{"kind":"wireguard","conf":"x","dev":true}`)); code != 400 {
		t.Fatal("开发标记打不上：未知字段应 400")
	}
	code, list := call(t, e.d, "GET", "/v1/exits", nil)
	if code != 200 || !strings.Contains(string(list), `"tag":"hk"`) || !strings.Contains(string(list), "已配置") {
		t.Fatalf("%d %s", code, list)
	}
	code, one := call(t, e.d, "GET", "/v1/exits/hk", nil)
	if code != 200 || !strings.Contains(string(one), `"endpoints":1`) {
		t.Fatalf("%d %s", code, one)
	}
	if code, _ := call(t, e.d, "GET", "/v1/exits/nope", nil); code != 404 {
		t.Fatal("没有的出口 404")
	}
	st, _ := json.Marshal(status(t, e.d))
	for name, s := range map[string]string{"列表": string(list), "单个": string(one), "status": string(st), "日志": e.logBuf.String(), "事件": fmt.Sprint(e.d.events.List())} {
		for _, secret := range []string{tKey, "vpn-secret-host", "51820"} {
			if strings.Contains(s, secret) {
				t.Fatalf("%s里不应出现 %q", name, secret)
			}
		}
	}
	// 改：同名再 PUT 是替换
	if code, msg := putExit(t, e.d, "hk", exitconf.WireGuard, strings.Replace(tWG, "10.66.0.2", "10.66.0.3", 1)); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	if code, _ := call(t, e.d, "DELETE", "/v1/exits/hk", nil); code != 200 {
		t.Fatal("删除")
	}
	if _, err := os.Stat(filepath.Join(pre, SecretsDir, "exit-hk.json")); !os.IsNotExist(err) || e.d.exits.Has("hk") {
		t.Fatal("删除后文件和内存里都没有")
	}
	if code, _ := call(t, e.d, "DELETE", "/v1/exits/hk", nil); code != 404 {
		t.Fatal("再删 404")
	}
}

// c. 与规则的一致性：引用不存在的出口被拒并指出序号；删除被引用的出口 409 并列出序号；和忙标记互斥。
func TestExitsRuleConsistency(t *testing.T) {
	e := newExitEnv(t, nil)
	withHK := strings.Replace(m3Rules, `"proxy:clash-b"`, `"proxy:hk"`, 1)
	code, msg := putRules(t, e.d, withHK, false)
	if code != 422 || !strings.Contains(msg, "第 2 条") || !strings.Contains(msg, "hk") {
		t.Fatalf("引用不存在的出口应 422 并指出第 2 条：%d %s", code, msg)
	}
	if code, msg := putExit(t, e.d, "hk", exitconf.WireGuard, tWG); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	if code, msg := putRules(t, e.d, withHK, false); code != 200 {
		t.Fatalf("出口加上后规则可以引用：%d %s", code, msg)
	}
	code, out := call(t, e.d, "DELETE", "/v1/exits/hk", nil)
	if code != 409 || !strings.Contains(string(out), "第 2 条") {
		t.Fatalf("删除被引用的出口应 409 并列出序号：%d %s", code, out)
	}
	// 忙：状态机正在做别的事时，增删得到 409，不留半套
	release := make(chan struct{})
	started := make(chan struct{})
	go func() {
		_ = e.d.m.Exclusive(func() error { close(started); <-release; return nil })
	}()
	<-started
	if code, _ := putExit(t, e.d, "vpn", exitconf.WireGuard, tWG); code != 409 {
		t.Fatalf("忙时增加应 409：%d", code)
	}
	if code, _ := call(t, e.d, "DELETE", "/v1/exits/hk", nil); code != 409 {
		t.Fatal("忙时删除应 409")
	}
	close(release)
	if e.d.exits.Has("vpn") {
		t.Fatal("忙时被拒的增加不应留下出口")
	}
	if _, err := os.Stat(filepath.Join(e.d.opts.StateDir, SecretsDir, "exit-vpn.json")); !os.IsNotExist(err) {
		t.Fatal("忙时被拒的增加不应留下文件")
	}
}

// n、g、e. 第一次明确通过之前不可用（走主代理）；下发成功后立即检查；通过后走它；不通过回退；恢复后走回去；
// 分流出口的检查结果不关闸门。
func TestExitsAvailabilityAndFallback(t *testing.T) {
	e := newExitEnv(t, nil)
	e.ch.set(func() { e.ch.split["hk"] = exitcheck.Unknown })
	if code, msg := putExit(t, e.d, "hk", exitconf.WireGuard, tWG); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	withHK := strings.Replace(m3Rules, `"proxy:clash-b"`, `"proxy:hk"`, 1)
	if code, msg := putRules(t, e.d, withHK, false); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	waitExit(t, "下发后立即检查一次", func() bool { return e.vm.nPushes() > 0 && e.ch.n("hk") > 0 })
	if e.up("hk") {
		t.Fatal("从没明确通过过：不可用")
	}
	plain := netip.AddrPortFrom(e.lookup(t, "plain.example.org"), 443)
	n0 := len(e.op.exits())
	if err := e.echo(t, 21001, plain); err != nil {
		t.Fatal(err)
	}
	if ex := e.op.exits(); ex[n0] != "main" {
		t.Fatalf("没通过检查的出口：连接走主代理：%v", ex[n0:])
	}
	e.ch.set(func() { e.ch.split["hk"] = exitcheck.Pass })
	e.d.exitLoop.wake()
	waitExit(t, "通过后可用", func() bool { return e.up("hk") })
	n1 := len(e.op.exits())
	if err := e.echo(t, 21002, plain); err != nil {
		t.Fatal(err)
	}
	if ex := e.op.exits(); ex[n1] != "hk" {
		t.Fatalf("通过后走 hk：%v", ex[n1:])
	}
	// 明确不通过（如大陆段）：回退主代理；闸门不关
	e.ch.set(func() { e.ch.split["hk"] = exitcheck.Fail })
	e.d.exitLoop.wake()
	waitExit(t, "不通过后不可用", func() bool { return !e.up("hk") })
	if s := status(t, e.d); !s.GateOpen {
		t.Fatal("分流出口的检查结果不关闸门")
	}
	n2 := len(e.op.exits())
	if err := e.echo(t, 21003, plain); err != nil {
		t.Fatal(err)
	}
	if ex := e.op.exits(); ex[n2] != "main" {
		t.Fatalf("不通过：回退主代理：%v", ex[n2:])
	}
	e.ch.set(func() { e.ch.split["hk"] = exitcheck.Pass })
	e.d.exitLoop.wake()
	waitExit(t, "恢复", func() bool { return e.up("hk") })
	n3 := len(e.op.exits())
	if err := e.echo(t, 21004, plain); err != nil {
		t.Fatal(err)
	}
	if ex := e.op.exits(); ex[n3] != "hk" {
		t.Fatalf("恢复后新连接走回去：%v", ex[n3:])
	}
	// status：按出口的计数和可用状态
	s := status(t, e.d)
	if s.Opens["hk"].TCP < 2 || s.Opens["main"].TCP < 2 || !exitUpIn(s, "hk") {
		t.Fatalf("status：%+v %+v", s.Opens, s.Exits)
	}
	// agent 报引擎不可用 → 不可用
	e.vm.set(func() { e.vm.agentDown["hk"] = true })
	e.d.exitLoop.wake()
	waitExit(t, "引擎不可用", func() bool { return !e.up("hk") })
}

// e. 下发：启用后有一次；出口变化再下发；启动标识变了自动重新下发；下发失败时出口不可用、有事件、闸门不受影响。
func TestExitsPush(t *testing.T) {
	e := newExitEnv(t, nil)
	if code, msg := putExit(t, e.d, "hk", exitconf.WireGuard, tWG); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	waitExit(t, "下发 hk", func() bool { return e.vm.nPushes() > 0 && e.up("hk") })
	e.vm.mu.Lock()
	last := e.vm.eps[len(e.vm.eps)-1]
	e.vm.mu.Unlock()
	if len(last) != 1 || last[0] != (ExitEndpoint{Host: "vpn-secret-host.example.com", Proto: "udp", Port: 51820}) {
		t.Fatalf("端点要精确到主机、协议、端口：%+v", last)
	}
	n := e.vm.nPushes()
	e.d.exitLoop.wake() // 启动标识没变：不重新下发
	time.Sleep(100 * time.Millisecond)
	if e.vm.nPushes() != n {
		t.Fatal("启动标识没变时不重新下发")
	}
	e.vm.set(func() { e.vm.boot = "boot-2" })
	e.d.exitLoop.wake()
	waitExit(t, "重启后重新下发", func() bool { return e.vm.nPushes() == n+1 })
	if !strings.Contains(fmt.Sprint(e.d.events.List()), "vm_rebooted") {
		t.Fatal("应有虚拟机重启的事件")
	}
	// 下发失败
	e.vm.set(func() { e.vm.pushErr = errors.New("agent 拒绝") })
	if code, msg := putExit(t, e.d, "vpn", exitconf.WireGuard, tWG); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	waitExit(t, "下发失败的事件", func() bool { return strings.Contains(fmt.Sprint(e.d.events.List()), "exit_push_failed") })
	if e.up("hk") || e.up("vpn") {
		t.Fatal("下发失败后不可用")
	}
	if s := status(t, e.d); !s.GateOpen {
		t.Fatal("下发失败不影响闸门")
	}
	e.vm.set(func() { e.vm.pushErr = nil })
	e.d.exitLoop.wake()
	waitExit(t, "恢复下发", func() bool { return e.up("hk") && e.up("vpn") })
	// 虚拟机没有就绪
	e.vm.set(func() { e.vm.bootErr = errors.New("没有就绪") })
	e.d.exitLoop.wake()
	waitExit(t, "虚拟机没有就绪时不可用", func() bool { return !e.up("hk") })
}

// g. 某个出口的检查卡住：主代理的续期、闸门不受影响，别的出口照常。
func TestExitsCheckStuck(t *testing.T) {
	e := newExitEnv(t, nil)
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	e.ch.set(func() { e.ch.block["slow"] = block })
	if code, msg := putExit(t, e.d, "slow", exitconf.WireGuard, tWG); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	if code, msg := putExit(t, e.d, "fast", exitconf.WireGuard, tWG); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	waitExit(t, "fast 可用", func() bool { return e.up("fast") })
	if e.up("slow") {
		t.Fatal("卡住的出口没有结论：不可用")
	}
	if s := status(t, e.d); !s.GateOpen || s.State != string(state.Enabled) {
		t.Fatal("出口检查卡住不影响闸门")
	}
}

// o. 重启后的重新下发和出口的增删并发：-race 下没有数据竞争，最后一次下发的集合等于最终的出口集合。
func TestExitsPushConcurrentWithChanges(t *testing.T) {
	e := newExitEnv(t, nil)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tag := fmt.Sprintf("x%d", i)
			for j := 0; j < 5; j++ {
				for {
					code, _ := putExit(t, e.d, tag, exitconf.WireGuard, tWG)
					if code != 409 {
						break
					}
				}
				if j%2 == 1 {
					for {
						code, _ := call(t, e.d, "DELETE", "/v1/exits/"+tag, nil)
						if code != 409 {
							break
						}
					}
				}
			}
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for k := 0; k < 20; k++ {
			e.vm.set(func() { e.vm.boot = fmt.Sprintf("boot-%d", k) })
			e.d.exitLoop.wake()
			time.Sleep(5 * time.Millisecond)
		}
	}()
	wg.Wait()
	want := []string{}
	for _, x := range e.d.exits.All() {
		want = append(want, x.Tag)
	}
	e.d.exitLoop.wake()
	waitExit(t, "最后一次下发等于最终集合", func() bool {
		e.vm.mu.Lock()
		defer e.vm.mu.Unlock()
		return len(e.vm.pushes) > 0 && fmt.Sprint(e.vm.pushes[len(e.vm.pushes)-1]) == fmt.Sprint(want)
	})
}
