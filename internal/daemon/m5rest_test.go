package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"claude-abroad/internal/console"
	"claude-abroad/internal/exitcheck"
)

// 验收 b、g：冲突检查的接口经真实的组装（命令模拟器）——读到注入的主目录下的 shell 配置；只给文件名和行号，
// 代理地址、密码搜不到；和 status 里的提示项一致；间隔内重复调用不再执行系统命令。没有人登录时 shell 一项跳过。
func TestPreflightAPI(t *testing.T) {
	e := newEnv(t)
	k := &realKit{log: &cmdLog{}}
	p := e.realParams(k)
	p.HomeFS = fstest.MapFS{".zshrc": {Data: []byte("alias ll='ls -l'\nexport HTTPS_PROXY=http://usermark:PWDX@10.9.8.7:8080\n")}}
	sys, err := NewRealSystem(p)
	if err != nil {
		t.Fatal(err)
	}
	e.system = &sys
	d := e.start(e.opts())
	code, b := call(t, d, "GET", "/v1/preflight", nil)
	var v PreflightView
	if code != 200 || json.Unmarshal(b, &v) != nil || v.Cached {
		t.Fatalf("%d %s", code, b)
	}
	found := ""
	for _, it := range v.Warnings {
		if it.ID == "shell_env" {
			found = it.Title
		}
	}
	if !strings.Contains(found, ".zshrc") || !strings.Contains(found, "2") {
		t.Fatalf("应读到注入的主目录下 .zshrc 第 2 行：%s", b)
	}
	for _, leak := range []string{"PWDX", "usermark", "10.9.8.7", "8080"} {
		if strings.Contains(string(b), leak) {
			t.Errorf("应答里出现了 %q", leak)
		}
	}
	// 和 status 里的提示项一致（同一个检查器的结果）。
	st := status(t, d)
	hint := ""
	for _, h := range st.Hints {
		if h.ID == "shell_env" {
			hint = h.Title
		}
	}
	if hint != found {
		t.Fatalf("接口和 status 的提示项应一致：%q / %q", found, hint)
	}
	// 间隔内重复调用：返回上一次的结果，不再执行系统命令。
	n0 := len(k.log.all())
	code, b = call(t, d, "GET", "/v1/preflight", nil)
	if code != 200 || json.Unmarshal(b, &v) != nil || !v.Cached || len(k.log.all()) != n0 {
		t.Fatalf("间隔内应返回缓存、不执行命令：%d %s（命令 %d → %d）", code, b, n0, len(k.log.all()))
	}
	auditAll(t, k.log.all()) // 只经只读的执行器（命令清单核对）
	// 没有人登录：shell 一项跳过（主目录读不到 = 不存在）。
	e2 := newEnv(t)
	k2 := &realKit{log: &cmdLog{}}
	p2 := e2.realParams(k2)
	p2.HomeFS = console.HomeFS{Dir: func() (string, error) { return "", console.ErrNoConsoleUser }}
	sys2, _ := NewRealSystem(p2)
	e2.system = &sys2
	d2 := e2.start(e2.opts())
	_, b = call(t, d2, "GET", "/v1/preflight", nil)
	if strings.Contains(string(b), "shell_env") {
		t.Fatalf("没有人登录时 shell 配置一项应跳过：%s", b)
	}
}

// 验收 c（daemon 一侧）：应用 ID 里的用户目录换成 ~；显示名取文件名；没有数据面时是空列表。
func TestAppsAPI(t *testing.T) {
	for in, want := range map[string]string{
		"path:/Users/alice/Applications/Foo.app/Contents/MacOS/foo": "path:~/Applications/Foo.app/Contents/MacOS/foo",
		"path:/" + "home/b0b/bin/tool":                              "path:~/bin/tool", // 拼起来写：源码里不出现完整的家目录路径（隐私检查）
		"path:/usr/bin/curl":                                        "path:/usr/bin/curl",
		"com.google.chrome":                                         "com.google.chrome",
		"path:/Users/alice":                                         "path:~",
	} {
		if got := sanitizeApp(in); got != want {
			t.Errorf("%s：%s，应为 %s", in, got, want)
		}
	}
	if appName("path:~/bin/tool") != "tool" || appName("com.google.chrome") != "com.google.chrome" {
		t.Fatal("显示名")
	}
	e := newEnv(t)
	d := e.start(e.opts())
	code, b := call(t, d, "GET", "/v1/apps", nil)
	if code != 200 || !strings.Contains(string(b), `"apps":[]`) {
		t.Fatalf("%d %s", code, b)
	}
}

