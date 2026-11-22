package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/exitpolicy"
	"claude-abroad/internal/gatesm"
	"claude-abroad/internal/region"
	"claude-abroad/internal/state"
	"claude-abroad/internal/usererror"
)

// 地区（m4-plan M4-5 v2.4）：锁定的目标在状态目录的 region.json（0600，原子写）。目标只在三种用户操作时变：
// 首次启用（还没有锁定时按出口基准确定）、应用主代理、切换地区。出口复查只比对、只发事件，不改目标（决定 6）。

// RegionFileName 是锁定的目标地区。
const RegionFileName = "region.json"

// 两步确认的操作类型（M4-5）。
const (
	OpEnable    = "enable"
	OpMainProxy = "main_proxy"
	OpRegion    = "region"
)

// Regions 是目标地区的锁定。
type Regions struct {
	Path         string
	BaselinePath string // 出口基准（跟随出口时由它的国家和时区确定目标；空表示没有基准）
	mu           sync.Mutex
}

// Locked 读锁定的目标（没有时 ok 为 false）。
func (r *Regions) Locked() (region.Target, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.load()
}

func (r *Regions) load() (region.Target, bool, error) {
	b, err := os.ReadFile(r.Path)
	if errors.Is(err, os.ErrNotExist) {
		return region.Target{}, false, nil
	}
	if err != nil {
		return region.Target{}, false, err
	}
	var t region.Target
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return region.Target{}, false, errors.New("目标地区的文件损坏")
	}
	if err := t.Validate(); err != nil {
		return region.Target{}, false, errors.New("目标地区的文件损坏")
	}
	return t, true, nil
}

// Lock 锁定一个目标。
func (r *Regions) Lock(t region.Target) error {
	if err := t.Validate(); err != nil {
		return err
	}
	b, _ := json.Marshal(t)
	r.mu.Lock()
	defer r.mu.Unlock()
	return writeFileAtomic(r.Path, b)
}

// FromBaseline 按出口基准确定“跟随出口”的目标（没有基准时 ok 为 false）。
func (r *Regions) FromBaseline() (region.Target, bool, error) {
	if r.BaselinePath == "" {
		return region.Target{}, false, nil
	}
	b, err := exitcheck.LoadBaseline(r.BaselinePath)
	if err != nil {
		return region.Target{}, false, nil
	}
	if b.Timezone == "" {
		return region.Target{}, false, errors.New("出口基准里没有时区")
	}
	t, err := region.FromExit(b.Country, b.Timezone)
	return t, err == nil, err
}

// Peek 是启用将要用的目标：已锁定的，或者按基准确定的（不锁定）。
func (r *Regions) Peek() (region.Target, bool, error) {
	if t, ok, err := r.Locked(); ok || err != nil {
		return t, ok, err
	}
	return r.FromBaseline()
}

// Target 实现 state.TargetSource：已锁定就用它；没有时按出口基准确定并锁定（首次启用）。
func (r *Regions) Target(context.Context) (region.Target, bool, error) {
	if t, ok, err := r.Locked(); ok || err != nil {
		return t, ok, err
	}
	t, ok, err := r.FromBaseline()
	if !ok || err != nil {
		return t, ok, err
	}
	return t, true, r.Lock(t)
}

// RegionChange 是确认框里的“地区将从 A 变为 B”。
type RegionChange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// regionStatus 是 status 里的地区（目标和来源可以显示，原值不显示）。
type regionStatus struct {
	Target *region.Target `json:"target,omitempty"`
	Error  string         `json:"error,omitempty"`
	// Pending 是回国时没能还原、正在后台重试的项（只有项目名，m4-plan v2.5）。
	Pending []string `json:"pending,omitempty"`
	Notice  string   `json:"notice,omitempty"`
}

// RegionRetryInterval 是待还原的项的重试间隔（不小于 5 分钟）。
const RegionRetryInterval = 5 * time.Minute

// regionRetryLoop：启动后先试一次，之后每 RegionRetryInterval 一次（时钟可注入）。
func (d *Daemon) regionRetryLoop(ctx context.Context) {
	clock := d.opts.Clock
	if clock == nil {
		clock = gatesm.RealClock
	}
	kick := make(chan struct{}, 1)
	for {
		_ = d.m.RetryRegionPending(ctx)
		t := clock.AfterFunc(RegionRetryInterval, func() {
			select {
			case kick <- struct{}{}:
			default:
			}
		})
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-kick:
		}
	}
}

