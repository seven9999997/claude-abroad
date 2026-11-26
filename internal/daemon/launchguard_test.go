package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-abroad/internal/claudeproc"
	"claude-abroad/internal/gatesm"
	"claude-abroad/internal/procinfo"
	"claude-abroad/internal/state"
)

// lgEnv：假系统 + 假进程世界 + 假签名（按进程号），第三层用真实时钟跑（200 毫秒一轮）。
type lgEnv struct {
	*env
	w   *claudeproc.FakeWorld
	sig *lgSigner
	d   *Daemon
}

type lgSigner struct{ teams map[int]string }

func (s *lgSigner) ProcTeamID(pid int) (string, error) { return s.teams[pid], nil }

const lgNative = "/Users/u/.local/share/claude/versions/2.1.319"

func newLGEnv(t *testing.T, mod func(*Options)) *lgEnv {
	t.Helper()
	e := &lgEnv{env: newEnv(t), w: claudeproc.NewFakeWorld(), sig: &lgSigner{teams: map[int]string{}}}
	o := e.opts()
	o.Sys.LaunchControl = true
	o.Sys.LaunchGuard = &claudeproc.LaunchGuard{Procs: e.w, Info: e.w.Info, Sig: e.w, Self: os.Getpid(),
		Judge: &claudeproc.AutoJudge{Signer: e.sig}, Interval: 20 * time.Millisecond, Grace: 50 * time.Millisecond}
	if mod != nil {
		mod(&o)
	}
	e.d = e.start(o)
	return e
}

// addClaude：签名是 Anthropic 的原生 Claude Code，启动时间 start。
func (e *lgEnv) addClaude(pid int, start time.Time) {
	e.sig.teams[pid] = claudeproc.AnthropicTeamID
	e.w.Add(procinfo.Proc{PID: pid, PPID: 1, Path: lgNative, Name: "2.1.319", Start: start}, claudeproc.ExitOnTerm, 0)
}

func waitGone(t *testing.T, w *claudeproc.FakeWorld, pid int) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for w.Alive(pid) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	return !w.Alive(pid)
}

// A：已启用时，闸门因出口检查、接管故障、正在更换主代理、虚拟机关着都算允许（不结束）；只有环境检测算不允许。
func TestLaunchGuardRuleA(t *testing.T) {
	e := newLGEnv(t, nil)
	post(t, e.d, "/v1/enable", 200)
	for _, r := range []gatesm.Reason{gatesm.ReasonExitCheck, gatesm.ReasonTakeover, gatesm.ReasonMainSwitch, gatesm.ReasonVM} {
		e.d.gate.AddReason(r, "测试")
		if p := e.d.guardPolicy(); !p.Allowed {
			t.Fatalf("%s：应允许启动（没有网络但不结束）", r)
		}
		e.d.gate.ClearReason(r)
	}
	e.d.gate.AddReason(gatesm.ReasonEnvCheck, "测试")
	if p := e.d.guardPolicy(); p.Allowed || p.CoverAll {
		t.Fatalf("环境检测没过：应不允许、只结束之后启动的：%+v", p)
	}
	// 停用：允许（可以启动、没有网络）
	e.d.gate.ClearReason(gatesm.ReasonEnvCheck)
	post(t, e.d, "/v1/disable", 200)
	if p := e.d.guardPolicy(); !p.Allowed {
		t.Fatal("停用时应允许")
	}
}

// B：“不允许”开始之前启动的不结束，之后启动的结束（端到端：第三层真的在跑）。
func TestLaunchGuardRuleB(t *testing.T) {
	e := newLGEnv(t, nil)
	post(t, e.d, "/v1/enable", 200)
	e.addClaude(100, time.Now().Add(-time.Hour)) // 早就在运行
	time.Sleep(10 * time.Millisecond)
	e.d.gate.AddReason(gatesm.ReasonEnvCheck, "测试")
	e.d.guardPolicy() // 记下“不允许”开始的时刻
	time.Sleep(10 * time.Millisecond)
	e.addClaude(101, time.Now()) // 之后才启动
	if !waitGone(t, e.w, 101) {
		t.Fatal("之后启动的应被结束")
	}
	time.Sleep(100 * time.Millisecond)
	if !e.w.Alive(100) {
		t.Fatal("之前已经在运行的不应被结束")
	}
	if !hasEvent(e.d, "launchguard", "blocked") {
		t.Fatal("应记拦截事件")
	}
}

