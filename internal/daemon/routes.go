package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"claude-abroad/internal/api"
	"claude-abroad/internal/envcheck"
	"claude-abroad/internal/rules"
	"claude-abroad/internal/usererror"
)

// 路由表（m5-plan v2.1 决定 4）：每个接口的路径、方法、级别（或按本次调用决定）、是否任务化、请求和应答类型都在这里；
// 处理函数在 handlerFuncs 里按 ID 对上。注册只发生在 handler()，源码检查固定（TestRoutesOnlyRegisteredHere）。

// RouteTable 返回全部接口的描述（平台无关；类型生成和级别矩阵测试都用它）。
func RouteTable() []api.Route {
	return []api.Route{
		{ID: "status", Method: "GET", Path: "/v1/status", Level: api.Read, Resp: Status{}, Note: "状态机、闸门、出口、计数、地区、环境检测摘要；带 security"},
		{ID: "enable", Method: "POST", Path: "/v1/enable", Level: api.PerCall, Async: true, Req: ConfirmRequest{}, Resp: Status{}, Note: "启用；地区会变时两步确认，签名校验推迟期间按敏感处理"},
		{ID: "disable", Method: "POST", Path: "/v1/disable", Level: api.Normal, Async: true, Resp: Status{}, Note: "停用"},
		{ID: "enable-preview", Method: "GET", Path: "/v1/enable/preview", Level: api.Read, Resp: EnablePreview{}, Note: "启用的预览（只查看：不签发确认码、不改任何状态、不拉起虚拟机；地区会不会变、会变时要结束的 Claude 进程、现在能不能启用；v0.71b）"},
		{ID: "restore", Method: "POST", Path: "/v1/restore", Level: api.Sensitive, Req: RestoreRequest{}, Resp: Status{}, Note: "回国：一律两步确认；可带 claude_proxy（回国之后第四道开或关，确认码按原值绑定；先回国、成功后才切换，结果在 claude_proxy_change；v0.72）"},
		{ID: "restore-preview", Method: "GET", Path: "/v1/restore/preview", Level: api.Read, Resp: RestorePreviewView{}, Note: "回国会结束的 Claude 进程清单和第四道的现状、冲突（只查看：不签发确认码、不改任何状态；?kill_browsers=true、?claude_proxy=on|off 按勾选预览；design 10.1、v0.72）"},
		{ID: "restore-alias", Method: "POST", Path: "/v1/actions/restore", Level: api.Sensitive, Req: RestoreRequest{}, Resp: Status{}, Note: "回国的别名"},
		{ID: "uninstall-preview", Method: "GET", Path: "/v1/uninstall/preview", Level: api.Read, Resp: UninstallPreviewView{}, Note: "卸载的预览（只查看：不签发确认码、不改任何状态、不拉起虚拟机；内容同卸载第一步；?kill_browsers=true 时按勾选浏览器预览；v0.71c）"},
		{ID: "uninstall", Method: "POST", Path: "/v1/actions/uninstall", Level: api.Sensitive, Req: ConfirmRequest{}, Resp: UninstallResult{}, Note: "一键卸载（两步确认）：结束 Claude、恢复本机网络、解除 Claude 启动管控；注销后台服务等由安装器完成"},
		{ID: "exits-list", Method: "GET", Path: "/v1/exits", Level: api.Read, Note: "分流出口摘要（不含配置原文和密钥）"},
		{ID: "exit-get", Method: "GET", Path: "/v1/exits/{tag}", Level: api.Read},
		{ID: "exit-put", Method: "PUT", Path: "/v1/exits/{tag}", Level: api.Sensitive, Note: "增加或替换分流出口"},
		{ID: "exit-delete", Method: "DELETE", Path: "/v1/exits/{tag}", Level: api.Sensitive},
		{ID: "rules-get", Method: "GET", Path: "/v1/rules", Level: api.Read, Resp: rules.File{}, Note: "当前规则：默认 YAML；?format=json 返回同样内容的 JSON（可原样 PUT 回来）"},
		{ID: "rules-put", Method: "PUT", Path: "/v1/rules", Level: api.PerCall, Resp: Status{}, Note: "改规则；放松保护时是敏感（api.RulesLevel）"},
		{ID: "main-get", Method: "GET", Path: "/v1/main-proxy", Level: api.Read, Resp: MainProxyView{}, Note: "主代理：保存的选择、是否已下发、是否需要重新选择、局域网节点是否待确认（不含服务器、密码、订阅地址）"},
		{ID: "main-select", Method: "PUT", Path: "/v1/main-proxy", Level: api.Sensitive, Req: MainSelectRequest{}, Resp: MainProxyResult{}, Note: "选节点或链并应用（已启用时：关闸门、换第一跳、下发、检查、失败回滚）"},
		{ID: "main-exit", Method: "GET", Path: "/v1/main-proxy/exit", Level: api.Read, Resp: MainExitView{}, Note: "主代理出口的 IP、国家、城市、时区（来自出口基准）：“应答里不含出口 IP”的唯一例外，只给界面显示"},
		{ID: "main-check", Method: "POST", Path: "/v1/main-proxy/check", Level: api.Normal, Resp: BaselineSummary{}, Note: "检查当前主代理（只检查，不保存）"},
		{ID: "main-sources", Method: "GET", Path: "/v1/main-proxy/sources", Level: api.Read, Resp: MainProxySourceView{}, Note: "主代理的来源（不含订阅地址和节点的服务器、密码）"},
		{ID: "main-source-add", Method: "POST", Path: "/v1/main-proxy/sources", Level: api.Sensitive, Req: AddSourceRequest{}, Resp: SourceAdded{}, Note: "添加来源：订阅、本地配置内容、手动节点"},
		{ID: "main-source-delete", Method: "DELETE", Path: "/v1/main-proxy/sources/{id}", Level: api.Sensitive, Note: "删除来源（正在使用的拒绝）"},
		{ID: "main-source-refresh", Method: "POST", Path: "/v1/main-proxy/sources/{id}/refresh", Level: api.Normal, Req: RefreshRequest{}, Resp: SourceAdded{}, Note: "更新订阅（只更新节点列表，不下发）"},
		{ID: "main-lan-confirm", Method: "POST", Path: "/v1/main-proxy/lan-confirm", Level: api.Sensitive, Req: LANConfirmRequest{}, Note: "确认局域网节点（绑定来源、节点、第一跳地址）"},
		{ID: "main-proxy-apply", Method: "POST", Path: "/v1/main-proxy/apply", Level: api.Sensitive, Req: MainProxyRequest{}, Resp: MainProxyResult{}, Note: "应用主代理"},
		{ID: "region-get", Method: "GET", Path: "/v1/region", Level: api.Read, Resp: regionStatus{}},
		{ID: "region-put", Method: "PUT", Path: "/v1/region", Level: api.PerCall, Req: RegionRequest{}, Resp: Status{}, Note: "切换地区；地区会变时两步确认，签名校验推迟期间按敏感处理"},
		{ID: "envcheck-get", Method: "GET", Path: "/v1/envcheck", Level: api.Read},
		{ID: "envcheck-run", Method: "POST", Path: "/v1/envcheck/run", Level: api.Normal, Resp: envcheck.Report{}},
		{ID: "envcheck-history", Method: "GET", Path: "/v1/envcheck/history", Level: api.Read, Resp: EnvHistoryView{}, Note: "环境检测的历史（补齐四；每条只有时间、总判定、各项的标识和结果）"},
		{ID: "envcheck-history-clear", Method: "DELETE", Path: "/v1/envcheck/history", Level: api.Normal, Resp: EnvHistoryView{}, Note: "清空环境检测的历史"},
		{ID: "envcheck-device", Method: "POST", Path: "/v1/envcheck/device", Level: api.Normal, Resp: envcheck.DeviceReport{}, Note: "设备体检（字体等）：只返回结果，不参与放行"},
		{ID: "diagnostics", Method: "GET", Path: "/v1/diagnostics", Level: api.Read, Resp: Diagnostics{}, Note: "诊断包：版本、状态、最近的环境检测、启动管控、事件（整段脱敏）"},
		{ID: "envcheck-settings-get", Method: "GET", Path: "/v1/envcheck/settings", Level: api.Read, Resp: EnvSettingsRequest{}},
		{ID: "envcheck-settings-put", Method: "PUT", Path: "/v1/envcheck/settings", Level: api.Normal, Req: EnvSettingsRequest{}, Resp: EnvSettingsRequest{}},
		{ID: "lan-get", Method: "GET", Path: "/v1/settings/lan", Level: api.Read, Resp: LANSetting{}, Note: "“允许局域网”开关（补齐三）"},
		{ID: "lan-put", Method: "PUT", Path: "/v1/settings/lan", Level: api.PerCall, Req: LANSetting{}, Resp: LANSetting{}, Note: "改“允许局域网”：关是收紧；从关到开是放松（敏感）"},
		{ID: "claude-proxy-get", Method: "GET", Path: "/v1/settings/claude-proxy", Level: api.Read, Resp: ClaudeProxyView{}, Note: "第四道（Claude Code 的本机代理）的开关和状态（默认关；没打开启动管控时 available 为假）"},
		{ID: "claude-proxy-put", Method: "PUT", Path: "/v1/settings/claude-proxy", Level: api.Sensitive, Req: ClaudeProxySetting{}, Resp: ClaudeProxyView{}, Note: "打开或关闭第四道：两个方向都要本人授权；打开时别的托管设置定义了代理变量就拒绝（422，conflicts 列出文件和变量）"},
		{ID: "preflight", Method: "GET", Path: "/v1/preflight", Level: api.Read, Resp: PreflightView{}, Note: "启用前的冲突检查（脱敏；最小间隔 2 秒）"},
		{ID: "apps", Method: "GET", Path: "/v1/apps", Level: api.Read, Resp: AppsView{}, Note: "在联网的应用（不含目的地址、域名、用户目录）"},
		{ID: "setup-get", Method: "GET", Path: "/v1/setup", Level: api.Read, Resp: SetupView{}, Note: "安装引导的进度"},
		{ID: "setup-complete", Method: "POST", Path: "/v1/setup/complete", Level: api.Normal, Resp: SetupView{}, Note: "完成安装引导（主代理没应用过时 422）"},
		{ID: "engines", Method: "GET", Path: "/v1/engines", Level: api.Read, Resp: EnginesView{}, Note: "引擎与虚拟机的状态（不含配置和端点）"},
		{ID: "claude-domains", Method: "GET", Path: "/v1/claude-domains", Level: api.Read, Resp: ClaudeDomainsView{}, Note: "启用时 Claude 访问过的、不在内置 Claude 域名之下的域名（只观察；本机、本人可看；不含地址）"},
		{ID: "claude-domains-clear", Method: "DELETE", Path: "/v1/claude-domains", Level: api.Normal, Resp: ClaudeDomainsView{}, Note: "清空 Claude 访问过的域名"},
		{ID: "launchguard", Method: "GET", Path: "/v1/launchguard", Level: api.Read, Resp: LaunchGuardView{}, Note: "启动管控：现在是否允许启动 Claude、四层各自是否生效、第三层的覆盖范围、最近的拦截（不含命令行和路径）"},
		{ID: "exit-check", Method: "POST", Path: "/v1/exits/{tag}/check", Level: api.Normal, Resp: ExitCheckResult{}, Note: "单独检查一个分流出口（同一出口同时只跑一个，有最小间隔）"},
		{ID: "job-get", Method: "GET", Path: "/v1/jobs/{id}", Level: api.Read, Resp: Job{}, Note: "任务的状态和结果（只在内存里，保留最近 16 个）"},
		{ID: "events", Method: "GET", Path: "/v1/events", Level: api.Read, Resp: StreamEvent{}, Note: "事件推送（Server-Sent Events）"},
	}
}

