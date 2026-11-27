package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-abroad/internal/api"
	"claude-abroad/internal/claudecfg"
	"claude-abroad/internal/pf"
	"claude-abroad/internal/resolverlock"
	"claude-abroad/internal/state"
)

// M6-5a 补审返工的测试（必须改 1、2 和“测试覆盖不够”一节）。

func uninstallResult(t *testing.T, raw []byte) UninstallResult {
	t.Helper()
	var r UninstallResult
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("%v %s", err, raw)
	}
	return r
}

// 卸载成功之后：网络、IPv6、地区等于安装前，pf 锚点空且引用全部释放，解析锁目录和托管设置目录都删掉了（是我们建的）。
func (e *uninstallEnv) assertClean(t *testing.T) {
	t.Helper()
	if !e.sys.Net.Snapshot().Equal(e.init) {
		t.Fatalf("网络、IPv6、地区应等于安装前：%+v", e.sys.Net.Snapshot())
	}
	if e.sys.PF.Active(pf.DefaultAnchor) || len(e.sys.PF.Tokens()) != 0 {
		t.Fatalf("pf 锚点应为空、启用引用全部释放：%v", e.sys.PF.Tokens())
	}
	if _, err := os.Stat(e.lock.Dir); !os.IsNotExist(err) {
		t.Fatal("解析锁目录应删掉")
	}
	if _, err := os.Stat(e.cs.Dir); !os.IsNotExist(err) {
		t.Fatal("托管设置目录应删掉")
	}
	if !e.d.uninstalled() {
		t.Fatal("应记下已卸载")
	}
}

// 必须改 1：卸载完成后，除回国、再次卸载、设备体检外，所有改动类接口一律拒绝（代码 uninstalled），状态不变；只读接口照常。
func TestUninstalledRejectsChanges(t *testing.T) {
	e := newUninstallEnv(t, true)
	post(t, e.d, "/v1/enable", 200)
	if code, raw := uninstallWith(t, e.d); code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	before := e.sys.Net.Snapshot()
	stBefore, _ := os.ReadFile(filepath.Join(e.dir, state.FileName))
	n := 0
	for _, rt := range RouteTable() {
		if rt.Method == http.MethodGet {
			continue
		}
		path := strings.NewReplacer("{tag}", "x", "{id}", "x").Replace(rt.Path)
		code, raw := call(t, e.d, rt.Method, path, []byte("{}"))
		if AllowedAfterUninstall[rt.ID] {
			if code == http.StatusConflict && strings.Contains(string(raw), string(api.CodeUninstalled)) {
				t.Errorf("%s 卸载后应仍然允许：%d %s", rt.ID, code, raw)
			}
			continue
		}
		n++
		if code != http.StatusConflict || !strings.Contains(string(raw), `"code":"uninstalled"`) || !strings.Contains(string(raw), "重新安装") {
			t.Errorf("%s 卸载后应拒绝（uninstalled）：%d %s", rt.ID, code, raw)
		}
	}
	if n < 15 {
		t.Fatalf("只检查了 %d 个改动类接口，路由表可能读错了", n)
	}
	if !e.sys.Net.Snapshot().Equal(before) {
		t.Fatal("被拒的请求不应改动网络")
	}
	if b, _ := os.ReadFile(filepath.Join(e.dir, state.FileName)); string(b) != string(stBefore) {
		t.Fatal("被拒的请求不应改动状态文件")
	}
	// 只读接口照常
	if code, _ := call(t, e.d, "GET", "/v1/status", nil); code != 200 {
		t.Fatal("状态接口应照常")
	}
	// 再卸载一次：成功，结果相同
	code, raw := uninstallWith(t, e.d)
	if code != 200 || !uninstallResult(t, raw).Complete {
		t.Fatalf("再卸载：%d %s", code, raw)
	}
	e.assertClean(t)
}

// 安装器那一步可以卡住（测卸载进行中）。
type blockingUninstaller struct {
	entered chan struct{}
	release chan struct{}
}

func (u *blockingUninstaller) RemoveInstall(ctx context.Context) error {
	close(u.entered)
	<-u.release
	return nil
}

