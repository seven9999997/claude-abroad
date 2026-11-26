package daemon

import (
	"claude-abroad/internal/gatesm"
	"claude-abroad/internal/vmmgr"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-abroad/internal/integrity"
	"claude-abroad/internal/pf"
	"claude-abroad/internal/preflight"
)

func badChecker(t *testing.T) *integrity.Checker {
	t.Helper()
	base := t.TempDir()
	_ = os.WriteFile(filepath.Join(base, "abroad-vm"), []byte("ok"), 0o755)
	b, _ := integrity.Generate(base, []string{"abroad-vm"})
	mf := filepath.Join(base, "manifest.json")
	_ = os.WriteFile(mf, b, 0o644)
	_ = os.WriteFile(filepath.Join(base, "abroad-vm"), []byte("tampered"), 0o755) // 被换掉
	return &integrity.Checker{Manifest: mf, Base: base, Digest: integrity.Digest(b)}
}

// M6-7（补审返工）：状态文件是已启用、完整性校验不通过时启动——校验在恢复状态和拉起虚拟机之前：虚拟机一次都不拉起；
// 闸门关着且带 integrity 原因，复查也打不开；断网规则在；状态里报告；冲突检查挡住启用；回国照常能走完。
func TestIntegrityFails(t *testing.T) {
	e := newEnv(t)
	post(t, e.start(e.opts()), "/v1/enable", 200)
	if err := e.d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	l := &dmLauncher{}
	l.healthy.Store(true)
	o := e.opts()
	o.Sys.VM = &vmmgr.Manager{Launcher: l, Health: l.health, Timing: dmTiming}
	o.Sys.Integrity = badChecker(t)
	o.Sys.Preflight = PreflightHints{C: &preflight.Checker{R: cleanRunner{t}}}
	d := e.start(o)
	d.re.Kick() // 请复查跑一轮：带着 integrity 原因也不能开
	time.Sleep(300 * time.Millisecond)
	st := status(t, d)
	if st.Integrity == nil || st.Integrity.OK || st.Integrity.Problems[0].Kind != "mismatch" {
		t.Fatalf("状态应报告：%+v", st.Integrity)
	}
	if l.launches.Load() != 0 {
		t.Fatalf("校验不通过时不应拉起虚拟机：拉起了 %d 次", l.launches.Load())
	}
	if st.GateOpen || !hasReason(st, gatesm.ReasonIntegrity) || !e.sys.PF.Active(pf.DefaultAnchor) {
		t.Fatalf("闸门应关着且带 integrity 原因、断网规则在：%+v", st.Gate)
	}
	if !hasEvent(d, "integrity", "failed") {
		t.Fatal("应记事件")
	}
	if p := d.guardPolicyPeek(); !p.Allowed {
		t.Fatal("integrity 原因按口径 A 处理：不算“不允许启动”，不结束 Claude")
	}
	_, raw := call(t, d, "GET", "/v1/preflight", nil)
	if !strings.Contains(string(raw), "abroad-vm 内容不对") {
		t.Fatalf("冲突检查应列出：%s", raw)
	}
	// 回国照常能走完
	b := post(t, d, "/v1/restore", 200)
	if !strings.Contains(string(b), `"state":"RESTORED"`) || !e.sys.Net.Snapshot().Equal(e.init) {
		t.Fatalf("回国应走完：%s", b)
	}
	// 回国之后启用仍被冲突检查挡住
	if code, b := callOp(t, d, "/v1/enable"); code != 422 || !strings.Contains(string(b), "abroad-vm 内容不对") {
		t.Fatalf("启用应被挡住：%d %s", code, b)
	}
}

// 校验通过：和没接校验时一样，虚拟机照常拉起。
func TestIntegrityOKStartsVM(t *testing.T) {
	e := newEnv(t)
	l := &dmLauncher{}
	l.healthy.Store(true)
	o := e.opts()
	o.Sys.VM = &vmmgr.Manager{Launcher: l, Health: l.health, Timing: dmTiming}
	o.Sys.Integrity = goodChecker(t)
	d := e.start(o)
	post(t, d, "/v1/enable", 200)
	if st := status(t, d); st.Integrity == nil || !st.Integrity.OK || hasReason(st, gatesm.ReasonIntegrity) || l.launches.Load() == 0 {
		t.Fatalf("校验通过时行为照常：%+v 拉起 %d", st.Integrity, l.launches.Load())
	}
}

func goodChecker(t *testing.T) *integrity.Checker {
	t.Helper()
	base := t.TempDir()
	_ = os.WriteFile(filepath.Join(base, "abroad-vm"), []byte("ok"), 0o755)
	b, _ := integrity.Generate(base, []string{"abroad-vm"})
	mf := filepath.Join(base, "manifest.json")
	_ = os.WriteFile(mf, b, 0o644)
	return &integrity.Checker{Manifest: mf, Base: base, Digest: integrity.Digest(b)}
}

// 没接校验（开发构建、测试）：状态里没有这一项，启用照常。
func TestIntegrityNotConfigured(t *testing.T) {
	e := newEnv(t)
	d := e.start(e.opts())
	if status(t, d).Integrity != nil {
		t.Fatal("没接时不应报告")
	}
	post(t, d, "/v1/enable", 200)
}