// handlerFuncs 把路由 ID 对上处理函数。
func (d *Daemon) handlerFuncs() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"status":                 d.handleStatus,
		"enable":                 d.handleEnable,
		"disable":                d.handleDisable,
		"restore":                d.handleRestore,
		"restore-alias":          d.handleRestore,
		"restore-preview":        d.handleRestorePreview,
		"enable-preview":         d.handleEnablePreview,
		"uninstall-preview":      d.handleUninstallPreview,
		"uninstall":              d.handleUninstall,
		"exits-list":             d.handleExitsList,
		"exit-get":               d.handleExitGet,
		"exit-put":               d.handleExitPut,
		"exit-delete":            d.handleExitDelete,
		"rules-get":              d.handleGetRules,
		"rules-put":              d.handlePutRules,
		"main-get":               d.handleMainGet,
		"main-exit":              d.handleMainExit,
		"main-select":            d.handleMainSelect,
		"main-check":             d.handleMainCheck,
		"main-sources":           d.handleSourcesList,
		"main-source-add":        d.handleSourceAdd,
		"main-source-delete":     d.handleSourceDelete,
		"main-source-refresh":    d.handleSourceRefresh,
		"main-lan-confirm":       d.handleLANConfirm,
		"main-proxy-apply":       d.handleApplyMainProxy,
		"region-get":             d.handleGetRegion,
		"region-put":             d.handleSwitchRegion,
		"envcheck-get":           d.handleEnvGet,
		"envcheck-run":           d.handleEnvRun,
		"envcheck-history":       d.handleEnvHistory,
		"envcheck-history-clear": d.handleEnvHistoryClear,
		"envcheck-settings-get":  d.handleEnvSettingsGet,
		"envcheck-settings-put":  d.handleEnvSettings,
		"lan-get":                d.handleLANGet,
		"lan-put":                d.handleLANPut,
		"claude-proxy-get":       d.handleClaudeProxyGet,
		"claude-proxy-put":       d.handleClaudeProxyPut,
		"preflight":              d.handlePreflight,
		"apps":                   d.handleApps,
		"setup-get":              d.handleSetupGet,
		"setup-complete":         d.handleSetupComplete,
		"engines":                d.handleEngines,
		"launchguard":            d.handleLaunchGuard,
		"claude-domains":         d.handleClaudeDomains,
		"claude-domains-clear":   d.handleClaudeDomainsClear,
		"envcheck-device":        d.handleDeviceCheck,
		"diagnostics":            d.handleDiagnostics,
		"exit-check":             d.handleExitCheck,
		"job-get":                d.handleJobGet,
		"events":                 d.handleEvents,
	}
}

