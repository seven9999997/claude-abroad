package guard

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"claude-abroad/internal/integrity"
	"claude-abroad/internal/pf"
	"claude-abroad/internal/state"
)

const testBoot = "boot-aaaa-1111"

type rig struct {
	stateDir, runDir string
	fake             *pf.FakePF
	ctl              *pf.Controller
	logs             []string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	base := t.TempDir()
	r := &rig{stateDir: filepath.Join(base, "st"), runDir: filepath.Join(base, "run"), fake: pf.NewFakePF()}
	if err := os.Mkdir(r.runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	r.ctl = &pf.Controller{R: r.fake}
	return r
}

func (r *rig) deps() Deps {
	return Deps{StateDir: r.stateDir, RunDir: r.runDir, PF: r.ctl, BootID: func() (string, error) { return testBoot, nil },
		Logf: func(f string, a ...any) { r.logs = append(r.logs, fmt.Sprintf(f, a...)) }}
}

func (r *rig) setState(t *testing.T, s state.State) {
	t.Helper()
	if err := state.NewStore(r.stateDir).Save(state.File{Version: state.Version, State: s}); err != nil {
		t.Fatal(err)
	}
}

var abroadCfg = pf.Config{User: "_abroad", LAN: []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")}, VMIfs: []string{"bridge100"}}

func (r *rig) setParams(t *testing.T, c pf.Config) {
	t.Helper()
	if err := os.MkdirAll(r.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteParams(r.stateDir, ParamsFrom(c)); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) run(t *testing.T) Outcome {
	t.Helper()
	o, err := Run(context.Background(), r.deps())
	if (o == LoadFailed) != (err != nil) {
		t.Fatalf("结果 %v 和错误 %v 不一致", o, err)
	}
	return o
}

func (r *rig) anchor() []byte { return r.fake.Raw(pf.DefaultAnchor) }

// a. 不存在（全新安装）、回国：没有任何 pf 调用。
func TestNoStateOrRestoredNoPF(t *testing.T) {
	r := newRig(t)
	if o := r.run(t); o != NotNeeded || len(r.fake.Calls()) != 0 {
		t.Fatalf("全新安装：%v，pf 调用 %v", o, r.fake.Calls())
	}
	r.setState(t, state.Restored)
	r.setParams(t, abroadCfg)
	if o := r.run(t); o != NotNeeded || len(r.fake.Calls()) != 0 {
		t.Fatalf("回国：%v，pf 调用 %v", o, r.fake.Calls())
	}
	if _, err := os.Stat(filepath.Join(r.runDir, TokenFile)); !os.IsNotExist(err) {
		t.Fatal("没加载时不应留启用引用")
	}
}

// a. 已启用、停用、启用中、恢复中：加载断网规则、启用 pf、留下启用引用。
func TestLoadForActiveStates(t *testing.T) {
	for _, s := range []state.State{state.Enabled, state.Disabled, state.Enabling, state.Restoring} {
		t.Run(string(s), func(t *testing.T) {
			r := newRig(t)
			r.setState(t, s)
			r.setParams(t, abroadCfg)
			if o := r.run(t); o != Loaded {
				t.Fatalf("%s：%v", s, o)
			}
			if got := strings.Join(r.fake.Calls(), ","); got != "load,enable" {
				t.Fatalf("pf 操作只应有加载和启用：%s", got)
			}
			want, _ := pf.Render(abroadCfg)
			if !bytes.Equal(r.anchor(), want) {
				t.Fatalf("规则不对：\n%s", r.anchor())
			}
			toks, err := TakeTokens(r.runDir)
			if err != nil || len(toks) != 1 {
				t.Fatalf("启用引用 %v %v", toks, err)
			}
		})
	}
}

// a. 损坏（截断、版本不认识、不是普通文件）：加载断网规则。
func TestCorruptStateLoads(t *testing.T) {
	cases := map[string]func(t *testing.T, r *rig){
		"截断": func(t *testing.T, r *rig) {
			r.setState(t, state.Enabled)
			p := filepath.Join(r.stateDir, state.FileName)
			b, _ := os.ReadFile(p)
			_ = os.WriteFile(p, b[:len(b)/2], 0o600)
		},
		"版本不认识": func(t *testing.T, r *rig) {
			r.setState(t, state.Enabled)
			_ = os.WriteFile(filepath.Join(r.stateDir, state.FileName), []byte(`{"version":999,"state":"ENABLED"}`), 0o600)
		},
		"不是普通文件": func(t *testing.T, r *rig) {
			_ = os.MkdirAll(filepath.Join(r.stateDir, state.FileName), 0o700)
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			_ = os.MkdirAll(r.stateDir, 0o700)
			mk(t, r)
			r.setParams(t, abroadCfg)
			if o := r.run(t); o != Loaded {
				t.Fatalf("%s：%v", name, o)
			}
			if r.anchor() == nil {
				t.Fatal("应加载断网规则")
			}
		})
	}
}

// b. 参数读不出、缺项、不合法：加载最严的一组（没有任何放行的例外）。
func TestBadParamsLockdown(t *testing.T) {
	cases := map[string]string{
		"不存在":   "",
		"解析失败":  "{",
		"缺用户":   `{"version":1}`,
		"版本不对":  `{"version":2,"user":"_abroad"}`,
		"公网段":   `{"version":1,"user":"_abroad","lan":["8.8.8.0/24"]}`,
		"段写错":   `{"version":1,"user":"_abroad","lan":["x"]}`,
		"网桥名不对": `{"version":1,"user":"_abroad","vm_ifs":["en0"]}`,
		"用户名不对": `{"version":1,"user":"root"}`,
		"多余字段":  `{"version":1,"user":"_abroad","pass":"all"}`,
	}
	lock, _ := pf.Render(pf.Config{Lockdown: true})
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.setState(t, state.Enabled)
			if body != "" {
				_ = os.WriteFile(filepath.Join(r.stateDir, ParamsFile), []byte(body), 0o600)
			}
			if o := r.run(t); o != Loaded {
				t.Fatalf("%v", o)
			}
			got := r.anchor()
			if !bytes.Equal(got, lock) {
				t.Fatalf("应是最严的一组：\n%s", got)
			}
			for _, bad := range []string{"user ", "abroad_lan", "bridge", "utun"} {
				if bytes.Contains(got, []byte(bad)) {
					t.Fatalf("最严的一组不应有放行例外 %q：\n%s", bad, got)
				}
			}
			// 放行用户发出的公网包也被拦
			if ok, err := r.fake.Eval(pf.DefaultAnchor, pf.Packet{If: "en0", User: "_abroad", Proto: "tcp", DstPort: 443, Dst: netip.MustParseAddr("160.79.104.10")}); err != nil || ok {
				t.Fatalf("最严的一组应拦下放行用户：%v %v", ok, err)
			}
		})
	}
	// 参数文件权限太宽也按不合法处理
	r := newRig(t)
	r.setState(t, state.Enabled)
	r.setParams(t, abroadCfg)
	_ = os.Chmod(filepath.Join(r.stateDir, ParamsFile), 0o644)
	r.run(t)
	if !bytes.Equal(r.anchor(), lock) {
		t.Fatal("参数文件权限太宽时应用最严的一组")
	}
}

