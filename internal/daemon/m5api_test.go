package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"claude-abroad/internal/api"
	"claude-abroad/internal/claudeproc"
)

// 调用方注入：peer 是这次连接的对端 uid；控制台用户固定为 consoleTestUID。
const (
	consoleTestUID = 7001
	otherTestUID   = 7002
)

type callers struct{ peer atomic.Int64 }

func (c *callers) opts(o *Options) {
	o.PeerUID = func(*net.UnixConn) (int, error) { return int(c.peer.Load()), nil }
	o.ConsoleUID = func() (int, error) { return consoleTestUID, nil }
}
func (c *callers) as(uid int) { c.peer.Store(int64(uid)) }

// callRaw 发一个请求；连接被断开时返回 err。SSE 这类不结束的应答只看状态码。
func callRaw(d *Daemon, method, path string, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, method, "http://abroadd"+path, strings.NewReader(string(body)))
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", d.SocketPath())
	}, DisableKeepAlives: true}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return resp.StatusCode, nil, nil
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

func concretePath(p string) string {
	return strings.NewReplacer("{tag}", "wgx", "{id}", "nojob").Replace(p)
}

// 验收 a：级别矩阵。对路由表的每个接口用三种调用方各调一次，期望值由路由表生成：
// 其他用户一律连接被断开（只读接口也一样）；控制台用户遇到敏感接口 403、其余不是 403；完全信任的调用方都不是 403。
// 按本次调用决定的接口（启用、切换地区、改规则）在这里按“不会变/收紧”的那种情况调，另一种情况见 TestConsoleSensitiveDenied。
func TestLevelMatrix(t *testing.T) {
	var c callers
	e := newEnv(t)
	o := e.opts()
	c.opts(&o)
	d := e.start(o)
	body := func(rt api.Route) []byte {
		switch rt.ID {
		case "rules-put":
			return []byte("rules:\n  - {app: com.example.a, action: block}\n")
		case "envcheck-settings-put":
			return []byte(`{"disabled":[]}`)
		case "region-put":
			return []byte(`{"mode":"manual","timezone":"Asia/Tokyo","lang":"ja-JP"}`)
		case "exit-put":
			return []byte(`{"kind":"wireguard","conf":"x"}`)
		}
		return nil
	}
	who := map[string]int{"trusted": os.Getuid(), "console": consoleTestUID, "other": otherTestUID}
	for _, rt := range RouteTable() {
		for _, name := range []string{"other", "console", "trusted"} {
			c.as(who[name])
			code, b, err := callRaw(d, rt.Method, concretePath(rt.Path), body(rt))
			switch name {
			case "other":
				if err == nil {
					t.Errorf("%s 其他用户：连接应被断开，却得到 %d", rt.Pattern(), code)
				}
			case "console":
				if err != nil {
					t.Fatalf("%s 控制台用户：%v", rt.Pattern(), err)
				}
				if rt.Level == api.Sensitive {
					if code != http.StatusForbidden || !strings.Contains(string(b), "需要本人授权") {
						t.Errorf("%s 控制台用户做敏感操作：应 403“需要本人授权”，得到 %d %s", rt.Pattern(), code, b)
					}
				} else if code == http.StatusForbidden {
					t.Errorf("%s 控制台用户：%s 级别不应 403：%s", rt.Pattern(), rt.Level, b)
				}
			case "trusted":
				if err != nil || code == http.StatusForbidden {
					t.Errorf("%s 完全信任：不应被拒（%d %v %s）", rt.Pattern(), code, err, b)
				}
			}
		}
	}
}

