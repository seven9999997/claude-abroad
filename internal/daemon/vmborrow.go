package daemon

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"claude-abroad/internal/api"
	"claude-abroad/internal/managedproxy"
	"claude-abroad/internal/redact"
	"claude-abroad/internal/state"
	"claude-abroad/internal/usererror"
	"claude-abroad/internal/vmmgr"
)

// 借用虚拟机（docs/vm-borrow-plan.md v2、design 10 的“M6-2a 起”一段）：回国状态下虚拟机不常驻；应用主代理、检查主代理
// 这两个本身要经虚拟机的操作，在状态机的忙标记里临时拉起虚拟机（拉起 → 等健康 → 下发 → 检查 → 停掉）。
// 期间不接管网络、不动 pf、不改地区，闸门保持关（state 原因一直在）。只读接口不拉起。

// BorrowRoutes 是未启用时会借用虚拟机的接口（路由 ID）。
var BorrowRoutes = map[string]bool{"main-proxy-apply": true, "main-check": true}

// ResidentRoutes 是由状态机拉起并常驻虚拟机的接口（不是借用）。其余接口一律不拉起虚拟机（测试按路由表核对）。
var ResidentRoutes = map[string]bool{"enable": true}

// BorrowTimeout 是一次借用的整体时限（等就绪 100 秒 + 下发 + 检查 + 停止，留余量；应用的请求时限 300 秒）。
const BorrowTimeout = 4 * time.Minute

// BorrowView 是 /v1/status 里借用的进度。
type BorrowView struct {
	Op    string    `json:"op"`   // main_apply、main_check
	Step  string    `json:"step"` // starting、pushing、checking、stopping
	Since time.Time `json:"since"`
}

// 借用的步骤（事件 vm_borrow 的 kind）。
const (
	BorrowStarting = "starting"
	BorrowPushing  = "pushing"
	BorrowChecking = "checking"
	BorrowStopping = "stopping"
	BorrowDone     = "done"
	BorrowFailed   = "failed"
)

var borrowStepText = map[string]string{
	BorrowStarting: "正在启动虚拟机（第一次可能要一两分钟）",
	BorrowPushing:  "正在下发主代理",
	BorrowChecking: "正在检查出口",
	BorrowStopping: "正在关闭虚拟机",
}

// 虚拟机不可用的情形。
const (
	KindVMLaunch       = "launch"        // 管理器拉不起来
	KindVMNotReady     = "not_ready"     // 拉起后没就绪
	KindVMStuck        = "stuck"         // 进程卡住
	KindVMExternalDown = "external_down" // 外部启动的虚拟机没在运行
	KindVMNotRunning   = "not_running"   // 已启用、常驻的虚拟机没就绪
	KindVMGivenUp      = "given_up"      // 停用状态下管理器已停止重启
	KindVMChannel      = "channel"       // 检查时到虚拟机的数据通道打不开
)

// VMUnavailable 是“虚拟机没起来 / 没在运行”。
type VMUnavailable struct {
	Kind   string
	Reason string // 中文原因（已脱敏）
	Err    error
}

func (e *VMUnavailable) Error() string { return e.Reason }
func (e *VMUnavailable) Unwrap() error { return e.Err }

// MainPushError 是把主代理配置下发到虚拟机失败（借用时，或启用第 ④ 步虚拟机就绪之后）。
type MainPushError struct{ Err error }

func (e *MainPushError) Error() string {
	return "把主代理配置下发到虚拟机失败：" + reasonOf(e.Err)
}
func (e *MainPushError) Unwrap() error { return e.Err }

func isMainPushErr(err error) bool {
	var pe *MainPushError
	return errors.As(err, &pe)
}

// ErrNoMainSelected 是借来的虚拟机里没有主代理配置、也没有保存的选择。
var ErrNoMainSelected = errors.New("还没有选主代理节点")

// ErrStateIncomplete 是崩溃后留下的半截状态（ENABLING、RESTORING，没有操作在进行）。
var ErrStateIncomplete = errors.New("上一次启用或回国没有完成")

// reasonOf 取给人看的原因：usererror 的原因，或带中文的原文；都没有时给类别（不原样给出底层文字）。脱敏。
func reasonOf(err error) string {
	if err == nil {
		return ""
	}
	return redact.String(usererror.Reason(mainErr("", err, "")))
}

