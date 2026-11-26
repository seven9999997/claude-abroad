package daemon

import (
	"claude-abroad/internal/api"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	"claude-abroad/internal/exitpolicy"
	"claude-abroad/internal/gatesm"
	"claude-abroad/internal/managedproxy"
	"claude-abroad/internal/state"
	"claude-abroad/internal/usererror"
	"claude-abroad/internal/usernet"
)

// 主代理的接口（M5-4）：查询、来源的增删与刷新、选择并应用、检查、局域网节点的确认。下发顺序和回滚见 switchMain。

// SourceFetchMax、SourceFetchTimeout 是抓取订阅的上限。
const (
	SourceFetchMax     = managedproxy.MaxImport
	SourceFetchTimeout = 30 * time.Second
)

// MainSelectRequest 是 PUT /v1/main-proxy：选节点（或链）并应用。
type MainSelectRequest struct {
	Selected string          `json:"selected"`
	Chain    []string        `json:"chain,omitempty"`
	Rule     exitpolicy.Rule `json:"rule,omitempty"`
	Confirm  string          `json:"confirm,omitempty"`
}

// MainSelectResult 是 PUT /v1/main-proxy 在未启用时的应答（只保存，启用时下发）。
type MainSelectResult struct {
	Saved   bool   `json:"saved"`
	Applied bool   `json:"applied"`
	Note    string `json:"note,omitempty"`
}

// LANConfirmRequest 是 POST /v1/main-proxy/lan-confirm。
type LANConfirmRequest struct {
	Node string `json:"node"`
}

// SourceAdded 是添加来源的应答。
type SourceAdded struct {
	ID            string                `json:"id"`
	NeedsReselect bool                  `json:"needs_reselect,omitempty"`
	Sources       []MainProxySourceView `json:"sources"`
}

func (d *Daemon) mainStore() *MainStore { return d.main }

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody{"请求体不是合法的 JSON，或有不认识的字段"})
		return false
	}
	return true
}

func (d *Daemon) handleMainGet(w http.ResponseWriter, r *http.Request) {
	v, err := d.main.view()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody{mainErr("读主代理的配置", err, "请检查状态目录。").Error()})
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (d *Daemon) handleSourcesList(w http.ResponseWriter, r *http.Request) {
	d.main.mu.Lock()
	s, err := d.main.load()
	d.main.mu.Unlock()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody{mainErr("读主代理的来源", err, "请检查状态目录。").Error()})
		return
	}
	vs := sourceViews(s)
	for i := range vs { // 补齐二：每个节点最近一次的 UDP 结果
		for j := range vs[i].Nodes {
			vs[i].Nodes[j].UDP = d.nodeUDP(vs[i].Nodes[j].ID)
		}
	}
	writeJSON(w, http.StatusOK, vs)
}

// fetchSource 抓取订阅（v2.2 决定 5）：启用时经主代理（abroadd 的流量和本机其他流量一样进虚拟网卡）；未启用时必须由调用方
// 显式带 via_host，否则拒绝。错误里不带订阅地址（只给分类）。
// ErrViaHostRequired 是未启用时抓取订阅、又没有显式选择经本机网络（代码 via_host_required）。
var ErrViaHostRequired = errors.New("现在没有启用，没有主代理可走：要经本机网络抓取订阅，请明确选择（via_host）；否则先启用再更新")

// fetchErr 写抓取订阅失败的应答（经本机网络的要求给特定代码）。
func fetchErr(w http.ResponseWriter, code int, err error) {
	b := api.ErrorBody{Error: err.Error()}
	if errors.Is(err, ErrViaHostRequired) {
		b.Code, b.Field = api.CodeViaHostRequired, "via_host"
	}
	writeErr(w, code, b)
}