// c. guard 生成的规则和 abroadd 停用状态下（没有 utun）的规则逐字相同：同一个函数、同一组输入（经参数文件往返）。
func TestSameRulesAsAbroadd(t *testing.T) {
	r := newRig(t)
	r.setState(t, state.Disabled)
	cfg := pf.Config{User: "_abroad", LAN: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("192.168.31.0/24")},
		VMIfs: []string{"vmenet0", "bridge100"}}
	r.setParams(t, cfg)
	r.run(t)
	want, err := pf.Render(cfg) // abroadd 停用状态：UtunName 为空
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(r.anchor(), want) {
		t.Fatalf("和 abroadd 的不同：\nguard:\n%s\nabroadd:\n%s", r.anchor(), want)
	}
	// 往返：ParamsFrom(带 utun 的配置) 去掉 utun
	withUtun := cfg
	withUtun.UtunName = "utun9"
	c2, err := ParamsFrom(withUtun).Config()
	if err != nil || c2.UtunName != "" {
		t.Fatalf("参数里不应带 utun：%+v %v", c2, err)
	}
}

// d. abroadd 本次开机已经加载过：不动；标记是上次开机的、读不出、没有开机标识：照常加载。
func TestMarker(t *testing.T) {
	r := newRig(t)
	r.setState(t, state.Enabled)
	r.setParams(t, abroadCfg)
	if err := WriteMarker(r.runDir, testBoot); err != nil {
		t.Fatal(err)
	}
	if o := r.run(t); o != NotNeeded || len(r.fake.Calls()) != 0 {
		t.Fatalf("已加载过：%v %v", o, r.fake.Calls())
	}
	_ = WriteMarker(r.runDir, "上一次开机")
	if o := r.run(t); o != Loaded {
		t.Fatalf("上次开机的标记：%v", o)
	}
	r2 := newRig(t)
	r2.setState(t, state.Enabled)
	_ = WriteMarker(r2.runDir, testBoot)
	d := r2.deps()
	d.BootID = func() (string, error) { return "", os.ErrNotExist }
	if o, _ := Run(context.Background(), d); o != Loaded {
		t.Fatalf("没有开机标识时应加载：%v", o)
	}
	r3 := newRig(t)
	r3.setState(t, state.Enabled)
	_ = os.WriteFile(filepath.Join(r3.runDir, MarkerFile), []byte(testBoot), 0o644) // 权限太宽：读不出
	if o := r3.run(t); o != Loaded {
		t.Fatalf("标记读不出时应加载：%v", o)
	}
	if err := WriteMarker(r.runDir, ""); err == nil {
		t.Fatal("没有开机标识时不应写标记")
	}
}