// 必须改 1：卸载进行中（卡在安装器那一步，已经过了回国和解除管控），别的改动请求得到“忙”；第二个卸载也是“忙”。
func TestUninstallBusyWhileRunning(t *testing.T) {
	e := &uninstallEnv{env: newEnv(t)}
	base := filepath.Dir(e.dir)
	_ = os.MkdirAll(e.dir, 0o700)
	e.lock = &resolverlock.Lock{Dir: filepath.Join(base, "resolver"), RecordDir: e.dir}
	e.cs = &claudecfg.Settings{Dir: filepath.Join(base, "ClaudeCode", "managed-settings.d"), RecordDir: e.dir}
	_ = os.MkdirAll(filepath.Dir(e.cs.Dir), 0o755)
	_, _ = e.lock.Write()
	_, _ = e.cs.Write()
	u := &blockingUninstaller{entered: make(chan struct{}), release: make(chan struct{})}
	o := e.opts()
	o.Sys.ResolverLock, o.Sys.ClaudeSettings, o.Sys.Uninstaller = e.lock, e.cs, u
	e.d = e.start(o)
	// 第一步拿确认码
	_, raw := call(t, e.d, "POST", "/v1/actions/uninstall", nil)
	var cn ConfirmNeeded
	_ = json.Unmarshal(raw, &cn)
	b, _ := json.Marshal(ConfirmRequest{Confirm: cn.Confirm})
	done := make(chan int, 1)
	go func() { c, _ := call(t, e.d, "POST", "/v1/actions/uninstall", b); done <- c }()
	select {
	case <-u.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("卸载没走到安装器那一步")
	}
	for _, p := range []string{"/v1/enable", "/v1/disable", "/v1/actions/uninstall"} {
		code, raw := call(t, e.d, "POST", p, nil)
		if code != http.StatusConflict || !strings.Contains(string(raw), `"code":"busy"`) {
			t.Errorf("卸载进行中 %s 应“忙”：%d %s", p, code, raw)
		}
	}
	if code, _ := call(t, e.d, "GET", "/v1/status", nil); code != 200 {
		t.Fatal("卸载进行中查询照常")
	}
	close(u.release)
	if c := <-done; c != 200 {
		t.Fatalf("卸载应完成：%d", c)
	}
}

// 必须改 2：回国和卸载的确认码绑定“同时强制退出浏览器”：第一步不带、第二步带（或反过来）都是“确认码无效”，什么都不做；一致时通过。
func TestConfirmBindsKillBrowsers(t *testing.T) {
	type step struct{ first, second bool }
	for _, path := range []string{"/v1/restore", "/v1/actions/uninstall"} {
		e := newUninstallEnv(t, true)
		post(t, e.d, "/v1/enable", 200)
		for _, s := range []step{{false, true}, {true, false}} {
			var first []byte
			if s.first {
				first = []byte(`{"kill_browsers":true}`)
			}
			code, raw := call(t, e.d, "POST", path, first)
			var cn ConfirmNeeded
			if code != 409 || json.Unmarshal(raw, &cn) != nil || cn.Confirm == "" {
				t.Fatalf("%s 第一步：%d %s", path, code, raw)
			}
			b, _ := json.Marshal(ConfirmRequest{Confirm: cn.Confirm, KillBrowsers: s.second})
			code, raw = call(t, e.d, "POST", path, b)
			if code != 403 || !strings.Contains(string(raw), `"code":"confirm_invalid"`) {
				t.Fatalf("%s 第一步 %v、第二步 %v 应确认码无效：%d %s", path, s.first, s.second, code, raw)
			}
			if f, _ := e.view.Load(); f.State != state.Enabled || e.d.uninstalled() {
				t.Fatalf("%s：确认码无效时不应有改动", path)
			}
		}
		// 一致：通过
		code, raw := call(t, e.d, "POST", path, []byte(`{"kill_browsers":true}`))
		var cn ConfirmNeeded
		_ = json.Unmarshal(raw, &cn)
		b, _ := json.Marshal(ConfirmRequest{Confirm: cn.Confirm, KillBrowsers: true})
		if code, raw = call(t, e.d, "POST", path, b); code != 200 {
			t.Fatalf("%s 两步一致应通过：%d %s", path, code, raw)
		}
	}
}

