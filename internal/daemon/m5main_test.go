package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"claude-abroad/internal/agent"
	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/managedproxy"
	"claude-abroad/internal/usernet"
)

// fakeMainVM 记录对虚拟机的调用时间线（M5-4 验收 d、h、i）。
type fakeMainVM struct {
	mu          sync.Mutex
	line        []string
	boot        string
	digest      string // 虚拟机里当前配置的摘要
	hops        usernet.MainHops
	failHops    error
	failHopsN   int // 接下来连续失败几次（测回退本身失败）
	failPush    error
	gateAt      func() bool // 下发配置时闸门是否开着
	hopPrivate  int64
	unreachable bool
	gateSeen    []bool
	engines     map[string]EngineView // health 里的引擎（nil 时两个都在运行）
}

func (f *fakeMainVM) log(s string) { f.line = append(f.line, s) }
func (f *fakeMainVM) BootID(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.boot, nil
}
func (f *fakeMainVM) PushMainHops(_ context.Context, m usernet.MainHops) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failHopsN > 0 {
		f.failHopsN--
		f.log("hops-fail")
		return errors.New("假的 host.sock 一直失败")
	}
	if f.failHops != nil {
		err := f.failHops
		f.failHops = nil // 只失败一次（回滚时的下发要成功）
		f.log("hops-fail")
		return err
	}
	f.hops = m
	f.log(fmt.Sprintf("hops:%s|lan:%d", strings.Join(m.Hops, ","), len(m.LAN)))
	return nil
}
func (f *fakeMainVM) PushMainProxy(_ context.Context, yaml []byte) error {
	f.mu.Lock()
	gate := f.gateAt
	f.mu.Unlock()
	open := false
	if gate != nil {
		open = gate()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gateSeen = append(f.gateSeen, open)
	if f.failPush != nil {
		err := f.failPush
		f.failPush = nil
		f.log("proxy-fail")
		return err
	}
	f.digest = agent.ConfigDigest(yaml)
	f.log("proxy:" + f.digest)
	return nil
}
func (f *fakeMainVM) HopPrivate(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hopPrivate, nil
}
func (f *fakeMainVM) MainProxyDigest(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.digest, nil
}

// 分流出口的下发也记进同一条时间线（验收 h：先主代理、再分流出口）。
func (f *fakeMainVM) PushEndpoints(context.Context, []ExitEndpoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("exit-endpoints")
	return nil
}
func (f *fakeMainVM) PushExits(_ context.Context, xs []StoredExit) ([]AgentExit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("exits")
	var out []AgentExit
	for _, x := range xs {
		out = append(out, AgentExit{Tag: x.Tag, Up: true})
	}
	return out, nil
}
func (f *fakeMainVM) AgentExits(context.Context) ([]AgentExit, error) { return nil, nil }

func (f *fakeMainVM) timeline() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.line...)
}

// mainLine 只取主代理的下发（出口循环同时会下发分流出口）。
func (f *fakeMainVM) mainLine() []string {
	var out []string
	for _, s := range f.timeline() {
		if strings.HasPrefix(s, "hops") || strings.HasPrefix(s, "proxy") {
			out = append(out, s)
		}
	}
	return out
}

func (f *fakeMainVM) reset() {
	f.mu.Lock()
	f.line, f.gateSeen = nil, nil
	f.mu.Unlock()
}

// 测试用的节点：密码、密钥、服务器名、端口里埋了标记（验收 b）。
const mainNodesYAML = `proxies:
  - {name: exit, type: ss, server: 8.8.4.20, port: 54321, cipher: aes-128-gcm, password: PWDMARK1}
  - {name: exit2, type: ss, server: srvmark.example.com, port: 8389, cipher: aes-128-gcm, password: PWDMARK2}
  - {name: lan, type: socks5, server: 192.168.1.20, port: 7890, username: usermark, password: PWDMARK3}
  - {name: relay, type: trojan, server: relay.example.com, port: 443, password: KEYMARK4, sni: relay.example.com}
  - {name: local, type: socks5, server: 127.0.0.1, port: 7890}
`

var mainMarkers = []string{"PWDMARK1", "PWDMARK2", "PWDMARK3", "KEYMARK4", "srvmark", "54321", "usermark", "SUBMARK", "8.8.4.20"}

type mainEnv struct {
	*env
	d     *Daemon
	vm    *fakeMainVM
	pr    *fakeProber
	fetch struct {
		mu      sync.Mutex
		body    []byte
		err     error
		viaHost []bool
	}
	logs *syncBuf
}