func (d *Daemon) regionStatus() *regionStatus {
	if d.regions == nil || !d.regionOn() {
		return nil
	}
	t, ok, err := d.regions.Locked()
	st := &regionStatus{}
	if err != nil {
		st.Error = usererror.Reason(err)
	}
	if ok {
		st.Target = &t
	}
	if items := d.m.PendingItems(); len(items) > 0 {
		st.Pending = items
		var labels []string
		for _, it := range items {
			labels = append(labels, state.ItemLabel(it))
		}
		st.Notice = "网络已恢复，但时区或语言有几项没能还原（" + strings.Join(labels, "、") + "），正在后台重试"
	}
	return st
}

func (d *Daemon) regionOn() bool { return d.opts.Sys.Locale != nil && d.opts.Sys.VMRegion != nil }

// regionCheck 判断 t 会不会改变 Mac 此刻的地区（会时返回确认框里的变化）。
func (d *Daemon) regionCheck(ctx context.Context, t region.Target) (*RegionChange, error) {
	cur, err := d.opts.Sys.Locale.Read(ctx)
	if err != nil {
		return nil, usererror.Wrap("读取 Mac 当前的时区和语言", err, "什么都没有改动。没有原值就没法保证回国时还原，请处理后再试。")
	}
	if !region.Changes(cur, t) {
		return nil, nil
	}
	return &RegionChange{From: region.Describe(cur), To: t.String()}, nil
}

func targetDigest(op string, t region.Target) string {
	return digestOf(op, t.Timezone, t.Lang, t.Locale, t.Source, t.Country)
}

// confirmGate 是地区会变的操作共用的两步确认：会变且没带确认码时回 409（地区变化 + 进程预览 + 确认码），返回 false；
// 带了就核对（不对回 403），通过返回 true 和一个“放回”函数（执行时遇到“忙”调用它）。不会变时直接返回 true。
func (d *Daemon) confirmGate(w http.ResponseWriter, r *http.Request, op string, code string, t region.Target, ch *RegionChange) (bool, func()) {
	noop := func() {}
	if ch == nil {
		return true, noop
	}
	// 签名校验推迟期间的临时加严（m5-plan v2、v2.1 决定 2）：本次调用需要两步确认（地区会变、要结束 Claude）就按敏感处理，
	// 用的是同一个判断（ch != nil）。不够格时在签发确认码之前拒绝。
	if !requireSensitive(w, r, "地区会变：要结束 Claude 进程并改 Mac 的时区和语言") {
		return false, noop
	}
	digest := targetDigest(op, t)
	if strings.TrimSpace(code) == "" {
		pv, err := d.preview(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errBody{usererror.Wrap("列出要结束的 Claude 进程", err, "什么都没有改动，请再试一次。").Error()})
			return false, noop
		}
		d.opts.logf("%s 的预览：地区会变，要结束的 Claude 进程 %d 个", op, len(pv.Processes))
		writeJSON(w, http.StatusConflict, ConfirmNeeded{
			Error:      "地区将从「" + ch.From + "」变为「" + ch.To + "」：会强制结束下面的 Claude 进程，未保存的内容会丢失。确认后带上确认码再调用一次（60 秒内有效）。",
			Confirm:    d.confirms.issue(op, digest),
			ExpiresInS: int(ConfirmTTL.Seconds()),
			Preview:    pv,
			Region:     ch,
		})
		return false, noop
	}
	held, err := d.confirms.take(code, op, digest)
	if err != nil {
		writeJSON(w, http.StatusForbidden, errBody{usererror.New("确认", "确认码无效、已过期、已经用过，或地区目标变了", "什么都没有改动。请重新发起，按新的预览确认。", err).Error()})
		return false, noop
	}
	return true, func() { d.confirms.giveBack(code, held) }
}

func readConfirmBody(w http.ResponseWriter, r *http.Request, v any) bool {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, errBody{"请求体超过 64 KiB"})
		return false
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return true
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody{"请求体不是合法的 JSON"})
		return false
	}
	return true
}

func (d *Daemon) fileState() state.State {
	f, err := d.store.Load()
	if err != nil {
		return ""
	}
	return f.State
}

// handleEnable 是启用（m4-plan v2 必须改 2）：地区会变时两步确认，不会变时一步。
func (d *Daemon) handleEnable(w http.ResponseWriter, r *http.Request) {
	async, aok := wantAsync(w, r)
	if !aok {
		return
	}
	var req ConfirmRequest
	if !readConfirmBody(w, r, &req) {
		return
	}
	if d.regionOn() && d.fileState() != state.Enabled {
		t, ok, err := d.regions.Peek()
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, errBody{usererror.Wrap("确定目标地区", err, "本机网络没有改动。请重新应用主代理，或手动切换地区。").Error()})
			return
		}
		if ok {
			ch, err := d.regionCheck(r.Context(), t)
			if err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, errBody{err.Error()})
				return
			}
			ok, giveBack := d.confirmGate(w, r, OpEnable, req.Confirm, t, ch)
			if !ok {
				return
			}
			d.run(w, r, "enable", async, d.m.Enable, giveBack)
			return
		}
	}
	d.run(w, r, "enable", async, d.m.Enable, nil)
}