func (d *Daemon) fetchSource(ctx context.Context, url string, viaHost bool) ([]byte, int, error) {
	f := d.opts.Sys.FetchSource
	if f == nil {
		return nil, http.StatusNotImplemented, errors.New("这个模式不能抓取订阅")
	}
	enabled := d.fileState() == state.Enabled
	if !enabled && !viaHost {
		return nil, http.StatusUnprocessableEntity, ErrViaHostRequired
	}
	ctx, cancel := context.WithTimeout(ctx, SourceFetchTimeout)
	defer cancel()
	b, err := f(ctx, url, !enabled)
	if err != nil {
		return nil, http.StatusBadGateway, usererror.New("抓取订阅", classifyFetchErr(err), "请检查订阅是否有效、主代理是否可用，稍后再试；原有的节点没有改动。", nil)
	}
	if len(b) > SourceFetchMax {
		return nil, http.StatusBadGateway, usererror.New("抓取订阅", "订阅内容超过上限", "请换一个更小的订阅；原有的节点没有改动。", nil)
	}
	return b, 0, nil
}

// classifyFetchErr 只给错误的类别（底层错误里常带订阅地址，不能原样给出）。
func classifyFetchErr(err error) string {
	var ne interface{ Timeout() bool }
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "超时"
	case errors.Is(err, ErrFetchTooLarge):
		return "订阅内容超过上限"
	case errors.Is(err, ErrFetchStatus):
		return "订阅服务器返回了错误"
	case errors.Is(err, ErrFetchRedirect):
		return "订阅地址跳转到了别的协议"
	}
	return "连不上订阅服务器"
}

// 抓取的错误类别（FetchSource 的实现用它们包装）。
var (
	ErrFetchTooLarge = errors.New("订阅内容超过上限")
	ErrFetchStatus   = errors.New("订阅服务器返回了错误")
	ErrFetchRedirect = errors.New("订阅地址跳转到了别的协议")
)

func (d *Daemon) handleSourceAdd(w http.ResponseWriter, r *http.Request) {
	var req AddSourceRequest
	if !decodeBody(w, r, &req) {
		return
	}
	var data []byte
	url := ""
	switch req.Kind {
	case "subscription":
		u := strings.TrimSpace(req.URL)
		if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
			writeErr(w, http.StatusUnprocessableEntity, api.ErrorBody{Error: "订阅地址要以 http:// 或 https:// 开头", Field: "url"})
			return
		}
		b, code, err := d.fetchSource(r.Context(), u, req.ViaHost)
		if err != nil {
			fetchErr(w, code, err)
			return
		}
		data, url = b, u
	case "yaml":
		data = []byte(req.Content)
	case "node":
		b, err := nodeYAML(req.Name, req.Node)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, errBody{err.Error()})
			return
		}
		data = b
	default:
		writeJSON(w, http.StatusUnprocessableEntity, errBody{"kind 只能是 subscription、yaml 或 node"})
		return
	}
	id, err := d.main.addSource(req.Name, url, data)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, errBody{mainErr("添加主代理的来源", err, "请检查配置；没有保存任何东西。").Error()})
		return
	}
	d.events.Add("main_proxy", "source_added", "来源已添加")
	d.main.mu.Lock()
	s, _ := d.main.load()
	d.main.mu.Unlock()
	writeJSON(w, http.StatusOK, SourceAdded{ID: id, Sources: sourceViews(s)})
}

func (d *Daemon) handleSourceDelete(w http.ResponseWriter, r *http.Request) {
	err := d.main.deleteSource(r.PathValue("id"))
	switch {
	case errors.Is(err, os.ErrNotExist):
		writeJSON(w, http.StatusNotFound, errBody{"没有这个来源"})
	case errors.Is(err, ErrMainInUse):
		writeErr(w, http.StatusConflict, api.ErrorBody{Code: api.CodeInUse, Error: err.Error()})
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, errBody{mainErr("删除主代理的来源", err, "请再试一次。").Error()})
	default:
		d.events.Add("main_proxy", "source_deleted", "来源已删除")
		w.WriteHeader(http.StatusNoContent)
	}
}