func newMainEnv(t *testing.T, region bool) *mainEnv {
	t.Helper()
	m := &mainEnv{vm: &fakeMainVM{boot: "boot-1"}, pr: &fakeProber{rep: passReport()}}
	var o Options
	if region {
		var e *env
		e, m.logs, o = confirmEnv(t)
		m.env = e
	} else {
		m.env = newEnv(t)
		o = m.env.opts()
		m.logs = &syncBuf{}
	}
	sys := m.env.sys.System()
	sys.Baseline = NewBaselineService(m.pr, m.env.dir)
	sys.MainVM, sys.ExitVM = m.vm, m.vm
	sys.FetchSource = func(_ context.Context, url string, viaHost bool) ([]byte, error) {
		m.fetch.mu.Lock()
		defer m.fetch.mu.Unlock()
		m.fetch.viaHost = append(m.fetch.viaHost, viaHost)
		return m.fetch.body, m.fetch.err
	}
	o.Sys = sys
	m.d = m.env.start(o)
	m.vm.gateAt = func() bool { return m.d.Status().GateOpen }
	return m
}

func (m *mainEnv) addYAML(t *testing.T, name, y string) string {
	t.Helper()
	b, _ := json.Marshal(AddSourceRequest{Kind: "yaml", Name: name, Content: y})
	code, rb := call(t, m.d, "POST", "/v1/main-proxy/sources", b)
	var r SourceAdded
	if code != 200 || json.Unmarshal(rb, &r) != nil {
		t.Fatalf("添加来源：%d %s", code, rb)
	}
	return r.ID
}

func (m *mainEnv) nodeID(t *testing.T, name string) string {
	t.Helper()
	_, b := call(t, m.d, "GET", "/v1/main-proxy/sources", nil)
	var vs []MainProxySourceView
	_ = json.Unmarshal(b, &vs)
	for _, s := range vs {
		for _, n := range s.Nodes {
			if n.Name == name {
				return n.ID
			}
		}
	}
	t.Fatalf("没有节点 %s：%s", name, b)
	return ""
}

func (m *mainEnv) sel(t *testing.T, body MainSelectRequest) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	return call(t, m.d, "PUT", "/v1/main-proxy", b)
}

// enableWith 未启用时选 exit、建立基准、启用（启用时下发一次）。
func (m *mainEnv) enableWith(t *testing.T, node string) {
	t.Helper()
	if code, b := m.sel(t, MainSelectRequest{Selected: m.nodeID(t, node)}); code != 200 || !strings.Contains(string(b), `"saved":true`) {
		t.Fatalf("未启用时选择应只保存：%d %s", code, b)
	}
	if len(m.vm.timeline()) != 0 {
		t.Fatalf("未启用时不应下发：%v", m.vm.timeline())
	}
	post(t, m.d, "/v1/main-proxy/apply", 200)
	post(t, m.d, "/v1/enable", 200)
}

// 验收 a：来源三种形式的增、列、删；不合法被拒不留文件；文件 0600；重启后还在；删除正在用的被拒。
func TestMainSources(t *testing.T) {
	m := newMainEnv(t, false)
	id := m.addYAML(t, "本地", mainNodesYAML)
	nb, _ := json.Marshal(AddSourceRequest{Kind: "node", Name: "手动", Node: map[string]any{"type": "socks5", "server": "9.9.9.9", "port": 1080, "username": "u", "password": "PWDMARK1"}})
	if code, b := call(t, m.d, "POST", "/v1/main-proxy/sources", nb); code != 200 {
		t.Fatalf("手动节点：%d %s", code, b)
	}
	m.fetch.body = []byte("proxies:\n  - {name: sub1, type: ss, server: 8.8.8.8, port: 8388, cipher: aes-128-gcm, password: PWDMARK2}\n")
	sb, _ := json.Marshal(AddSourceRequest{Kind: "subscription", Name: "订阅", URL: "https://sub.example.com/SUBMARK", ViaHost: true})
	if code, b := call(t, m.d, "POST", "/v1/main-proxy/sources", sb); code != 200 {
		t.Fatalf("订阅：%d %s", code, b)
	}
	_, b := call(t, m.d, "GET", "/v1/main-proxy/sources", nil)
	var vs []MainProxySourceView
	if json.Unmarshal(b, &vs) != nil || len(vs) != 3 || vs[2].Kind != SourceSubscription || vs[0].Kind != SourceLocal {
		t.Fatalf("应列出三个来源：%s", b)
	}
	f := filepath.Join(managedproxy.Dir(m.d.main.Profile), "sources.json")
	if fi, err := os.Stat(f); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("存储应是 0600：%v %v", fi, err)
	}
	before, _ := os.ReadFile(f)
	for name, req := range map[string]AddSourceRequest{
		"YAML 不合法":   {Kind: "yaml", Name: "坏", Content: "proxies: [ {name: x, type: nope} ]"},
		"节点缺字段":      {Kind: "node", Name: "坏", Node: map[string]any{"type": "ss"}},
		"种类不对":       {Kind: "ftp", Name: "坏"},
		"订阅地址协议不对":   {Kind: "subscription", Name: "坏", URL: "file:///etc/passwd", ViaHost: true},
		"名字为空的 YAML": {Kind: "yaml", Name: "", Content: mainNodesYAML},
	} {
		bb, _ := json.Marshal(req)
		if code, rb := call(t, m.d, "POST", "/v1/main-proxy/sources", bb); code == 200 {
			t.Errorf("%s：应被拒（%s）", name, rb)
		}
	}
	if after, _ := os.ReadFile(f); string(after) != string(before) {
		t.Fatal("被拒的来源不能改动存储")
	}
	// 删除正在用的被拒；没用的可以删。
	m.enableWith(t, "exit")
	if code, b := call(t, m.d, "DELETE", "/v1/main-proxy/sources/"+id, nil); code != http.StatusConflict || !strings.Contains(string(b), "正在作为主代理") {
		t.Fatalf("删除正在用的来源应 409：%d %s", code, b)
	}
	if code, _ := call(t, m.d, "DELETE", "/v1/main-proxy/sources/"+vs[1].ID, nil); code != http.StatusNoContent {
		t.Fatalf("删除没用的来源：%d", code)
	}
	if code, _ := call(t, m.d, "DELETE", "/v1/main-proxy/sources/nope", nil); code != http.StatusNotFound {
		t.Fatal("没有的来源 404")
	}
	// 重启后还在。
	m.d.Shutdown()
	d2 := m.env.start(m.d.opts)
	_, b = call(t, d2, "GET", "/v1/main-proxy/sources", nil)
	if json.Unmarshal(b, &vs) != nil || len(vs) != 2 {
		t.Fatalf("重启后来源应还在：%s", b)
	}
	d2.Shutdown()
}

