package daemon

import (
	"bytes"
	"claude-abroad/internal/api"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"claude-abroad/internal/claudecfg"
	"claude-abroad/internal/claudeproc"
	"claude-abroad/internal/state"
	"claude-abroad/internal/usererror"
)

// 两步确认（m4-plan v2.3 决定 4）：恢复、启用、应用主代理、切换地区共用。M4-4 只接恢复，其余在 M4-5 接入。
//   - 确认码：随机 128 位，一次性，60 秒有效，绑定操作类型和参数摘要；
//   - 不绑定进程清单：预览到执行之间新出现的进程，以执行时的扫描为准；
//   - 只在内存里：进程重启后全部失效；不出现在日志和 status 里。

// ConfirmTTL 是确认码的有效期。
const ConfirmTTL = 60 * time.Second

// 需要确认的操作类型。
const (
	OpRestore = "restore"
)

// ErrConfirm 是确认码无效：不存在、过期、已经用过、用于别的操作或参数不符。
var ErrConfirm = errors.New("确认码无效")

type pendingConfirm struct {
	op, digest string
	exp        time.Time
}

type confirms struct {
	now func() time.Time
	mu  sync.Mutex
	m   map[string]pendingConfirm
}

func newConfirms(now func() time.Time) *confirms {
	if now == nil {
		now = time.Now
	}
	return &confirms{now: now, m: map[string]pendingConfirm{}}
}