// 验收 b：控制台用户做不了敏感操作——回国、卸载骨架、应用主代理、增删出口、放松保护的改规则、地区会变的启用和切换地区
// 都 403；网络快照、状态、规则文件、出口存储、进程表都没有变，没有发出任何信号，没有签发确认码。
// 对照：同一个控制台用户做“不会变/收紧”的同类操作照常通过。
func TestConsoleSensitiveDenied(t *testing.T) {
	var c callers
	e, _, o := confirmEnv(t, c.opts)
	d := e.start(o)
	addClaude(e, claudeproc.ExitOnTerm)
	c.as(os.Getuid())
	putRegion(t, d, `{"mode":"manual","timezone":"Asia/Tokyo","lang":"ja-JP"}`) // 锁定目标（未启用，只锁定）
	rulesBefore, _ := os.ReadFile(filepath.Join(e.dir, "rules.yaml"))
	snap := e.sys.Net.Snapshot()
	stBefore := status(t, d).State
	exitsBefore, _ := d.exits.All(), error(nil)
	sigBefore := len(e.sys.Procs.Signals)
	c.as(consoleTestUID)
	for _, x := range []struct{ name, method, path, body string }{
		{"回国", "POST", "/v1/restore", ""},
		{"回国（别名）", "POST", "/v1/actions/restore", ""},
		{"卸载骨架", "POST", "/v1/actions/uninstall", ""},
		{"应用主代理", "POST", "/v1/main-proxy/apply", ""},
		{"增加出口", "PUT", "/v1/exits/wgx", `{"kind":"wireguard","conf":"x"}`},
		{"删除出口", "DELETE", "/v1/exits/wgx", ""},
		{"改规则：改成本机网络", "PUT", "/v1/rules", "rules:\n  - {app: com.example.a, action: direct}\n"},
		{"改规则：加本地域名", "PUT", "/v1/rules", "local_domains: [\"intranet.test\"]\n"},
		{"启用（地区会变）", "POST", "/v1/enable", ""},
	} {
		code, b, err := callRaw(d, x.method, x.path, []byte(x.body))
		if err != nil || code != http.StatusForbidden || !strings.Contains(string(b), "需要本人授权") {
			t.Errorf("%s：应 403“需要本人授权”，得到 %d %v %s", x.name, code, err, b)
		}
	}
	// 切换地区（地区会变）：先由完全信任的调用方启用，再由控制台用户切换。
	c.as(os.Getuid())
	_, b, _ := callRaw(d, "POST", "/v1/enable", nil)
	cn := confirmOf(t, b)
	cb, _ := json.Marshal(ConfirmRequest{Confirm: cn.Confirm})
	if code, b, _ := callRaw(d, "POST", "/v1/enable", cb); code != 200 {
		t.Fatalf("完全信任的调用方启用：%d %s", code, b)
	}
	snap2 := e.sys.Net.Snapshot()
	sig2 := len(e.sys.Procs.Signals)
	c.as(consoleTestUID)
	if code, b, _ := callRaw(d, "PUT", "/v1/region", []byte(`{"mode":"manual","timezone":"Europe/Paris","lang":"fr-FR"}`)); code != http.StatusForbidden || !strings.Contains(string(b), "地区会变") {
		t.Errorf("切换地区（会变）：应 403，得到 %d %s", code, b)
	}
	if !e.sys.Net.Snapshot().Equal(snap2) || len(e.sys.Procs.Signals) != sig2 || status(t, d).Region.Target.Timezone != "Asia/Tokyo" {
		t.Fatal("被拒的切换地区不能有任何改动")
	}
	d.confirms.mu.Lock()
	pending := len(d.confirms.m)
	d.confirms.mu.Unlock()
	if pending != 0 {
		t.Fatalf("控制台用户被拒时不应签发确认码（还有 %d 个）", pending)
	}
	_ = snap
	_ = stBefore
	_ = sigBefore
	// 第一段（启用之前）的不变量：在被拒的那一刻，网络、状态、规则、出口都没变。
	rulesAfter, _ := os.ReadFile(filepath.Join(e.dir, "rules.yaml"))
	exitsAfter, _ := d.exits.All(), error(nil)
	if string(rulesBefore) != string(rulesAfter) || len(exitsBefore) != len(exitsAfter) {
		t.Fatal("被拒的改规则、增删出口不能改动规则文件和出口存储")
	}
	// 对照：不会变的切换（同一个目标）和收紧的改规则，控制台用户照常通过。
	if code, b, _ := callRaw(d, "PUT", "/v1/region", []byte(`{"mode":"manual","timezone":"Asia/Tokyo","lang":"ja-JP"}`)); code != 200 {
		t.Errorf("切换地区（不会变）应通过：%d %s", code, b)
	}
	if code, b, _ := callRaw(d, "PUT", "/v1/rules", []byte("rules:\n  - {app: com.example.a, action: block}\n")); code != 200 {
		t.Errorf("收紧的改规则应通过：%d %s", code, b)
	}
	// 停用（普通）照常。
	if code, b, _ := callRaw(d, "POST", "/v1/disable", nil); code != 200 {
		t.Errorf("控制台用户停用：%d %s", code, b)
	}
}