// 验收 d：已启用时应用主代理的完整顺序；失败的四种情况都回到旧配置。
func TestMainSwitchSequence(t *testing.T) {
	m := newMainEnv(t, false)
	m.addYAML(t, "本地", mainNodesYAML)
	m.enableWith(t, "exit")
	tl := m.vm.mainLine()
	if len(tl) != 2 || !strings.HasPrefix(tl[0], "hops:") || !strings.HasPrefix(tl[1], "proxy:") {
		t.Fatalf("启用时应下发一次（先第一跳再配置）：%v", tl)
	}
	oldDigest := m.vm.digest
	oldHops := tl[0]
	baseBefore, _ := os.ReadFile(filepath.Join(m.env.dir, BaselineFile))
	m.vm.reset()
	m.pr.mu.Lock()
	nBefore := m.pr.n
	m.pr.mu.Unlock()
	code, b := m.sel(t, MainSelectRequest{Selected: m.nodeID(t, "exit2")})
	if code != 200 {
		t.Fatalf("换主代理：%d %s", code, b)
	}
	tl = m.vm.mainLine()
	if len(tl) != 2 || tl[0] != "hops:srvmark.example.com:8389|lan:0" || !strings.HasPrefix(tl[1], "proxy:") || tl[1] == "proxy:"+oldDigest {
		t.Fatalf("成功的顺序应是：换第一跳 → 下发新配置：%v", tl)
	}
	if m.vm.gateSeen[0] {
		t.Fatal("下发新配置时闸门应关着（正在更换主代理）")
	}
	m.pr.mu.Lock()
	if m.pr.n != nBefore+1 {
		t.Fatalf("下发之后应跑一轮出口检查：%d", m.pr.n-nBefore)
	}
	m.pr.mu.Unlock()
	st := status(t, m.d)
	for _, r := range st.Gate.Reasons {
		if r == "main_proxy_switch" {
			t.Fatal("成功后要清掉“正在更换主代理”")
		}
	}
	evs := fmt.Sprint(st.Events)
	if !strings.Contains(evs, "main_proxy_switch") || !strings.Contains(evs, "switched") {
		t.Fatalf("应有关闸门（正在更换主代理）和更换成功的事件：%s", evs)
	}
	newDigest := m.vm.digest
	// 失败的四种情况：都回到刚才成功的那份（exit2），时间线里有重新下发旧配置；基准不变。
	baseNow, _ := os.ReadFile(filepath.Join(m.env.dir, BaselineFile))
	_ = baseBefore
	cases := []struct {
		name  string
		setup func()
		want  string
	}{
		{"替换第一跳失败", func() { m.vm.failHops = errors.New("假的 host.sock 失败") }, "假的 host.sock 失败"},
		{"下发配置失败（引擎不就绪）", func() { m.vm.failPush = errors.New("HTTP 504：引擎没有就绪") }, "引擎没有就绪"},
		{"检查不通过（出口在大陆）", func() {
			m.pr.mu.Lock()
			r := passReport()
			r.Items[2] = exitcheck.Item{Name: "mainland", Result: exitcheck.Fail, Cause: exitcheck.CauseMainland}
			m.pr.rep = r
			m.pr.mu.Unlock()
		}, "大陆"},
		{"检查无法判断", func() {
			m.pr.mu.Lock()
			r := passReport()
			r.Items[0] = exitcheck.Item{Name: "https", Result: exitcheck.Unknown, Cause: exitcheck.CauseTimeout}
			m.pr.rep = r
			m.pr.mu.Unlock()
		}, "检查新的主代理"},
	}
	for _, c := range cases {
		m.vm.reset()
		c.setup()
		code, b := m.sel(t, MainSelectRequest{Selected: m.nodeID(t, "exit")})
		m.pr.mu.Lock()
		m.pr.rep = passReport()
		m.pr.mu.Unlock()
		if code == 200 || !strings.Contains(string(b), c.want) || !strings.Contains(string(b), "已经回到原来的主代理") {
			t.Errorf("%s：应失败并说明原因和下一步：%d %s", c.name, code, b)
			continue
		}
		tl := m.vm.mainLine()
		last2 := tl[len(tl)-2:]
		if last2[0] != "hops:srvmark.example.com:8389|lan:0" || last2[1] != "proxy:"+newDigest {
			t.Errorf("%s：最后应重新下发旧的第一跳和旧配置：%v", c.name, tl)
		}
		if after, _ := os.ReadFile(filepath.Join(m.env.dir, BaselineFile)); string(after) != string(baseNow) {
			t.Errorf("%s：基准不能变", c.name)
		}
		if a, _ := m.d.main.applied(); a == nil || a.Digest != newDigest {
			t.Errorf("%s：“已下发”记录应还是旧的", c.name)
		}
	}
	_ = oldHops
	// 局域网节点不带确认、本机回环：在下发之前就拒绝，什么都不下发。
	m.vm.reset()
	if code, b := m.sel(t, MainSelectRequest{Selected: m.nodeID(t, "local")}); code != http.StatusUnprocessableEntity || !strings.Contains(string(b), "回环") {
		t.Fatalf("回环第一跳应 422：%d %s", code, b)
	}
	if len(m.vm.mainLine()) != 0 {
		t.Fatal("被拒时不应下发任何东西")
	}
}

