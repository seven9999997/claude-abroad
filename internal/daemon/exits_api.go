package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"claude-abroad/internal/exitconf"
	"claude-abroad/internal/gatesm"
	"claude-abroad/internal/state"
	"claude-abroad/internal/usererror"
)

// 分流出口的接口（M4-8，m4-plan v2.11 决定 1、2；design 11.1）：
//
//	GET    /v1/exits          全部出口的摘要和可用状态
//	GET    /v1/exits/{tag}    一个出口的摘要和可用状态
//	PUT    /v1/exits/{tag}    新增或替换：{"kind":"wireguard|openvpn|clash","conf":"<配置原文>"}；不合法 422 并指出行号
//	DELETE /v1/exits/{tag}    删除；还被规则引用时 409 并列出规则序号
//
// 增删和启用、停用、恢复、改规则共用状态机的忙标记（冲突 409）。应答里没有配置原文、密钥、端点主机名和地址。
// 请求体只在内存和 secrets/ 里，不进日志和事件。

type exitPutReq struct {
	Kind exitconf.Kind `json:"kind"`
	Conf string        `json:"conf"`
}

// ExitView 是接口和 status 里一个出口的样子。
type ExitView struct {
	ExitSummary
	Avail gatesm.ExitView `json:"avail"`
}

// errExitRefs 是删除还被规则引用的出口。
type errExitRefs struct {
	tag  string
	refs []int
}

func (e errExitRefs) Error() string {
	ns := make([]string, len(e.refs))
	for i, n := range e.refs {
		ns[i] = fmt.Sprintf("第 %d 条", n)
	}
	return fmt.Sprintf("出口 %s 还被规则引用（rules %s），请先改这些规则", e.tag, strings.Join(ns, "、"))
}

// errUnknownExit 是改规则时引用了不存在的出口。
type errUnknownExit struct{ err error }

func (e errUnknownExit) Error() string { return e.err.Error() }

func (d *Daemon) exitView(s ExitSummary) ExitView {
	return ExitView{ExitSummary: s, Avail: d.exitAvail.View(s.Tag)}
}

func (d *Daemon) handleExitsList(w http.ResponseWriter, r *http.Request) {
	if _, err := io.ReadAll(r.Body); err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, errBody{"请求体太大"})
		return
	}
	out := []ExitView{}
	for _, s := range d.exits.Summaries() {
		out = append(out, d.exitView(s))
	}
	writeJSON(w, http.StatusOK, map[string]any{"exits": out})
}

func (d *Daemon) handleExitGet(w http.ResponseWriter, r *http.Request) {
	tag := r.PathValue("tag")
	if _, err := io.ReadAll(r.Body); err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, errBody{"请求体太大"})
		return
	}
	s, ok := d.exits.Summary(tag)
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody{ErrExitNotFound.Error()})
		return
	}
	writeJSON(w, http.StatusOK, d.exitView(s))
}

func (d *Daemon) handleExitPut(w http.ResponseWriter, r *http.Request) {
	tag := r.PathValue("tag")
	if !ValidTag(tag) {
		writeJSON(w, http.StatusUnprocessableEntity, errBody{"出口标签不合法（小写字母开头，最多 32 位，只能有小写字母、数字和 -，不能叫 main）"})
		return
	}
	var req exitPutReq
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errBody{"请求体超过 2 MiB"})
			return
		}
		writeJSON(w, http.StatusBadRequest, errBody{"请求体不对：要是 {\"kind\":…,\"conf\":…}"})
		return
	}
	if _, err := parseExit(req.Kind, req.Conf); err != nil {
		// exitconf 的错误指出行号和指令，不回显配置里的值。
		writeJSON(w, http.StatusUnprocessableEntity, errBody{"出口 " + tag + " 的配置：" + err.Error()})
		return
	}
	x := StoredExit{Tag: tag, Kind: req.Kind, Conf: req.Conf}
	err := d.m.Exclusive(func() error {
		d.exitPushMu.Lock()
		defer d.exitPushMu.Unlock()
		if err := d.exits.Put(x); err != nil {
			return usererror.Wrap("保存出口 "+tag, err, "请再试一次。")
		}
		d.events.Add("exits", "exit_saved", "出口 "+tag+" 已保存")
		d.exitsChangedLocked()
		return nil
	})
	if err != nil {
		writeJSON(w, exitErrCode(err), errBody{err.Error()})
		return
	}
	s, _ := d.exits.Summary(tag)
	writeJSON(w, http.StatusOK, d.exitView(s))
}