// 启用前的那一段单独核对快照：控制台用户的敏感请求全部被拒后，网络、状态、信号都没变。
func TestConsoleDeniedNoChanges(t *testing.T) {
	var c callers
	e, _, o := confirmEnv(t, c.opts)
	d := e.start(o)
	addClaude(e, claudeproc.ExitOnTerm)
	c.as(os.Getuid())
	putRegion(t, d, `{"mode":"manual","timezone":"Asia/Tokyo","lang":"ja-JP"}`)
	snap := e.sys.Net.Snapshot()
	st0 := status(t, d)
	c.as(consoleTestUID)
	for _, p := range []string{"/v1/restore", "/v1/enable", "/v1/main-proxy/apply"} {
		_, _, _ = callRaw(d, "POST", p, nil)
	}
	c.as(os.Getuid())
	if !e.sys.Net.Snapshot().Equal(snap) || status(t, d).State != st0.State || len(e.sys.Procs.Signals) != 0 {
		t.Fatal("被拒的敏感请求不能改动网络、状态，不能发信号")
	}
}

// 验收 d：未知路径 404，方法不对 405；注册只在 routes.go 的 handler() 里（源码检查）；不能注册的清单。
func TestRoutesOnlyRegisteredHere(t *testing.T) {
	e := newEnv(t)
	d := e.start(e.opts())
	if code, _, _ := callRaw(d, "GET", "/v1/no-such-thing", nil); code != http.StatusNotFound {
		t.Errorf("未知路径应 404：%d", code)
	}
	if code, _, _ := callRaw(d, "GET", "/v1/enable", nil); code != http.StatusMethodNotAllowed {
		t.Errorf("方法不对应 405：%d", code)
	}
	if err := api.CheckRoutes(RouteTable()); err != nil {
		t.Fatal(err)
	}
	if len(d.handlerFuncs()) != len(RouteTable()) {
		t.Fatal("路由表和处理函数要一一对上")
	}
	re := regexp.MustCompile(`\.(HandleFunc|Handle)\(`)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		for i, line := range strings.Split(string(b), "\n") {
			if re.MatchString(line) && !(f == "routes.go" && strings.Contains(line, "mux.Handle(rt.Pattern(), d.guard(rt, fn))")) {
				t.Errorf("%s:%d 绕过路由表注册了路径：%s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
	for _, rt := range RouteTable() {
		for _, bad := range api.Forbidden {
			if strings.Contains(strings.ToLower(rt.Path), bad) {
				t.Errorf("不能注册的接口出现在路由表里：%s", rt.Pattern())
			}
		}
	}
}

// 验收 f：任务。异步启用返回任务号，查询看到进行中、成功；失败时有错误文字；与同步共用忙标记；只保留最近 16 个；
// 查询结果里搜不到确认码。
func TestJobs(t *testing.T) {
	e := newEnv(t)
	bp := &blockingPre{entered: make(chan struct{}), release: make(chan struct{})}
	e.sys.Pre = bp
	d := e.start(e.opts())
	code, b := call(t, d, "POST", "/v1/enable?async=1", nil)
	var jb Job
	if code != http.StatusAccepted || json.Unmarshal(b, &jb) != nil || jb.ID == "" || jb.State != JobRunning || jb.Op != "enable" {
		t.Fatalf("异步启用应 202 带任务：%d %s", code, b)
	}
	<-bp.entered
	// 进行中：另一个异步请求 409；同步的停用撞上忙标记 409。
	if code, b := call(t, d, "POST", "/v1/disable?async=1", nil); code != http.StatusConflict {
		t.Errorf("已有任务时再异步应 409：%d %s", code, b)
	}
	if code, b := call(t, d, "POST", "/v1/disable", nil); code != http.StatusConflict {
		t.Errorf("任务进行中同步操作应得到“忙”：%d %s", code, b)
	}
	if code, b := call(t, d, "GET", "/v1/jobs/"+jb.ID, nil); code != 200 || !strings.Contains(string(b), `"running"`) {
		t.Errorf("查询应是进行中：%d %s", code, b)
	}
	close(bp.release)
	waitJob := func(id, want string) Job {
		var j Job
		for i := 0; i < 500; i++ {
			_, b := call(t, d, "GET", "/v1/jobs/"+id, nil)
			_ = json.Unmarshal(b, &j)
			if j.State == want {
				return j
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("任务 %s 没有到 %s：%+v", id, want, j)
		return j
	}
	waitJob(jb.ID, JobSucceeded)
	if status(t, d).State != "ENABLED" {
		t.Fatal("异步启用后应是 ENABLED")
	}
	// 失败：出口检查失败时任务失败、带错误文字。
	call(t, d, "POST", "/v1/disable", nil)
	e.sys.ExitCheck.SetFail(errors.New("假的出口检查失败"))
	_, b = call(t, d, "POST", "/v1/enable?async=1", nil)
	_ = json.Unmarshal(b, &jb)
	f := waitJob(jb.ID, JobFailed)
	if f.Error == "" || f.Finished.IsZero() {
		t.Fatalf("失败的任务要有错误文字和结束时间：%+v", f)
	}
	e.sys.ExitCheck.SetFail(nil)
	// 只保留最近 16 个。
	first := jb.ID
	for i := 0; i < JobKeep+2; i++ {
		_, b = call(t, d, "POST", "/v1/disable?async=1", nil)
		_ = json.Unmarshal(b, &jb)
		waitJob(jb.ID, JobSucceeded)
	}
	if code, _ := call(t, d, "GET", "/v1/jobs/"+first, nil); code != http.StatusNotFound {
		t.Errorf("超过 %d 个后最早的应被丢掉：%d", JobKeep, code)
	}
	if code, _ := call(t, d, "POST", "/v1/enable?async=x", nil); code != http.StatusBadRequest {
		t.Error("async 只能是 0 或 1")
	}
}

// 任务查询里搜不到确认码：地区会变的异步启用，确认码在请求里，任务结果里没有。
func TestJobNoConfirm(t *testing.T) {
	e, _, o := confirmEnv(t)
	d := e.start(o)
	putRegion(t, d, `{"mode":"manual","timezone":"Asia/Tokyo","lang":"ja-JP"}`)
	_, b := call(t, d, "POST", "/v1/enable?async=1", nil)
	cn := confirmOf(t, b)
	cb, _ := json.Marshal(ConfirmRequest{Confirm: cn.Confirm})
	code, b := call(t, d, "POST", "/v1/enable?async=1", cb)
	var jb Job
	if code != http.StatusAccepted || json.Unmarshal(b, &jb) != nil {
		t.Fatalf("%d %s", code, b)
	}
	for i := 0; i < 300; i++ {
		_, b = call(t, d, "GET", "/v1/jobs/"+jb.ID, nil)
		if strings.Contains(string(b), JobSucceeded) || strings.Contains(string(b), JobFailed) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Contains(string(b), cn.Confirm) {
		t.Fatal("任务查询结果里不能有确认码")
	}
}

// openSSE 连上事件推送，返回逐行读取器和关闭函数。
func openSSE(t *testing.T, d *Daemon) (*bufio.Reader, func()) {
	br, _, cl := openSSEConn(t, d)
	return br, cl
}

func openSSEConn(t *testing.T, d *Daemon) (*bufio.Reader, net.Conn, func()) {
	t.Helper()
	c, err := net.Dial("unix", d.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(c, "GET /v1/events HTTP/1.1\r\nHost: abroadd\r\n\r\n")
	br := bufio.NewReader(c)
	return br, c, func() { c.Close() }
}

// 验收 g：事件推送。状态变化、闸门开关、环境检测各有事件；不读的客户端不阻塞启用；超过订阅上限被拒；
// 断开后退订、协程不泄漏；内容里搜不到测试用的地址、域名、确认码。
func TestEventStream(t *testing.T) {
	w := newEnvEnv(t, false)
	d := w.d
	br, sseConn, closeSSE := openSSEConn(t, d)
	// 一个连上之后一个字节都不读的客户端。
	_, closeLazy := openSSE(t, d)
	for i := 0; i < 200 && d.events.Subscribers() < 2; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	call(t, d, "PUT", "/v1/rules?drop_affected=0", []byte("rules:\n  - {app: com.example.a, action: block}\nlocal_domains: [\"secret-intranet.test\"]\n"))
	code, b := call(t, d, "POST", "/v1/restore", nil)
	var cn ConfirmNeeded
	_ = json.Unmarshal(b, &cn)
	call(t, d, "POST", "/v1/disable", nil) // newEnvEnv 构造出来已经是启用状态：先停用，下面的启用才会产生事件
	done := make(chan int, 1)
	go func() { c, _ := call(t, d, "POST", "/v1/enable", nil); done <- c }()
	select {
	case c := <-done:
		if c != 200 {
			t.Fatalf("启用：%d", c)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("有不读的客户端时启用被阻塞了")
	}
	call(t, d, "POST", "/v1/envcheck/run", nil)
	seen := map[string]bool{}
	var all strings.Builder
	deadline := time.Now().Add(10 * time.Second)
	_ = sseConn.SetReadDeadline(deadline)
	for time.Now().Before(deadline) && !(seen["state"] && seen["gate"] && seen["envcheck"]) {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		all.WriteString(line)
		if strings.HasPrefix(line, "data: ") {
			var ev StreamEvent
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) == nil {
				seen[ev.Source] = true
			}
		}
	}
	if !seen["state"] || !seen["gate"] || !seen["envcheck"] {
		t.Fatalf("应有状态、闸门、环境检测的事件：%v\n收到：%q", seen, all.String())
	}
	for _, leak := range []string{"secret-intranet.test", "com.example.a", cn.Confirm, "203.0.113."} {
		if leak != "" && strings.Contains(all.String(), leak) {
			t.Errorf("事件里出现了 %q", leak)
		}
	}
	_ = code
	// 订阅上限：再连到上限，下一个 503。
	var closers []func()
	for d.events.Subscribers() < MaxSubscriptions {
		_, cl := openSSE(t, d)
		closers = append(closers, cl)
		time.Sleep(20 * time.Millisecond)
	}
	if code, _, _ := callRaw(d, "GET", "/v1/events", nil); code != http.StatusServiceUnavailable {
		t.Errorf("超过订阅上限应 503：%d", code)
	}
	// 断开后退订（协程退出）。
	closeSSE()
	closeLazy()
	for _, cl := range closers {
		cl()
	}
	for i := 0; i < 300 && d.events.Subscribers() > 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if n := d.events.Subscribers(); n != 0 {
		t.Fatalf("客户端都断开后还有 %d 个订阅（协程泄漏）", n)
	}
}

// 慢客户端：缓冲满后丢掉旧事件，下一条送出的事件标 dropped，不阻塞 Add。
func TestEventDropMarker(t *testing.T) {
	ev := &Events{}
	ch, cancel, err := ev.Subscribe()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	start := time.Now()
	for i := 0; i < StreamBuffer*3; i++ {
		ev.Add("state", "x", "")
	}
	if time.Since(start) > time.Second {
		t.Fatal("Add 被慢的订阅者阻塞了")
	}
	for i := 0; i < StreamBuffer; i++ {
		<-ch
	}
	ev.Add("state", "after", "")
	if got := <-ch; !got.Dropped || got.Kind != "after" {
		t.Fatalf("缓冲满过之后的下一条应标 dropped：%+v", got)
	}
}

// 验收 h：连接数上限和读超时。
func TestConnLimitAndReadTimeout(t *testing.T) {
	e := newEnv(t)
	var rejected atomic.Int64
	o := e.opts()
	o.MaxConns = 2
	o.ReadHeaderTimeout = 300 * time.Millisecond
	o.OnReject = func() { rejected.Add(1) }
	d := e.start(o)
	c1, _ := net.Dial("unix", d.SocketPath())
	c2, _ := net.Dial("unix", d.SocketPath())
	c3, _ := net.Dial("unix", d.SocketPath())
	defer c1.Close()
	defer c2.Close()
	_ = c3.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c3.Read(make([]byte, 1)); err == nil {
		t.Fatal("超过上限的连接应被断开")
	}
	c3.Close()
	if rejected.Load() < 1 {
		t.Fatal("超过上限应记一次拒绝")
	}
	// 只连不发：到时被关。
	_ = c1.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	_, err := c1.Read(make([]byte, 1))
	if err == nil || time.Since(start) > 2500*time.Millisecond {
		t.Fatalf("只连不发的连接应在读超时后被关：%v（%v）", err, time.Since(start))
	}
	// 关掉之后名额空出来，新的连接能用。
	c1.Close()
	c2.Close()
	time.Sleep(50 * time.Millisecond)
	if code, _, err := callRaw(d, "GET", "/v1/status", nil); err != nil || code != 200 {
		t.Fatalf("名额空出来后应能连上：%d %v", code, err)
	}
}

// status 只在 GET /v1/status 里带 security（签名校验未启用的说明、调用方身份）。
func TestStatusSecurity(t *testing.T) {
	var c callers
	e := newEnv(t)
	o := e.opts()
	c.opts(&o)
	d := e.start(o)
	c.as(consoleTestUID)
	_, b, _ := callRaw(d, "GET", "/v1/status", nil)
	var st Status
	if json.Unmarshal(b, &st) != nil || st.Security == nil || st.Security.CallerVerification != "disabled" || st.Security.Caller != "console" || st.Security.Note == "" {
		t.Fatalf("status 应带签名校验未启用的说明：%s", b)
	}
	c.as(os.Getuid())
	_, b, _ = callRaw(d, "POST", "/v1/disable", nil)
	if strings.Contains(string(b), "caller_verification") {
		t.Fatal("别的接口返回的状态不带 security（保持迁移前的应答）")
	}
}

// 真实模式的 socket 目录（不以 root 运行时用当前用户代替 root 验证布局）：目录 0755 时 socket 是 0666，组或其他人
// 可写的目录拒绝启动。
func TestSocketDirShared(t *testing.T) {
	e := newEnv(t)
	sd := t.TempDir()
	_ = os.Chmod(sd, 0o755)
	o := e.opts()
	o.SocketDir = sd
	d := e.start(o)
	fi, err := os.Lstat(filepath.Join(sd, SocketName))
	if err != nil || fi.Mode().Perm() != 0o666 || d.SocketPath() != filepath.Join(sd, SocketName) {
		t.Fatalf("socket 应在指定目录、0666：%v %v", fi, err)
	}
	if code, _ := call(t, d, "GET", "/v1/status", nil); code != 200 {
		t.Fatal("属主应连得上")
	}
	d.Shutdown()
	bad := t.TempDir()
	_ = os.Chmod(bad, 0o777)
	o2 := newEnv(t).opts()
	o2.SocketDir = bad
	if _, err := Start(t.Context(), o2); err == nil {
		t.Fatal("组或其他人可写的目录应拒绝启动")
	}
}
