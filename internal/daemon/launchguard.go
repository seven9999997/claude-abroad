package daemon

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"claude-abroad/internal/claudeproc"
	"claude-abroad/internal/gatesm"
	"claude-abroad/internal/guard"
	"claude-abroad/internal/state"
)

// 启动管控（设计 10.2，M6-4b）：abroadd 按状态表决定“允许启动”，不允许时由第三层（claudeproc.LaunchGuard）
// 每 200 毫秒扫描、结束确认过的 Claude 进程。口径（m6-plan 10.2 口径 A–D）：
//   - A：已启用时“不允许”只等价于闸门上有“环境检测”这个原因；闸门因别的原因关着（出口检查、接管故障、
//     正在更换主代理、虚拟机）时 Claude 没有网络，但不算不允许，不结束任何进程；停用同理（可以启动、没有网络）；
//   - B：已启用或启用中而不允许时，只结束“不允许”开始之后才启动的进程；
//   - C：这一段“不允许”最早从 abroadd 这次启动的时刻算起（自己重启不结束正在运行的 Claude）；例外是开机后第一次
//     启动（运行目录里没有本次开机的标记）：这时不看启动时间；标记读写失败按“崩溃后重启”处理（偏向不误杀）；
//   - D：回国中、已回国：所有确认过的都结束，不看启动时间；
//   - 状态文件读不出：不结束（偏向不误杀；不泄漏不靠这一层）。

// LaunchControlOffNote 是没打开启动管控时各层的说明。
const LaunchControlOffNote = "未启用：这次安装没有打开启动管控"

// GuardMarkerFile 是“本次开机 abroadd 已经启动过”的标记（运行目录里，内容是开机标识）。
const GuardMarkerFile = "launchguard-boot"

type guardState struct {
	mu         sync.Mutex
	notAllowed bool
	since      time.Time
	started    time.Time
	bootFirst  bool
}

func (d *Daemon) guardNow() time.Time {
	if d.opts.Clock != nil {
		return d.opts.Clock.Now()
	}
	return time.Now()
}

// startLaunchGuard 接上第三层（Sys.LaunchGuard 非空时：真实模式；测试注入假的）。
func (d *Daemon) startLaunchGuard() {
	g := d.opts.Sys.LaunchGuard
	if g == nil {
		return
	}
	d.gs = &guardState{started: d.guardNow(), bootFirst: d.firstStartThisBoot()}
	g.Policy = d.guardPolicy
	g.OnBlock = func(r claudeproc.GuardRecord) {
		d.events.Add("launchguard", "blocked", "现在不允许启动 Claude，已结束："+r.Name)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	d.lgMu.Lock()
	d.lgStop, d.lgDone = cancel, done
	d.lgMu.Unlock()
	go func() { defer close(done); g.Run(ctx) }()
}

// stopLaunchGuard 停掉第三层并等它退出（卸载和退出都走这里；只停一次，可以重复调用）。
func (d *Daemon) stopLaunchGuard() {
	d.lgMu.Lock()
	stop, done := d.lgStop, d.lgDone
	d.lgStop, d.lgDone = nil, nil
	d.lgMu.Unlock()
	if stop != nil {
		stop()
		<-done
	}
}

// firstStartThisBoot：运行目录里没有本次开机的标记，并且能写下标记，才算开机后第一次启动。读写失败按崩溃后重启处理。
func (d *Daemon) firstStartThisBoot() bool {
	run := d.opts.RunDir
	if run == "" {
		return false
	}
	bootID := d.opts.BootID
	if bootID == nil {
		bootID = guard.BootID
	}
	b, err := bootID()
	if err != nil || b == "" {
		return false
	}
	p := filepath.Join(run, GuardMarkerFile)
	if cur, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(cur)) == b {
		return false // 本次开机已经启动过：这是崩溃或被结束后的重启
	} else if err != nil && !os.IsNotExist(err) {
		return false
	}
	if err := os.WriteFile(p, []byte(b+"\n"), 0o600); err != nil {
		return false
	}
	return true
}

// guardPolicy 按状态表和口径算当前的启动管控（第三层每轮调用；会更新“这一段不允许”的起点）。
func (d *Daemon) guardPolicy() claudeproc.GuardPolicy { return d.guardPolicyAt(true) }

// guardPolicyPeek 是只读的版本（接口用）：不改“这一段不允许”的起点。
func (d *Daemon) guardPolicyPeek() claudeproc.GuardPolicy { return d.guardPolicyAt(false) }

