package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-abroad/internal/claudecfg"
	"claude-abroad/internal/claudeproc"
	"claude-abroad/internal/sysnet"
)

// v0.72：回国确认里选第四道（docs/restore-claudeproxy-plan.md v3，验收 a–i）。

func bp(v bool) *bool { return &v }

// restoreStep 发一次回国请求（带或不带确认码）；监护短暂占着忙标记时回的“忙”稍后重试（确认码在忙时会放回）。
func restoreStep(t *testing.T, d *Daemon, kill bool, cp *bool, confirm string) (int, []byte) {
	t.Helper()
	body, _ := json.Marshal(RestoreRequest{Confirm: confirm, KillBrowsers: kill, ClaudeProxy: cp})
	var code int
	var b []byte
	for i := 0; i < 100; i++ {
		code, b = call(t, d, "POST", "/v1/restore", body)
		if code != http.StatusConflict || codeOf(t, b).Code != "busy" {
			return code, b
		}
		time.Sleep(10 * time.Millisecond)
	}
	return code, b
}

// restoreAsk 是第一步：应是 409 带确认码，返回预览。
func restoreAsk(t *testing.T, d *Daemon, kill bool, cp *bool) ConfirmNeeded {
	t.Helper()
	code, b := restoreStep(t, d, kill, cp, "")
	var cn ConfirmNeeded
	if code != http.StatusConflict || json.Unmarshal(b, &cn) != nil || cn.Confirm == "" {
		t.Fatalf("第一步应回 409 带确认码：%d %s", code, b)
	}
	return cn
}

// restoreCP 两步都带同样的选项，返回第二步的结果。
func restoreCP(t *testing.T, d *Daemon, kill bool, cp *bool) (int, Status, []byte) {
	t.Helper()
	cn := restoreAsk(t, d, kill, cp)
	code, b := restoreStep(t, d, kill, cp, cn.Confirm)
	var st Status
	_ = json.Unmarshal(b, &st)
	return code, st, b
}

func restorePreview(t *testing.T, d *Daemon, q string) RestorePreviewView {
	t.Helper()
	code, b := call(t, d, "GET", "/v1/restore/preview"+q, nil)
	var v RestorePreviewView
	if code != 200 || json.Unmarshal(b, &v) != nil {
		t.Fatalf("只读预览：%d %s", code, b)
	}
	return v
}

// cpSnapshot 是第四道的全部可见状态：开关、端口、分片、开关文件。
func (e *cpsEnv) cpSnapshot(d *Daemon) string {
	frag, _ := os.ReadFile(e.fragment())
	sw, _ := os.ReadFile(d.switchPath())
	return fmt.Sprintf("on=%v listening=%v frag=%q switch=%q", d.cpOn.Load(), d.curCP() != nil, frag, sw)
}

// assertOnNow：开关开、分片内容逐字对、端口在听、开关文件记开（非测试模式）。
func (e *cpsEnv) assertOnNow(t *testing.T, d *Daemon, what string) {
	t.Helper()
	if !d.cpOn.Load() || d.curCP() == nil {
		t.Fatalf("%s：开关应开、端口应在听", what)
	}
	want, _ := claudecfg.Content(claudecfg.DefaultPort)
	if got, err := os.ReadFile(e.fragment()); err != nil || string(got) != string(want) {
		t.Fatalf("%s：分片应写入：%v", what, err)
	}
	if on, why := readClaudeSwitch(d.switchPath()); !on || why != "" {
		t.Fatalf("%s：开关文件应记开", what)
	}
	st := status(t, d).ClaudeCode
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", st.Port))
	if err != nil {
		t.Fatalf("%s：端口应在听：%v", what, err)
	}
	conn.Close()
}