func (d *Daemon) borrowStep(op, step, detail string) {
	switch step {
	case BorrowDone, BorrowFailed:
		d.borrow.Store(nil)
	default:
		d.borrow.Store(&BorrowView{Op: op, Step: step, Since: d.now().Truncate(time.Second)})
		if detail == "" {
			detail = borrowStepText[step]
		}
	}
	d.events.Add("vm_borrow", step, redact.String(detail))
}

// vmUse 是这次检查用的虚拟机：borrowed 为真时是临时拉起的，release 停掉它（可重复调用）。
type vmUse struct {
	borrowed bool
	release  func(context.Context) error
}

// probeVM 是能只读确认是否在运行的虚拟机（ExternalVM 实现）。
type probeVM interface {
	Probe(ctx context.Context) error
}

// vmReadyNow 报告虚拟机此刻能不能用（不拉起）：管理器只读阶段；外部启动的查一次；假的当作能用。
func (d *Daemon) vmReadyNow(ctx context.Context) error {
	if d.vmm != nil {
		if err := d.vmm.Ready(); err != nil {
			return &VMUnavailable{Kind: KindVMNotRunning, Reason: "虚拟机现在没有就绪（" + vmPhaseText(d.vmm.Status().Phase) + "）", Err: err}
		}
		return nil
	}
	if p, ok := d.opts.Sys.VM.(probeVM); ok {
		pctx, cancel := context.WithTimeout(ctx, ExitVMTimeout)
		defer cancel()
		if err := p.Probe(pctx); err != nil {
			return &VMUnavailable{Kind: KindVMExternalDown, Reason: "虚拟机由外部启动，现在没有在运行", Err: err}
		}
	}
	return nil
}

func vmPhaseText(p string) string {
	switch p {
	case vmmgr.PhaseIdle:
		return "没有运行"
	case vmmgr.PhaseStarting:
		return "正在启动"
	case vmmgr.PhaseUnhealthy:
		return "健康检查不通过"
	case vmmgr.PhaseBackoff:
		return "正在重新拉起"
	case vmmgr.PhaseStopping:
		return "正在关闭"
	case vmmgr.PhaseFailed:
		return "连续启动失败，已停止重启"
	case vmmgr.PhaseStuck:
		return "进程卡住"
	}
	return "阶段 " + p
}

// borrowVM 取一个能用的虚拟机：由管理器管理时借用（没被要求常驻就临时拉起）；外部启动的只确认在运行；假的当作能用。
func (d *Daemon) borrowVM(ctx context.Context, op string) (vmUse, error) {
	noop := vmUse{release: func(context.Context) error { return nil }}
	if d.vmm == nil {
		return noop, d.vmReadyNow(ctx)
	}
	if d.vmm.Status().Phase == vmmgr.PhaseIdle {
		d.borrowStep(op, BorrowStarting, "")
	}
	rel, borrowed, err := d.vmm.Borrow(ctx)
	u := vmUse{borrowed: borrowed, release: rel}
	if err == nil {
		return u, nil
	}
	var be *vmmgr.BorrowError
	if errors.As(err, &be) {
		k := map[string]string{vmmgr.KindLaunch: KindVMLaunch, vmmgr.KindNotReady: KindVMNotReady, vmmgr.KindStuck: KindVMStuck}[be.Kind]
		return u, &VMUnavailable{Kind: k, Reason: redact.String(be.Error()), Err: err}
	}
	return u, &VMUnavailable{Kind: KindVMNotReady, Reason: "虚拟机管理器已经停止", Err: err}
}

// withVM 让 fn 在“虚拟机能用”的前提下执行（两个可借用的接口都经这里）：
//   - 已启用：不加锁、不借用（虚拟机常驻），先确认就绪；
//   - 其他状态：在状态机的忙标记里（ExclusiveHold：和启用、停用、回国、卸载、改规则互斥；期间回国拦截的巡检照常）借用，
//     fn 结束后不论成败都停掉借来的虚拟机，然后才放锁。after 在停掉虚拟机之后、放锁之前执行（可为 nil）。
func (d *Daemon) withVM(ctx context.Context, op string, fn func(ctx context.Context, u vmUse) (string, error), after func() error) error {
	if d.fileState() == state.Enabled {
		if err := d.vmReadyNow(ctx); err != nil {
			return err
		}
		done, err := fn(ctx, vmUse{release: func(context.Context) error { return nil }})
		d.borrowEnd(op, done, err)
		if err == nil && after != nil {
			err = after()
		}
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), BorrowTimeout)
	defer cancel()
	return d.m.ExclusiveHold(func() error {
		f, err := d.store.Load()
		if err != nil {
			return err
		}
		switch f.State {
		case state.Enabled: // 入口之后刚好被启用了（拿锁之前）
			return usererror.New("执行操作", "状态刚刚变了", "请再试一次。", state.ErrBusy)
		case state.Enabling, state.Restoring:
			return ErrStateIncomplete
		case state.Disabled:
			if d.vmm != nil && d.vmm.Status().Phase == vmmgr.PhaseFailed {
				return &VMUnavailable{Kind: KindVMGivenUp, Reason: "虚拟机已停止重启（连续启动失败）"}
			}
		}
		d.borrowing.Store(true)
		defer d.borrowing.Store(false)
		u, err := d.borrowVM(ctx, op)
		done := ""
		if err == nil {
			done, err = fn(ctx, u)
		}
		if u.borrowed {
			d.borrowStep(op, BorrowStopping, "")
		}
		rerr := u.release(context.WithoutCancel(ctx))
		if rerr != nil {
			d.events.Add("vm_borrow", "stuck", "虚拟机没能按时停下，正在等它退出")
		}
		d.borrowEnd(op, done, err)
		if err == nil && after != nil {
			err = after()
		}
		return err
	})
}

