package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claude-abroad/internal/api"
	"claude-abroad/internal/claudeproc"
	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/gatesm"
)

// codeOf 取错误应答里的代码和结构化数据。
func codeOf(t *testing.T, b []byte) api.ErrorBody {
	t.Helper()
	var e api.ErrorBody
	if err := json.Unmarshal(b, &e); err != nil {
		t.Fatalf("不是错误应答：%s", b)
	}
	if e.Error == "" {
		t.Fatalf("错误应答缺文字：%s", b)
	}
	return e
}

// m5-plan v2.7 验收 a：每种情况的错误应答带对应的代码（文字照旧）。
func TestErrorCodes(t *testing.T) {
	seen := map[api.ErrorCode]bool{}
	expect := func(name string, code int, b []byte, wantStatus int, want api.ErrorCode) api.ErrorBody {
		t.Helper()
		if code != wantStatus {
			t.Fatalf("%s：状态码 %d，应为 %d：%s", name, code, wantStatus, b)
		}
		e := codeOf(t, b)
		if e.Code != want {
			t.Fatalf("%s：代码 %q，应为 %q：%s", name, e.Code, want, b)
		}
		seen[want] = true
		return e
	}

	// 需要本人授权、需要确认、确认码无效。
	var c callers
	e1 := newEnv(t)
	o1 := e1.opts()
	c.opts(&o1)
	d1 := e1.start(o1)
	c.as(os.Getuid())
	if code, b, _ := callRaw(d1, "POST", "/v1/enable", nil); code != 200 {
		t.Fatalf("启用：%d %s", code, b)
	}
	c.as(consoleTestUID)
	code, b, _ := callRaw(d1, "POST", "/v1/restore", nil)
	expect("控制台用户回国", code, b, http.StatusForbidden, api.CodeNeedsAuthorization)
	c.as(os.Getuid())
	code, b, _ = callRaw(d1, "POST", "/v1/restore", nil)
	expect("回国不带确认码", code, b, http.StatusConflict, api.CodeNeedsConfirm)
	code, b, _ = callRaw(d1, "POST", "/v1/restore", []byte(`{"confirm":"00000000000000000000000000000000"}`))
	expect("确认码无效", code, b, http.StatusForbidden, api.CodeConfirmInvalid)
	// 不提供（这个环境没有环境检测）、找不到。
	code, b, _ = callRaw(d1, "GET", "/v1/envcheck", nil)
	expect("没有环境检测", code, b, http.StatusNotImplemented, api.CodeUnsupported)
	code, b, _ = callRaw(d1, "GET", "/v1/exits/nope", nil)
	expect("没有这个出口", code, b, http.StatusNotFound, api.CodeNotFound)
	// 校验不通过（带是哪一项）：规则引用了不存在的出口、出口标签不合法。
	code, b, _ = callRaw(d1, "PUT", "/v1/rules", []byte("rules:\n  - {app: com.example.a, action: block}\n  - {app: com.example.b, action: \"proxy:nope\"}\n"))
	if e := expect("规则引用不存在的出口", code, b, http.StatusUnprocessableEntity, api.CodeInvalid); e.Field != "rules[1]" {
		t.Fatalf("field 应指出第二条：%+v", e)
	}
	code, b, _ = callRaw(d1, "PUT", "/v1/exits/Bad_Tag", []byte(`{"kind":"wireguard","conf":"x"}`))
	if e := expect("出口标签不合法", code, b, http.StatusUnprocessableEntity, api.CodeInvalid); e.Field != "tag" {
		t.Fatalf("field 应是 tag：%+v", e)
	}
	// 出口被规则引用时删除：正在使用。
	if err := d1.exits.Put(StoredExit{Tag: "wg", Kind: "wireguard", Conf: tWG}); err != nil {
		t.Fatal(err)
	}
	if code, b, _ := callRaw(d1, "PUT", "/v1/rules", []byte("rules:\n  - {app: com.example.b, action: \"proxy:wg\"}\n")); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	code, b, _ = callRaw(d1, "DELETE", "/v1/exits/wg", nil)
	expect("删除被引用的出口", code, b, http.StatusConflict, api.CodeInUse)

	// 忙。
	e2 := newEnv(t)
	bp := &blockingPre{entered: make(chan struct{}), release: make(chan struct{})}
	e2.sys.Pre = bp
	d2 := e2.start(e2.opts())
	done := make(chan int, 1)
	go func() { c, _ := call(t, d2, "POST", "/v1/enable", nil); done <- c }()
	<-bp.entered
	code, b = call(t, d2, "POST", "/v1/enable", nil)
	expect("启用中再启用", code, b, http.StatusConflict, api.CodeBusy)
	close(bp.release)
	<-done

	// 主代理：没有基准、经本机网络、局域网待确认、更换失败已回退、回退未完成、正在使用。
	m := newMainEnv(t, false)
	code, b = call(t, m.d, "POST", "/v1/enable", nil)
	e := codeOf(t, b)
	if code == 200 || e.Code != api.CodeNoBaseline {
		t.Fatalf("没有基准时启用：%d %s", code, b)
	}
	seen[api.CodeNoBaseline] = true
	code, b = call(t, m.d, "POST", "/v1/setup/complete", nil)
	expect("没有基准时完成引导", code, b, http.StatusUnprocessableEntity, api.CodeNoBaseline)
	code, b = call(t, m.d, "POST", "/v1/main-proxy/sources", []byte(`{"kind":"subscription","name":"s","url":"https://sub.example.com/x"}`))
	if e := expect("未启用时抓取订阅", code, b, http.StatusUnprocessableEntity, api.CodeViaHostRequired); e.Field != "via_host" {
		t.Fatalf("%+v", e)
	}
	m.addYAML(t, "本地", mainNodesYAML)
	code, b = m.sel(t, MainSelectRequest{Selected: m.nodeID(t, "exit"), Chain: []string{m.nodeID(t, "lan"), m.nodeID(t, "exit")}})
	if e := expect("第一跳是局域网节点", code, b, http.StatusUnprocessableEntity, api.CodeLANUnconfirmed); e.Node != m.nodeID(t, "lan") {
		t.Fatalf("node 应是局域网节点的 ID：%+v", e)
	}
	m.enableWith(t, "exit")
	m.vm.mu.Lock()
	m.vm.failPush = errors.New("HTTP 504：引擎没有就绪")
	m.vm.mu.Unlock()
	code, b = m.sel(t, MainSelectRequest{Selected: m.nodeID(t, "exit2")})
	if e := codeOf(t, b); code == 200 || e.Code != api.CodeSwitchRolledBack {
		t.Fatalf("更换失败已回退：%d %s", code, b)
	}
	seen[api.CodeSwitchRolledBack] = true
	m.d.exitLoop.round(context.Background())
	m.vm.mu.Lock()
	m.vm.failHopsN = 2
	m.vm.mu.Unlock()
	code, b = m.sel(t, MainSelectRequest{Selected: m.nodeID(t, "exit2")})
	if e := codeOf(t, b); code == 200 || e.Code != api.CodeSwitchRollbackIncomplete {
		t.Fatalf("回退未完成：%d %s", code, b)
	}
	seen[api.CodeSwitchRollbackIncomplete] = true
	srcID := ""
	_, sb := call(t, m.d, "GET", "/v1/main-proxy/sources", nil)
	var srcs []MainProxySourceView
	_ = json.Unmarshal(sb, &srcs)
	if len(srcs) > 0 {
		srcID = srcs[0].ID
	}
	code, b = call(t, m.d, "DELETE", "/v1/main-proxy/sources/"+srcID, nil)
	expect("删除正在使用的来源", code, b, http.StatusConflict, api.CodeInUse)

	// 架构师列出的每种情况都覆盖到。
	for _, want := range []api.ErrorCode{api.CodeNeedsAuthorization, api.CodeNeedsConfirm, api.CodeBusy, api.CodeViaHostRequired, api.CodeLANUnconfirmed,
		api.CodeSwitchRolledBack, api.CodeSwitchRollbackIncomplete, api.CodeNoBaseline, api.CodeUnsupported, api.CodeInvalid} {
		if !seen[want] {
			t.Errorf("没有测到代码 %s", want)
		}
	}
}