// C：abroadd 重启（运行目录里有本次开机的标记）不结束已经在运行的；开机后第一次启动不看启动时间；标记读写失败按重启处理。
func TestLaunchGuardRuleC(t *testing.T) {
	run := t.TempDir()
	boot := func() (string, error) { return "boot-A", nil }
	// 开机后第一次启动：没有标记 → bootFirst
	// 扫描间隔放到很长：开机后第一轮之后不再自动跑，“这一段不允许”何时结束由测试控制
	e1 := newLGEnv(t, func(o *Options) {
		o.RunDir, o.BootID = run, boot
		o.Sys.LaunchGuard.Interval = time.Hour
	})
	if !e1.d.gs.bootFirst {
		t.Fatal("没有标记时应是开机后第一次启动")
	}
	if b, _ := os.ReadFile(filepath.Join(run, GuardMarkerFile)); strings.TrimSpace(string(b)) != "boot-A" {
		t.Fatal("应写下本次开机的标记")
	}
	post(t, e1.d, "/v1/enable", 200)
	e1.d.gate.AddReason(gatesm.ReasonEnvCheck, "测试")
	if p := e1.d.guardPolicy(); !p.CoverAll {
		t.Fatalf("开机后第一次启动的这一段不允许：应不看启动时间：%+v", p)
	}
	// 允许过一次之后，开机第一段结束
	e1.d.gate.ClearReason(gatesm.ReasonEnvCheck)
	e1.d.guardPolicy()
	e1.d.gate.AddReason(gatesm.ReasonEnvCheck, "测试")
	if p := e1.d.guardPolicy(); p.CoverAll {
		t.Fatal("开机第一段结束后应只看启动时间")
	}
	// 崩溃后重启：标记是本次开机的 → 不是第一次；since 不早于这次启动
	e2 := newLGEnv(t, func(o *Options) { o.RunDir, o.BootID = run, boot })
	if e2.d.gs.bootFirst {
		t.Fatal("有本次开机的标记：应按重启处理")
	}
	post(t, e2.d, "/v1/enable", 200)
	e2.d.gate.AddReason(gatesm.ReasonEnvCheck, "测试")
	if p := e2.d.guardPolicy(); p.CoverAll || p.Since.Before(e2.d.gs.started) {
		t.Fatalf("重启：不看启动时间不对、since 应不早于这次启动：%+v", p)
	}
	// 标记读写失败（运行目录不存在）：按重启处理
	e3 := newLGEnv(t, func(o *Options) { o.RunDir, o.BootID = filepath.Join(run, "nope", "x"), boot })
	if e3.d.gs.bootFirst {
		t.Fatal("标记写不下：应按重启处理（偏向不误杀）")
	}
}

// D：回国状态：所有确认过的都结束，不看启动时间；状态文件读不出：不结束。
func TestLaunchGuardRuleD(t *testing.T) {
	e := newLGEnv(t, nil) // 全新安装：RESTORED
	e.addClaude(200, time.Now().Add(-time.Hour))
	if p := e.d.guardPolicy(); p.Allowed || !p.CoverAll {
		t.Fatalf("回国：应不允许、不看启动时间：%+v", p)
	}
	if !waitGone(t, e.w, 200) {
		t.Fatal("回国状态下应结束早就在运行的 Claude")
	}
	// npm 装的不碰
	e.w.Add(procinfo.Proc{PID: 201, PPID: 1, Path: "/opt/homebrew/bin/node", Args: []string{"node", "/x/node_modules/@anthropic-ai/claude-code/cli.js"}, Name: "node"}, claudeproc.ExitOnTerm, 0)
	time.Sleep(100 * time.Millisecond)
	if !e.w.Alive(201) {
		t.Fatal("npm 装的确认不了，不应结束")
	}
	// 状态文件损坏：不结束
	_ = os.WriteFile(filepath.Join(e.dir, state.FileName), []byte("{"), 0o600)
	if p := e.d.guardPolicy(); !p.Allowed {
		t.Fatal("状态文件读不出时偏向不误杀")
	}
}