func writeCorp(t *testing.T, e *cpsEnv) string {
	t.Helper()
	_ = os.MkdirAll(e.cs.Dir, 0o755)
	corp := filepath.Join(e.cs.Dir, "corp.json")
	if err := os.WriteFile(corp, []byte(`{"env":{"HTTPS_PROXY":"http://proxy.corp.example:8080"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return corp
}

// a. 预览：不带字段时 {current, target=current, test_mode}；没打开管控、卸载过时没有这一块，明确要打开分别 501、409，不签发。
func TestRestoreCPPreview(t *testing.T) {
	t.Run("关着", func(t *testing.T) {
		e := newCPSEnv(t)
		d := e.start()
		post(t, d, "/v1/enable", 200)
		cn := restoreAsk(t, d, false, nil)
		if cn.ClaudeProxy == nil || cn.ClaudeProxy.Current || cn.ClaudeProxy.Target || cn.ClaudeProxy.TestMode {
			t.Fatalf("不带字段：%+v", cn.ClaudeProxy)
		}
		if cn := restoreAsk(t, d, false, bp(true)); !cn.ClaudeProxy.Target || cn.ClaudeProxy.Current {
			t.Fatalf("带 true：%+v", cn.ClaudeProxy)
		}
	})
	t.Run("开着", func(t *testing.T) {
		e := newCPSEnv(t)
		d := e.start()
		if code, b := putCP(t, d, true); code != 200 {
			t.Fatalf("%d %s", code, b)
		}
		post(t, d, "/v1/enable", 200)
		if cn := restoreAsk(t, d, false, nil); cn.ClaudeProxy == nil || !cn.ClaudeProxy.Current || !cn.ClaudeProxy.Target {
			t.Fatalf("开着、不带：%+v", cn.ClaudeProxy)
		}
		if cn := restoreAsk(t, d, false, bp(false)); cn.ClaudeProxy.Target {
			t.Fatalf("开着、带 false：%+v", cn.ClaudeProxy)
		}
	})
	t.Run("没打开管控", func(t *testing.T) {
		e, _, o := confirmEnv(t)
		d := e.start(o)
		post(t, d, "/v1/enable", 200)
		if cn := restoreAsk(t, d, false, nil); cn.ClaudeProxy != nil {
			t.Fatalf("没打开管控时不应有这一块：%+v", cn.ClaudeProxy)
		}
		if cn := restoreAsk(t, d, false, bp(false)); cn.ClaudeProxy != nil {
			t.Fatal("要关按保持，照常预览")
		}
		n := len(d.confirms.m)
		code, b := restoreStep(t, d, false, bp(true), "")
		if code != http.StatusNotImplemented || codeOf(t, b).Code != "unsupported" || strings.Contains(string(b), `"confirm"`) {
			t.Fatalf("要打开应回 501：%d %s", code, b)
		}
		if len(d.confirms.m) != n {
			t.Fatal("501 时不应签发确认码")
		}
		if v := restorePreview(t, d, ""); v.ClaudeProxy != nil {
			t.Fatal("只读预览也不应有这一块")
		}
	})
	t.Run("卸载过", func(t *testing.T) {
		e := newCPSEnv(t)
		d := e.start()
		if code, raw := uninstallWith(t, d); code != 200 {
			t.Fatalf("%d %s", code, raw)
		}
		code, b := restoreStep(t, d, false, bp(true), "")
		if code != http.StatusConflict || codeOf(t, b).Code != "uninstalled" {
			t.Fatalf("卸载后要打开应回 409：%d %s", code, b)
		}
		for _, cp := range []*bool{nil, bp(false)} {
			if code, b := restoreStep(t, d, false, cp, ""); code != 200 {
				t.Fatalf("卸载后要关或不带：已是回国状态应 200：%d %s", code, b)
			}
		}
		if v := restorePreview(t, d, ""); v.ClaudeProxy != nil {
			t.Fatal("卸载后只读预览不应有这一块")
		}
	})
	t.Run("卸载标记在、状态还没回国", func(t *testing.T) {
		e := newCPSEnv(t)
		d := e.start()
		post(t, d, "/v1/enable", 200)
		if err := os.WriteFile(filepath.Join(e.dir, UninstalledFile), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if cn := restoreAsk(t, d, false, nil); cn.ClaudeProxy != nil {
			t.Fatal("卸载过时不应有这一块")
		}
		n := len(d.confirms.m)
		if code, b := restoreStep(t, d, false, bp(true), ""); code != http.StatusConflict || codeOf(t, b).Code != "uninstalled" {
			t.Fatalf("应回 409：%d %s", code, b)
		}
		if len(d.confirms.m) != n {
			t.Fatal("409 时不应签发确认码")
		}
	})
}

// a、c（只读预览）：内容和第一步一致；?claude_proxy= 改目标、非法值 400；现在关时带冲突；不签发、状态文件不变。
func TestRestorePreviewClaudeProxy(t *testing.T) {
	e := newCPSEnv(t)
	d := e.start()
	post(t, d, "/v1/enable", 200)
	addClaude(e.env, claudeproc.ExitOnTerm)
	stBefore, _ := os.ReadFile(filepath.Join(e.dir, "state.json"))
	n := len(d.confirms.m)
	v := restorePreview(t, d, "")
	if v.ClaudeProxy == nil || v.ClaudeProxy.Current || v.ClaudeProxy.Target || len(v.ClaudeProxy.Conflicts) != 0 || v.Note != RestorePreviewRecheckNote {
		t.Fatalf("只读预览：%+v", v)
	}
	cn := restoreAsk(t, d, false, nil)
	if len(v.Processes) != len(cn.Preview.Processes) || len(v.Processes) == 0 || v.ClaudeProxy.Current != cn.ClaudeProxy.Current || v.ClaudeProxy.Target != cn.ClaudeProxy.Target || v.ClaudeProxy.TestMode != cn.ClaudeProxy.TestMode {
		t.Fatalf("只读预览应和第一步一致：%+v %+v", v, cn)
	}
	if v := restorePreview(t, d, "?claude_proxy=on&kill_browsers=true"); !v.ClaudeProxy.Target {
		t.Fatalf("?claude_proxy=on 应改目标：%+v", v.ClaudeProxy)
	}
	if code, _ := call(t, d, "GET", "/v1/restore/preview?claude_proxy=maybe", nil); code != 400 {
		t.Fatalf("非法值应 400：%d", code)
	}
	writeCorp(t, e)
	if v := restorePreview(t, d, ""); len(v.ClaudeProxy.Conflicts) != 1 || v.ClaudeProxy.Conflicts[0].File != "corp.json" || v.ClaudeProxy.Conflicts[0].Var != "HTTPS_PROXY" {
		t.Fatalf("现在关时应带冲突：%+v", v.ClaudeProxy)
	}
	if len(d.confirms.m) != n+1 { // 只有上面那次 POST 第一步签发过
		t.Fatalf("只读预览不应签发确认码：%d → %d", n, len(d.confirms.m))
	}
	if after, _ := os.ReadFile(filepath.Join(e.dir, "state.json")); string(after) != string(stBefore) {
		t.Fatal("只读预览不应改状态文件")
	}
	if d.cpOn.Load() {
		t.Fatal("预览不应改开关")
	}
}

// b. 绑定：两步的 (kill_browsers, claude_proxy) 不一致一律 confirm_invalid，开关和网络都没变；一致时成功。
func TestRestoreCPBinding(t *testing.T) {
	e := newCPSEnv(t)
	d := e.start()
	post(t, d, "/v1/enable", 200)
	before := e.cpSnapshot(d)
	net0 := e.sys.Net.Snapshot()
	cps := []*bool{nil, bp(true), bp(false)}
	for _, k1 := range []bool{false, true} {
		for _, c1 := range cps {
			for _, k2 := range []bool{false, true} {
				for _, c2 := range cps {
					if k1 == k2 && cpChoice(c1) == cpChoice(c2) {
						continue
					}
					cn := restoreAsk(t, d, k1, c1)
					code, b := restoreStep(t, d, k2, c2, cn.Confirm)
					name := fmt.Sprintf("%v/%s → %v/%s", k1, cpChoice(c1), k2, cpChoice(c2))
					if code != http.StatusForbidden || codeOf(t, b).Code != "confirm_invalid" {
						t.Fatalf("%s：应 confirm_invalid：%d %s", name, code, b)
					}
					if s := status(t, d); s.State != "ENABLED" || !e.sys.Net.Snapshot().Equal(net0) || e.cpSnapshot(d) != before {
						t.Fatalf("%s：什么都不应改（%s）", name, s.State)
					}
				}
			}
		}
	}
	// 现在关时带 false（和现状一样，按保持）也按原值绑定：第二步不带就不一致
	cn := restoreAsk(t, d, false, bp(false))
	if code, b := restoreStep(t, d, false, nil, cn.Confirm); code != http.StatusForbidden {
		t.Fatalf("原值 off 和 keep 不应混用：%d %s", code, b)
	}
	// 对照：一致时成功
	if code, st, b := restoreCP(t, d, false, bp(true)); code != 200 || st.State != "RESTORED" || st.ClaudeProxyChange == nil || !st.ClaudeProxyChange.OK {
		t.Fatalf("一致时应成功：%d %s", code, b)
	}
}

// c. 预览时选开但有冲突：422 加 conflicts，不签发，什么都没变；选关或不带照常预览。
func TestRestoreCPPreviewConflict(t *testing.T) {
	e := newCPSEnv(t)
	d := e.start()
	post(t, d, "/v1/enable", 200)
	writeCorp(t, e)
	before := e.cpSnapshot(d)
	n := len(d.confirms.m)
	code, b := restoreStep(t, d, false, bp(true), "")
	var body struct {
		Code      string               `json:"code"`
		Error     string               `json:"error"`
		Conflicts []claudecfg.Conflict `json:"conflicts"`
		Confirm   string               `json:"confirm"`
	}
	_ = json.Unmarshal(b, &body)
	if code != http.StatusUnprocessableEntity || body.Code != "invalid" || body.Confirm != "" || len(body.Conflicts) != 1 ||
		body.Conflicts[0].File != "corp.json" || !strings.Contains(body.Error, "corp.json 的 HTTPS_PROXY") || !strings.Contains(body.Error, "没有回国") {
		t.Fatalf("应 422 带冲突：%d %s", code, b)
	}
	if len(d.confirms.m) != n || status(t, d).State != "ENABLED" || e.cpSnapshot(d) != before {
		t.Fatal("冲突时什么都不应改、不签发")
	}
	restoreAsk(t, d, false, bp(false))
	restoreAsk(t, d, false, nil)
}

// d. 回国成功加选开：回国状态、开关开、分片写了、端口在听、开关文件记开；经本机代理端口一律 403；ok 为真。
func TestRestoreCPTurnOn(t *testing.T) {
	e := newCPSEnv(t)
	d := e.start()
	post(t, d, "/v1/enable", 200)
	code, st, b := restoreCP(t, d, false, bp(true))
	if code != 200 || st.State != "RESTORED" || st.ClaudeProxyChange == nil || !st.ClaudeProxyChange.Target || !st.ClaudeProxyChange.OK || len(st.ClaudeProxyChange.Notes) != 0 {
		t.Fatalf("%d %s", code, b)
	}
	if st.ClaudeCode == nil || !st.ClaudeCode.Enabled {
		t.Fatalf("应答里的状态应已是切换之后的：%+v", st.ClaudeCode)
	}
	e.assertOnNow(t, d, "回国后打开")
	if s := status(t, d); s.ClaudeProxyChange != nil {
		t.Fatal("/v1/status 不应带 claude_proxy_change")
	}
	// 回国状态下经本机代理端口：403
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", status(t, d).ClaudeCode.Port))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n")
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, 64)
	n, _ := conn.Read(buf)
	if !strings.HasPrefix(string(buf[:n]), "HTTP/1.1 403") {
		t.Fatalf("回国状态下应 403：%q", buf[:n])
	}
	if !hasEvent(d, "restore", "claude_proxy_on") {
		t.Fatal("应记事件")
	}
}

// e. 开切到关：分片删、端口不听、开关文件记关；分片被改过时 ok 为假、notes 带 leftover，回国仍 200。
func TestRestoreCPTurnOff(t *testing.T) {
	setup := func(t *testing.T) (*cpsEnv, *Daemon) {
		e := newCPSEnv(t)
		d := e.start()
		if code, b := putCP(t, d, true); code != 200 {
			t.Fatalf("%d %s", code, b)
		}
		post(t, d, "/v1/enable", 200)
		return e, d
	}
	t.Run("正常", func(t *testing.T) {
		e, d := setup(t)
		code, st, b := restoreCP(t, d, false, bp(false))
		if code != 200 || st.State != "RESTORED" || st.ClaudeProxyChange == nil || st.ClaudeProxyChange.Target || !st.ClaudeProxyChange.OK {
			t.Fatalf("%d %s", code, b)
		}
		e.assertOff(t, d, "回国后关掉")
	})
	t.Run("分片被改过", func(t *testing.T) {
		e, d := setup(t)
		if err := os.WriteFile(e.fragment(), []byte(`{"env":{}}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		code, st, b := restoreCP(t, d, false, bp(false))
		if code != 200 || st.State != "RESTORED" || st.ClaudeProxyChange == nil || st.ClaudeProxyChange.OK ||
			!strings.Contains(strings.Join(st.ClaudeProxyChange.Notes, "\n"), ClaudeProxyLeftoverNote) ||
			!strings.Contains(strings.Join(st.ClaudeProxyChange.Notes, "\n"), "sudo abroad claude-proxy off") {
			t.Fatalf("%d %s", code, b)
		}
		if d.cpOn.Load() || d.curCP() != nil {
			t.Fatal("开关应记为关、端口不听")
		}
		if on, _ := readClaudeSwitch(d.switchPath()); on {
			t.Fatal("开关文件应记关")
		}
	})
}