// 验收 d：地区会变时，不带确认码 409、回到旧配置、基准不变；带确认码之后才生效。控制台用户 403。
func TestMainSwitchRegionConfirm(t *testing.T) {
	m := newMainEnv(t, true)
	m.addYAML(t, "本地", mainNodesYAML)
	if code, b := m.sel(t, MainSelectRequest{Selected: m.nodeID(t, "exit")}); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	post(t, m.d, "/v1/main-proxy/apply", 200)
	_, b := call(t, m.d, "POST", "/v1/enable", nil)
	cn := confirmOf(t, b)
	cb, _ := json.Marshal(ConfirmRequest{Confirm: cn.Confirm})
	if code, b := call(t, m.d, "POST", "/v1/enable", cb); code != 200 {
		t.Fatalf("启用：%d %s", code, b)
	}
	applied, _ := m.d.main.applied()
	baseBefore, _ := os.ReadFile(filepath.Join(m.env.dir, BaselineFile))
	m.pr.mu.Lock()
	m.pr.rep = geoReport("JP", "Asia/Tokyo")
	m.pr.mu.Unlock()
	m.vm.reset()
	code, b := m.sel(t, MainSelectRequest{Selected: m.nodeID(t, "exit2")})
	if code != http.StatusConflict {
		t.Fatalf("地区会变时应 409：%d %s", code, b)
	}
	cn = confirmOf(t, b)
	tl := m.vm.timeline()
	if !strings.HasSuffix(tl[len(tl)-1], applied.Digest) {
		t.Fatalf("要确认时应回到旧配置：%v", tl)
	}
	if after, _ := os.ReadFile(filepath.Join(m.env.dir, BaselineFile)); string(after) != string(baseBefore) {
		t.Fatal("要确认时基准不能变")
	}
	if a, _ := m.d.main.applied(); a.Digest != applied.Digest {
		t.Fatal("要确认时“已下发”记录不能变")
	}
	if code, b := m.sel(t, MainSelectRequest{Selected: m.nodeID(t, "exit2"), Confirm: cn.Confirm}); code != 200 {
		t.Fatalf("带确认码后应生效：%d %s", code, b)
	}
	if a, _ := m.d.main.applied(); a.Digest == applied.Digest {
		t.Fatal("确认后“已下发”记录应更新")
	}
}

