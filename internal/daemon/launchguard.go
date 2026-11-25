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
	d.lgStop, d.lgDone = cancel, make(chan struct{})
	go func() { defer close(d.lgDone); g.Run(ctx) }()
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

// guardPolicy 按状态表和口径算当前的启动管控（第三层每轮调用）。
func (d *Daemon) guardPolicy() claudeproc.GuardPolicy {
	gs := d.gs
	allowed, cover := true, false
	f, err := d.store.Load()
	switch {
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
	gs.mu.Lock()
	defer gs.mu.Unlock()
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
	Allowed  bool                     `json:"allowed"`             // 现在允许启动 Claude
	Since    *time.Time               `json:"since,omitempty"`     // 这一段“不允许”开始的时刻
	CoverAll bool                     `json:"cover_all,omitempty"` // 不看启动时间（回国、开机后第一次启动）
	Layers   []LaunchGuardLayer       `json:"layers"`              // 四层各自是否生效
	Coverage []claudeproc.CoverageRow `json:"coverage"`            // 第三层对各种安装方式的覆盖
	Recent   []claudeproc.GuardRecord `json:"recent"`              // 最近的拦截
}

func (d *Daemon) launchGuardView() LaunchGuardView {
	v := LaunchGuardView{Allowed: true, Coverage: claudeproc.Coverage(), Recent: []claudeproc.GuardRecord{}}
	if g := d.opts.Sys.LaunchGuard; g != nil && d.gs != nil {
		p := d.guardPolicy()
		v.Allowed, v.CoverAll = p.Allowed, p.CoverAll
		if !p.Allowed {
			s := p.Since
			v.Since = &s
		}
		v.Recent = g.Records()
	}
	lock := false
	if lk := d.opts.Sys.ResolverLock; lk != nil {
		lock = lk.Check().Effective
	}
	cc := d.claudeCodeStatus()
	v.Layers = []LaunchGuardLayer{
		{Layer: 1, Name: "Claude 域名解析锁", Effective: lock},
		{Layer: 2, Name: "启动拦截（系统扩展）", Effective: false, Note: "需要付费开发者账号和 Apple 的授权，暂不提供"},
		{Layer: 3, Name: "发现即结束", Effective: d.opts.Sys.LaunchGuard != nil},
		{Layer: 4, Name: "Claude Code 的本机代理", Effective: cc != nil && cc.Effective, Note: "只覆盖 Claude Code"},
	}
	return v
}

func (d *Daemon) handleLaunchGuard(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, d.launchGuardView())
}