// f. 回国失败（结束不了 Claude 中止、状态机出错）：两个方向开关都不动；忙时确认码放回，开关不动。
func TestRestoreLeftoversClaudeProxy(t *testing.T) {
	for _, startOn := range []bool{false, true} {
		t.Run(fmt.Sprintf("现在%s", onOffText(startOn)), func(t *testing.T) {
			e := newCPSEnv(t)
			d := e.start()
			if startOn {
				if code, b := putCP(t, d, true); code != 200 {
					t.Fatalf("%d %s", code, b)
				}
			}
			post(t, d, "/v1/enable", 200)
			addClaude(e.env, claudeproc.Unkillable)
			before := e.cpSnapshot(d)
			code, st, b := restoreCP(t, d, false, bp(!startOn))
			if code != http.StatusInternalServerError || !strings.Contains(string(b), "还有 1 个 Claude 进程结束不了") || st.ClaudeProxyChange != nil {
				t.Fatalf("应中止：%d %s", code, b)
			}
			e.env.stillEnabled(t, d, "结束不了")
			if e.cpSnapshot(d) != before {
				t.Fatalf("中止时开关不应动：%s → %s", before, e.cpSnapshot(d))
			}
		})
	}
}

func TestRestoreCPStateMachineError(t *testing.T) {
	for _, startOn := range []bool{false, true} {
		t.Run(fmt.Sprintf("现在%s", onOffText(startOn)), func(t *testing.T) {
			e := newCPSEnv(t)
			d := e.start()
			if startOn {
				if code, b := putCP(t, d, true); code != 200 {
					t.Fatalf("%d %s", code, b)
				}
			}
			post(t, d, "/v1/enable", 200)
			before := e.cpSnapshot(d)
			cn := restoreAsk(t, d, false, bp(!startOn))
			e.sys.Net.FailNext(sysnet.OpDNSRemove, errors.New("注入的删除 DNS 失败"))
			code, b := restoreStep(t, d, false, bp(!startOn), cn.Confirm)
			if code != http.StatusInternalServerError || strings.Contains(string(b), "claude_proxy_change") {
				t.Fatalf("状态机出错应失败：%d %s", code, b)
			}
			if s := status(t, d).State; s == "RESTORED" {
				t.Fatal("状态机出错时不应是已回国")
			}
			if e.cpSnapshot(d) != before {
				t.Fatalf("状态机出错时开关不应动：%s → %s", before, e.cpSnapshot(d))
			}
		})
	}
}