func (d *Daemon) guardPolicyAt(update bool) claudeproc.GuardPolicy {
	gs := d.gs
	allowed, cover := true, false
	f, err := d.store.Load()
	switch {
	case d.uninstalled(): // 已卸载：启动管控解除（设计 10.3）
		allowed = true
	case err != nil:
		allowed = true // 读不出：不结束（偏向不误杀）
	case f.State == state.Enabled:
		allowed = !d.gateHas(gatesm.ReasonEnvCheck)
	case f.State == state.Disabled:
		allowed = true
	case f.State == state.Enabling:
		allowed = false
	case f.State == state.Restoring, f.State == state.Restored:
		allowed, cover = false, true
	}
	if gs == nil { // 没接第三层：只按状态表给结论，不记“这一段”的开始
		return claudeproc.GuardPolicy{Allowed: allowed, CoverAll: cover}
	}
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if !update { // 只读：按当前记下的起点给结论（还没开始这一段时起点就是现在）
		if allowed {
			return claudeproc.GuardPolicy{Allowed: true}
		}
		since := gs.since
		if !gs.notAllowed {
			since = d.guardNow()
			if since.Before(gs.started) {
				since = gs.started
			}
		}
		return claudeproc.GuardPolicy{Since: since, CoverAll: cover || gs.bootFirst}
	}
	if allowed {
		gs.notAllowed = false
		gs.bootFirst = false // 开机后第一段“不允许”到此结束
		return claudeproc.GuardPolicy{Allowed: true}
	}
	if !gs.notAllowed {
		gs.notAllowed = true
		gs.since = d.guardNow()
		if gs.since.Before(gs.started) {
			gs.since = gs.started
		}
	}
	return claudeproc.GuardPolicy{Since: gs.since, CoverAll: cover || gs.bootFirst}
}

func (d *Daemon) gateHas(r gatesm.Reason) bool {
	for _, x := range d.gate.Status().Reasons {
		if x == string(r) {
			return true
		}
	}
	return false
}

// LaunchGuardLayer 是一层的生效情况。
type LaunchGuardLayer struct {
	Layer     int    `json:"layer"`
	Name      string `json:"name"`
	Effective bool   `json:"effective"`
	Note      string `json:"note,omitempty"`
}

// LaunchGuardView 是 GET /v1/launchguard 的应答（不含命令行参数和路径）。
type LaunchGuardView struct {
	LaunchControl bool                     `json:"launch_control,omitempty"` // 这次安装打开了启动管控（--launch-control）
	Allowed       bool                     `json:"allowed"`                  // 现在允许启动 Claude
	Since         *time.Time               `json:"since,omitempty"`          // 这一段“不允许”开始的时刻
	CoverAll      bool                     `json:"cover_all,omitempty"`      // 不看启动时间（回国、开机后第一次启动）
	Layers        []LaunchGuardLayer       `json:"layers"`                   // 四层各自是否生效
	Coverage      []claudeproc.CoverageRow `json:"coverage"`                 // 第三层对各种安装方式的覆盖
	Recent        []claudeproc.GuardRecord `json:"recent"`                   // 最近的拦截
}

func (d *Daemon) launchGuardView() LaunchGuardView {
	v := LaunchGuardView{Allowed: true, Recent: []claudeproc.GuardRecord{}}
	if g := d.opts.Sys.LaunchGuard; g != nil && d.gs != nil {
		p := d.guardPolicyPeek()
		v.Allowed, v.CoverAll = p.Allowed, p.CoverAll
		if !p.Allowed {
			s := p.Since.UTC()
			v.Since = &s
		}
		for _, r := range g.Records() { // 接口里的时间用 UTC（不带本机时区偏移）
			r.Time = r.Time.UTC()
			v.Recent = append(v.Recent, r)
		}
	}
	lock := false
	if lk := d.opts.Sys.ResolverLock; lk != nil {
		lock = lk.Check().Effective
	}
	cc := d.claudeCodeStatus()
	l4 := LaunchGuardLayer{Layer: 4, Name: "Claude Code 的本机代理", Effective: cc != nil && cc.Effective, Note: "只覆盖 Claude Code"}
	switch { // v0.68：第四道可选、默认关；关着写“未开启”，和“未生效”区分开
	case cc != nil && !cc.Enabled:
		l4.Note = ClaudeProxyOffNote
	case cc != nil && cc.TestMode:
		l4.Note = ClaudeProxyTestNote
	}
	v.Coverage = claudeproc.Coverage(d.layer4Covers())
	v.Layers = []LaunchGuardLayer{
		{Layer: 1, Name: "Claude 域名解析锁", Effective: lock},
		{Layer: 2, Name: "启动拦截（系统扩展）", Effective: false, Note: "需要付费开发者账号和 Apple 的授权，暂不提供"},
		{Layer: 3, Name: "发现即结束", Effective: d.opts.Sys.LaunchGuard != nil},
		l4,
	}
	if !d.opts.Sys.LaunchControl { // M6-4b-1 返工：这次安装没打开启动管控，第一、三、四层是“未启用”而不是“未生效”
		v.LaunchControl = false
		for i := range v.Layers {
			if v.Layers[i].Layer != 2 {
				v.Layers[i].Note = LaunchControlOffNote
			}
		}
	} else {
		v.LaunchControl = true
	}
	return v
}

func (d *Daemon) handleLaunchGuard(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, d.launchGuardView())
}

// layer4Covers 报告第四道现在是否管着本机的 Claude Code（打开、不是测试模式）：覆盖范围里的兜底说明按它生成。
func (d *Daemon) layer4Covers() bool {
	return d.cpAvailable() && d.cpOn.Load() && !d.cpTestMode()
}
