package daemon

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"claude-abroad/internal/guard"
	"claude-abroad/internal/pf"
	"claude-abroad/internal/state"
)

// M6-11a：打开了首次准备且做完了 → 回国后锚点恰好是回国拦截那一组、留一个 token，状态里报告生效和地址段；
// 卸载标记在、没打开首次准备时 → 回国行为和以前一样（锚点清空、token 全部释放）。
func TestReturnBlockSwitch(t *testing.T) {
	want, _ := pf.Render(pf.ReturnBlockConfig())
	t.Run("打开且做完首次准备", func(t *testing.T) {
		e := newPrepEnv(t)
		d := e.start(false)
		st := status(t, d)
		// 全新安装时状态就是“已回国”：启动时就加载这一组（和解析锁一样，从安装起就生效）
		if st.ReturnBlock == nil || !st.ReturnBlock.On || !st.ReturnBlock.Active || !slices.Equal(st.ReturnBlock.Nets, []string{"160.79.104.0/21", "2607:6bc0::/32"}) || st.ReturnBlock.Note == "" {
			t.Fatalf("启用前：%+v", st.ReturnBlock)
		}
		if string(e.sys.PF.Raw(pf.DefaultAnchor)) != string(want) || len(e.sys.PF.Tokens()) != 1 {
			t.Fatalf("启用前锚点：%s", e.sys.PF.Raw(pf.DefaultAnchor))
		}
		post(t, d, "/v1/enable", 200)
		restoreWith(t, d, false)
		st = status(t, d)
		if string(e.sys.PF.Raw(pf.DefaultAnchor)) != string(want) || !e.sys.PF.Active(pf.DefaultAnchor) || len(e.sys.PF.Tokens()) != 1 {
			t.Fatalf("回国后锚点：%s tokens=%v", e.sys.PF.Raw(pf.DefaultAnchor), e.sys.PF.Tokens())
		}
		if st.State != "RESTORED" || !st.ReturnBlock.Active || st.Protected {
			t.Fatalf("回国后状态：%s %+v 保护=%v", st.State, st.ReturnBlock, st.Protected)
		}
		// 重启 abroadd：照样生效
		d = e.restart(t, d, false)
		if st = status(t, d); !st.ReturnBlock.Active || string(e.sys.PF.Raw(pf.DefaultAnchor)) != string(want) {
			t.Fatalf("重启后：%+v", st.ReturnBlock)
		}
	})
	t.Run("卸载过", func(t *testing.T) {
		e := newPrepEnv(t)
		d := e.start(false)
		post(t, d, "/v1/enable", 200)
		_ = os.WriteFile(filepath.Join(e.dir, UninstalledFile), []byte("x\n"), 0o600)
		if d.returnBlockOn() {
			t.Fatal("卸载过时开关应关")
		}
		if st := status(t, d); st.ReturnBlock == nil || st.ReturnBlock.On {
			t.Fatalf("%+v", st.ReturnBlock)
		}
	})
	t.Run("没打开首次准备", func(t *testing.T) {
		e := newEnv(t)
		d := e.start(e.opts())
		post(t, d, "/v1/enable", 200)
		restoreWith(t, d, false)
		st := status(t, d)
		if st.ReturnBlock != nil || e.sys.PF.Active(pf.DefaultAnchor) || len(e.sys.PF.Tokens()) != 0 {
			t.Fatalf("应和以前一样：%+v %v", st.ReturnBlock, e.sys.PF.Tokens())
		}
	})
	t.Run("首次准备没做完", func(t *testing.T) {
		e := newPrepEnv(t)
		d := e.start(false)
		_ = os.Remove(filepath.Join(e.dir, PreparedFile))
		if d.returnBlockOn() {
			t.Fatal("没有完成标记时开关应关")
		}
	})
}