func TestRestoreCPBusyGiveBack(t *testing.T) {
	e := newCPSEnv(t)
	d := e.start()
	post(t, d, "/v1/enable", 200)
	before := e.cpSnapshot(d)
	cn := restoreAsk(t, d, false, bp(true))
	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- d.m.ExclusiveHold(func() error { close(held); <-release; return nil })
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("没拿到忙标记：%v", err)
	}
	body, _ := json.Marshal(RestoreRequest{Confirm: cn.Confirm, ClaudeProxy: bp(true)})
	code, b := call(t, d, "POST", "/v1/restore", body)
	if code != http.StatusConflict || codeOf(t, b).Code != "busy" {
		t.Fatalf("忙时应 409：%d %s", code, b)
	}
	if e.cpSnapshot(d) != before || status(t, d).State != "ENABLED" {
		t.Fatal("忙时开关和状态都不应动")
	}
	close(release)
	<-done
	code, b = restoreStep(t, d, false, bp(true), cn.Confirm)
	var st Status
	_ = json.Unmarshal(b, &st)
	if code != 200 || st.ClaudeProxyChange == nil || !st.ClaudeProxyChange.OK {
		t.Fatalf("忙结束后同一个确认码应能用：%d %s", code, b)
	}
	e.assertOnNow(t, d, "忙之后")
}