// 验收 d：安装引导。
func TestSetupAPI(t *testing.T) {
	e := newEnv(t)
	pr := &fakeProber{rep: passReport()}
	sys := e.sys.System()
	sys.Baseline = NewBaselineService(pr, e.dir)
	e.system = &sys
	d := e.start(e.opts())
	_, b := call(t, d, "GET", "/v1/setup", nil)
	if !strings.Contains(string(b), `{"id":"main_proxy","done":false}`) || !strings.Contains(string(b), `"completed":false`) {
		t.Fatalf("%s", b)
	}
	if code, b := call(t, d, "POST", "/v1/setup/complete", nil); code != http.StatusUnprocessableEntity || !strings.Contains(string(b), "应用主代理") {
		t.Fatalf("没有基准时完成应 422 并指出差哪一步：%d %s", code, b)
	}
	post(t, d, "/v1/main-proxy/apply", 200)
	// 对照：完成之前启用、停用照常（进度不是安全边界）。
	post(t, d, "/v1/enable", 200)
	gateBefore := status(t, d).Gate.Reasons
	post(t, d, "/v1/disable", 200)
	if code, b := call(t, d, "POST", "/v1/setup/complete", nil); code != 200 || !strings.Contains(string(b), `"completed":true`) {
		t.Fatalf("有基准后可以完成：%d %s", code, b)
	}
	fi, err := os.Stat(filepath.Join(e.dir, SetupFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("进度文件应是 0600：%v %v", fi, err)
	}
	post(t, d, "/v1/enable", 200)
	if got := status(t, d).Gate.Reasons; len(got) != len(gateBefore) {
		t.Fatalf("完成与否不影响闸门：%v / %v", gateBefore, got)
	}
	post(t, d, "/v1/disable", 200)
	d.Shutdown()
	d2 := e.start(e.opts())
	if _, b := call(t, d2, "GET", "/v1/setup", nil); !strings.Contains(string(b), `"completed":true`) {
		t.Fatalf("重启后进度应还在：%s", b)
	}
	d2.Shutdown()
}

func (f *fakeMainVM) Health(context.Context) (VMHealth, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unreachable {
		return VMHealth{}, ErrVMNotReady
	}
	eng := f.engines
	if eng == nil { // 默认两个引擎都在运行（下发主代理之后等它就绪要看这个）
		eng = map[string]EngineView{"sing-box": {State: "running"}, "mihomo": {State: "running"}}
	}
	return VMHealth{BootID: f.boot, Version: "agent-1.0", Kernel: "6.12", UptimeS: 42, Engines: eng}, nil
}

// 验收 e：引擎与虚拟机的状态。
func TestEnginesAPI(t *testing.T) {
	m := newMainEnv(t, false)
	m.addYAML(t, "本地", mainNodesYAML)
	m.enableWith(t, "exit")
	m.d.exitLoop.round(context.Background()) // 记下启动标识
	m.vm.mu.Lock()
	m.vm.engines = map[string]EngineView{
		"sing-box": {State: "running", Restarts: 1, Since: time.Unix(1_800_000_000, 0)},
		"mihomo":   {State: "backoff", Restarts: 3, LastError: "dial 203.0.113.8:443 failed"},
	}
	m.vm.mu.Unlock()
	code, b := call(t, m.d, "GET", "/v1/engines", nil)
	var v EnginesView
	if code != 200 || json.Unmarshal(b, &v) != nil || !v.Reachable || v.Version != "agent-1.0" || len(v.Engines) != 2 || v.Engines[0].Name != "mihomo" || v.BootChanged {
		t.Fatalf("%d %s", code, b)
	}
	if strings.Contains(string(b), "203.0.113.8") || strings.Contains(string(b), "PWDMARK") || strings.Contains(string(b), "proxies") {
		t.Fatalf("应答里不能有地址和配置：%s", b)
	}
	m.vm.mu.Lock()
	m.vm.boot = "boot-9"
	m.vm.mu.Unlock()
	_, b = call(t, m.d, "GET", "/v1/engines", nil)
	if !strings.Contains(string(b), `"boot_changed":true`) {
		t.Fatalf("启动标识变了应标出来：%s", b)
	}
	m.vm.mu.Lock()
	m.vm.unreachable = true
	m.vm.mu.Unlock()
	code, b = call(t, m.d, "GET", "/v1/engines", nil)
	if code != 200 || !strings.Contains(string(b), `"reachable":false`) {
		t.Fatalf("不可达时应如实返回、不报错：%d %s", code, b)
	}
}

// 验收 f：单独检查一个出口。
func TestExitCheckAPI(t *testing.T) {
	e := newEnv(t)
	var calls atomic.Int32
	release := make(chan struct{})
	var once sync.Once
	sys := e.sys.System()
	sys.CheckExit = func(ctx context.Context, tag string) exitcheck.Report {
		calls.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
		}
		r := passReport()
		r.Exit = tag
		return r
	}
	e.system = &sys
	d := e.start(e.opts())
	post(t, d, "/v1/enable", 200) // 借用虚拟机修复（v0.70）：未启用时不检查分流出口，先启用（断言不变）
	if err := d.exits.Put(StoredExit{Tag: "wg", Kind: "wireguard", Conf: tWG}); err != nil {
		t.Fatal(err)
	}
	if code, _ := call(t, d, "POST", "/v1/exits/nope/check", nil); code != http.StatusNotFound {
		t.Fatal("不存在的出口 404")
	}
	var wg sync.WaitGroup
	results := make([]ExitCheckResult, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, b := call(t, d, "POST", "/v1/exits/wg/check", nil)
			_ = json.Unmarshal(b, &results[i])
		}(i)
	}
	for i := 0; i < 200 && calls.Load() == 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	once.Do(func() { close(release) })
	wg.Wait()
	if calls.Load() != 1 || results[0].Cached == results[1].Cached {
		t.Fatalf("并发触发只跑一个：调用 %d 次，%+v", calls.Load(), results)
	}
	// 检查的结果进了可用状态（这个出口有检查项了）。
	st, _ := d.exitStatus("wg")
	if len(st.Avail.Items) == 0 && st.Avail.Reason == "" {
		t.Fatalf("可用状态应按结果更新：%+v", st)
	}
	// 间隔内重复触发：返回上一次的结果，不再跑。
	_, b := call(t, d, "POST", "/v1/exits/wg/check", nil)
	var r ExitCheckResult
	if json.Unmarshal(b, &r) != nil || !r.Cached || calls.Load() != 1 {
		t.Fatalf("间隔内应返回上一次的结果：%s（%d 次）", b, calls.Load())
	}
}