// 验收 b：订阅地址、服务器、端口、密码、密钥里的标记，走完添加、刷新、选择、应用、检查、查询、状态、事件、日志，都搜不到。
func TestMainNoLeak(t *testing.T) {
	m := newMainEnv(t, false)
	var outs []string
	rec := func(code int, b []byte) { outs = append(outs, string(b)) }
	bb, _ := json.Marshal(AddSourceRequest{Kind: "yaml", Name: "本地", Content: mainNodesYAML})
	rec(call(t, m.d, "POST", "/v1/main-proxy/sources", bb))
	m.fetch.body = []byte("proxies:\n  - {name: sub1, type: ss, server: 8.8.8.8, port: 8388, cipher: aes-128-gcm, password: PWDMARK2}\n")
	sb, _ := json.Marshal(AddSourceRequest{Kind: "subscription", Name: "订阅", URL: "https://sub.example.com/SUBMARK", ViaHost: true})
	code, b := call(t, m.d, "POST", "/v1/main-proxy/sources", sb)
	rec(code, b)
	var sa SourceAdded
	_ = json.Unmarshal(b, &sa)
	rb, _ := json.Marshal(RefreshRequest{ViaHost: true})
	rec(call(t, m.d, "POST", "/v1/main-proxy/sources/"+sa.ID+"/refresh", rb))
	m.fetch.err = errors.New(`Get "https://sub.example.com/SUBMARK": dial tcp: lookup failed`)
	rec(call(t, m.d, "POST", "/v1/main-proxy/sources/"+sa.ID+"/refresh", rb))
	m.fetch.err = nil
	m.enableWith(t, "exit")
	rec(m.sel(t, MainSelectRequest{Selected: m.nodeID(t, "exit2")}))
	rec(call(t, m.d, "POST", "/v1/main-proxy/check", nil))
	rec(call(t, m.d, "GET", "/v1/main-proxy", nil))
	rec(call(t, m.d, "GET", "/v1/main-proxy/sources", nil))
	rec(call(t, m.d, "GET", "/v1/status", nil))
	_, jb := call(t, m.d, "POST", "/v1/disable?async=1", nil)
	var jb2 Job
	_ = json.Unmarshal(jb, &jb2)
	rec(call(t, m.d, "GET", "/v1/jobs/"+jb2.ID, nil))
	outs = append(outs, fmt.Sprint(m.d.events.List()), m.logs.String())
	all := strings.Join(outs, "\n")
	for _, mk := range mainMarkers {
		if strings.Contains(all, mk) {
			t.Errorf("输出里出现了标记 %q", mk)
		}
	}
}

// 验收 c：订阅刷新。启用时不带“经本机网络”；未启用不带参数被拒；失败时原列表不变；选中的节点不在了标需要重新选择、
// 正在运行的主代理和基准不变。
func TestMainRefresh(t *testing.T) {
	m := newMainEnv(t, false)
	sub := "proxies:\n  - {name: s1, type: ss, server: 8.8.8.8, port: 8388, cipher: aes-128-gcm, password: p1}\n  - {name: s2, type: ss, server: 8.8.8.9, port: 8388, cipher: aes-128-gcm, password: p2}\n"
	m.fetch.body = []byte(sub)
	sb, _ := json.Marshal(AddSourceRequest{Kind: "subscription", Name: "订阅", URL: "https://sub.example.com/x"})
	if code, b := call(t, m.d, "POST", "/v1/main-proxy/sources", sb); code != http.StatusUnprocessableEntity || !strings.Contains(string(b), "via_host") {
		t.Fatalf("未启用不带 via_host 应拒绝：%d %s", code, b)
	}
	sb, _ = json.Marshal(AddSourceRequest{Kind: "subscription", Name: "订阅", URL: "https://sub.example.com/x", ViaHost: true})
	_, b := call(t, m.d, "POST", "/v1/main-proxy/sources", sb)
	var sa SourceAdded
	_ = json.Unmarshal(b, &sa)
	if m.fetch.viaHost[len(m.fetch.viaHost)-1] != true {
		t.Fatal("未启用时抓取应经本机网络")
	}
	m.enableWith(t, "s1")
	viaBefore := len(m.fetch.viaHost)
	if code, b := call(t, m.d, "POST", "/v1/main-proxy/sources/"+sa.ID+"/refresh", nil); code != 200 {
		t.Fatalf("启用时刷新：%d %s", code, b)
	}
	if len(m.fetch.viaHost) != viaBefore+1 || m.fetch.viaHost[viaBefore] {
		t.Fatal("启用时抓取不应带“经本机网络”")
	}
	storeFile := filepath.Join(managedproxy.Dir(m.d.main.Profile), "sources.json")
	before, _ := os.ReadFile(storeFile)
	for name, setup := range map[string]func(){
		"超时":   func() { m.fetch.err = context.DeadlineExceeded },
		"太大":   func() { m.fetch.err = nil; m.fetch.body = make([]byte, SourceFetchMax+1) },
		"不合法":  func() { m.fetch.err = nil; m.fetch.body = []byte("not: [valid") },
		"抓取出错": func() { m.fetch.err = ErrFetchStatus },
	} {
		setup()
		if code, b := call(t, m.d, "POST", "/v1/main-proxy/sources/"+sa.ID+"/refresh", nil); code == 200 {
			t.Errorf("%s：应失败（%s）", name, b)
		}
		if after, _ := os.ReadFile(storeFile); string(after) != string(before) {
			t.Errorf("%s：原有的节点列表不能变", name)
		}
	}
	// 选中的节点不在了：更新、标需要重新选择；正在运行的主代理（已下发的记录、虚拟机）和基准不变。
	m.fetch.err, m.fetch.body = nil, []byte("proxies:\n  - {name: s2, type: ss, server: 8.8.8.9, port: 8388, cipher: aes-128-gcm, password: p2}\n")
	applied, _ := m.d.main.applied()
	baseBefore, _ := os.ReadFile(filepath.Join(m.env.dir, BaselineFile))
	m.vm.reset()
	code, b := call(t, m.d, "POST", "/v1/main-proxy/sources/"+sa.ID+"/refresh", nil)
	if code != 200 || !strings.Contains(string(b), `"needs_reselect":true`) {
		t.Fatalf("应更新并标需要重新选择：%d %s", code, b)
	}
	if a, _ := m.d.main.applied(); a.Digest != applied.Digest || len(m.vm.timeline()) != 0 {
		t.Fatal("刷新不能下发、不能改已下发的记录")
	}
	if after, _ := os.ReadFile(filepath.Join(m.env.dir, BaselineFile)); string(after) != string(baseBefore) {
		t.Fatal("刷新不能改基准")
	}
	_, vb := call(t, m.d, "GET", "/v1/main-proxy", nil)
	if !strings.Contains(string(vb), `"needs_reselect":true`) {
		t.Fatalf("查询应显示需要重新选择：%s", vb)
	}
}