// GET /v1/launchguard：是否允许、四层、覆盖范围、最近拦截；不含命令行和路径。
func TestLaunchGuardAPI(t *testing.T) {
	e := newLGEnv(t, nil)
	e.addClaude(300, time.Now())
	waitGone(t, e.w, 300)
	code, raw := call(t, e.d, "GET", "/v1/launchguard", nil)
	var v LaunchGuardView
	if code != 200 || json.Unmarshal(raw, &v) != nil {
		t.Fatalf("%d %s", code, raw)
	}
	if v.Allowed || !v.CoverAll || v.Since == nil || len(v.Layers) != 4 || len(v.Coverage) != 4 || len(v.Recent) != 1 {
		t.Fatalf("%+v", v)
	}
	if !v.Layers[2].Effective || v.Layers[1].Effective {
		t.Fatalf("第三层生效、第二层不提供：%+v", v.Layers)
	}
	if strings.Contains(string(raw), lgNative) || strings.Contains(string(raw), "/Users/") {
		t.Fatalf("不应含路径：%s", raw)
	}
	// 没接第三层：允许、第三层不生效
	e2 := newEnv(t)
	d2 := e2.start(e2.opts())
	_, raw2 := call(t, d2, "GET", "/v1/launchguard", nil)
	var v2 LaunchGuardView
	_ = json.Unmarshal(raw2, &v2)
	if !v2.Allowed || v2.Layers[2].Effective {
		t.Fatalf("没接第三层：%+v", v2)
	}
}

// M6-4b-1 返工：没打开启动管控（真实模式不带 --launch-control）时，状态和 /v1/launchguard 写“未启用”（不是“未生效”）。
func TestLaunchControlOff(t *testing.T) {
	e := newEnv(t)
	d := e.start(e.opts())
	if st := status(t, d); st.LaunchControl || st.ResolverLock != nil || st.Prepare != nil {
		t.Fatalf("没打开管控：%+v", st)
	}
	_, raw := call(t, d, "GET", "/v1/launchguard", nil)
	var v LaunchGuardView
	if json.Unmarshal(raw, &v) != nil || v.LaunchControl || !v.Allowed {
		t.Fatalf("%s", raw)
	}
	for _, l := range v.Layers {
		if l.Layer != 2 && (l.Effective || l.Note != LaunchControlOffNote) {
			t.Fatalf("第 %d 层应写“未启用”：%+v", l.Layer, l)
		}
	}
	e2 := newLGEnv(t, nil)
	if st := status(t, e2.d); !st.LaunchControl {
		t.Fatal("打开管控时状态应写 launch_control")
	}
}

// M6-4b-1 建议改：接口只读（不改“这一段不允许”的起点），时间用 UTC。
func TestLaunchGuardAPIReadOnlyUTC(t *testing.T) {
	e := newLGEnv(t, nil)
	e.addClaude(300, time.Now())
	waitGone(t, e.w, 300)
	before := e.d.gs.since
	for i := 0; i < 3; i++ {
		_, raw := call(t, e.d, "GET", "/v1/launchguard", nil)
		var v LaunchGuardView
		if json.Unmarshal(raw, &v) != nil || v.Since == nil || v.Since.Location() != time.UTC || len(v.Recent) == 0 || v.Recent[0].Time.Location() != time.UTC {
			t.Fatalf("时间应是 UTC（不带本机时区偏移）：%s", raw)
		}
	}
	if !e.d.gs.since.Equal(before) {
		t.Fatal("读接口不应改起点")
	}
}