func (d *Daemon) handleSourceRefresh(w http.ResponseWriter, r *http.Request) {
	var req RefreshRequest
	if r.ContentLength != 0 && !decodeBody(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	d.main.mu.Lock()
	s, err := d.main.load()
	d.main.mu.Unlock()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody{err.Error()})
		return
	}
	url := ""
	found := false
	for _, src := range s.Sources {
		if src.ID == id {
			url, found = src.URL, true
		}
	}
	if !found {
		writeJSON(w, http.StatusNotFound, errBody{"没有这个来源"})
		return
	}
	if url == "" {
		writeJSON(w, http.StatusUnprocessableEntity, errBody{"这个来源不是订阅，不能更新；要换内容请删除后重新添加"})
		return
	}
	b, code, err := d.fetchSource(r.Context(), url, req.ViaHost)
	if err != nil {
		fetchErr(w, code, err)
		return
	}
	reselect, err := d.main.refreshSource(id, b)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, errBody{mainErr("更新订阅", err, "原有的节点没有改动。").Error()})
		return
	}
	if reselect {
		d.events.Add("main_proxy", "needs_reselect", "订阅更新后选中的节点不在了：请重新选择（正在运行的主代理不受影响）")
	} else {
		d.events.Add("main_proxy", "source_refreshed", "订阅已更新")
	}
	d.main.mu.Lock()
	s, _ = d.main.load()
	d.main.mu.Unlock()
	writeJSON(w, http.StatusOK, SourceAdded{ID: id, NeedsReselect: reselect, Sources: sourceViews(s)})
}

func (d *Daemon) handleLANConfirm(w http.ResponseWriter, r *http.Request) {
	var req LANConfirmRequest
	if !decodeBody(w, r, &req) {
		return
	}
	n, err := d.main.confirmLAN(req.Node)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, errBody{mainErr("确认局域网节点", err, "请检查节点。").Error()})
		return
	}
	d.events.Add("main_proxy", "lan_confirmed", "局域网节点已确认")
	writeJSON(w, http.StatusOK, map[string]int{"confirmed": n})
}

func (d *Daemon) handleMainCheck(w http.ResponseWriter, r *http.Request) {
	bs := d.opts.Sys.Baseline
	if bs == nil {
		writeJSON(w, http.StatusNotImplemented, errBody{"这个模式没有出口检查"})
		return
	}
	base, err := bs.Probe(r.Context(), exitpolicy.Rule(r.URL.Query().Get("rule")))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errBody{mainErr("检查主代理", err, "请检查主代理是否可用。").Error()})
		return
	}
	writeJSON(w, http.StatusOK, summarizeBaseline(base))
}

// handleMainSelect 是 PUT /v1/main-proxy：选节点（或链）并应用。未启用时只保存（启用时下发）；已启用时走 switchMain。
func (d *Daemon) handleMainSelect(w http.ResponseWriter, r *http.Request) {
	var req MainSelectRequest
	if !decodeBody(w, r, &req) {
		return
	}
	d.main.mu.Lock()
	s, err := d.main.load()
	d.main.mu.Unlock()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody{err.Error()})
		return
	}
	cand := s
	cand.Selected, cand.Chain = req.Selected, req.Chain
	if len(cand.Chain) == 1 {
		cand.Chain = nil
	}
	p, err := d.main.plan(cand)
	if err != nil {
		b := api.ErrorBody{Error: mainErr("应用主代理", err, mainPlanHint(err)).Error()}
		if errors.Is(err, ErrMainLANPending) {
			b.Code, b.Node = api.CodeLANUnconfirmed, cand.Selected // 第一跳：有链时是链的第一个
			if len(cand.Chain) > 0 {
				b.Node = cand.Chain[0]
			}
		}
		writeErr(w, http.StatusUnprocessableEntity, b)
		return
	}
	if d.fileState() != state.Enabled {
		d.main.mu.Lock()
		err := d.main.save(p.store)
		if err == nil {
			err = writeJSONFile(d.main.file("pending.json"), map[string]string{"digest": p.digest})
		}
		d.main.mu.Unlock()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errBody{err.Error()})
			return
		}
		d.events.Add("main_proxy", "saved", "主代理的选择已保存，启用时下发")
		writeJSON(w, http.StatusOK, MainSelectResult{Saved: true, Note: "现在没有启用：已保存，启用时下发并检查"})
		return
	}
	d.switchMain(w, r, p, MainProxyRequest{Rule: req.Rule, Confirm: req.Confirm})
}