// MainProxyRequest 是应用主代理的请求体。
type MainProxyRequest struct {
	Confirm string          `json:"confirm,omitempty"`
	Rule    exitpolicy.Rule `json:"rule,omitempty"`
}

// MainProxyResult 是应用主代理的应答。
type MainProxyResult struct {
	Baseline BaselineSummary `json:"baseline"`
	Region   *region.Target  `json:"region,omitempty"`
	Synced   bool            `json:"synced"` // 已在启用状态，地区已同步
	Note     string          `json:"note,omitempty"`
}

// handleApplyMainProxy 是“应用主代理”（m4-plan v2.4 决定 4）：经主代理跑一轮带位置查询的检查 → 保存基准 →
// 按基准确定目标地区（手动指定的目标保持不变）并锁定 → 已在启用状态时同步地区（会变时两步确认）。
// 主代理配置下发给虚拟机仍由开发工具在此之前完成。POST /v1/baseline 是它的别名（规则从查询参数 rule 取）。
func (d *Daemon) handleApplyMainProxy(w http.ResponseWriter, r *http.Request) {
	var req MainProxyRequest
	if !readConfirmBody(w, r, &req) {
		return
	}
	if q := r.URL.Query().Get("rule"); q != "" && req.Rule == "" {
		req.Rule = exitpolicy.Rule(q)
	}
	bs := d.opts.Sys.Baseline
	if bs == nil {
		writeJSON(w, http.StatusNotImplemented, errBody{"这个模式没有出口基准（开发用的最小检查）"})
		return
	}
	base, err := bs.Probe(r.Context(), req.Rule)
	if err != nil {
		d.events.Add("baseline", "baseline_failed", usererror.Reason(err))
		writeJSON(w, http.StatusUnprocessableEntity, errBody{err.Error()})
		return
	}
	res := MainProxyResult{}
	var target *region.Target
	if d.regionOn() {
		t, err := d.mainProxyTarget(base)
		if err != nil {
			res.Note = "没能按出口确定目标地区（" + usererror.Reason(err) + "），目标保持不变"
		} else {
			target = &t
		}
	}
	enabled := d.fileState() == state.Enabled
	giveBack := func() {}
	if target != nil && enabled {
		ch, err := d.regionCheck(r.Context(), *target)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, errBody{err.Error()})
			return
		}
		ok, gb := d.confirmGate(w, r, OpMainProxy, req.Confirm, *target, ch)
		if !ok {
			return
		}
		giveBack = gb
	}
	sum, err := bs.Save(base)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody{err.Error()})
		return
	}
	res.Baseline = sum
	d.events.Add("baseline", "baseline_saved", "规则 "+string(sum.Rule)+"，国家 "+sum.Country)
	if target != nil {
		if code, err := d.applyTarget(r.Context(), *target, enabled); err != nil {
			if errors.Is(err, state.ErrBusy) {
				giveBack()
			}
			writeJSON(w, code, errBody{err.Error()})
			return
		}
		res.Region, res.Synced = target, enabled
	} else {
		d.envKick() // 没有地区同步时也算一次“应用主代理”
	}
	writeJSON(w, http.StatusOK, res)
}

// mainProxyTarget：已锁定的是手动指定的目标时保持不变；否则按这次的出口确定。
func (d *Daemon) mainProxyTarget(base exitcheck.Baseline) (region.Target, error) {
	if t, ok, err := d.regions.Locked(); err == nil && ok && t.Source == region.SourceManual {
		return t, nil
	}
	if base.Timezone == "" {
		return region.Target{}, errors.New("出口的时区查不到")
	}
	return region.FromExit(base.Country, base.Timezone)
}

// applyTarget 锁定目标；已在启用状态时同步地区（结束 Claude、虚拟机、Mac、补一轮复查）。
func (d *Daemon) applyTarget(ctx context.Context, t region.Target, enabled bool) (int, error) {
	if enabled {
		err := d.m.SyncRegion(context.WithoutCancel(ctx), t)
		switch {
		case errors.Is(err, state.ErrNotEnabled): // 期间被停用或恢复了：只锁定
		case err != nil:
			return errCode(err), err
		}
	}
	if err := d.regions.Lock(t); err != nil {
		return http.StatusInternalServerError, usererror.Wrap("保存目标地区", err, "请检查状态目录的权限。")
	}
	d.events.Add("region", "region_locked", t.String())
	d.envKick() // 切换地区、应用主代理之后立即补一轮环境检测（m4-plan v2.6 决定 4）
	return http.StatusOK, nil
}