// borrowEnd 记结束：成功时 done 是“出口在 <国家>（<时区>）”（不含 IP、节点服务器和端口），失败时是中文原因。
func (d *Daemon) borrowEnd(op, done string, err error) {
	if err != nil {
		d.borrowStep(op, BorrowFailed, vmErrText(err))
		return
	}
	d.borrowStep(op, BorrowDone, done)
}

// exitDone 是成功时事件里的一句：只有国家和时区。
func exitDone(country, tz string) string {
	if tz == "" {
		return "出口在 " + country
	}
	return "出口在 " + country + "（" + tz + "）"
}

// vmErrText 是失败的中文原因（事件里用；不含地址）。
func vmErrText(err error) string {
	var vu *VMUnavailable
	var pe *MainPushError
	var pf *ProbeFailure
	var mr *MainNotReadyError
	switch {
	case errors.As(err, &mr):
		return mr.Error()
	case errors.As(err, &vu):
		return vu.Reason
	case errors.As(err, &pe):
		return pe.Error()
	case errors.As(err, &pf):
		if pf.OnlyChannel {
			return "到虚拟机的数据通道打不开"
		}
		return "检查没有通过：" + failedItems(pf.Report)
	case errors.Is(err, state.ErrBusy):
		return "忙：有别的操作在进行"
	case errors.Is(err, ErrNoMainSelected):
		return "还没有选主代理节点"
	case errors.Is(err, ErrStateIncomplete):
		return "上一次启用或回国没有完成"
	}
	return reasonOf(err)
}

// writeVMErr 把 withVM 的错误写成应答（probeStatus 是出口检查没通过时原来的状态码：应用主代理 422、检查主代理 502）。
func writeVMErr(w http.ResponseWriter, action string, err error, probeStatus int) {
	var vu *VMUnavailable
	var pe *MainPushError
	var pf *ProbeFailure
	switch {
	case errors.Is(err, state.ErrBusy):
		msg := err.Error()
		if !usererror.Is(err) {
			msg = usererror.New(action, "有别的操作在进行（启用、回国、卸载或另一次检查）", "请等它完成后再试。", err).Error()
		}
		writeErr(w, http.StatusConflict, api.ErrorBody{Code: api.CodeBusy, Error: msg})
	case errors.Is(err, ErrUninstalledOp):
		writeErr(w, http.StatusConflict, api.ErrorBody{Code: api.CodeUninstalled, Error: UninstalledText})
	case errors.As(err, &vu):
		next := "请在“引擎与虚拟机”页看虚拟机的状态和最近的错误，处理后再试；本机网络没有改动。"
		switch vu.Kind {
		case KindVMExternalDown:
			next = "请先启动虚拟机再试；本机网络没有改动。"
		case KindVMGivenUp:
			next = "请重新启用后再试。"
		case KindVMNotRunning:
			next = "请等虚拟机恢复后再试。"
		}
		writeErr(w, http.StatusServiceUnavailable, api.ErrorBody{Code: api.CodeVMUnavailable, Error: usererror.New("启动虚拟机", vu.Reason, next, err).Error()})
	case isMainNotReady(err):
		writeErr(w, probeStatus, api.ErrorBody{Code: api.CodeMainUnreachable, Error: usererror.New(action, err.Error(), "请确认主代理节点可用，稍后再试；虚拟机已经关闭，本机网络没有改动。", err).Error()})
	case errors.As(err, &pe):
		writeErr(w, http.StatusBadGateway, api.ErrorBody{Code: api.CodeMainPushFailed, Error: usererror.New(action, pe.Error(), "请检查选择的节点；虚拟机已经关闭，本机网络没有改动。", err).Error()})
	case errors.As(err, &pf) && pf.OnlyChannel:
		writeErr(w, http.StatusServiceUnavailable, api.ErrorBody{Code: api.CodeVMUnavailable, Error: err.Error()})
	case errors.As(err, &pf) && pf.Unreachable:
		writeErr(w, probeStatus, api.ErrorBody{Code: api.CodeMainUnreachable, Error: err.Error()})
	case errors.As(err, &pf):
		writeErr(w, probeStatus, api.ErrorBody{Error: err.Error()})
	case errors.Is(err, ErrNoMainSelected):
		writeErr(w, http.StatusUnprocessableEntity, api.ErrorBody{Code: api.CodeNoMainSelected, Error: usererror.New(action, "还没有选主代理节点", "请先选好主代理节点（abroad main use <节点>），再应用。", err).Error()})
	case errors.Is(err, ErrStateIncomplete):
		writeErr(w, http.StatusConflict, api.ErrorBody{Code: api.CodeStateIncomplete, Error: usererror.New(action, "上一次启用或回国没有完成", "请先执行“恢复本机网络”（sudo abroad restore），再试。", err).Error()})
	default:
		writeErr(w, probeStatus, api.ErrorBody{Error: err.Error()})
	}
}