// M6-11b 验收 b：重启后 guard 加载的回国拦截和 abroadd 自己加载的逐字相同；abroadd 起来后收养 guard 的那个引用，
// 不重复加载、不重复取，状态文件里只留这一个（重启前的旧 token 已失效，丢掉）。
func TestReturnBlockGuardAdoptedAfterReboot(t *testing.T) {
	e := newPrepEnv(t)
	run := filepath.Join(filepath.Dir(e.dir), "run")
	_ = os.Mkdir(run, 0o700)
	start := func(boot string) *Daemon {
		o := e.opts()
		o.Sys.ResolverLock, o.Sys.ClaudeSettings = e.lock, e.cs
		o.Sys.FirstRun = true
		o.RunDir = run
		o.BootID = func() (string, error) { return boot, nil }
		return e.env.start(o)
	}
	d := start("boot-1")
	post(t, d, "/v1/enable", 200)
	restoreWith(t, d, false)
	abroaddRules := e.sys.PF.Raw(pf.DefaultAnchor)
	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	// 重启：pf 复位、运行时目录清空；guard 先跑
	e.sys.PF.Reset()
	_ = os.RemoveAll(run)
	_ = os.Mkdir(run, 0o700)
	out, err := guard.Run(context.Background(), guard.Deps{StateDir: e.dir, RunDir: run, PF: &pf.Controller{R: e.sys.PF},
		BootID: func() (string, error) { return "boot-2", nil }})
	if err != nil || out != guard.Loaded {
		t.Fatalf("guard：%v %v", out, err)
	}
	if !bytes.Equal(e.sys.PF.Raw(pf.DefaultAnchor), abroaddRules) {
		t.Fatalf("guard 加载的和 abroadd 的不一样：\n%s\n%s", e.sys.PF.Raw(pf.DefaultAnchor), abroaddRules)
	}
	guardTok := e.sys.PF.Tokens()
	e.sys.PF.ResetCalls()
	d = start("boot-2")
	defer d.Shutdown()
	for _, c := range e.sys.PF.Calls() {
		if c == "enable" || c == "load" || c == "flush" {
			t.Fatalf("abroadd 不应重复加载、取引用或清空：%v", e.sys.PF.Calls())
		}
	}
	f, _ := e.view.Load()
	if len(guardTok) != 1 || !slices.Equal(e.sys.PF.Tokens(), guardTok) || !slices.Equal(f.PFTokens, guardTok) {
		t.Fatalf("应只留 guard 的那一个：pf %v，文件 %v，guard %v", e.sys.PF.Tokens(), f.PFTokens, guardTok)
	}
	if st := status(t, d); !st.ReturnBlock.Active {
		t.Fatalf("应生效：%+v", st.ReturnBlock)
	}
}

// M6-11b 验收 d：从已回国（回国拦截在生效）、已启用、已停用发起卸载，最终都是锚点空、引用全部释放、pf 回到安装前
// 的启用状态（假 pf 安装前没启用）、网络和初始值一样；之后巡检、重启 abroadd、guard、应急程序都不再加载这一组。
func TestReturnBlockUninstall(t *testing.T) {
	for _, from := range []string{"RESTORED", "ENABLED", "DISABLED"} {
		t.Run(from, func(t *testing.T) {
			e := newPrepEnv(t)
			run := filepath.Join(filepath.Dir(e.dir), "run")
			_ = os.Mkdir(run, 0o700)
			start := func() *Daemon {
				o := e.opts()
				o.Sys.ResolverLock, o.Sys.ClaudeSettings = e.lock, e.cs
				o.Sys.FirstRun = true
				o.RunDir = run
				o.BootID = func() (string, error) { return "boot-1", nil }
				return e.env.start(o)
			}
			d := start()
			post(t, d, "/v1/enable", 200)
			switch from {
			case "RESTORED":
				restoreWith(t, d, false)
				if !status(t, d).ReturnBlock.Active {
					t.Fatal("卸载前回国拦截应在生效")
				}
			case "DISABLED":
				post(t, d, "/v1/disable", 200)
			}
			if code, raw := uninstallWith(t, d); code != 200 {
				t.Fatalf("卸载：%d %s", code, raw)
			}
			check := func(where string) {
				t.Helper()
				f, _ := e.view.Load()
				if e.sys.PF.Raw(pf.DefaultAnchor) != nil || len(e.sys.PF.Tokens()) != 0 || e.sys.PF.Active(pf.DefaultAnchor) || len(f.PFTokens) != 0 {
					t.Fatalf("%s：锚点 %q，pf 里的引用 %v，文件里的 %v", where, e.sys.PF.Raw(pf.DefaultAnchor), e.sys.PF.Tokens(), f.PFTokens)
				}
				if !e.sys.Net.Snapshot().Equal(e.init) {
					t.Fatalf("%s：网络和初始值不同", where)
				}
			}
			check("卸载后")
			if st := status(t, d); st.ReturnBlock == nil || st.ReturnBlock.On || st.ReturnBlock.Active {
				t.Fatalf("卸载后开关应关：%+v", st.ReturnBlock)
			}
			if err := d.sup.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			check("巡检一轮后")
			d.Shutdown()
			d = start()
			check("重启 abroadd 后")
			d.Shutdown()
			e.sys.PF.Reset()
			e.sys.PF.ResetCalls()
			if out, _ := guard.Run(context.Background(), guard.Deps{StateDir: e.dir, RunDir: run, PF: &pf.Controller{R: e.sys.PF},
				BootID: func() (string, error) { return "boot-2", nil }}); out != guard.NotNeeded || len(e.sys.PF.Calls()) != 0 {
				t.Fatalf("卸载后 guard 不应加载：%v %v", out, e.sys.PF.Calls())
			}
		})
	}
}