// g. 回国成功但打开失败（写分片失败、端口被占、开关记录写不进去）：200，ok 为假并说明原因，开关是关，什么都没留下。
func TestRestoreCPOnFails(t *testing.T) {
	cases := []struct {
		name, why string
		mut       func(e *cpsEnv, d *Daemon)
		pre       func(e *cpsEnv) func()
	}{
		{name: "写分片失败", why: "托管设置", mut: func(e *cpsEnv, d *Daemon) {
			_ = os.MkdirAll(filepath.Dir(e.cs.Dir), 0o755)
			_ = os.WriteFile(e.cs.Dir, []byte("x"), 0o644)
		}},
		{name: "监听失败", why: "监听", mut: func(e *cpsEnv, d *Daemon) {
			d.cpSwitchMu.Lock()
			d.cpListen = func(string) (net.Listener, error) { return nil, errors.New("注入的监听失败") }
			d.cpSwitchMu.Unlock()
		}},
		{name: "记开关失败", why: "记下开关", mut: func(e *cpsEnv, d *Daemon) {
			d.cpSwitchMu.Lock()
			d.writeSwitch = func(string, []byte) error { return errors.New("注入的写文件失败") }
			d.cpSwitchMu.Unlock()
		}},
		{name: "端口真被占", why: "占", pre: func(e *cpsEnv) func() {
			occ, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				panic(err)
			}
			e.addr = occ.Addr().String()
			return func() { occ.Close() }
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newCPSEnv(t)
			if c.pre != nil {
				defer c.pre(e)()
			}
			d := e.start()
			post(t, d, "/v1/enable", 200)
			cn := restoreAsk(t, d, false, bp(true))
			if c.mut != nil {
				c.mut(e, d)
			}
			code, b := restoreStep(t, d, false, bp(true), cn.Confirm)
			var st Status
			_ = json.Unmarshal(b, &st)
			notes := ""
			if st.ClaudeProxyChange != nil {
				notes = strings.Join(st.ClaudeProxyChange.Notes, "\n")
			}
			if code != 200 || st.State != "RESTORED" || st.ClaudeProxyChange == nil || st.ClaudeProxyChange.OK ||
				!strings.Contains(notes, c.why) || !strings.Contains(notes, "第四道没有打开") || !strings.Contains(notes, "sudo abroad claude-proxy on") {
				t.Fatalf("%d %s", code, b)
			}
			d.cpSwitchMu.Lock()
			d.cpListen, d.writeSwitch = nil, nil
			d.cpSwitchMu.Unlock()
			if c.name == "写分片失败" {
				_ = os.Remove(e.cs.Dir)
			}
			e.assertOff(t, d, c.name)
		})
	}
	t.Run("确认后才出现的冲突", func(t *testing.T) {
		e := newCPSEnv(t)
		d := e.start()
		post(t, d, "/v1/enable", 200)
		cn := restoreAsk(t, d, false, bp(true))
		writeCorp(t, e)
		code, b := restoreStep(t, d, false, bp(true), cn.Confirm)
		var st Status
		_ = json.Unmarshal(b, &st)
		if code != 200 || st.State != "RESTORED" || st.ClaudeProxyChange == nil || st.ClaudeProxyChange.OK || len(st.ClaudeProxyChange.Conflicts) != 1 {
			t.Fatalf("%d %s", code, b)
		}
		e.assertOff(t, d, "冲突")
	})
}

