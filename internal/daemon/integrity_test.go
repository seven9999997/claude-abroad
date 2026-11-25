package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claude-abroad/internal/integrity"
	"claude-abroad/internal/pf"
	"claude-abroad/internal/preflight"
	"claude-abroad/internal/state"
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

// M6-7：完整性校验不通过：状态里报告、启用被挡住；已启用的重启后转成停用（断网规则还在、不接管）。
func TestIntegrityFails(t *testing.T) {
	e := newEnv(t)
	post(t, e.start(e.opts()), "/v1/enable", 200)
	if err := e.d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	o := e.opts()
	o.Sys.Integrity = badChecker(t)
	chk := &preflight.Checker{R: cleanRunner{t}}
	o.Sys.Preflight = PreflightHints{C: chk}
	d := e.start(o)
	st := status(t, d)
	if st.Integrity == nil || st.Integrity.OK || st.Integrity.Problems[0].Kind != "mismatch" {
		t.Fatalf("状态应报告：%+v", st.Integrity)
	}
	if st.State != string(state.Disabled) || !e.sys.PF.Active(pf.DefaultAnchor) {
		t.Fatalf("已启用的应转成停用、断网规则还在：%s", st.State)
	}
	code, b := callOp(t, d, "/v1/enable")
	if code != 422 || !strings.Contains(string(b), "abroad-vm 内容不对") {
		t.Fatalf("启用应被挡住：%d %s", code, b)
	}
	if !hasEvent(d, "integrity", "failed") {
		t.Fatal("应记事件")
	}
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