// 默认代码按状态码；代码表不重复。
func TestDefaultCodes(t *testing.T) {
	for status, want := range map[int]api.ErrorCode{400: api.CodeInvalid, 422: api.CodeInvalid, 403: api.CodeNeedsAuthorization, 404: api.CodeNotFound,
		409: api.CodeBusy, 413: api.CodeTooLarge, 501: api.CodeUnsupported, 503: api.CodeUnavailable, 500: api.CodeInternal, 502: api.CodeInternal} {
		if got := api.DefaultCode(status); got != want {
			t.Errorf("%d → %s，应为 %s", status, got, want)
		}
	}
	seen := map[api.ErrorCode]bool{}
	for _, c := range api.ErrorCodes {
		if seen[c] {
			t.Errorf("重复：%s", c)
		}
		seen[c] = true
	}
}

// M5-9：GET /v1/rules?format=json 与 YAML 内容相同，JSON 原样 PUT 回去得到同样的规则。
func TestRulesJSON(t *testing.T) {
	e := newEnv(t)
	d := e.start(e.opts())
	y := "default: proxy:main\nrules:\n  - {app: com.example.a, action: block}\n  - {app: \"path:/usr/bin/curl\", action: \"proxy:main\"}\nlocal_domains: []\n"
	if code, b := call(t, d, "PUT", "/v1/rules", []byte(y)); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	code, jb := call(t, d, "GET", "/v1/rules?format=json", nil)
	var f struct {
		Default string `json:"default"`
		Rules   []struct{ App, Action string }
	}
	if code != 200 || json.Unmarshal(jb, &f) != nil || f.Default != "proxy:main" || len(f.Rules) != 2 || f.Rules[0].App != "com.example.a" || f.Rules[1].Action != "proxy:main" {
		t.Fatalf("%d %s", code, jb)
	}
	_, y1 := call(t, d, "GET", "/v1/rules", nil)
	if code, b := call(t, d, "PUT", "/v1/rules", jb); code != 200 {
		t.Fatalf("JSON 原样 PUT 回去：%d %s", code, b)
	}
	if _, y2 := call(t, d, "GET", "/v1/rules", nil); string(y1) != string(y2) {
		t.Fatalf("回写后规则应相同：\n%s\n%s", y1, y2)
	}
	if code, b := call(t, d, "GET", "/v1/rules?format=xml", nil); code != 400 || codeOf(t, b).Field != "format" {
		t.Fatalf("%d %s", code, b)
	}
}