// pushForProbe 在检查之前按保存的选择下发主代理（只对借来的虚拟机：它是新起的，里面没有配置）。不写“已下发”记录
// （虚拟机马上要停），启用时 ensureMainPushed 照常下发。不是借来的虚拟机（已启用、外部启动、停用时常驻的）不在这里下发，
// 和以前相同（开发工具手动下发的那条路不受影响）。
func (d *Daemon) pushForProbe(ctx context.Context, op string, u vmUse) error {
	if !u.borrowed {
		return nil
	}
	vm := d.opts.Sys.MainVM
	if vm == nil {
		return nil
	}
	if !d.managedMain() {
		return ErrNoMainSelected
	}
	d.borrowStep(op, BorrowPushing, "")
	d.exitPushMu.Lock()
	err := d.pushManagedLocked(ctx, vm, "应用主代理", false)
	d.exitPushMu.Unlock()
	if err != nil {
		return &MainPushError{Err: err}
	}
	return d.waitMainReady(ctx) // 下发之后等主代理就绪再检查
}

// mainPushVM 包住状态机用的虚拟机：启用第 ④ 步（状态文件是 ENABLING）虚拟机就绪之后按保存的选择下发主代理
// （由管理器管理时，启用前虚拟机还没运行、下发不了）。按摘要比较，已经是这份就跳过。别的状态（地区同步后的“等虚拟机
// 恢复”）不下发：那时更换主代理的 switchMain 可能正持有 exitPushMu。
type mainPushVM struct {
	inner state.VM
	d     *Daemon
}

func (v mainPushVM) Start(ctx context.Context) error {
	if err := v.inner.Start(ctx); err != nil {
		return err
	}
	if v.d == nil || v.d.fileState() != state.Enabling {
		return nil
	}
	undo := v.d.mainRecordSnapshot()
	if err := v.d.ensureMainPushed(ctx); err != nil {
		// 包成面向用户的错误：状态机按第 ④ 步失败回滚，Wrap 取得到这里的原因（应答代码 main_push_failed）。
		return usererror.New("把主代理配置下发到虚拟机", reasonOf(err), "", &MainPushError{Err: err})
	}
	if v.d.managedMain() {
		// 第 ⑤ 步紧接着经主代理检查出口：先等它就绪。没就绪按第 ④ 步失败回滚（应答代码 main_unreachable）。
		if err := v.d.waitMainReady(ctx); err != nil {
			undo() // 回到启用前：“已下发”记录、待下发的选择都还原（虚拟机按回滚停掉）
			return usererror.New("等主代理在虚拟机里就绪", err.Error(), "", err)
		}
	}
	return nil
}

func (v mainPushVM) Stop(ctx context.Context) error { return v.inner.Stop(ctx) }

// 下发主代理之后等它在虚拟机里就绪（2026-11-27 真机：借用时下发后立即检查，出口检查全是“虚拟机拒绝了数据流”，
// 主代理引擎还没起来）。agent 的 health 报告各引擎的状态：出口 main 要 sing-box 和 mihomo 都是 running 才能开流
// （internal/agent 的 MainExits），所以按 health 轮询，连续两次都可用才算就绪（刚起来又退出的不算）。
var (
	mainReadyTimeout = 30 * time.Second       // 测试改短
	mainReadyPoll    = 500 * time.Millisecond // 轮询间隔
)