// RegionRequest 是切换地区的请求体：mode 为 manual（带时区和语言）或 exit（回到跟随出口）。
type RegionRequest struct {
	Mode     string `json:"mode"`
	Timezone string `json:"timezone,omitempty"`
	Lang     string `json:"lang,omitempty"`
	Locale   string `json:"locale,omitempty"` // 可选：手动模式下显式指定地区格式（不给时按语言推导）
	Confirm  string `json:"confirm,omitempty"`
}

// handleSwitchRegion 是 PUT /v1/region（m4-plan v2.4 决定 5）：只改地区，不影响网络状态；
// 启用状态下会变时两步确认，改完补一轮复查。
func (d *Daemon) handleSwitchRegion(w http.ResponseWriter, r *http.Request) {
	var req RegionRequest
	if !readConfirmBody(w, r, &req) {
		return
	}
	if !d.regionOn() {
		writeJSON(w, http.StatusNotImplemented, errBody{"这个模式没有地区同步"})
		return
	}
	var t region.Target
	var err error
	switch req.Mode {
	case region.SourceManual:
		t, err = region.ManualLocale(req.Timezone, req.Lang, req.Locale)
	case region.SourceExit:
		if req.Locale != "" {
			err = errors.New("跟随出口时不能指定地区格式（locale 只用于手动模式）")
			break
		}
		var ok bool
		t, ok, err = d.regions.FromBaseline()
		if err == nil && !ok {
			err = errors.New("还没有出口基准")
		}
	default:
		err = fmt.Errorf("mode 只能是 manual 或 exit")
	}
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, errBody{usererror.Wrap("切换地区", err, "地区没有改动。").Error()})
		return
	}
	enabled := d.fileState() == state.Enabled
	giveBack := func() {}
	if enabled {
		ch, err := d.regionCheck(r.Context(), t)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, errBody{err.Error()})
			return
		}
		ok, gb := d.confirmGate(w, r, OpRegion, req.Confirm, t, ch)
		if !ok {
			return
		}
		giveBack = gb
	}
	if code, err := d.applyTarget(r.Context(), t, enabled); err != nil {
		if errors.Is(err, state.ErrBusy) {
			giveBack()
		}
		writeJSON(w, code, errBody{err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, d.Status())
}

// ---- 虚拟机一侧 ----

// SetRegion 实现 state.VMRegion：经控制面（ctl.sock）PUT /v1/region。
func (v ExternalVM) SetRegion(ctx context.Context, t region.Target) error {
	body, _ := json.Marshal(map[string]string{"timezone": t.Timezone, "lang": t.Lang})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://abroad-agent/v1/region", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := unixClient(filepath.Join(v.Dir, "ctl.sock"), 30*time.Second).Do(req)
	if err != nil {
		return usererror.Wrap("同步虚拟机的地区", err, "请确认虚拟机在运行。")
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return usererror.New("同步虚拟机的地区", fmt.Sprintf("虚拟机回 HTTP %d：%s", resp.StatusCode, strings.TrimSpace(string(b))), "请检查虚拟机的日志。", nil)
	}
	return nil
}

// ---- 出口漂移只发事件（决定 6）----

// noteDrift 比对复查得到的出口国家、时区和锁定的目标：不同只发事件（状态变化时发一次），不改目标、不改设置、不关闸门。
// 国家不同由 M4-6 的环境检测第 03 项处理。事件里不写具体的国家和时区。
func (d *Daemon) noteDrift(country, tz string) {
	if d.regions == nil || !d.regionOn() {
		return
	}
	t, ok, err := d.regions.Locked()
	if !ok || err != nil {
		return
	}
	key := ""
	switch {
	case country != "" && t.Country != "" && country != t.Country:
		key = "country"
	case tz != "" && tz != t.Timezone:
		key = "timezone"
	}
	if prev := d.drift.Swap(&key); prev != nil && *prev == key {
		return
	}
	switch key {
	case "country":
		d.events.Add("region", "region_drift", "出口的国家与锁定的目标地区不同（地区不变；由环境检测处理）")
	case "timezone":
		d.events.Add("region", "region_drift", "出口的时区与锁定的目标地区不同（地区不变；可以手动切换地区）")
	}
}
