package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"claude-abroad/internal/guard"
	"claude-abroad/internal/pf"
)

// M6-2b：abroadd 给开机时的 guard 另存参数、写本次开机的标记；guard 取得的 pf 启用引用被收养，回国时一起释放（回国干净）。
func TestGuardFilesAndTokenAdoption(t *testing.T) {
	e := newEnv(t)
	run := filepath.Join(filepath.Dir(e.dir), "run")
	if err := os.Mkdir(run, 0o700); err != nil {
		t.Fatal(err)
	}
	// 开机时 guard 已经加载过、取了一个启用引用（模拟：经同一个假 pf 启用）
	ctl := &pf.Controller{R: e.sys.PF}
	tok, err := ctl.Enable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := guard.AppendToken(run, tok); err != nil {
		t.Fatal(err)
	}
	o := e.opts()
	o.RunDir = run
	o.BootID = func() (string, error) { return "boot-test", nil }
	d := e.start(o)
	post(t, d, "/v1/enable", 200)
	// 参数另存了，和 guard 读回来的一致；不带 utun
	cfg, err := guard.ReadParams(e.dir)
	if err != nil {
		t.Fatalf("参数文件：%v", err)
	}
	if cfg.UtunName != "" || cfg.User == "" {
		t.Fatalf("参数 %+v", cfg)
	}
	b, err := os.ReadFile(filepath.Join(run, guard.MarkerFile))
	if err != nil || string(b) != "boot-test\n" {
		t.Fatalf("标记 %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(run, guard.TokenFile)); !os.IsNotExist(err) {
		t.Fatal("guard 的启用引用应已被收养（文件删掉）")
	}
	f, _ := e.view.Load()
	found := false
	for _, x := range f.PFTokens {
		found = found || x == tok
	}
	if !found {
		t.Fatalf("guard 的启用引用应在状态文件里：%v", f.PFTokens)
	}
	// 回国：全部释放，pf 不再被我们启用
	post(t, d, "/v1/restore", 200)
	if toks := e.sys.PF.Tokens(); len(toks) != 0 {
		t.Fatalf("回国后应释放全部启用引用（含 guard 的）：%v", toks)
	}
}

// 回国释放之前才出现的 guard 启用引用（启动之后 guard 才跑完）：回国时也收养并释放。
func TestGuardTokenAdoptedAtRestore(t *testing.T) {
	e := newEnv(t)
	run := filepath.Join(filepath.Dir(e.dir), "run")
	_ = os.Mkdir(run, 0o700)
	o := e.opts()
	o.RunDir = run
	o.BootID = func() (string, error) { return "boot-test", nil }
	d := e.start(o)
	post(t, d, "/v1/enable", 200)
	tok, _ := (&pf.Controller{R: e.sys.PF}).Enable(context.Background())
	_ = guard.AppendToken(run, tok)
	post(t, d, "/v1/restore", 200)
	if toks := e.sys.PF.Tokens(); len(toks) != 0 {
		t.Fatalf("回国后应释放全部启用引用：%v", toks)
	}
}

// 没给 RunDir（开发工具、测试）：不写任何 guard 的文件。
func TestNoRunDirNoGuardFiles(t *testing.T) {
	e := newEnv(t)
	d := e.start(e.opts())
	post(t, d, "/v1/enable", 200)
	if _, err := os.Stat(filepath.Join(e.dir, guard.ParamsFile)); !os.IsNotExist(err) {
		t.Fatal("没给 RunDir 时不应另存参数")
	}
}