// HopPrivateHint 是第一跳域名解析到局域网地址时给用户的提示。
const HopPrivateHint = "局域网里的代理请直接填 IP 地址"

// mainErr 组装面向用户的错误：中文的产品规则原样作为原因，系统错误按 usererror 归类（不原样给出底层文字）。
func mainErr(action string, err error, next string) error {
	if err == nil {
		return nil
	}
	reason := usererror.Reason(err)
	if reason == "" {
		if strings.IndexFunc(err.Error(), func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0 {
			reason = err.Error()
		} else {
			reason = "未能取得可识别的错误原因"
		}
	}
	return usererror.New(action, reason, next, err)
}

func mainPlanHint(err error) string {
	switch {
	case errors.Is(err, ErrMainLANPending):
		return "请先在应用里确认这个局域网节点（POST /v1/main-proxy/lan-confirm），再应用。"
	case errors.Is(err, ErrMainLoopback):
		return "请换成远端或局域网的节点。"
	}
	return "请检查选择的节点。"
}

// switchMain 是已启用时“应用主代理”的完整顺序（v2.2 决定 2）：关闸门（正在更换主代理）→ 替换第一跳名单 → 下发新配置
// （agent 等引擎端口就绪才返回）→ 带位置查询的出口检查 → 地区（会变时两步确认）、保存基准 → 保存选择和“已下发”记录
// → 清掉关闭原因、叫醒复查。任何一步失败（包括要确认、被拒）都恢复旧的第一跳名单和旧配置并重新下发，闸门由复查决定。
func (d *Daemon) switchMain(w http.ResponseWriter, r *http.Request, p mainPlan, req MainProxyRequest) {
	vm, bs := d.opts.Sys.MainVM, d.opts.Sys.Baseline
	if vm == nil || bs == nil {
		writeJSON(w, http.StatusNotImplemented, errBody{"这个模式不能下发主代理"})
		return
	}
	d.exitPushMu.Lock()
	defer d.exitPushMu.Unlock()
	old, _ := d.main.applied()
	oldBase, oldBaseErr := os.ReadFile(bs.Path) // 更换前的基准（保存失败时整体回退要用；没有时为空）
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*MainPushTimeout+ExitCheckTimeout)
	defer cancel()
	d.gate.AddReason(gatesm.ReasonMainSwitch, "正在更换主代理")
	done := func() {
		d.gate.ClearReason(gatesm.ReasonMainSwitch)
		d.re.Kick()
	}
	// rollback 回到旧的第一跳和旧配置；回退的下发没成功时返回 false（事件和应答要如实，下一轮会重新下发）。
	rollback := func() bool {
		ok := d.restoreMain(ctx, vm, old)
		done()
		return ok
	}
	hint := "" // 能照着做的提示（例如第一跳域名解析到了局域网地址），放在“下一步”的最前面
	next := func(restored bool) string {
		if restored {
			return hint + "已经回到原来的主代理；请检查新节点后再试。"
		}
		return hint + "新节点没有生效；恢复原来的主代理没有完成，稍后会自动重试。"
	}
	fail := func(code int, what string, err error) {
		restored := rollback()
		d.events.Add("main_proxy", "switch_failed", what+"："+usererror.Reason(err))
		c := api.CodeSwitchRolledBack
		if !restored {
			c = api.CodeSwitchRollbackIncomplete
		}
		writeErr(w, code, api.ErrorBody{Code: c, Error: mainErr(what, err, next(restored)).Error()})
	}
	hp0, hpErr := vm.HopPrivate(ctx)
	if err := vm.PushMainHops(ctx, p.hops); err != nil {
		fail(http.StatusBadGateway, "替换虚拟机出站白名单里的主代理第一跳", err)
		return
	}
	if err := vm.PushMainProxy(ctx, p.yaml); err != nil {
		fail(http.StatusBadGateway, "把新的主代理配置下发到虚拟机", err)
		return
	}
	d.setPushedMain(p.store.Selected) // 补齐二：接下来的检查结果记在新节点名下
	base, err := bs.Probe(ctx, req.Rule)
	if err != nil {
		// 第一跳域名解析到了局域网地址（网关不放行，v2.3 偏差的结论）：给出能照着做的提示。
		if hp1, err2 := vm.HopPrivate(ctx); hpErr == nil && err2 == nil && hp1 > hp0 {
			hint = "节点的域名解析到了局域网地址，" + HopPrivateHint + "；"
		}
		fail(http.StatusUnprocessableEntity, "检查新的主代理", err)
		return
	}
	// 后半段（地区、基准）的应答先暂存：选择和“已下发”记录保存成功之后才发出去（M5-4 建议改 1）。
	rec := &bufferedResponse{header: http.Header{}}
	if !d.finishMainProxy(rec, r, req, base) { // 要确认 409、被拒 403、出错：回退后照原样转发
		rollback()
		rec.flushTo(w)
		return
	}
	d.main.mu.Lock()
	err = d.main.save(p.store)
	if err == nil {
		err = d.main.saveApplied(appliedRecord{Store: p.store, Digest: p.digest, Hops: p.hops})
	}
	if err == nil {
		_ = os.Remove(d.main.file("pending.json"))
	}
	d.main.mu.Unlock()
	if err != nil {
		// 新主代理已经生效、但没记下：不能报告成功（下一轮核对会按旧记录把旧的下发回去）。整体回退，含基准。
		if oldBaseErr == nil {
			_ = managedproxy.Write(bs.Path, oldBase)
		} else {
			_ = os.Remove(bs.Path)
		}
		fail(http.StatusInternalServerError, "保存主代理的选择", err)
		return
	}
	d.events.Add("main_proxy", "switched", "主代理已更换")
	done()
	rec.flushTo(w)
}