// 测试覆盖：五个步骤逐个注入失败——停在那一步、应答里的步骤清单如实；去掉故障重试走完，结果和一次成功的相同。
func TestUninstallEachStepFails(t *testing.T) {
	cases := []struct {
		name     string
		inject   func(e *uninstallEnv) func() // 返回去掉故障的函数
		failStep int                          // 失败的那一步（0 起）
	}{
		{"回国（结束 Claude）失败", func(e *uninstallEnv) func() {
			e.sys.Kill.SetFail(ErrFakeKill)
			return func() { e.sys.Kill.SetFail(nil) }
		}, 0},
		{"删解析锁失败", func(e *uninstallEnv) func() { return blockFile(t, filepath.Join(e.dir, resolverlock.RecordFile)) }, 1},
		{"删托管设置失败", func(e *uninstallEnv) func() { return blockFile(t, filepath.Join(e.dir, claudecfg.RecordFile)) }, 1},
		{"写已卸载标记失败", func(e *uninstallEnv) func() {
			p := filepath.Join(e.dir, UninstalledFile)
			_ = os.MkdirAll(filepath.Join(p, "x"), 0o700) // 同名的非空目录：原子改名写不进去
			return func() { _ = os.RemoveAll(p) }
		}, 1},
		{"安装器那一步失败", func(e *uninstallEnv) func() {
			e.un.fail.Store(true)
			return func() { e.un.fail.Store(false) }
		}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newUninstallEnv(t, true)
			post(t, e.d, "/v1/enable", 200)
			fix := c.inject(e)
			code, raw := uninstallWith(t, e.d)
			r := uninstallResult(t, raw)
			if code != 500 || len(r.Steps) != c.failStep+1 || r.Steps[c.failStep].Done {
				t.Fatalf("应停在第 %d 步：%d %s", c.failStep+1, code, raw)
			}
			for i := 0; i < c.failStep; i++ {
				if !r.Steps[i].Done {
					t.Fatalf("前面的步骤应已完成：%s", raw)
				}
			}
			if c.failStep < 1 && (e.d.uninstalled() || !e.lock.Check().Written) {
				t.Fatal("回国失败时不应解除管控")
			}
			fix()
			code, raw = uninstallWith(t, e.d)
			if code != 200 || !uninstallResult(t, raw).Complete {
				t.Fatalf("去掉故障后重试应走完：%d %s", code, raw)
			}
			e.assertClean(t)
		})
	}
}

// blockFile 把记录文件换成同名目录（读记录失败）；返回恢复原文件的函数。
func blockFile(t *testing.T, p string) func() {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(p)
	_ = os.MkdirAll(p, 0o700)
	return func() {
		_ = os.RemoveAll(p)
		_ = os.WriteFile(p, b, 0o600)
	}
}

// 测试覆盖：从已启用、已停用、已回国三种状态卸载，都和安装前相同；别人写在解析锁目录里的文件留下并报告。
func TestUninstallFromEachState(t *testing.T) {
	for _, from := range []string{"ENABLED", "DISABLED", "RESTORED"} {
		t.Run(from, func(t *testing.T) {
			e := newUninstallEnv(t, true)
			switch from {
			case "ENABLED":
				post(t, e.d, "/v1/enable", 200)
			case "DISABLED":
				post(t, e.d, "/v1/enable", 200)
				post(t, e.d, "/v1/disable", 200)
			}
			code, raw := uninstallWith(t, e.d)
			if code != 200 || !uninstallResult(t, raw).Complete {
				t.Fatalf("%d %s", code, raw)
			}
			e.assertClean(t)
		})
	}
	// 别人的文件：留下，步骤说明里写出来，目录不删
	e := newUninstallEnv(t, true)
	other := filepath.Join(e.lock.Dir, "corp.example")
	_ = os.WriteFile(other, []byte("nameserver 192.0.2.53\n"), 0o644)
	_ = os.WriteFile(filepath.Join(e.lock.Dir, "claude.com"), []byte("nameserver 1.1.1.1\n"), 0o644) // 我们写的被人改了
	code, raw := uninstallWith(t, e.d)
	if code != 200 || !strings.Contains(uninstallResult(t, raw).Steps[1].Note, "claude.com") {
		t.Fatalf("被改过的锁文件应在说明里：%d %s", code, raw)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("别人的文件不应删")
	}
}