// M6-11b（architect 2026-11-27 决定 A）：首次准备只管测试目标时选择文件写“测试”，不只管时写“真实”。开发构建按它
// 加载测试段（状态标“测试段”），重启后 guard 加载的和 abroadd 的逐字相同；发布构建一律真实段。
func TestReturnBlockSetFromPrepare(t *testing.T) {
	for _, testOnly := range []bool{false, true} {
		e := newPrepEnv(t)
		d := e.start(testOnly)
		set, err := state.ReadReturnBlockSet(e.dir)
		want := pf.SetReal
		if testOnly {
			want = pf.SetTest
		}
		if err != nil || set != want {
			t.Fatalf("只管测试目标=%v：选择文件 %q %v", testOnly, set, err)
		}
		st := status(t, d)
		useTest := testOnly && pf.DevBuild
		wantNets := []string{"160.79.104.0/21", "2607:6bc0::/32"}
		if useTest {
			wantNets = []string{"192.0.2.0/24", "2001:db8:abd::/48"}
		}
		if st.ReturnBlock == nil || !st.ReturnBlock.Active || st.ReturnBlock.Test != useTest || !slices.Equal(st.ReturnBlock.Nets, wantNets) {
			t.Fatalf("只管测试目标=%v：%+v", testOnly, st.ReturnBlock)
		}
		abroadd := e.sys.PF.Raw(pf.DefaultAnchor)
		wantRules, _ := pf.Render(state.ChooseReturnBlock(e.dir).Config())
		if !bytes.Equal(abroadd, wantRules) {
			t.Fatalf("abroadd 加载的：\n%s", abroadd)
		}
		if err := d.Shutdown(); err != nil {
			t.Fatal(err)
		}
		e.sys.PF.Reset()
		run := filepath.Join(filepath.Dir(e.dir), "run")
		_ = os.Mkdir(run, 0o700)
		if out, err := guard.Run(context.Background(), guard.Deps{StateDir: e.dir, RunDir: run, PF: &pf.Controller{R: e.sys.PF},
			BootID: func() (string, error) { return "b", nil }}); err != nil || out != guard.Loaded || !bytes.Equal(e.sys.PF.Raw(pf.DefaultAnchor), abroadd) {
			t.Fatalf("guard 加载的应和 abroadd 的逐字相同：%v %v\n%s", out, err, e.sys.PF.Raw(pf.DefaultAnchor))
		}
	}
}

// 开发构建：选择文件读不出时不加载（开关关）；发布构建照样按真实段。
func TestReturnBlockSetMissing(t *testing.T) {
	e := newPrepEnv(t)
	d := e.start(true)
	_ = os.Remove(filepath.Join(e.dir, state.ReturnBlockSetFile))
	if got := d.returnBlockOn(); got == pf.DevBuild {
		t.Fatalf("选择文件不在：开关 %v（开发构建应关、发布构建应开）", got)
	}
}