// bufferedResponse 暂存一个应答（状态码、头、正文），确定要发时再写给真正的 ResponseWriter。
type bufferedResponse struct {
	header http.Header
	code   int
	body   []byte
}

func (b *bufferedResponse) Header() http.Header { return b.header }
func (b *bufferedResponse) WriteHeader(code int) {
	if b.code == 0 {
		b.code = code
	}
}
func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.code == 0 {
		b.code = http.StatusOK
	}
	b.body = append(b.body, p...)
	return len(p), nil
}
func (b *bufferedResponse) flushTo(w http.ResponseWriter) {
	for k, v := range b.header {
		w.Header()[k] = v
	}
	if b.code == 0 {
		b.code = http.StatusOK
	}
	w.WriteHeader(b.code)
	_, _ = w.Write(b.body)
}

// restoreMain 把虚拟机恢复到旧的第一跳名单和旧配置（没有旧记录时清空第一跳名单：新配置不会被放行）。
// 回退的下发有一步没成功时返回 false：发“回退没有完成”的事件，并让下一轮的核对无条件重新下发（mainDirty）。
func (d *Daemon) restoreMain(ctx context.Context, vm MainProxyVM, old *appliedRecord) bool {
	var errs []error
	if old == nil {
		errs = append(errs, vm.PushMainHops(ctx, usernet.MainHops{}))
	} else if cfg, err := old.Store.VMRuntimeConfig(); err != nil {
		errs = append(errs, vm.PushMainHops(ctx, usernet.MainHops{}), err)
	} else {
		errs = append(errs, vm.PushMainHops(ctx, old.Hops), vm.PushMainProxy(ctx, cfg.YAML))
	}
	if errors.Join(errs...) != nil {
		d.mainDirty = true
		d.events.Add("main_proxy", "rollback_incomplete", "回退没有完成，下一轮会重试")
		return false
	}
	if old == nil {
		d.setPushedMain("")
		d.events.Add("main_proxy", "rolled_back", "没有旧的主代理可以恢复：清空了第一跳名单")
	} else {
		d.setPushedMain(old.Store.Selected)
		d.events.Add("main_proxy", "rolled_back", "已回到原来的主代理")
	}
	return true
}