// handler 是唯一的注册入口：按路由表注册，每个接口先过级别检查。
func (d *Daemon) handler() http.Handler {
	routes := RouteTable()
	if err := api.CheckRoutes(routes); err != nil {
		panic("路由表不对：" + err.Error()) // 由测试保证不会发生
	}
	fns := d.handlerFuncs()
	mux := http.NewServeMux()
	for _, rt := range routes {
		fn, ok := fns[rt.ID]
		if !ok {
			panic("路由 " + rt.ID + " 没有处理函数")
		}
		mux.Handle(rt.Pattern(), d.guard(rt, fn))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := int64(MaxBody)
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/exits/") {
			limit = MaxExitBody // 出口的配置（订阅最大 1 MiB）
		}
		if r.ContentLength > limit {
			writeJSON(w, http.StatusRequestEntityTooLarge, errBody{"请求体太大"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		mux.ServeHTTP(w, r)
	})
}

// guard 是级别检查：调用方不够格时 403（只读和普通对控制台用户开放；敏感只有完全信任的调用方）。
func (d *Daemon) guard(rt api.Route, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !api.Allowed(callerOf(r), rt.Level) {
			writeJSON(w, http.StatusForbidden, errBody{api.NeedAuthText})
			return
		}
		// 一键卸载（M6-5a 补审返工）：卸载进行中，除查询外一律“忙”；卸载完成后，除回国、再次卸载和设备体检外，
		// 会接管网络或改管控的操作一律拒绝（卸载后还能启用，回国后就没有任何管控了）。
		if r.Method != http.MethodGet {
			if d.uninstalling.Load() {
				writeErr(w, http.StatusConflict, api.ErrorBody{Code: api.CodeBusy, Error: usererror.New("执行操作", "正在一键卸载", "等卸载完成。", nil).Error()})
				return
			}
			if !AllowedAfterUninstall[rt.ID] && d.uninstalled() {
				writeErr(w, http.StatusConflict, api.ErrorBody{Code: api.CodeUninstalled, Error: UninstalledText})
				return
			}
		}
		fn(w, r)
	}
}