// d. 并发：guard 和 abroadd 的巡检无论谁先，最终锚点里都是 abroadd 自己的期望（不比它宽）。
func TestRaceWithAbroadd(t *testing.T) {
	stale := pf.Config{User: "_abroad", LAN: []netip.Prefix{netip.MustParsePrefix("10.9.0.0/16")}} // 另存的旧参数（多一个段）
	for _, order := range []string{"guard 先", "abroadd 先", "同时"} {
		t.Run(order, func(t *testing.T) {
			r := newRig(t)
			r.setState(t, state.Disabled)
			r.setParams(t, stale)
			var toks []string
			p := &pf.Patrol{C: &pf.Controller{R: r.fake, Matcher: pf.PrintMatcher{User: "_abroad"}}, Net: emptyNet{}, User: "_abroad",
				AllowLAN: func() bool { return false }, OnToken: func(s string) { toks = append(toks, s) }}
			guardRun := func() { _, _ = Run(context.Background(), r.deps()) }
			abroadd := func() { _ = p.Apply(context.Background()) }
			switch order {
			case "guard 先":
				guardRun()
				abroadd()
			case "abroadd 先":
				abroadd()
				guardRun() // 标记没写（这里不经 daemon）：guard 照样加载旧参数
				_ = p.Step(context.Background())
			case "同时":
				var wg sync.WaitGroup
				wg.Add(2)
				go func() { defer wg.Done(); guardRun() }()
				go func() { defer wg.Done(); abroadd() }()
				wg.Wait()
				_ = p.Step(context.Background())
			}
			want, _ := pf.Render(pf.Config{User: "_abroad"})
			if !bytes.Equal(r.anchor(), want) {
				t.Fatalf("最终应是 abroadd 的期望：\n%s", r.anchor())
			}
		})
	}
}

type emptyNet struct{}

func (emptyNet) Snapshot(context.Context) (pf.NetSnapshot, error) { return pf.NetSnapshot{}, nil }

// f. 三种结果的退出码可区分；加载失败带原因。
func TestOutcomeCodesAndFailure(t *testing.T) {
	if Loaded.Code() == NotNeeded.Code() || Loaded.Code() == LoadFailed.Code() || NotNeeded.Code() == LoadFailed.Code() || LoadFailed.Code() == 0 {
		t.Fatal("退出码应三种可区分，失败非 0")
	}
	r := newRig(t)
	r.setState(t, state.Enabled)
	r.fake.FailNext("load", os.ErrPermission)
	o, err := Run(context.Background(), r.deps())
	if o != LoadFailed || err == nil || !strings.Contains(err.Error(), "加载断网规则") {
		t.Fatalf("%v %v", o, err)
	}
	r.fake.FailNext("enable", os.ErrPermission)
	o, err = Run(context.Background(), r.deps())
	if o != LoadFailed || err == nil || !strings.Contains(err.Error(), "启用 pf 失败") {
		t.Fatalf("%v %v", o, err)
	}
}

// 启用引用：追加、收养后删除。
func TestTokens(t *testing.T) {
	dir := t.TempDir()
	if toks, err := TakeTokens(dir); err != nil || toks != nil {
		t.Fatal("没有文件时应为空")
	}
	_ = AppendToken(dir, "11")
	_ = AppendToken(dir, "12")
	toks, err := TakeTokens(dir)
	if err != nil || strings.Join(toks, ",") != "11,12" {
		t.Fatalf("%v %v", toks, err)
	}
	if _, err := os.Stat(filepath.Join(dir, TokenFile)); !os.IsNotExist(err) {
		t.Fatal("收养后应删除")
	}
}

// 回国、全新安装时 guard 不留下任何东西（运行时目录也不建）；要加载时才建目录、写引用。
func TestRunDirOnlyWhenLoading(t *testing.T) {
	r := newRig(t)
	r.runDir = filepath.Join(filepath.Dir(r.runDir), "run-absent")
	r.setState(t, state.Restored)
	r.run(t)
	if _, err := os.Stat(r.runDir); !os.IsNotExist(err) {
		t.Fatal("回国时不应建运行时目录")
	}
	r.setState(t, state.Enabled)
	if o := r.run(t); o != Loaded {
		t.Fatalf("%v", o)
	}
	if toks, err := TakeTokens(r.runDir); err != nil || len(toks) != 1 {
		t.Fatalf("要加载时应建目录并写引用：%v %v", toks, err)
	}
}