// 验收 f：局域网节点的确认。
func TestMainLANConfirm(t *testing.T) {
	m := newMainEnv(t, false)
	m.addYAML(t, "本地", mainNodesYAML)
	m.enableWith(t, "exit")
	lan := m.nodeID(t, "lan")
	m.vm.reset()
	if code, b := m.sel(t, MainSelectRequest{Selected: lan}); code != http.StatusUnprocessableEntity || !strings.Contains(string(b), "局域网") {
		t.Fatalf("未确认的局域网节点应被拒：%d %s", code, b)
	}
	if len(m.vm.timeline()) != 0 || len(m.vm.hops.LAN) != 0 {
		t.Fatal("未确认时白名单里不能有它")
	}
	// 未启用时的查询也显示待确认（先选 lan 会被拒，这里看 plan 的结果）。
	cb, _ := json.Marshal(LANConfirmRequest{Node: lan})
	if code, b := call(t, m.d, "POST", "/v1/main-proxy/lan-confirm", cb); code != 200 {
		t.Fatalf("确认：%d %s", code, b)
	}
	if code, b := m.sel(t, MainSelectRequest{Selected: lan}); code != 200 {
		t.Fatalf("确认后应可以应用：%d %s", code, b)
	}
	if len(m.vm.hops.LAN) != 1 || m.vm.hops.LAN[0].Addr != "192.168.1.20:7890" || !m.vm.hops.LAN[0].Confirmed {
		t.Fatalf("白名单里应有确认过的局域网节点：%+v", m.vm.hops)
	}
	// 第一跳地址变了（同一个来源里改了服务器）：确认作废。
	id := m.addYAML(t, "另一个", strings.Replace(mainNodesYAML, "192.168.1.20", "192.168.1.21", 1))
	_ = id
	cs, _ := m.d.main.confirms()
	for _, x := range cs {
		if strings.Contains(x.Hop, "192.168.1.21") {
			t.Fatal("别的节点不能沾这条确认")
		}
	}
	lan2 := ""
	_, b := call(t, m.d, "GET", "/v1/main-proxy/sources", nil)
	var vs []MainProxySourceView
	_ = json.Unmarshal(b, &vs)
	for _, n := range vs[1].Nodes {
		if n.Name == "lan" {
			lan2 = n.ID
		}
	}
	if code, _ := m.sel(t, MainSelectRequest{Selected: lan2}); code != http.StatusUnprocessableEntity {
		t.Fatal("第一跳地址不同的节点要另行确认")
	}
}

// 验收 f 的最后一条：确认接口是敏感级别，控制台用户 403（级别矩阵也覆盖）。
func TestMainLANConfirmConsole(t *testing.T) {
	var c callers
	e := newEnv(t)
	o := e.opts()
	c.opts(&o)
	d := e.start(o)
	c.as(consoleTestUID)
	if code, b, _ := callRaw(d, "POST", "/v1/main-proxy/lan-confirm", []byte(`{"node":"x"}`)); code != http.StatusForbidden {
		t.Fatalf("控制台用户确认局域网节点应 403：%d %s", code, b)
	}
	if code, _, _ := callRaw(d, "PUT", "/v1/main-proxy", []byte(`{"selected":"x"}`)); code != http.StatusForbidden {
		t.Fatal("控制台用户换主代理应 403")
	}
}