// AllowedAfterUninstall 是卸载完成后仍然允许的改动类接口（回国和再次卸载是幂等的；设备体检只读）。
var AllowedAfterUninstall = map[string]bool{"restore": true, "restore-alias": true, "uninstall": true, "envcheck-device": true, "claude-domains-clear": true}

// ErrUninstalledOp 是正在卸载或已经卸载时，改动类操作在状态机里被拒（状态机的 Admit）。
var ErrUninstalledOp = errors.New("Claude 出国正在卸载或已经卸载")

// UninstalledText 是卸载后被拒时给用户的话。
var UninstalledText = usererror.New("执行操作", "Claude 出国已经一键卸载", "要继续使用请重新安装。", nil).Error()

// requireSensitive 用在“按本次调用决定”的接口算出敏感之后：不够格时写 403 并返回 false。
func requireSensitive(w http.ResponseWriter, r *http.Request, why string) bool {
	if api.AllowedSensitive(callerOf(r)) {
		return true
	}
	msg := api.NeedAuthText
	if why != "" {
		msg += "（" + why + "）"
	}
	writeJSON(w, http.StatusForbidden, errBody{msg})
	return false
}

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	if _, err := io.ReadAll(r.Body); err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, errBody{"请求体超过 64 KiB"})
		return
	}
	st := d.Status()
	st.Security = &api.Security{CallerVerification: d.verifier().Name(), Note: api.SecurityNote, Caller: callerOf(r).String()}
	if d.verifier().Enabled() {
		st.Security.Note = ""
	}
	writeJSON(w, http.StatusOK, st)
}