// M6-7 补审返工：完整性校验不通过时 guard 只记日志，行为（加载什么、pf 调用、启用引用）和通过时逐项相同。
func TestIntegrityOnlyLogged(t *testing.T) {
	base := t.TempDir()
	_ = os.WriteFile(filepath.Join(base, "abroad-vm"), []byte("ok"), 0o755)
	b, _ := integrity.Generate(base, []string{"abroad-vm"})
	mf := filepath.Join(base, "manifest.json")
	_ = os.WriteFile(mf, b, 0o644)
	good := &integrity.Checker{Manifest: mf, Base: base, Digest: integrity.Digest(b)}
	bad := &integrity.Checker{Manifest: mf, Base: base, Digest: integrity.Digest([]byte("别的清单"))}
	for _, s := range []state.State{state.Enabled, state.Restored} {
		var calls [2]string
		var outs [2]Outcome
		var anchors [2][]byte
		for i, c := range []*integrity.Checker{good, bad} {
			r := newRig(t)
			r.setState(t, s)
			r.setParams(t, abroadCfg)
			d := r.deps()
			d.Integrity = c
			o, _ := Run(context.Background(), d)
			outs[i], calls[i], anchors[i] = o, strings.Join(r.fake.Calls(), ","), r.anchor()
			want := map[int]string{0: "完整性校验通过", 1: "完整性校验不通过"}[i]
			if !strings.Contains(strings.Join(r.logs, "\n"), want) {
				t.Fatalf("%s：日志里应有“%s”：%v", s, want, r.logs)
			}
		}
		if outs[0] != outs[1] || calls[0] != calls[1] || !bytes.Equal(anchors[0], anchors[1]) {
			t.Fatalf("%s：校验通过和不通过时 guard 的行为应相同：%v/%v %q/%q", s, outs[0], outs[1], calls[0], calls[1])
		}
	}
}

func (r *rig) mark(t *testing.T, name string) {
	t.Helper()
	if err := os.MkdirAll(r.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.stateDir, name), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// M6-11b 验收 a：guard 在回国状态下的情况表。
func TestRestoredReturnBlockTable(t *testing.T) {
	want, _ := pf.Render(pf.ReturnBlockConfig())
	cases := []struct {
		name      string
		stateFile bool
		prepared  bool
		uninst    bool
		load      bool
	}{
		{"已回国、准备过、未卸载", true, true, false, true},
		{"已回国、没准备过", true, false, false, false},
		{"已回国、已卸载", true, true, true, false},
		{"没有状态文件（准备过也不动）", false, true, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			r.setParams(t, abroadCfg)
			if c.stateFile {
				r.setState(t, state.Restored)
			}
			if c.prepared {
				r.mark(t, state.PreparedFile)
				if err := state.WriteReturnBlockSet(r.stateDir, pf.SetReal); err != nil {
					t.Fatal(err)
				}
			}
			if c.uninst {
				r.mark(t, state.UninstalledFile)
			}
			o := r.run(t)
			if !c.load {
				if o != NotNeeded || len(r.fake.Calls()) != 0 {
					t.Fatalf("不应碰 pf：%v %v", o, r.fake.Calls())
				}
				if _, err := os.Stat(filepath.Join(r.runDir, TokenFile)); !os.IsNotExist(err) {
					t.Fatal("不应留启用引用")
				}
				return
			}
			if o != Loaded || !bytes.Equal(r.anchor(), want) || !r.fake.Active(pf.DefaultAnchor) {
				t.Fatalf("应加载回国拦截：%v\n%s", o, r.anchor())
			}
			toks, err := TakeTokens(r.runDir)
			if err != nil || len(toks) != 1 || len(r.fake.Tokens()) != 1 || toks[0] != r.fake.Tokens()[0] {
				t.Fatalf("应留一个启用引用交 abroadd 收养：%v %v", toks, r.fake.Tokens())
			}
			if r.fake.Kills() != 0 {
				t.Fatal("回国拦截不清状态")
			}
		})
	}
	// abroadd 本次开机已经起来过：不动
	r := newRig(t)
	r.setState(t, state.Restored)
	r.mark(t, state.PreparedFile)
	if err := WriteMarker(r.runDir, testBoot); err != nil {
		t.Fatal(err)
	}
	if o := r.run(t); o != NotNeeded || len(r.fake.Calls()) != 0 {
		t.Fatalf("abroadd 已经起来过：%v %v", o, r.fake.Calls())
	}
}