// h. 已经是回国状态：值不同回 422 并提示命令；值一样或不带照旧 200。
func TestRestoreCPAlreadyRestored(t *testing.T) {
	e := newCPSEnv(t)
	d := e.start()
	post(t, d, "/v1/enable", 200)
	post(t, d, "/v1/restore", 200)
	before := e.cpSnapshot(d)
	code, b := restoreStep(t, d, false, bp(true), "")
	if eb := codeOf(t, b); code != http.StatusUnprocessableEntity || eb.Code != "invalid" || eb.Field != "claude_proxy" ||
		!strings.Contains(eb.Error, "已经是回国状态") || !strings.Contains(eb.Error, "sudo abroad claude-proxy on") {
		t.Fatalf("值不同应 422：%d %s", code, b)
	}
	for _, cp := range []*bool{nil, bp(false)} {
		if code, b := restoreStep(t, d, false, cp, ""); code != 200 || strings.Contains(string(b), "claude_proxy_change") {
			t.Fatalf("值一样或不带应 200：%d %s", code, b)
		}
	}
	if e.cpSnapshot(d) != before {
		t.Fatal("开关不应动")
	}
	// 开着时要关：同样 422，提示 off
	if code, b := putCP(t, d, true); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	if code, b := restoreStep(t, d, false, bp(false), ""); code != http.StatusUnprocessableEntity || !strings.Contains(string(b), "sudo abroad claude-proxy off") {
		t.Fatalf("开着要关：%d %s", code, b)
	}
}

