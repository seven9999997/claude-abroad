package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"claude-abroad/internal/claudecfg"
	"claude-abroad/internal/pf"
	"claude-abroad/internal/resolverlock"
)

type fakeUninstaller struct {
	calls atomic.Int32
	fail  atomic.Bool
}

func (u *fakeUninstaller) RemoveInstall(context.Context) error {
	u.calls.Add(1)
	if u.fail.Load() {
		return errors.New("注入的安装器失败")
	}
	return nil
}

// uninstallWith 两步卸载：返回第二步的状态码和应答。
func uninstallWith(t *testing.T, d *Daemon) (int, []byte) {
	t.Helper()
	code, raw := call(t, d, "POST", "/v1/actions/uninstall", nil)
	var cn struct {
		Confirm string          `json:"confirm"`
		Preview json.RawMessage `json:"preview"`
	}
	if code != 409 || json.Unmarshal(raw, &cn) != nil || cn.Confirm == "" {
		t.Fatalf("第一步：%d %s", code, raw)
	}
	b, _ := json.Marshal(ConfirmRequest{Confirm: cn.Confirm})
	return call(t, d, "POST", "/v1/actions/uninstall", b)
}

type uninstallEnv struct {
	*env
	lock *resolverlock.Lock
	cs   *claudecfg.Settings
	un   *fakeUninstaller
	d    *Daemon
}

// 安装：写入解析锁和托管设置（临时目录）。
func newUninstallEnv(t *testing.T, withUninstaller bool) *uninstallEnv {
	t.Helper()
	e := &uninstallEnv{env: newEnv(t)}
	base := filepath.Dir(e.dir)
	_ = os.MkdirAll(e.dir, 0o700)
	e.lock = &resolverlock.Lock{Dir: filepath.Join(base, "resolver"), RecordDir: e.dir}
	e.cs = &claudecfg.Settings{Dir: filepath.Join(base, "ClaudeCode", "managed-settings.d"), RecordDir: e.dir}
	_ = os.MkdirAll(filepath.Dir(e.cs.Dir), 0o755)
	if _, err := e.lock.Write(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cs.Write(); err != nil {
		t.Fatal(err)
	}
	o := e.opts()
	o.Sys.ResolverLock, o.Sys.ClaudeSettings = e.lock, e.cs
	if withUninstaller {
		e.un = &fakeUninstaller{}
		o.Sys.Uninstaller = e.un
	}
	e.d = e.start(o)
	return e
}

// 安装前快照 → 安装 → 启用 → 卸载 → 对照快照；安装器没接时如实报告最后一步要由安装器完成。
func TestUninstallRestoresEverything(t *testing.T) {
	e := newUninstallEnv(t, false)
	post(t, e.d, "/v1/enable", 200)
	code, raw := uninstallWith(t, e.d)
	var res UninstallResult
	if code != 200 || json.Unmarshal(raw, &res) != nil {
		t.Fatalf("%d %s", code, raw)
	}
	if len(res.Steps) != 3 || !res.Steps[0].Done || !res.Steps[1].Done || res.Steps[2].Done || res.Complete || res.Steps[2].Note == "" {
		t.Fatalf("步骤 %+v", res)
	}
	if !e.sys.Net.Snapshot().Equal(e.init) || e.sys.PF.Active(pf.DefaultAnchor) {
		t.Fatal("网络和 pf 应恢复到安装前")
	}
	if _, err := os.Stat(e.lock.Dir); !os.IsNotExist(err) {
		t.Fatal("解析锁应删掉（目录是我们建的，一并删）")
	}
	if st := e.cs.Check(); st.Written {
		t.Fatal("托管设置应删掉")
	}
	if !e.d.uninstalled() {
		t.Fatal("应记下已卸载")
	}
	if p := e.d.guardPolicy(); !p.Allowed {
		t.Fatal("卸载后启动管控应解除")
	}
	// 再卸载一次：可重入
	if code, _ := uninstallWith(t, e.d); code != 200 {
		t.Fatalf("重复卸载：%d", code)
	}
	e.noViolations()
}

// 失败即停：结束 Claude 失败时什么都不删、网络不变；去掉失败后重试能完成。安装器那一步失败时停下，重试能完成。
func TestUninstallStopsOnFailure(t *testing.T) {
	e := newUninstallEnv(t, true)
	post(t, e.d, "/v1/enable", 200)
	before := e.sys.Net.Snapshot()
	e.sys.Kill.SetFail(ErrFakeKill)
	code, raw := uninstallWith(t, e.d)
	if code != 500 {
		t.Fatalf("结束 Claude 失败应 500：%d %s", code, raw)
	}
	if !e.sys.Net.Snapshot().Equal(before) || !e.lock.Check().Written || e.d.uninstalled() || e.un.calls.Load() != 0 {
		t.Fatal("失败时不应有任何改动")
	}
	e.sys.Kill.SetFail(nil)
	e.un.fail.Store(true)
	code, raw = uninstallWith(t, e.d)
	if code != 500 || e.un.calls.Load() != 1 {
		t.Fatalf("安装器失败应停下：%d %s", code, raw)
	}
	e.un.fail.Store(false)
	code, raw = uninstallWith(t, e.d)
	var res UninstallResult
	if code != 200 || json.Unmarshal(raw, &res) != nil || !res.Complete {
		t.Fatalf("重试应完成：%d %s", code, raw)
	}
}

// 没有确认码、确认码不对：什么都不做。
func TestUninstallNeedsConfirm(t *testing.T) {
	e := newUninstallEnv(t, false)
	b, _ := json.Marshal(ConfirmRequest{Confirm: "00000000000000000000000000000000"})
	if code, _ := call(t, e.d, "POST", "/v1/actions/uninstall", b); code != 403 {
		t.Fatalf("确认码不对应 403：%d", code)
	}
	if !e.lock.Check().Written || e.d.uninstalled() {
		t.Fatal("不应有改动")
	}
}
