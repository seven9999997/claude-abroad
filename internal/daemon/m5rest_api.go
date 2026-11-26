package daemon

import (
	"claude-abroad/internal/api"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"claude-abroad/internal/preflight"
	"claude-abroad/internal/redact"
	"claude-abroad/internal/stack"
	"claude-abroad/internal/vmmgr"
)

// M5-5 的其余接口（m5-plan v2.3）：冲突检查、在联网的应用、安装引导的进度、引擎与虚拟机的状态、单独检查一个出口。

// ---- 冲突检查（决定 1）----

// PreflightMinInterval 是冲突检查接口的最小间隔：间隔内重复调用返回上一次的结果，不再执行系统命令。
const PreflightMinInterval = 2 * time.Second

// PreflightItem 是冲突检查的一项（沿用 preflight 包的脱敏：代理地址只写“已配置”，进程只给软件名，shell 配置只给文件名和行号）。
type PreflightItem struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Fix         string `json:"fix"`
	Undecidable bool   `json:"undecidable,omitempty"`
}

// PreflightView 是 GET /v1/preflight。
type PreflightView struct {
	CheckedAt time.Time       `json:"checked_at"`
	Cached    bool            `json:"cached"` // 间隔内的重复调用，返回的是上一次的结果
	Blocking  []PreflightItem `json:"blocking"`
	Warnings  []PreflightItem `json:"warnings"`
}

// preflightReporter 是能给出完整结果的冲突检查（PreflightHints 实现）。
type preflightReporter interface {
	Report(ctx context.Context) preflight.Report
}

// Report 实现 preflightReporter：跑一次检查（只经只读执行器，preflight 包保证）。
func (p PreflightHints) Report(ctx context.Context) preflight.Report { return p.C.Run(ctx) }

type preflightCache struct {
	mu   sync.Mutex
	at   time.Time
	last PreflightView
}

func toItems(fs []preflight.Finding) []PreflightItem {
	out := []PreflightItem{}
	for _, f := range fs {
		out = append(out, PreflightItem{ID: f.ID, Title: f.Title, Fix: f.Fix, Undecidable: f.Undecidable})
	}
	return out
}

func (d *Daemon) handlePreflight(w http.ResponseWriter, r *http.Request) {
	rp, ok := d.opts.Sys.Preflight.(preflightReporter)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, errBody{"这个模式没有冲突检查"})
		return
	}
	d.pf.mu.Lock()
	defer d.pf.mu.Unlock()
	now := d.now()
	if !d.pf.at.IsZero() && now.Sub(d.pf.at) < PreflightMinInterval {
		v := d.pf.last
		v.Cached = true
		writeJSON(w, http.StatusOK, v)
		return
	}
	rep := rp.Report(r.Context())
	v := PreflightView{CheckedAt: now.Truncate(time.Second), Blocking: toItems(rep.Blocking()), Warnings: toItems(rep.Warnings())}
	d.pf.at, d.pf.last = now, v
	writeJSON(w, http.StatusOK, v)
}

// ---- 在联网的应用（决定 2）----

// MaxApps 是在联网的应用的条目上限。
const MaxApps = 200

// AppView 是一个在联网的应用（不含目的地址、域名、进程路径里的用户目录）。
type AppView struct {
	App    string   `json:"app"`
	Name   string   `json:"name"`
	Conns  int      `json:"conns"`
	Exits  []string `json:"exits"`
	Reason string   `json:"reason"`
}

// AppsView 是 GET /v1/apps。
type AppsView struct {
	Apps      []AppView `json:"apps"`
	Truncated bool      `json:"truncated,omitempty"`
	// Fallbacks 是正在回退到主代理的规则（补齐二：规则指向的分流出口现在不可用）。
	Fallbacks []RuleFallback `json:"fallbacks,omitempty"`
}

// sanitizeApp 把应用 ID 里的用户目录换成 ~（path:/Users/<名字>/… → path:~/…）。
func sanitizeApp(id string) string {
	p, ok := strings.CutPrefix(id, "path:")
	if !ok {
		return id
	}
	for _, root := range []string{"/Users/", "/home/"} {
		if rest, ok := strings.CutPrefix(p, root); ok {
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				return "path:~" + rest[i:]
			}
			return "path:~"
		}
	}
	return id
}

// appName 是显示用的名字：路径形式取文件名，其余用应用 ID 本身。
func appName(id string) string {
	if p, ok := strings.CutPrefix(id, "path:"); ok {
		return path.Base(p)
	}
	return id
}