// i. 测试模式：打开只监听，不写托管设置；预览 test_mode 为真；有冲突文件也不拒绝。
func TestRestoreCPTestMode(t *testing.T) {
	e := newCPSEnv(t)
	e.testOnly = true
	d := e.start()
	post(t, d, "/v1/enable", 200)
	writeCorp(t, e)
	cn := restoreAsk(t, d, false, bp(true))
	if cn.ClaudeProxy == nil || !cn.ClaudeProxy.TestMode {
		t.Fatalf("预览应标测试模式：%+v", cn.ClaudeProxy)
	}
	if v := restorePreview(t, d, ""); !v.ClaudeProxy.TestMode || len(v.ClaudeProxy.Conflicts) != 0 {
		t.Fatalf("测试模式不查托管设置的冲突：%+v", v.ClaudeProxy)
	}
	code, b := restoreStep(t, d, false, bp(true), cn.Confirm)
	var st Status
	_ = json.Unmarshal(b, &st)
	if code != 200 || st.ClaudeProxyChange == nil || !st.ClaudeProxyChange.OK {
		t.Fatalf("%d %s", code, b)
	}
	if d.curCP() == nil || !d.cpOn.Load() {
		t.Fatal("测试模式打开应监听")
	}
	if _, err := os.Lstat(e.fragment()); err == nil {
		t.Fatal("测试模式不应写分片")
	}
}
