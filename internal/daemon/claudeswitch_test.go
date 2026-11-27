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

	"claude-abroad/internal/claudecfg"
	"claude-abroad/internal/preflight"
	"claude-abroad/internal/resolverlock"
)

// ---- v0.68 第四道的开关（默认关；用户 2026-11-27）----

// presetClaudeProxyOn 在启动前把第四道的开关记为打开（给“开关已打开”前提下的原有测试用）。
func presetClaudeProxyOn(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ClaudeProxySwitchFile), []byte(`{"version":1,"on":true}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// cpsEnv：打开了启动管控的安装（解析锁、托管设置、首次准备、本机代理端口都接上，临时目录），调用方身份可切换。
type cpsEnv struct {
	*prepEnv
	c        callers
	addr     string
	testOnly bool
}

func newCPSEnv(t *testing.T) *cpsEnv {
	t.Helper()
	e := &cpsEnv{prepEnv: newPrepEnv(t), addr: "127.0.0.1:0"}
	_ = os.MkdirAll(filepath.Dir(e.cs.Dir), 0o755)
	return e
}

func (e *cpsEnv) start() *Daemon {
	o := e.opts()
	o.Sys.ResolverLock, o.Sys.ClaudeSettings = e.lock, e.cs
	o.Sys.FirstRun, o.Sys.PrepareTestOnly, o.Sys.LaunchControl = true, e.testOnly, true
	o.Sys.Preflight = PreflightHints{C: &preflight.Checker{R: cleanRunner{e.t}}}
	o.ClaudeProxyAddr = e.addr
	e.c.opts(&o)
	e.c.as(os.Getuid())
	return e.env.start(o)
}

func putCP(t *testing.T, d *Daemon, on bool) (int, []byte) {
	t.Helper()
	code, b, err := callRaw(d, "PUT", "/v1/settings/claude-proxy", []byte(fmt.Sprintf(`{"enabled":%v}`, on)))
	if err != nil {
		t.Fatal(err)
	}
	return code, b
}

func cpView(t *testing.T, b []byte) ClaudeProxyView {
	t.Helper()
	var v ClaudeProxyView
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%v %s", err, b)
	}
	return v
}

func (e *cpsEnv) fragment() string { return filepath.Join(e.cs.Dir, claudecfg.FileName) }

// assertOff：开关关着、端口不在听、我们的分片和记录都不在、开关文件不是“开”。
func (e *cpsEnv) assertOff(t *testing.T, d *Daemon, what string) {
	t.Helper()
	if d.cpOn.Load() || d.curCP() != nil {
		t.Fatalf("%s：开关应是关、端口不应在听", what)
	}
	if _, err := os.Lstat(e.fragment()); err == nil {
		t.Fatalf("%s：不应留下我们的分片", what)
	}
	if _, err := os.Lstat(filepath.Join(e.dir, claudecfg.RecordFile)); err == nil {
		t.Fatalf("%s：不应留下托管设置的记录", what)
	}
	if on, _ := readClaudeSwitch(d.switchPath()); on {
		t.Fatalf("%s：开关文件不应记为开", what)
	}
	if st := status(t, d).ClaudeCode; st == nil || st.Enabled || st.Listening || st.Effective || st.Settings != nil {
		t.Fatalf("%s：状态应是未开启：%+v", what, st)
	}
}

func layer4(t *testing.T, d *Daemon) (LaunchGuardLayer, string) {
	t.Helper()
	code, b := call(t, d, "GET", "/v1/launchguard", nil)
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	var v LaunchGuardView
	_ = json.Unmarshal(b, &v)
	var cov []string
	for _, c := range v.Coverage {
		cov = append(cov, c.Reason)
	}
	for _, l := range v.Layers {
		if l.Layer == 4 {
			return l, strings.Join(cov, "\n")
		}
	}
	t.Fatal("没有第四层")
	return LaunchGuardLayer{}, ""
}

func findingIDs(t *testing.T, d *Daemon) string {
	t.Helper()
	code, b := call(t, d, "GET", "/v1/preflight", nil)
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	return string(b)
}

// a. 默认关：全新的状态目录、打开了管控——首次准备完成后没有托管设置的分片，端口不监听（即使端口被占着、别的托管设置
// 定义了代理变量，冲突检查里也没有和它有关的两项，启用不受影响）；状态“未开启”；覆盖范围不出现“由第四层兜着”。
func TestClaudeProxyDefaultOff(t *testing.T) {
	occ, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occ.Close()
	e := newCPSEnv(t)
	e.addr = occ.Addr().String()
	d := e.start()
	st := status(t, d)
	if st.Prepare == nil || !st.Prepare.Done {
		t.Fatalf("首次准备应完成（不再和托管设置挂钩）：%+v", st.Prepare)
	}
	if _, err := os.Stat(e.cs.Dir); !os.IsNotExist(err) {
		t.Fatal("默认关时不应建托管设置目录、不应写分片")
	}
	e.assertOff(t, d, "默认")
	if !hasEvent(d, "claude_proxy", "switch_off") || hasEvent(d, "claude_proxy", "listen_failed") {
		t.Fatal("应记“按关处理”的事件，且不应尝试监听")
	}
	l, cov := layer4(t, d)
	if l.Effective || l.Note != ClaudeProxyOffNote {
		t.Fatalf("第四层应是“未开启”：%+v", l)
	}
	if strings.Contains(cov, "第四层（本机代理）兜着") || !strings.Contains(cov, "第四层未开启") {
		t.Fatalf("关着时覆盖范围不应写第四层：%s", cov)
	}
	// 别的托管设置定义了代理变量、端口被占着：冲突检查里都没有，启用通过
	_ = os.MkdirAll(e.cs.Dir, 0o755)
	_ = os.WriteFile(filepath.Join(e.cs.Dir, "corp.json"), []byte(`{"env":{"HTTPS_PROXY":"http://proxy.corp.example:8080"}}`), 0o644)
	if f := findingIDs(t, d); strings.Contains(f, "claude_settings") || strings.Contains(f, "claude_proxy_port") {
		t.Fatalf("关着时冲突检查里不应有第四道的两项：%s", f)
	}
	post(t, d, "/v1/enable", 200)
	if _, err := os.Lstat(e.fragment()); err == nil {
		t.Fatal("启用不应写分片")
	}
}

// b. 打开：没有授权时被拒、什么都没改；有冲突时拒绝、开关仍是关、应答里有文件和变量；成功时分片写入、端口在听、开关记下；
// 覆盖范围出现“由第四层兜着”。再打开一次什么都不改。
func TestClaudeProxyTurnOn(t *testing.T) {
	e := newCPSEnv(t)
	d := e.start()
	e.c.as(consoleTestUID)
	code, b := putCP(t, d, true)
	if code != http.StatusForbidden || codeOf(t, b).Code != "needs_authorization" {
		t.Fatalf("没有授权时应被拒：%d %s", code, b)
	}
	if _, err := os.Stat(e.cs.Dir); !os.IsNotExist(err) {
		t.Fatal("被拒时不应写任何东西")
	}
	if _, err := os.Lstat(d.switchPath()); err == nil {
		t.Fatal("被拒时不应记开关")
	}
	e.assertOff(t, d, "被拒")
	// 有冲突
	e.c.as(os.Getuid())
	_ = os.MkdirAll(e.cs.Dir, 0o755)
	corp := filepath.Join(e.cs.Dir, "corp.json")
	_ = os.WriteFile(corp, []byte(`{"env":{"HTTPS_PROXY":"http://proxy.corp.example:8080"}}`), 0o644)
	code, b = putCP(t, d, true)
	var cb struct {
		Code      string               `json:"code"`
		Error     string               `json:"error"`
		Conflicts []claudecfg.Conflict `json:"conflicts"`
	}
	_ = json.Unmarshal(b, &cb)
	if code != http.StatusUnprocessableEntity || cb.Code != "invalid" || !strings.Contains(cb.Error, "corp.json 的 HTTPS_PROXY") ||
		len(cb.Conflicts) != 1 || cb.Conflicts[0].File != "corp.json" || cb.Conflicts[0].Var != "HTTPS_PROXY" {
		t.Fatalf("有冲突时应拒绝并列出文件和变量：%d %s", code, b)
	}
	e.assertOff(t, d, "有冲突")
	_ = os.Remove(corp)
	// 成功
	code, b = putCP(t, d, true)
	if code != 200 {
		t.Fatalf("打开：%d %s", code, b)
	}
	v := cpView(t, b)
	if !v.Available || !v.Enabled || v.TestMode || v.Status == nil || !v.Status.Listening || !v.Status.Effective {
		t.Fatalf("打开后的应答：%+v", v)
	}
	want, _ := claudecfg.Content(claudecfg.DefaultPort)
	if got, err := os.ReadFile(e.fragment()); err != nil || string(got) != string(want) {
		t.Fatalf("分片应写入：%v", err)
	}
	if on, why := readClaudeSwitch(d.switchPath()); !on || why != "" {
		t.Fatalf("开关应记为开：%v %s", on, why)
	}
	if info, _ := os.Stat(d.switchPath()); info.Mode().Perm() != 0o600 {
		t.Fatalf("开关文件应是 0600：%v", info.Mode())
	}
	st := status(t, d).ClaudeCode
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", st.Port))
	if err != nil {
		t.Fatalf("端口应在听：%v", err)
	}
	conn.Close()
	l, cov := layer4(t, d)
	if !l.Effective || !strings.Contains(cov, "第四层（本机代理）兜着") || strings.Contains(cov, "第四层未开启") {
		t.Fatalf("打开后第四层生效、覆盖范围写第四层：%+v\n%s", l, cov)
	}
	if !hasEvent(d, "claude_proxy", "switch_on") {
		t.Fatal("应记打开的事件")
	}
	fi1, _ := os.Stat(e.fragment())
	if code, b := putCP(t, d, true); code != 200 || !cpView(t, b).Enabled {
		t.Fatalf("再打开一次：%d %s", code, b)
	}
	if fi2, _ := os.Stat(e.fragment()); !fi2.ModTime().Equal(fi1.ModTime()) {
		t.Fatal("已经打开时不应重写分片")
	}
}

// b. 写分片、监听、记开关三步逐个注入失败：都回到关的状态，没有残留的分片和记录；去掉注入后能打开。
func TestClaudeProxyTurnOnRollback(t *testing.T) {
	e := newCPSEnv(t)
	d := e.start()
	setHooks := func(listen func(string) (net.Listener, error), ws func(string, []byte) error) {
		d.cpSwitchMu.Lock()
		d.cpListen, d.writeSwitch = listen, ws
		d.cpSwitchMu.Unlock()
	}
	// 1. 写分片失败：分片目录的位置被一个普通文件占着
	_ = os.WriteFile(e.cs.Dir, []byte("x"), 0o644)
	if code, b := putCP(t, d, true); code != 500 || !strings.Contains(string(b), "托管设置") {
		t.Fatalf("写分片失败应报错：%d %s", code, b)
	}
	if _, err := os.Lstat(filepath.Join(e.dir, claudecfg.RecordFile)); err == nil {
		t.Fatal("写分片失败后不应留下记录")
	}
	if d.cpOn.Load() || d.curCP() != nil {
		t.Fatal("写分片失败后应仍是关")
	}
	_ = os.Remove(e.cs.Dir)
	// 2. 监听失败（注入）
	setHooks(func(string) (net.Listener, error) { return nil, errors.New("注入的监听失败") }, nil)
	if code, b := putCP(t, d, true); code != 500 || !strings.Contains(string(b), "监听") {
		t.Fatalf("监听失败应报错：%d %s", code, b)
	}
	e.assertOff(t, d, "监听失败")
	// 3. 记开关失败（注入）
	setHooks(nil, func(string, []byte) error { return errors.New("注入的写文件失败") })
	if code, b := putCP(t, d, true); code != 500 || !strings.Contains(string(b), "记下开关") {
		t.Fatalf("记开关失败应报错：%d %s", code, b)
	}
	e.assertOff(t, d, "记开关失败")
	if !hasEvent(d, "claude_proxy", "switch_on_failed") {
		t.Fatal("应记失败的事件")
	}
	setHooks(nil, nil)
	if code, b := putCP(t, d, true); code != 200 || !cpView(t, b).Enabled {
		t.Fatalf("去掉注入后应能打开：%d %s", code, b)
	}
}

// b. 端口真的被占着时打开：监听这一步失败，回到关，分片撤掉。
func TestClaudeProxyTurnOnPortBusy(t *testing.T) {
	occ, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occ.Close()
	e := newCPSEnv(t)
	e.addr = occ.Addr().String()
	d := e.start()
	if code, b := putCP(t, d, true); code != 500 || !strings.Contains(string(b), "占") {
		t.Fatalf("端口被占着时打开应失败：%d %s", code, b)
	}
	e.assertOff(t, d, "端口被占着")
}

// c. 关掉：没有授权时被拒；成功时分片删掉、端口不再监听、开关记下；分片被改过时不删、如实报告，开关记为关，状态提示留下的文件。
func TestClaudeProxyTurnOff(t *testing.T) {
	e := newCPSEnv(t)
	d := e.start()
	if code, b := putCP(t, d, true); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	port := status(t, d).ClaudeCode.Port
	e.c.as(consoleTestUID)
	if code, b := putCP(t, d, false); code != http.StatusForbidden || codeOf(t, b).Code != "needs_authorization" {
		t.Fatalf("关掉也要授权：%d %s", code, b)
	}
	if !d.cpOn.Load() || d.curCP() == nil {
		t.Fatal("被拒时应仍是开")
	}
	if _, err := os.Lstat(e.fragment()); err != nil {
		t.Fatal("被拒时分片应还在")
	}
	e.c.as(os.Getuid())
	code, b := putCP(t, d, false)
	if v := cpView(t, b); code != 200 || v.Enabled || v.Leftover || len(v.Notes) != 0 {
		t.Fatalf("关掉：%d %s", code, b)
	}
	e.assertOff(t, d, "关掉")
	if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
		c.Close()
		t.Fatal("关掉后端口不应再监听")
	}
	if _, err := os.Stat(e.cs.Dir); !os.IsNotExist(err) {
		t.Fatal("我们建的空目录应一并删掉")
	}
	// 被改过
	if code, b := putCP(t, d, true); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	_ = os.WriteFile(e.fragment(), []byte(`{"env":{}}`+"\n"), 0o644)
	code, b = putCP(t, d, false)
	v := cpView(t, b)
	if code != 200 || v.Enabled || !v.Leftover || !strings.Contains(strings.Join(v.Notes, ""), "claude-abroad.json") {
		t.Fatalf("被改过时应如实报告：%d %s", code, b)
	}
	if got, _ := os.ReadFile(e.fragment()); string(got) != `{"env":{}}`+"\n" {
		t.Fatal("被改过的分片不应删")
	}
	if on, _ := readClaudeSwitch(d.switchPath()); on || d.cpOn.Load() || d.curCP() != nil {
		t.Fatal("开关应记为关、端口不在听")
	}
	if st := status(t, d).ClaudeCode; st.Enabled || !st.Leftover {
		t.Fatalf("状态应提示留下的文件：%+v", st)
	}
}

// d. 开关文件不在、损坏、权限过宽、不是普通文件：按关处理，不写托管设置，不监听，有事件记录。
func TestClaudeProxySwitchFileInvalid(t *testing.T) {
	for _, c := range []struct {
		name, kind string
		prep       func(p string)
	}{
		{"不在", "switch_off", func(string) {}},
		{"损坏", "switch_invalid", func(p string) { _ = os.WriteFile(p, []byte(`{"version":1,"on":tr`), 0o600) }},
		{"权限过宽", "switch_invalid", func(p string) {
			_ = os.WriteFile(p, []byte(`{"version":1,"on":true}`), 0o600)
			_ = os.Chmod(p, 0o644)
		}},
		{"符号链接", "switch_invalid", func(p string) {
			real := p + ".real"
			_ = os.WriteFile(real, []byte(`{"version":1,"on":true}`), 0o600)
			_ = os.Symlink(real, p)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newCPSEnv(t)
			_ = os.MkdirAll(e.dir, 0o700)
			c.prep(filepath.Join(e.dir, ClaudeProxySwitchFile))
			d := e.start()
			if d.cpOn.Load() || d.curCP() != nil {
				t.Fatal("应按关处理")
			}
			if _, err := os.Stat(e.cs.Dir); !os.IsNotExist(err) {
				t.Fatal("不应写托管设置")
			}
			if st := status(t, d).ClaudeCode; st == nil || st.Enabled || st.Listening {
				t.Fatalf("%+v", st)
			}
			if !hasEvent(d, "claude_proxy", c.kind) {
				t.Fatalf("应记 %s 事件", c.kind)
			}
		})
	}
}

// f. 卸载：开着和关着（含上一版首次准备写下的分片）两种起点都清理干净；卸载后分片目录里没有我们的文件，重启也不再监听。
func TestClaudeProxyUninstall(t *testing.T) {
	t.Run("开着", func(t *testing.T) {
		e := newCPSEnv(t)
		d := e.start()
		if code, b := putCP(t, d, true); code != 200 {
			t.Fatalf("%d %s", code, b)
		}
		if code, raw := uninstallWith(t, d); code != 200 {
			t.Fatalf("%d %s", code, raw)
		}
		e.assertOff(t, d, "卸载后")
		if err := d.Shutdown(); err != nil {
			t.Fatal(err)
		}
		d = e.start()
		if d.curCP() != nil || d.cpOn.Load() {
			t.Fatal("卸载后重启不应监听")
		}
	})
	t.Run("关着", func(t *testing.T) {
		e := newCPSEnv(t)
		if _, err := e.cs.Write(); err != nil { // 上一版首次准备写下的
			t.Fatal(err)
		}
		d := e.start()
		if st := status(t, d).ClaudeCode; st.Enabled || !st.Leftover {
			t.Fatalf("关着、上一版的分片还在：应提示：%+v", st)
		}
		if code, raw := uninstallWith(t, d); code != 200 {
			t.Fatalf("%d %s", code, raw)
		}
		e.assertOff(t, d, "卸载后")
		if ents, _ := os.ReadDir(e.cs.Dir); len(ents) != 0 {
			t.Fatalf("分片目录里不应还有文件：%v", ents)
		}
	})
}

// g. 测试模式（只管测试目标）：打开只监听，分片目录没有任何写入（别人的冲突也不挡）；状态和第四层都标“测试模式”。
func TestClaudeProxyTestMode(t *testing.T) {
	e := newCPSEnv(t)
	e.testOnly = true
	e.lock.Domains = []string{resolverlock.TestDomain}
	d := e.start()
	code, b := putCP(t, d, true)
	v := cpView(t, b)
	if code != 200 || !v.Enabled || !v.TestMode || v.Status == nil || !v.Status.Listening || v.Status.Settings != nil || v.Status.Effective {
		t.Fatalf("测试模式打开：%d %s", code, b)
	}
	if _, err := os.Stat(e.cs.Dir); !os.IsNotExist(err) {
		t.Fatal("测试模式不应在分片目录里写任何东西")
	}
	if _, err := os.Lstat(filepath.Join(e.dir, claudecfg.RecordFile)); err == nil {
		t.Fatal("测试模式不应写托管设置的记录")
	}
	l, cov := layer4(t, d)
	if l.Note != ClaudeProxyTestNote || strings.Contains(cov, "第四层（本机代理）兜着") {
		t.Fatalf("应标测试模式，覆盖范围不算第四层（本机真实的 Claude Code 不受它管）：%+v\n%s", l, cov)
	}
	if code, b := putCP(t, d, false); code != 200 || cpView(t, b).Enabled {
		t.Fatalf("%d %s", code, b)
	}
	if d.curCP() != nil {
		t.Fatal("关掉后不应监听")
	}
	if _, err := os.Stat(e.cs.Dir); !os.IsNotExist(err) {
		t.Fatal("关掉也不应碰分片目录")
	}
}

// j. 重启 abroadd 后开关保持；开着时重启，端口重新监听，分片不重写。
func TestClaudeProxyPersistsAcrossRestart(t *testing.T) {
	e := newCPSEnv(t)
	d := e.start()
	if code, b := putCP(t, d, true); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	fi1, _ := os.Stat(e.fragment())
	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	d = e.start()
	st := status(t, d).ClaudeCode
	if !st.Enabled || !st.Listening || !st.Effective {
		t.Fatalf("重启后应仍打开、端口在听：%+v", st)
	}
	if fi2, err := os.Stat(e.fragment()); err != nil || !fi2.ModTime().Equal(fi1.ModTime()) {
		t.Fatal("重启后分片不应重写")
	}
	if code, b := putCP(t, d, false); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	d = e.start()
	if st := status(t, d).ClaudeCode; st.Enabled || st.Listening || d.curCP() != nil {
		t.Fatalf("关掉后重启应仍是关：%+v", st)
	}
}

// 没打开启动管控：没有开关——查看回“未启用”，切换回 501（这次安装没有打开启动管控），状态里没有第四层。
func TestClaudeProxyUnavailable(t *testing.T) {
	e := newEnv(t)
	d := e.start(e.opts())
	code, b := call(t, d, "GET", "/v1/settings/claude-proxy", nil)
	if v := cpView(t, b); code != 200 || v.Available || v.Enabled || v.Note != LaunchControlOffNote {
		t.Fatalf("%d %s", code, b)
	}
	code, b = call(t, d, "PUT", "/v1/settings/claude-proxy", []byte(`{"enabled":true}`))
	if code != http.StatusNotImplemented || !strings.Contains(string(b), "没有打开启动管控") {
		t.Fatalf("%d %s", code, b)
	}
	if status(t, d).ClaudeCode != nil || d.curCP() != nil {
		t.Fatal("没有开关的安装不应报告第四层、不应监听")
	}
	if code, b := call(t, d, "PUT", "/v1/settings/claude-proxy", []byte(`{"enabled":1}`)); code != 400 {
		t.Fatalf("请求体不合法应 400：%d %s", code, b)
	}
}

// 打开第四道的后果：命令行打印的（daemon.ClaudeProxyConsequences）和应用文字表里的是同一份内容，至少说清三件事。
func TestClaudeProxyConsequencesInAppStrings(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "app", "Sources", "AbroadKit", "Strings.swift"))
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(ClaudeProxyConsequences, "")
	for _, must := range []string{"所有 Claude Code", "本机代理", "已启用", "别的代理线路", "关掉开关", "卸载"} {
		if !strings.Contains(all, must) {
			t.Fatalf("后果里应有“%s”", must)
		}
	}
	for _, c := range ClaudeProxyConsequences {
		if !strings.Contains(string(b), `"`+c+`"`) {
			t.Fatalf("应用的文字表里没有这一句：%s", c)
		}
	}
}

// 卸载之后：打开被拒（uninstalled），什么都不写；别人的同名文件（不是我们写的）不覆盖，说明里写清。
func TestClaudeProxyAfterUninstallAndForeignSameName(t *testing.T) {
	e := newCPSEnv(t)
	_ = os.MkdirAll(e.cs.Dir, 0o755)
	_ = os.WriteFile(e.fragment(), []byte(`{"env":{"FOO":"1"}}`+"\n"), 0o644) // 同名、不是我们写的
	d := e.start()
	code, b := putCP(t, d, true)
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(b), "同名文件不是我们写的") {
		t.Fatalf("同名的别人的文件应拒绝、不覆盖：%d %s", code, b)
	}
	if got, _ := os.ReadFile(e.fragment()); string(got) != `{"env":{"FOO":"1"}}`+"\n" {
		t.Fatal("别人的同名文件被改了")
	}
	if d.cpOn.Load() || d.curCP() != nil {
		t.Fatal("应仍是关")
	}
	_ = os.Remove(e.fragment())
	if code, raw := uninstallWith(t, d); code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	code, b = putCP(t, d, true)
	if code != http.StatusConflict || codeOf(t, b).Code != "uninstalled" {
		t.Fatalf("卸载之后打开应被拒：%d %s", code, b)
	}
	if _, err := os.Lstat(e.fragment()); err == nil || d.curCP() != nil {
		t.Fatal("卸载之后不应写分片、不应监听")
	}
}