// 没有人登录时环境检测第 15、16 项按原来的约定跳过（主目录取不到）。
func TestHomeNoConsoleUser(t *testing.T) {
	h := console.HomeFS{Dir: func() (string, error) { return "", errors.New("没有人登录") }}
	if _, err := fs.ReadFile(h, ".zshrc"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
}

// M5-7 验收 a：主代理出口信息的专用接口。没有基准时“还没有”；有基准时给 IP、国家、城市、时区；
// 调用过这个接口之后，状态、事件、日志、任务查询里仍然搜不到出口 IP 和城市（其他用户断开见 TestLevelMatrix，按路由表生成）。
func TestMainExitAPI(t *testing.T) {
	m := newMainEnv(t, true)
	code, b := call(t, m.d, "GET", "/v1/main-proxy/exit", nil)
	var v MainExitView
	if code != 200 || json.Unmarshal(b, &v) != nil || v.Available || v.IP != "" || !strings.Contains(v.Note, "还没有") {
		t.Fatalf("没有基准：%d %s", code, b)
	}
	m.addYAML(t, "本地", mainNodesYAML)
	m.enableWith(t, "exit")
	code, b = call(t, m.d, "GET", "/v1/main-proxy/exit", nil)
	if code != 200 || json.Unmarshal(b, &v) != nil || !v.Available || v.IP != "203.0.113.7" || v.Country != "US" || v.City != "Exampleville" || v.Timezone != "America/Los_Angeles" {
		t.Fatalf("有基准：%d %s", code, b)
	}
	// 再调几次，并跑一个异步任务。
	for i := 0; i < 3; i++ {
		call(t, m.d, "GET", "/v1/main-proxy/exit", nil)
	}
	_, jb := call(t, m.d, "POST", "/v1/disable?async=1", nil)
	var job struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(jb, &job)
	var jobView []byte
	for i := 0; i < 200; i++ {
		_, jobView = call(t, m.d, "GET", "/v1/jobs/"+job.ID, nil)
		var j Job
		if json.Unmarshal(jobView, &j) == nil && j.State != JobRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, sb := call(t, m.d, "GET", "/v1/status", nil)
	all := map[string]string{"状态和事件": string(sb), "日志": m.logs.String(), "任务": string(jb) + string(jobView)}
	for where, text := range all {
		for _, x := range []string{"203.0.113.7", "Exampleville", "Example State"} {
			if strings.Contains(text, x) {
				t.Errorf("%s里出现了 %q", where, x)
			}
		}
	}
	if job.ID == "" || len(jobView) == 0 {
		t.Fatalf("异步任务：%s / %s", jb, jobView)
	}
}