func (d *Daemon) verifier() api.CallerVerifier { return api.Disabled{} }

func (d *Daemon) handleGetRegion(w http.ResponseWriter, r *http.Request) {
	rs := d.regionStatus()
	if rs == nil {
		writeJSON(w, http.StatusNotImplemented, errBody{"这个模式没有地区同步"})
		return
	}
	writeJSON(w, http.StatusOK, rs)
}

func (d *Daemon) handleEnvSettingsGet(w http.ResponseWriter, r *http.Request) {
	if d.envSettings == nil {
		writeJSON(w, http.StatusNotImplemented, errBody{"这个模式没有环境检测"})
		return
	}
	var ids []string
	for id, off := range d.envSettings.Disabled() {
		if off {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if ids == nil {
		ids = []string{}
	}
	writeJSON(w, http.StatusOK, EnvSettingsRequest{Disabled: ids})
}

// ---- 调用方身份 ----

type callerKey struct{}

// callerOf 取这个请求的调用方身份（没有时按“其他”，什么都做不了）。
func callerOf(r *http.Request) api.Caller {
	if c, ok := r.Context().Value(callerKey{}).(api.Caller); ok {
		return c
	}
	return api.Other
}

// DefaultMaxConns 是同时存在的连接数上限（m5-plan v2.1：防止别的本机用户用大量连接占满）。
const DefaultMaxConns = 32

// callerConn 是带调用方身份的连接；关闭时把连接数减回去。
type callerConn struct {
	net.Conn
	caller api.Caller
	once   sync.Once
	n      *atomic.Int64
}

func (c *callerConn) Close() error {
	c.once.Do(func() { c.n.Add(-1) })
	return c.Conn.Close()
}

// peerListener 按对端 uid 归类（api.Classify）：完全信任、控制台用户放行，其余在解析任何请求之前断开；
// 超过连接数上限的也断开。
type peerListener struct {
	net.Listener
	uid      int // 状态目录的属主（开发模式是当前用户，真实模式是 root）
	peer     func(*net.UnixConn) (int, error)
	console  func() (int, error)
	onReject func()
	max      int64
	n        atomic.Int64
}

func (l *peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		caller := api.Other
		if uc, ok := c.(*net.UnixConn); ok {
			if id, err := l.peer(uc); err == nil {
				cons := -1
				if l.console != nil {
					if u, err := l.console(); err == nil {
						cons = u
					}
				}
				caller = api.Classify(id, l.uid, cons)
			}
		}
		if caller != api.Other && l.n.Load() < l.max {
			l.n.Add(1)
			return &callerConn{Conn: c, caller: caller, n: &l.n}, nil
		}
		// 先记拒绝再关：对端看到连接被关时，计数已经加上（测试据此断言，不会读到旧值）。
		if l.onReject != nil {
			l.onReject()
		}
		c.Close()
	}
}

// connContext 把连接上的调用方身份放进请求的 context。
func connContext(ctx context.Context, c net.Conn) context.Context {
	if cc, ok := c.(*callerConn); ok {
		return context.WithValue(ctx, callerKey{}, cc.caller)
	}
	return ctx
}

var _ = fmt.Sprintf