// 测试覆盖：没有打开启动管控（不带 --launch-control）时卸载：不报错，第 2 步如实写“没有打开，不需要解除”。
func TestUninstallWithoutLaunchControl(t *testing.T) {
	e := newEnv(t)
	d := e.start(e.opts())
	post(t, d, "/v1/enable", 200)
	code, raw := uninstallWith(t, d)
	r := uninstallResult(t, raw)
	if code != 200 || len(r.Steps) != 3 || !r.Steps[1].Done || !strings.Contains(r.Steps[1].Note, "没有打开启动管控") {
		t.Fatalf("%d %s", code, raw)
	}
	if !e.sys.Net.Snapshot().Equal(e.init) {
		t.Fatal("网络应恢复")
	}
}

// 建议改：卸载完成后本机代理端口也停掉。
func TestUninstallStopsClaudeProxy(t *testing.T) {
	e := &uninstallEnv{env: newEnv(t), un: &fakeUninstaller{}}
	base := filepath.Dir(e.dir)
	_ = os.MkdirAll(e.dir, 0o700)
	e.lock = &resolverlock.Lock{Dir: filepath.Join(base, "resolver"), RecordDir: e.dir}
	e.cs = &claudecfg.Settings{Dir: filepath.Join(base, "ClaudeCode", "managed-settings.d"), RecordDir: e.dir}
	_ = os.MkdirAll(filepath.Dir(e.cs.Dir), 0o755)
	_, _ = e.lock.Write()
	_, _ = e.cs.Write()
	o := e.opts()
	o.Sys.ResolverLock, o.Sys.ClaudeSettings, o.Sys.Uninstaller = e.lock, e.cs, e.un
	o.ClaudeProxyAddr = "127.0.0.1:0"
	presetClaudeProxyOn(t, o.StateDir) // 第四道打开着（v0.68：默认关）
	e.d = e.start(o)
	if st := status(t, e.d); st.ClaudeCode == nil || !st.ClaudeCode.Listening {
		t.Fatalf("卸载前端口应在听：%+v", st.ClaudeCode)
	}
	if code, raw := uninstallWith(t, e.d); code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	if st := status(t, e.d); st.ClaudeCode.Listening {
		t.Fatal("卸载后端口应停掉")
	}
}

// M6-5a 补审返工（剩下的 1 处）：入口检查和拿到状态机互斥之间开始了卸载——状态机拿到互斥后再判一次，启用被拒。
// 模拟“已经过了入口”：直接调用状态机（不经路由）。卸载进行中、卸载完成后各一次。
func TestUninstallWindowClosedInStateMachine(t *testing.T) {
	e := newUninstallEnv(t, true)
	// 卸载进行中
	e.d.uninstalling.Store(true)
	if err := e.d.m.Enable(context.Background()); err == nil || !strings.Contains(err.Error(), "卸载") {
		t.Fatalf("卸载进行中状态机应拒绝启用：%v", err)
	}
	e.d.uninstalling.Store(false)
	if code, raw := uninstallWith(t, e.d); code != 200 {
		t.Fatalf("%d %s", code, raw)
	}
	// 卸载完成后：启用、停用、互斥执行都拒绝；回国仍可以
	for name, op := range map[string]func() error{
		"启用":   func() error { return e.d.m.Enable(context.Background()) },
		"停用":   func() error { return e.d.m.Disable(context.Background(), false) },
		"互斥执行": func() error { return e.d.m.Exclusive(func() error { return nil }) },
	} {
		if err := op(); err == nil {
			t.Fatalf("卸载后状态机应拒绝%s", name)
		}
	}
	if err := e.d.m.Restore(context.Background()); err != nil {
		t.Fatalf("卸载后回国仍应可以：%v", err)
	}
	if f, _ := e.view.Load(); f.State != state.Restored || !e.d.uninstalled() {
		t.Fatalf("状态应是已回国加已卸载：%s", f.State)
	}
	e.assertClean(t)
}