// M5-9 复审第 5 条：只读的回国预览。假的进程表里有 Claude：返回清单；调用前后确认码仓库没有新增；状态不变。
func TestRestorePreview(t *testing.T) {
	var c callers
	e, _, o := confirmEnv(t, c.opts)
	d := e.start(o)
	addClaude(e, claudeproc.ExitOnTerm)
	c.as(os.Getuid())
	post(t, d, "/v1/enable", 200)
	st0 := status(t, d).State
	c.as(consoleTestUID) // 只读级别：控制台用户也能看
	for i := 0; i < 3; i++ {
		code, b, _ := callRaw(d, "GET", "/v1/restore/preview", nil)
		var pv claudeproc.Preview
		if code != 200 || json.Unmarshal(b, &pv) != nil || len(pv.Processes) == 0 || strings.Contains(string(b), "confirm") {
			t.Fatalf("%d %s", code, b)
		}
	}
	d.confirms.mu.Lock()
	n := len(d.confirms.m)
	d.confirms.mu.Unlock()
	if n != 0 {
		t.Fatalf("预览不能签发确认码（有 %d 个）", n)
	}
	if status(t, d).State != st0 || len(e.sys.Procs.Signals) != 0 {
		t.Fatal("预览不能改状态、不能发信号")
	}
}

// M5-9 复审建议改：409 的错误应答都要显式给代码（不靠“409 默认是忙”）。
func TestNo409WithPlainText(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "StatusConflict, errBody{") {
				t.Errorf("%s:%d：409 要用 writeErr 显式给代码：%s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// M5-10 复审建议改：出口检查不通过时，闸门关闭事件的说明（应用会把它放进“保护断开”的通知）里不含出口 IP、城市、州省。
// 复查的说明只有没通过的项名和原因代码（failedItems）。
func TestExitCheckEventNoAddress(t *testing.T) {
	m := newMainEnv(t, false)
	m.addYAML(t, "本地", mainNodesYAML)
	m.enableWith(t, "exit")
	r := passReport()
	r.Verdict = exitcheck.Fail
	r.Items[2] = exitcheck.Item{Name: "mainland", Result: exitcheck.Fail, Cause: exitcheck.CauseMainland, Detail: "203.0.113.7 在大陆段"}
	// 真实模式的复查是 FullExitCheck → verdictOf → failedItems；这里把带地址的报告原样接进去。
	m.d.re.Check = func(context.Context) (gatesm.Result, string) { return verdictOf(r) }
	m.d.re.RunOnce(context.Background())
	st := status(t, m.d)
	if st.GateOpen {
		t.Fatal("出口检查不通过应关闸门")
	}
	found := false
	for _, ev := range st.Events {
		if ev.Source == "gate" && ev.Kind == "gate_closed" && strings.HasPrefix(ev.Detail, "exit_check：") {
			found = true
		}
		for _, x := range []string{"203.0.113.7", "Exampleville", "Example State"} {
			if strings.Contains(ev.Detail, x) {
				t.Errorf("事件说明里出现了 %q：%+v", x, ev)
			}
		}
	}
	if !found {
		t.Fatalf("应有 exit_check 的闸门关闭事件：%+v", st.Events)
	}
	if got := failedItems(r); !strings.Contains(got, "是否在大陆") || strings.Contains(got, "203.0.113.7") {
		t.Fatalf("failedItems：%q", got)
	}
}