// 验收 g：链式代理——白名单里只有链上第一个节点，下发的配置里链的顺序正确。
func TestMainChain(t *testing.T) {
	m := newMainEnv(t, false)
	m.addYAML(t, "本地", mainNodesYAML)
	m.enableWith(t, "exit")
	relay, exit2 := m.nodeID(t, "relay"), m.nodeID(t, "exit2")
	m.vm.reset()
	if code, b := m.sel(t, MainSelectRequest{Selected: exit2, Chain: []string{relay, exit2}}); code != 200 {
		t.Fatalf("链式：%d %s", code, b)
	}
	if m.vm.hops.Hops == nil || len(m.vm.hops.Hops) != 1 || m.vm.hops.Hops[0] != "relay.example.com:443" {
		t.Fatalf("白名单里只能有链上第一个节点：%+v", m.vm.hops)
	}
	a, _ := m.d.main.applied()
	cfg, _ := a.Store.VMRuntimeConfig()
	y := string(cfg.YAML)
	if !strings.Contains(y, "dialer-proxy: "+relay) {
		t.Fatalf("链的顺序：exit2 应经 relay 拨出：\n%s", y)
	}
}

// 验收 h、i：虚拟机重启后先下发主代理、再下发分流出口；配置被改掉后检测到摘要不一致、重新下发、有事件。
func TestMainRebootAndDrift(t *testing.T) {
	m := newMainEnv(t, false)
	m.addYAML(t, "本地", mainNodesYAML)
	m.enableWith(t, "exit")
	if err := m.d.exits.Put(StoredExit{Tag: "wg", Kind: "wireguard", Conf: tWG}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m.d.exitLoop.round(ctx) // 第一轮：记下启动标识、下发出口
	m.vm.mu.Lock()
	m.vm.boot = "boot-2"
	m.vm.digest = "" // 重启后虚拟机里没有配置
	m.vm.mu.Unlock()
	m.vm.reset()
	m.d.exitLoop.round(ctx)
	tl := m.vm.timeline()
	idx := func(prefix string) int {
		for i, s := range tl {
			if strings.HasPrefix(s, prefix) {
				return i
			}
		}
		return -1
	}
	h, p, ex := idx("hops:"), idx("proxy:"), idx("exit-endpoints")
	if h < 0 || p < 0 || ex < 0 || !(h < p && p < ex) {
		t.Fatalf("重启后应先下发主代理（第一跳、配置）再下发分流出口：%v", tl)
	}
	if !strings.Contains(fmt.Sprint(m.d.events.List()), "vm_rebooted") {
		t.Fatal("应有虚拟机重启的事件")
	}
	// 配置被开发工具改掉：摘要不一致 → 重新下发 + 事件。
	m.vm.mu.Lock()
	m.vm.digest = "deadbeefdeadbeef"
	m.vm.mu.Unlock()
	m.vm.reset()
	m.d.exitLoop.round(ctx)
	if tl := m.vm.timeline(); len(tl) < 2 || !strings.HasPrefix(tl[0], "hops:") || !strings.HasPrefix(tl[1], "proxy:") {
		t.Fatalf("摘要不一致时应重新下发：%v", tl)
	}
	if !strings.Contains(fmt.Sprint(m.d.events.List()), "repushed") {
		t.Fatal("应有重新下发的事件")
	}
	// 摘要一致：不下发。
	m.vm.reset()
	m.d.exitLoop.round(ctx)
	for _, s := range m.vm.timeline() {
		if strings.HasPrefix(s, "proxy:") {
			t.Fatal("摘要一致时不应重新下发主代理")
		}
	}
}

// 验收 h：重启后的重新下发与并发的“应用主代理”“增删出口”不交叉（-race）。
func TestMainConcurrent(t *testing.T) {
	m := newMainEnv(t, false)
	m.addYAML(t, "本地", mainNodesYAML)
	m.enableWith(t, "exit")
	ids := []string{m.nodeID(t, "exit"), m.nodeID(t, "exit2")}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(3)
		go func(i int) {
			defer wg.Done()
			_, _ = m.sel(t, MainSelectRequest{Selected: ids[i%2]})
		}(i)
		go func(i int) {
			defer wg.Done()
			m.vm.mu.Lock()
			m.vm.boot = fmt.Sprintf("boot-c%d", i)
			m.vm.mu.Unlock()
			m.d.exitLoop.round(context.Background())
		}(i)
		go func(i int) {
			defer wg.Done()
			b, _ := json.Marshal(map[string]any{"kind": "wireguard", "conf": tWG})
			call(t, m.d, "PUT", fmt.Sprintf("/v1/exits/wg%d", i), b)
		}(i)
	}
	wg.Wait()
	// 每次下发主代理都是“第一跳、配置”成对相邻出现（没有被别的下发插进来）。
	tl := m.vm.timeline()
	for i, s := range tl {
		if strings.HasPrefix(s, "hops:") && i+1 < len(tl) && !strings.HasPrefix(tl[i+1], "proxy") && !strings.HasPrefix(tl[i+1], "hops") {
			t.Fatalf("第一跳和配置之间插进了别的下发：%v", tl)
		}
	}
}