// mainReadyStreak 是连续几次读到“可用”才算就绪。
const mainReadyStreak = 2

// MainNotReadyError 是下发之后主代理在时限内没有就绪（应答代码 main_unreachable）。Reason 是 agent 报告的原因（已脱敏）。
type MainNotReadyError struct {
	Wait   time.Duration
	Reason string
}

func (e *MainNotReadyError) Error() string {
	return "主代理在虚拟机里 " + strconv.FormatFloat(e.Wait.Seconds(), 'f', -1, 64) + " 秒内没有就绪：" + e.Reason
}

func isMainNotReady(err error) bool {
	var mr *MainNotReadyError
	return errors.As(err, &mr)
}

var engineStateText = map[string]string{
	"stopped":      "没有运行",
	"checking":     "正在校验配置",
	"check_failed": "配置校验没有通过",
	"starting":     "正在启动",
	"backoff":      "意外退出，正在等待重启",
	"":             "没有报告状态",
}

// mainEngineWhy 是主代理还不能用的原因（能用时为空）：先看转发引擎 sing-box，再看主代理引擎 mihomo。
func mainEngineWhy(h VMHealth) string {
	for _, e := range []struct{ key, name string }{{"sing-box", "转发引擎（sing-box）"}, {"mihomo", "主代理引擎（mihomo）"}} {
		v := h.Engines[e.key]
		if v.State == "running" {
			continue
		}
		st, ok := engineStateText[v.State]
		if !ok {
			st = "还没有运行"
		}
		why := e.name + st
		if le := strings.TrimSpace(redact.String(v.LastError)); le != "" {
			why += "（" + le + "）"
		}
		return why
	}
	return ""
}

// waitMainReady 在下发主代理之后、检查出口之前调用：每 mainReadyPoll 读一次 agent 的 health，主代理引擎连续报告
// 可用才返回；mainReadyTimeout 内没有就绪返回 *MainNotReadyError（原因取最后一次 agent 报告的）。读不了 health 的
// 虚拟机（开发工具的假实现）不等。
func (d *Daemon) waitMainReady(ctx context.Context) error {
	hr, ok := d.opts.Sys.MainVM.(healthReporter)
	if !ok {
		return nil
	}
	wait := mainReadyTimeout
	deadline := time.Now().Add(wait)
	why, streak := "还没有读到虚拟机的健康状态", 0
	for {
		hctx, cancel := context.WithTimeout(ctx, ExitVMTimeout)
		h, err := hr.Health(hctx)
		cancel()
		switch {
		case err != nil:
			streak, why = 0, "读不到虚拟机的健康状态"
		default:
			if w := mainEngineWhy(h); w != "" {
				streak, why = 0, w
			} else if streak++; streak >= mainReadyStreak {
				return nil
			}
		}
		if !time.Now().Before(deadline) || ctx.Err() != nil {
			return &MainNotReadyError{Wait: wait, Reason: why}
		}
		t := time.NewTimer(min(mainReadyPoll, max(time.Until(deadline), 0)))
		select {
		case <-ctx.Done():
			t.Stop()
			return &MainNotReadyError{Wait: wait, Reason: why}
		case <-t.C:
		}
	}
}

// mainRecordSnapshot 记下“已下发”记录和待下发的选择这两个文件的原样，返回还原它们的函数（启用第 ④ 步在下发之后失败时用）。
func (d *Daemon) mainRecordSnapshot() func() {
	if d.main == nil {
		return func() {}
	}
	type saved struct {
		b  []byte
		ok bool
	}
	names := []string{"applied.json", "pending.json"}
	snap := map[string]saved{}
	d.main.mu.Lock()
	for _, n := range names {
		b, err := os.ReadFile(d.main.file(n))
		snap[n] = saved{b, err == nil}
	}
	d.main.mu.Unlock()
	return func() {
		d.main.mu.Lock()
		for _, n := range names {
			if sv := snap[n]; sv.ok {
				_ = managedproxy.Write(d.main.file(n), sv.b)
			} else {
				_ = os.Remove(d.main.file(n))
			}
		}
		d.main.mu.Unlock()
		if a, _ := d.main.applied(); a != nil {
			d.setPushedMain(a.Store.Selected)
		} else {
			d.setPushedMain("")
		}
	}
}