// managedMain 报告 abroadd 是否持有主代理的配置（v2.2 决定 8：有“已下发”记录或“待下发”的选择）。开发工具手动下发的
// 那条路（真机脚本、测试）没有这两个文件，abroadd 不碰它。
func (d *Daemon) managedMain() bool {
	if d.main == nil {
		return false
	}
	if a, _ := d.main.applied(); a != nil {
		return true
	}
	_, err := os.Stat(d.main.file("pending.json"))
	return err == nil
}

// ensureMainPushed 在启用之前调用：abroadd 持有配置、而虚拟机里不是这份时，先替换第一跳名单、下发配置。
func (d *Daemon) ensureMainPushed(ctx context.Context) error {
	vm := d.opts.Sys.MainVM
	if vm == nil || !d.managedMain() {
		return nil
	}
	d.exitPushMu.Lock()
	defer d.exitPushMu.Unlock()
	return d.pushManagedLocked(ctx, vm, "启用")
}

// pushManagedLocked 按保存的选择下发（调用方持有 exitPushMu）。
func (d *Daemon) pushManagedLocked(ctx context.Context, vm MainProxyVM, why string) error {
	d.main.mu.Lock()
	s, err := d.main.load()
	d.main.mu.Unlock()
	if err != nil {
		return err
	}
	p, err := d.main.plan(s)
	if err != nil {
		return mainErr("下发主代理", err, mainPlanHint(err))
	}
	if cur, err := vm.MainProxyDigest(ctx); err == nil && cur == p.digest {
		if a, _ := d.main.applied(); a != nil && a.Digest == p.digest {
			return nil
		}
	}
	if err := vm.PushMainHops(ctx, p.hops); err != nil {
		return err
	}
	if err := vm.PushMainProxy(ctx, p.yaml); err != nil {
		return err
	}
	d.setPushedMain(p.store.Selected)
	d.main.mu.Lock()
	err = d.main.saveApplied(appliedRecord{Store: p.store, Digest: p.digest, Hops: p.hops})
	_ = os.Remove(d.main.file("pending.json"))
	d.main.mu.Unlock()
	d.events.Add("main_proxy", "pushed", "主代理已下发（"+why+"）")
	return err
}

// maintainMain 在出口循环每一轮的最前面调用（已启用时）：虚拟机重启过（启动标识变了），或虚拟机里的配置不是自己下发的
// （摘要不一致），就按“已下发”记录重新下发——先主代理，再由出口循环下发分流出口（v2.2 决定 3、8）。
func (d *Daemon) maintainMain(ctx context.Context) {
	vm := d.opts.Sys.MainVM
	if vm == nil || d.main == nil {
		return
	}
	a, _ := d.main.applied()
	if a == nil {
		return
	}
	d.exitPushMu.Lock()
	defer d.exitPushMu.Unlock()
	bctx, cancel := context.WithTimeout(ctx, ExitVMTimeout)
	boot, err := vm.BootID(bctx)
	cur, derr := vm.MainProxyDigest(bctx)
	cancel()
	if err != nil {
		return
	}
	rebooted := d.mainBoot != "" && boot != d.mainBoot
	drift := derr == nil && cur != a.Digest || d.mainDirty // 回退没有完成时无条件重新下发
	if !rebooted && !drift && d.mainBoot != "" {
		return
	}
	if rebooted {
		d.events.Add("main_proxy", "vm_rebooted", "虚拟机重启过，重新下发主代理")
	} else if drift {
		d.events.Add("main_proxy", "repushed", "虚拟机里的主代理配置不是 abroadd 下发的，重新下发")
	}
	if rebooted || drift {
		pctx, cancel := context.WithTimeout(ctx, 2*MainPushTimeout)
		defer cancel()
		cfg, err := a.Store.VMRuntimeConfig()
		if err != nil {
			return
		}
		if vm.PushMainHops(pctx, a.Hops) != nil || vm.PushMainProxy(pctx, cfg.YAML) != nil {
			return // 下一轮再试；主代理没下发成功之前出口检查不通过，闸门保持关闭
		}
		d.mainDirty = false
	}
	d.mainBoot = boot
}