// M5-4 建议改 1：新主代理已经生效、但选择没存下来时整体回退——虚拟机回到旧配置、基准回到更换前，应答是失败。
func TestMainSwitchSaveFails(t *testing.T) {
	m := newMainEnv(t, false)
	m.addYAML(t, "本地", mainNodesYAML)
	m.enableWith(t, "exit")
	old, _ := m.d.main.applied()
	baseBefore, _ := os.ReadFile(filepath.Join(m.env.dir, BaselineFile))
	m.pr.mu.Lock()
	r := passReport() // 同一个国家、换一个出口 IP：基准文件不同（看得出有没有回退），地区不变（不触发确认）
	r.IP = netip.MustParseAddr("203.0.113.99")
	r.Geo.IP = r.IP
	m.pr.rep = r
	m.pr.mu.Unlock()
	m.d.main.failSave = func() error { return errors.New("假的写盘失败") }
	code, b := m.sel(t, MainSelectRequest{Selected: m.nodeID(t, "exit2")})
	m.d.main.failSave = nil
	if code != http.StatusInternalServerError || !strings.Contains(string(b), "保存主代理的选择") || !strings.Contains(string(b), "已经回到原来的主代理") {
		t.Fatalf("保存失败应整体回退并报失败：%d %s", code, b)
	}
	if m.vm.digest != old.Digest {
		t.Fatal("虚拟机里应回到旧配置")
	}
	if after, _ := os.ReadFile(filepath.Join(m.env.dir, BaselineFile)); string(after) != string(baseBefore) {
		t.Fatal("基准应恢复成更换前的")
	}
	if a, _ := m.d.main.applied(); a.Digest != old.Digest {
		t.Fatal("“已下发”记录应还是旧的")
	}
	// 下一轮核对：摘要一致，不会把别的配置下发回去。
	m.vm.reset()
	m.d.exitLoop.round(context.Background())
	for _, s := range m.vm.mainLine() {
		if strings.HasPrefix(s, "proxy:") {
			t.Fatalf("回退成功后下一轮不应再下发：%v", m.vm.mainLine())
		}
	}
}

// M5-4 建议改 2：回退本身没成功时，事件和应答如实，下一轮重新下发旧配置。
func TestMainRollbackIncomplete(t *testing.T) {
	m := newMainEnv(t, false)
	m.addYAML(t, "本地", mainNodesYAML)
	m.enableWith(t, "exit")
	old, _ := m.d.main.applied()
	m.d.exitLoop.round(context.Background()) // 记下启动标识
	m.vm.mu.Lock()
	m.vm.failHopsN = 2 // 换第一跳失败，回退时换回旧第一跳也失败
	m.vm.mu.Unlock()
	code, b := m.sel(t, MainSelectRequest{Selected: m.nodeID(t, "exit2")})
	if code == 200 || !strings.Contains(string(b), "恢复原来的主代理没有完成") || strings.Contains(string(b), "已经回到原来的主代理") {
		t.Fatalf("回退没完成时应答要如实：%d %s", code, b)
	}
	evs := fmt.Sprint(m.d.events.List())
	if !strings.Contains(evs, "rollback_incomplete") {
		t.Fatalf("应有“回退没有完成”的事件：%s", evs)
	}
	m.vm.reset()
	m.d.exitLoop.round(context.Background())
	tl := m.vm.mainLine()
	if len(tl) < 2 || !strings.HasPrefix(tl[0], "hops:8.8.4.20") || tl[1] != "proxy:"+old.Digest {
		t.Fatalf("下一轮应重新下发旧的第一跳和旧配置：%v", tl)
	}
	m.vm.reset()
	m.d.exitLoop.round(context.Background())
	if len(m.vm.mainLine()) != 0 {
		t.Fatal("补发成功后不应再重复下发")
	}
}

// v2.3 偏差的结论：第一跳域名解析到局域网地址（网关拒绝、计 dns_hop_private）导致检查失败时，错误里给出“填 IP”的提示。
func TestMainHopPrivateHint(t *testing.T) {
	m := newMainEnv(t, false)
	m.addYAML(t, "本地", mainNodesYAML)
	m.enableWith(t, "exit")
	m.pr.mu.Lock()
	r := passReport()
	r.Items[0] = exitcheck.Item{Name: "https", Result: exitcheck.Unknown, Cause: exitcheck.CauseTimeout}
	m.pr.rep = r
	m.pr.mu.Unlock()
	m.vm.mu.Lock()
	m.vm.gateAt = func() bool { m.vm.hopPrivate++; return false } // 下发期间网关拒了一次私有答案
	m.vm.mu.Unlock()
	code, b := m.sel(t, MainSelectRequest{Selected: m.nodeID(t, "exit2")})
	if code == 200 || !strings.Contains(string(b), HopPrivateHint) {
		t.Fatalf("应给出“局域网里的代理请直接填 IP 地址”：%d %s", code, b)
	}
	// 没有拒过私有答案的失败：不给这条提示。
	m.vm.mu.Lock()
	m.vm.gateAt = nil
	m.vm.mu.Unlock()
	code, b = m.sel(t, MainSelectRequest{Selected: m.nodeID(t, "exit2")})
	if code == 200 || strings.Contains(string(b), HopPrivateHint) {
		t.Fatalf("别的原因的失败不应给这条提示：%d %s", code, b)
	}
}