func (d *Daemon) handleApps(w http.ResponseWriter, r *http.Request) {
	v := AppsView{Apps: []AppView{}}
	if dp := d.opts.Sys.Data; dp != nil {
		var list []stack.AppFlows
		list, v.Truncated = dp.Apps(MaxApps)
		for _, a := range list {
			id := sanitizeApp(string(a.App))
			v.Apps = append(v.Apps, AppView{App: id, Name: appName(id), Conns: a.Conns, Exits: a.Exits, Reason: a.Reason})
		}
	}
	v.Fallbacks = d.ruleFallbacks()
	writeJSON(w, http.StatusOK, v)
}

// Apps 是数据面在途连接的按应用汇总（只读）。
func (p *DataPlane) Apps(max int) ([]stack.AppFlows, bool) { return p.proxy.Apps(max) }

// ---- 安装引导的进度（决定 3）----

// SetupFile 是安装引导进度的文件（状态目录里，0600）。进度不是安全边界，标记完成不改变任何保护行为。
const SetupFile = "setup.json"

// SetupStep 是安装引导的一步。
type SetupStep struct {
	ID   string `json:"id"`
	Done bool   `json:"done"`
}

// SetupView 是 GET /v1/setup。
type SetupView struct {
	Completed   bool        `json:"completed"`
	CompletedAt time.Time   `json:"completed_at,omitempty"`
	Steps       []SetupStep `json:"steps"`
}

type setupFile struct {
	Completed   bool      `json:"completed"`
	CompletedAt time.Time `json:"completed_at"`
}

func (d *Daemon) setupPath() string { return filepath.Join(d.opts.StateDir, SetupFile) }

// mainProxyApplied：主代理已经应用过（有出口基准）。
func (d *Daemon) mainProxyApplied() bool {
	bs := d.opts.Sys.Baseline
	return bs != nil && bs.Ready() == nil
}

func (d *Daemon) setupView() SetupView {
	var f setupFile
	_, _ = readJSONFile(d.setupPath(), &f)
	return SetupView{Completed: f.Completed, CompletedAt: f.CompletedAt, Steps: []SetupStep{
		{ID: "service", Done: true}, // abroadd 在运行才答得上这个接口
		{ID: "main_proxy", Done: d.mainProxyApplied()},
	}}
}

func (d *Daemon) handleSetupGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, d.setupView())
}

func (d *Daemon) handleSetupComplete(w http.ResponseWriter, r *http.Request) {
	if !d.mainProxyApplied() {
		writeErr(w, http.StatusUnprocessableEntity, api.ErrorBody{Code: api.CodeNoBaseline, Error: "还不能完成安装引导：还差一步“应用主代理”（选好主代理节点并应用，建立出口基准）"})
		return
	}
	f := setupFile{Completed: true, CompletedAt: d.now().Truncate(time.Second)}
	b, _ := json.Marshal(f)
	if err := writePrivate(d.setupPath(), b); err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody{mainErr("保存安装引导的进度", err, "请检查状态目录。").Error()})
		return
	}
	writeJSON(w, http.StatusOK, d.setupView())
}