// digestOf 是参数摘要（顺序有关）。
func digestOf(params ...string) string {
	h := sha256.New()
	for _, p := range params {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// issue 发一个确认码。
func (c *confirms) issue(op, digest string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("读随机数失败：" + err.Error()) // crypto/rand 在受支持的系统上不会失败
	}
	code := hex.EncodeToString(b[:])
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, p := range c.m { // 顺手清掉过期的
		if !now.Before(p.exp) {
			delete(c.m, k)
		}
	}
	c.m[code] = pendingConfirm{op: op, digest: digest, exp: now.Add(ConfirmTTL)}
	return code
}

// take 核对并作废确认码：存在、没过期、操作和参数摘要都相符才通过。只要码存在，不论是否通过都作废（一次性）。
// 通过时返回它（执行时遇到“忙”可以用 giveBack 放回）。
func (c *confirms) take(code, op, digest string) (pendingConfirm, error) {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.m[code]
	if !ok {
		return pendingConfirm{}, ErrConfirm
	}
	delete(c.m, code)
	if !now.Before(p.exp) || subtle.ConstantTimeCompare([]byte(p.op), []byte(op)) != 1 || p.digest != digest {
		return pendingConfirm{}, ErrConfirm
	}
	return p, nil
}

// giveBack 把通过核对、但执行时遇到“已经有一个操作在进行”的确认码放回去（保留原来的过期时间）：
// 操作没有执行，用户不必重新预览。
func (c *confirms) giveBack(code string, p pendingConfirm) {
	if code == "" || p.op == "" || !c.now().Before(p.exp) {
		return
	}
	c.mu.Lock()
	c.m[code] = p
	c.mu.Unlock()
}

// ClaudePreviewer 给确认框看要结束的 Claude 进程（M4-4）。
type ClaudePreviewer interface {
	Preview(ctx context.Context) (claudeproc.Preview, error)
}

// ConfirmRequest 是需要确认的操作的请求体（可为空）。
type ConfirmRequest struct {
	Confirm string `json:"confirm,omitempty"`
	// KillBrowsers：回国时同时结束确认过的浏览器（用户显式选择，M6-4b-3；只对 /v1/restore 有意义）。
	KillBrowsers bool `json:"kill_browsers,omitempty"`
}

// RestoreRequest 是 POST /v1/restore 的请求体（可为空；v0.72 起单独一个类型：启用、切换地区、卸载仍用 ConfirmRequest，不认 claude_proxy）。
type RestoreRequest struct {
	Confirm      string `json:"confirm,omitempty"`
	KillBrowsers bool   `json:"kill_browsers,omitempty"`
	// ClaudeProxy：回国之后第四道（Claude Code 的本机代理）是开还是关（v0.72）。不带 = 保持现状；值和现状一样也按保持。
	// 确认码按请求里的原值绑定（keep、on、off），不按现状归一。
	ClaudeProxy *bool `json:"claude_proxy,omitempty"`
}

// RestoreClaudeProxy 是回国预览里第四道的那一块（v0.72；POST 的 409 和只读预览共用）：只在打开了启动管控、没卸载过时有。
type RestoreClaudeProxy struct {
	Current  bool `json:"current"`   // 现在开着
	Target   bool `json:"target"`    // 回国之后
	TestMode bool `json:"test_mode"` // 开发构建只管测试目标：打开只监听，不写托管设置
	// Conflicts：只读预览里，现在关、不是测试模式时，打开会撞上的冲突（别的托管设置文件定义了代理变量；只读）。
	Conflicts []claudecfg.Conflict `json:"conflicts,omitempty"`
}

// RestorePreviewView 是 GET /v1/restore/preview 的应答（v0.72）：原来的进程清单（字段平铺，旧客户端照样能读）加第四道的那一块。
type RestorePreviewView struct {
	claudeproc.Preview
	ClaudeProxy *RestoreClaudeProxy `json:"claude_proxy,omitempty"`
	Note        string              `json:"note,omitempty"` // omitempty：老后台没有这一项，应用解码时它是可选的
}

// RestorePreviewRecheckNote 是只读回国预览下面的一句（执行时以当时为准）。
const RestorePreviewRecheckNote = "执行时会按当时的情况重新列出；第四道的冲突也会再查一次"

// ConfirmNeeded 是不带确认码时的 409 应答：预览和确认码。
type ConfirmNeeded struct {
	Error      string             `json:"error"`
	Code       api.ErrorCode      `json:"code"` // 固定是 needs_confirm（m5-plan v2.7 决定 0）
	Confirm    string             `json:"confirm"`
	ExpiresInS int                `json:"expires_in_s"`
	Preview    claudeproc.Preview `json:"preview"`
	Region     *RegionChange      `json:"region,omitempty"` // 地区会变的操作：从 A 变为 B
	// ReturnBlock：这次回国后会保留回国拦截（M6-11b；确认框写“回国后，本机任何程序（包括浏览器）都连不上这些地址”）。
	ReturnBlock bool `json:"return_block,omitempty"`
	// ClaudeProxy：回国的预览里第四道的现状和回国之后的目标（v0.72；只在打开了启动管控、没卸载过时有）。
	ClaudeProxy *RestoreClaudeProxy `json:"claude_proxy,omitempty"`
}

func (d *Daemon) preview(ctx context.Context) (claudeproc.Preview, error) {
	if d.opts.Sys.Claude == nil {
		return claudeproc.Preview{Processes: []claudeproc.Target{}, WillKill: true}, nil
	}
	return d.opts.Sys.Claude.Preview(ctx)
}

// handleRestore 是两步的恢复（m4-plan v2.3 决定 5）：状态已经是 RESTORED 时直接返回；
// 否则不带确认码回 409（预览 + 确认码），带正确的确认码才执行。
// v0.72：可带 claude_proxy（回国之后第四道开或关）。先回国、成功后才切换；回国失败开关不动；切换失败仍 200，
// 应答的 claude_proxy_change 写明原因和下一步。
func (d *Daemon) handleRestore(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, errBody{"请求体超过 64 KiB"})
		return
	}
	var req RestoreRequest
	if len(bytes.TrimSpace(b)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, errBody{"请求体不是合法的 JSON：只认 confirm、kill_browsers、claude_proxy 三项"})
			return
		}
	}
	if d.borrowing.Load() { // 借用虚拟机进行中（docs/vm-borrow-plan.md v2）：和启用、卸载一样回“忙”
		writeErr(w, http.StatusConflict, api.ErrorBody{Code: api.CodeBusy, Error: usererror.New("恢复本机网络", "正在临时启动虚拟机做出口检查", "请等它完成后再试。", state.ErrBusy).Error()})
		return
	}
	// 第四道的选项（v0.72）：两步都在核对确认码之前过这一关，不过时不签发、也不消耗确认码。
	target, ok := d.restoreCPTarget(w, req.ClaudeProxy)
	if !ok {
		return
	}
	if f, err := d.store.Load(); err == nil && f.State == state.Restored {
		if target != nil { // 已经是回国状态，又要求切换到另一个状态：回国这一步不再执行，不替用户切开关
			writeErr(w, http.StatusUnprocessableEntity, api.ErrorBody{Code: api.CodeInvalid, Field: "claude_proxy", Error: usererror.New("回国并切换第四道",
				"已经是回国状态，回国这一步不再执行，第四道也没有改", "要切换第四道请用 sudo abroad claude-proxy "+onOff(*target)+"。", nil).Error()})
			return
		}
		writeJSON(w, http.StatusOK, d.Status()) // 已经是回国状态：不需要确认，什么都不做
		return
	}
	// 确认码绑定“同时强制退出浏览器”（M6-5a 补审返工）和第四道的选项（v0.72，按请求里的原值）：两步不一致时确认码无效。
	digest := digestOf(OpRestore, browserChoice(req.KillBrowsers), cpChoice(req.ClaudeProxy))
	if strings.TrimSpace(req.Confirm) == "" {
		if target != nil && *target {
			if conf := d.restoreCPConflicts(); len(conf) > 0 { // 预览时就查（只读）：不让用户确认之后才发现打不开
				writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"code": api.CodeInvalid, "field": "claude_proxy", "conflicts": conf,
					"error": usererror.New("回国前检查第四道", "Claude Code 的托管设置里别的文件也定义了代理变量："+strings.Join(conflictParts(conf), "、"),
						"开关仍是关，没有回国。去掉这些变量后再试，或这次不选打开（不带 --claude-proxy）回国；不会替你改别人的文件。", claudecfg.ErrConflict).Error()})
				return
			}
		}
		pv, err := d.preview(r.Context())
		pv = pv.WithBrowserChoice(req.KillBrowsers)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errBody{usererror.Wrap("列出要结束的 Claude 进程", err, "本机网络没有改动，请再试一次。").Error()})
			return
		}
		cp := d.restoreCPBlock()
		if cp != nil && target != nil {
			cp.Target = *target
		}
		d.opts.logf("恢复本机网络的预览：要结束的 Claude 进程 %d 个，没能确认的 %d 个", len(pv.Processes), len(pv.Unverified))
		if cp != nil {
			d.opts.logf("恢复本机网络的预览：第四道现在%s，回国后%s", onOffText(cp.Current), onOffText(cp.Target))
		}
		writeJSON(w, http.StatusConflict, ConfirmNeeded{
			Code:        api.CodeNeedsConfirm,
			Error:       "恢复本机网络需要确认：会强制结束下面的 Claude 进程，未保存的内容会丢失。确认后带上确认码再调用一次（60 秒内有效）。",
			Confirm:     d.confirms.issue(OpRestore, digest),
			ExpiresInS:  int(ConfirmTTL / time.Second),
			Preview:     pv,
			ReturnBlock: d.returnBlockOn(),
			ClaudeProxy: cp,
		})
		return
	}
	held, err := d.confirms.take(req.Confirm, OpRestore, digest)
	if err != nil {
		writeErr(w, http.StatusForbidden, api.ErrorBody{Code: api.CodeConfirmInvalid, Error: usererror.New("恢复本机网络", "确认码无效、已过期或已经用过", "本机网络没有改动。请重新发起恢复，按新的预览确认。", err).Error()})
		return
	}
	if req.KillBrowsers { // 带进恢复流程：结束 Claude 之后结束确认过的浏览器
		r = r.WithContext(claudeproc.WithKillBrowsers(r.Context()))
	}
	var after func() *ClaudeProxyChange
	if target != nil { // 先回国、成功之后才切开关（回国失败时 after 不会被调用）
		on := *target
		after = func() *ClaudeProxyChange { return d.restoreSwitchCP(on) }
	}
	if err := d.execThen(w, r, d.m.Restore, true, after); errors.Is(err, state.ErrBusy) {
		d.confirms.giveBack(req.Confirm, held)
	}
}