func (d *Daemon) handleExitDelete(w http.ResponseWriter, r *http.Request) {
	tag := r.PathValue("tag")
	if _, err := io.ReadAll(r.Body); err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, errBody{"请求体太大"})
		return
	}
	err := d.m.Exclusive(func() error {
		d.exitPushMu.Lock()
		defer d.exitPushMu.Unlock()
		if !d.exits.Has(tag) {
			return ErrExitNotFound
		}
		if refs := RuleRefs(d.currentRules(), tag); len(refs) > 0 {
			return errExitRefs{tag: tag, refs: refs}
		}
		if err := d.exits.Delete(tag); err != nil {
			return usererror.Wrap("删除出口 "+tag, err, "请再试一次。")
		}
		d.events.Add("exits", "exit_deleted", "出口 "+tag+" 已删除")
		d.exitsChangedLocked()
		return nil
	})
	if err != nil {
		writeJSON(w, exitErrCode(err), errBody{err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func exitErrCode(err error) int {
	var refs errExitRefs
	switch {
	case errors.Is(err, state.ErrBusy), errors.As(err, &refs):
		return http.StatusConflict
	case errors.Is(err, ErrExitNotFound):
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

// wireExits 读出 secrets/ 下的出口（Start 时、Recover 之前）。坏文件发事件（不含内容），不进虚拟机。
func (d *Daemon) wireExits() {
	d.exits = &ExitStore{Dir: filepath.Join(d.opts.StateDir, SecretsDir), UID: d.store.UID}
	bad, err := d.exits.Load()
	if err != nil {
		d.events.Add("exits", "exits_unreadable", "出口目录读不出来，暂时没有分流出口："+usererror.Reason(err))
	}
	for _, b := range bad {
		d.events.Add("exits", "exit_invalid", b+"（这个出口不会使用，请重新添加）")
	}
	d.exitPushMu.Lock()
	d.exitsChangedLocked()
	d.exitPushMu.Unlock()
}

// wireExitAvail 定下可用状态的来源（有数据面时用它的，网络栈直接查）并登记开发用的出口。
func (d *Daemon) wireExitAvail() {
	if dp := d.opts.Sys.Data; dp != nil {
		d.exitAvail = dp.avail
	} else {
		d.exitAvail = &gatesm.ExitAvail{Clock: d.opts.Clock}
	}
	for _, t := range d.opts.DevExits {
		d.exitAvail.SetDev(t)
	}
	d.exitLoop = &exitLoop{d: d, kick: make(chan struct{}, 1), sem: make(chan struct{}, ExitCheckParallel)}
}

// isDevExit 报告是不是开发用的出口（构造时给的）。
func (d *Daemon) isDevExit(tag string) bool {
	for _, t := range d.opts.DevExits {
		if t == tag {
			return true
		}
	}
	return false
}

// exitKnown 报告规则能不能引用这个出口：已配置的正式出口或开发用的出口。
func (d *Daemon) exitKnown(tag string) bool { return d.exits.Has(tag) || d.isDevExit(tag) }

// ExitStatus 是 status 里的一个分流出口。
type ExitStatus struct {
	Tag   string          `json:"tag"`
	Kind  exitconf.Kind   `json:"kind,omitempty"` // 开发用的出口没有
	Avail gatesm.ExitView `json:"avail"`
}

// exitStatuses 返回正式的和开发用的分流出口（按标签排序）。
func (d *Daemon) exitStatuses() []ExitStatus {
	var out []ExitStatus
	for _, s := range d.exits.Summaries() {
		out = append(out, ExitStatus{Tag: s.Tag, Kind: s.Kind, Avail: d.exitAvail.View(s.Tag)})
	}
	for _, t := range d.opts.DevExits {
		if !d.exits.Has(t) {
			out = append(out, ExitStatus{Tag: t, Avail: d.exitAvail.View(t)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tag < out[j].Tag })
	return out
}