// writePrivate 原子写 0600 的文件（状态目录已经是 0700）。
func writePrivate(p string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(b)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if e := tmp.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// ---- 引擎与虚拟机的状态（决定 4）----

// EngineView 是一个引擎的状态（不含配置内容和端点）。
type EngineView struct {
	Name      string    `json:"name"`
	State     string    `json:"state"`
	Restarts  int       `json:"restarts"`
	Since     time.Time `json:"since,omitempty"`
	LastError string    `json:"last_error,omitempty"` // 脱敏后
}

// EnginesView 是 GET /v1/engines。
type EnginesView struct {
	Reachable   bool         `json:"reachable"` // 虚拟机答得上（不可达时其余字段为空，不报错）
	Version     string       `json:"version,omitempty"`
	Kernel      string       `json:"kernel,omitempty"`
	UptimeS     int64        `json:"uptime_s,omitempty"`
	BootChanged bool         `json:"boot_changed"` // 启动标识和 abroadd 上次看到的不同（虚拟机重启过）
	Engines     []EngineView `json:"engines"`
	// M6-2a：由 abroadd 管理虚拟机时的管理器状态（虚拟机由外部启动时没有）。虚拟机不可达时也照样给出。
	VMManager *vmmgr.Status `json:"vm_manager,omitempty"`
}

// VMHealth 是 agent health 里给这个接口用的部分。
type VMHealth struct {
	BootID  string                `json:"boot_id"`
	Version string                `json:"version"`
	Kernel  string                `json:"kernel"`
	UptimeS int64                 `json:"uptime_s"`
	Engines map[string]EngineView `json:"engines"`
}

// healthReporter 是能读 agent health 的虚拟机（ExternalVM 实现）。
type healthReporter interface {
	Health(ctx context.Context) (VMHealth, error)
}

// Health 实现 healthReporter。
func (v ExternalVM) Health(ctx context.Context) (VMHealth, error) {
	var h VMHealth
	if err := getJSON(ctx, v.ctlSock(), "http://abroad-agent/v1/health", &h); err != nil {
		return VMHealth{}, ErrVMNotReady
	}
	return h, nil
}

func (d *Daemon) handleEngines(w http.ResponseWriter, r *http.Request) {
	v := EnginesView{Engines: []EngineView{}}
	if d.vmm != nil {
		vs := d.vmm.Status()
		v.VMManager = &vs
	}
	hr, ok := d.opts.Sys.MainVM.(healthReporter)
	if !ok {
		writeJSON(w, http.StatusOK, v)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ExitVMTimeout)
	defer cancel()
	h, err := hr.Health(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, v) // 不可达：如实给出，不报错
		return
	}
	v.Reachable, v.Version, v.Kernel, v.UptimeS = true, h.Version, h.Kernel, h.UptimeS
	d.exitPushMu.Lock()
	v.BootChanged = d.mainBoot != "" && h.BootID != d.mainBoot
	d.exitPushMu.Unlock()
	for name, e := range h.Engines {
		e.Name, e.LastError = name, redact.String(e.LastError)
		v.Engines = append(v.Engines, e)
	}
	sort.Slice(v.Engines, func(i, j int) bool { return v.Engines[i].Name < v.Engines[j].Name })
	writeJSON(w, http.StatusOK, v)
}

// ---- 单独检查一个出口（决定 5）----

// ExitCheckMinInterval 是同一个出口两次手动检查的最小间隔。
const ExitCheckMinInterval = 10 * time.Second

// ExitCheckResult 是 POST /v1/exits/{tag}/check 的应答。
type ExitCheckResult struct {
	Tag    string     `json:"tag"`
	Cached bool       `json:"cached"` // 间隔内或正在跑：返回上一次的结果
	Status ExitStatus `json:"status"`
}

func (d *Daemon) handleExitCheck(w http.ResponseWriter, r *http.Request) {
	tag := r.PathValue("tag")
	if !d.exits.Has(tag) {
		writeJSON(w, http.StatusNotFound, errBody{"没有这个出口"})
		return
	}
	if d.opts.Sys.CheckExit == nil {
		writeJSON(w, http.StatusNotImplemented, errBody{"这个模式不检查出口"})
		return
	}
	ran := d.checkExitOnce(r.Context(), tag)
	st, _ := d.exitStatus(tag)
	writeJSON(w, http.StatusOK, ExitCheckResult{Tag: tag, Cached: !ran, Status: st})
}

// checkExitOnce 跑一次按出口的检查（结果进现有的可用状态）：同一出口同时只跑一个，间隔内不重复跑。返回这次是否真的跑了。
func (d *Daemon) checkExitOnce(ctx context.Context, tag string) bool {
	d.exitChk.mu.Lock()
	if d.exitChk.running == nil {
		d.exitChk.running, d.exitChk.last = map[string]bool{}, map[string]time.Time{}
	}
	now := d.now()
	if d.exitChk.running[tag] || (!d.exitChk.last[tag].IsZero() && now.Sub(d.exitChk.last[tag]) < ExitCheckMinInterval) {
		d.exitChk.mu.Unlock()
		return false
	}
	d.exitChk.running[tag] = true
	d.exitChk.mu.Unlock()
	defer func() {
		d.exitChk.mu.Lock()
		delete(d.exitChk.running, tag)
		d.exitChk.last[tag] = d.now()
		d.exitChk.mu.Unlock()
	}()
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ExitCheckTimeout)
	defer cancel()
	d.exitLoop.checkOne(cctx, tag)
	return true
}

type exitCheckGuard struct {
	mu      sync.Mutex
	running map[string]bool
	last    map[string]time.Time
}

// exitStatus 取 status 里这个出口的那一项。
func (d *Daemon) exitStatus(tag string) (ExitStatus, bool) {
	for _, x := range d.Status().Exits {
		if x.Tag == tag {
			return x, true
		}
	}
	return ExitStatus{}, false
}