// browserChoice 是“同时强制退出浏览器”在确认码摘要里的写法。
func browserChoice(kill bool) string {
	if kill {
		return "kill_browsers=1"
	}
	return "kill_browsers=0"
}

// cpChoice 是第四道的选项在确认码摘要里的写法（v0.72）：按请求里的原值，不按现状归一。
func cpChoice(p *bool) string {
	switch {
	case p == nil:
		return "claude_proxy=keep"
	case *p:
		return "claude_proxy=on"
	}
	return "claude_proxy=off"
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

func onOffText(on bool) string {
	if on {
		return "开"
	}
	return "关"
}

// restoreCPBlock 是回国预览里第四道的那一块：开关不可用（没打开启动管控）、正在卸载或卸载过时没有。目标先等于现状。
func (d *Daemon) restoreCPBlock() *RestoreClaudeProxy {
	if !d.cpAvailable() || d.uninstalling.Load() || d.uninstalled() {
		return nil
	}
	on := d.cpOn.Load()
	return &RestoreClaudeProxy{Current: on, Target: on, TestMode: d.cpTestMode()}
}

// restoreCPConflicts 是打开第四道会撞上的冲突（只读；测试模式不写托管设置，不查）。
func (d *Daemon) restoreCPConflicts() []claudecfg.Conflict {
	cs := d.opts.Sys.ClaudeSettings
	if cs == nil || d.cpTestMode() {
		return nil
	}
	return cs.Conflicts()
}

// restoreCPTarget 把请求里的 claude_proxy 换成“回国之后要切到的状态”：nil 表示保持（不带、值和现状一样、开关不可用或卸载过时要关）。
// 没打开启动管控又要打开回 501，卸载过又要打开回 409（写好应答，返回 false）。
func (d *Daemon) restoreCPTarget(w http.ResponseWriter, req *bool) (*bool, bool) {
	if req == nil {
		return nil, true
	}
	if !d.cpAvailable() {
		if *req {
			writeErr(w, http.StatusNotImplemented, api.ErrorBody{Code: api.CodeUnsupported, Field: "claude_proxy", Error: usererror.New("回国并打开第四道",
				"这次安装没有打开启动管控", "第四道只在打开了启动管控的安装里有；不带 --claude-proxy 再回国。", nil).Error()})
			return nil, false
		}
		return nil, true
	}
	if d.uninstalling.Load() || d.uninstalled() {
		if *req {
			writeErr(w, http.StatusConflict, api.ErrorBody{Code: api.CodeUninstalled, Error: UninstalledText})
			return nil, false
		}
		return nil, true
	}
	if *req == d.cpOn.Load() {
		return nil, true
	}
	t := *req
	return &t, true
}

// handleRestorePreview 是 GET /v1/restore/preview（只读）：回国会结束的 Claude 进程和没能确认的项，内容和回国第一步的预览相同，
// 但不签发确认码、不改任何状态（design 10.1、11.1；M5-9 设置窗口的进程清单）。
// v0.72：加第四道的那一块（现状、目标、测试模式；现在关、不是测试模式时带只读查出的冲突）；?kill_browsers=true、
// ?claude_proxy=on|off 按勾选预览。只如实报告，不按目标拒绝（501、409、422 只在 POST 里出现）。
func (d *Daemon) handleRestorePreview(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
	q := r.URL.Query()
	var want *bool
	switch v := q.Get("claude_proxy"); v {
	case "":
	case "on", "off":
		on := v == "on"
		want = &on
	default:
		writeErr(w, http.StatusBadRequest, api.ErrorBody{Code: api.CodeInvalid, Field: "claude_proxy", Error: "查询参数 claude_proxy 只认 on 或 off"})
		return
	}
	pv, err := d.preview(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody{usererror.Wrap("列出要结束的 Claude 进程", err, "请再试一次。").Error()})
		return
	}
	v := RestorePreviewView{Preview: pv.WithBrowserChoice(q.Get("kill_browsers") == "true"), Note: RestorePreviewRecheckNote}
	if cp := d.restoreCPBlock(); cp != nil {
		if want != nil {
			cp.Target = *want
		}
		if !cp.Current {
			cp.Conflicts = d.restoreCPConflicts()
		}
		v.ClaudeProxy = cp
	}
	writeJSON(w, http.StatusOK, v)
}
