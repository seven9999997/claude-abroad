package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"claude-abroad/internal/api"
	"claude-abroad/internal/claudeproc"
	"claude-abroad/internal/state"
	"claude-abroad/internal/usererror"
)

// 一键卸载（设计 10.3，M6-5a）：两步确认之后按固定顺序执行，每一步都可以重入、失败即停（停下后重试从头再走，
// 已完成的步骤再走一遍是空操作）：
//  1. 强制结束 Claude 并恢复本机网络（就是回国：先结束 Claude、复查，失败中止；网络、IPv6、地区、pf 恢复原样）；
//  2. 解除 Claude 启动管控：删除解析锁（只删自己写的）、删除 Claude Code 的托管设置（只删自己写的）、停掉第三层，
//     在状态目录写“已卸载”的标记（之后启动管控一律允许）；
//  3. 注销后台服务、删除专用用户和目录：由安装器完成（M6-8 接上 Uninstaller；没接时如实报告“需要安装器完成”）。

// OpUninstall 是卸载的确认操作。
const OpUninstall = "uninstall"

// UninstalledFile 是“已卸载”的标记（状态目录里）。有它时启动管控一律允许。
const UninstalledFile = state.UninstalledFile

// Uninstaller 是安装器那一侧的卸载（注销后台服务、删除专用用户、目录、日志和凭据；M6-8 接上）。
type Uninstaller interface {
	RemoveInstall(ctx context.Context) error
}

// UninstallStep 是一步的结果。
type UninstallStep struct {
	Name string `json:"name"`
	Done bool   `json:"done"`
	Note string `json:"note,omitempty"`
}

// UninstallResult 是卸载的应答。
type UninstallResult struct {
	Steps    []UninstallStep `json:"steps"`
	Complete bool            `json:"complete"` // 全部步骤完成（含安装器那一步）
	// BrowsersLeft：选了“同时强制退出浏览器”但还在运行的浏览器（M6-4b-3 补审返工）。
	BrowsersLeft []string `json:"browsers_left,omitempty"`
}

// browsersLeft 取回国服务记下的“还在运行的浏览器”（没有这个能力的实现返回空）。
func (d *Daemon) browsersLeft() []string {
	for _, x := range []any{d.opts.Sys.Claude, d.opts.Sys.Killer} {
		if bl, ok := x.(interface{ BrowsersLeft() []string }); ok && x != nil {
			return bl.BrowsersLeft()
		}
	}
	return nil
}

func (d *Daemon) uninstalled() bool {
	_, err := os.Stat(filepath.Join(d.opts.StateDir, UninstalledFile))
	return err == nil
}

// runUninstall 按固定顺序执行；失败即停，返回已经走到的步骤和错误。
func (d *Daemon) runUninstall(ctx context.Context) (UninstallResult, error) {
	var res UninstallResult
	add := func(name string, done bool, note string) {
		res.Steps = append(res.Steps, UninstallStep{Name: name, Done: done, Note: note})
	}
	// 1. 结束 Claude、恢复本机网络（回国；已经是回国状态时也再结束一次 Claude 并复查）
	if err := d.m.Restore(ctx); err != nil {
		add("结束 Claude 并恢复本机网络", false, "")
		return res, err
	}
	if f, err := d.store.Load(); err == nil && f.State == state.Restored {
		if k := d.opts.Sys.Killer; k != nil {
			if err := k.KillClaude(ctx); err != nil {
				add("结束 Claude 并恢复本机网络", false, "")
				return res, err
			}
		}
	}
	add("结束 Claude 并恢复本机网络", true, "")
	res.BrowsersLeft = d.browsersLeft()
	// 2. 解除启动管控
	var notes []string
	if d.opts.Sys.ResolverLock == nil && d.opts.Sys.ClaudeSettings == nil && d.opts.Sys.LaunchGuard == nil {
		notes = append(notes, "这次安装没有打开启动管控，不需要解除") // 不带 --launch-control（M6-4b-1 返工）
	}
	if lk := d.opts.Sys.ResolverLock; lk != nil {
		rr, err := lk.Remove()
		if err != nil {
			add("解除 Claude 启动管控", false, "")
			return res, usererror.Wrap("删除 Claude 域名解析锁", err, "请重试卸载。")
		}
		if len(rr.Left) > 0 {
			notes = append(notes, "解析锁里有别人写的或被改过的文件没有删："+strings.Join(rr.Left, "、"))
		}
	}
	if cs := d.opts.Sys.ClaudeSettings; cs != nil { // 不管第四道开没开，都按记录检查并删掉我们写的分片（v0.68）
		left, err := cs.Remove()
		if err != nil {
			add("解除 Claude 启动管控", false, "")
			return res, usererror.Wrap("删除 Claude Code 的托管设置", err, "请重试卸载。")
		}
		if left {
			notes = append(notes, "Claude Code 的托管设置被改过，没有删")
		}
	}
	if err := writeFileAtomic(filepath.Join(d.opts.StateDir, UninstalledFile), []byte(time.Now().UTC().Format(time.RFC3339)+"\n")); err != nil {
		add("解除 Claude 启动管控", false, "")
		return res, usererror.Wrap("记下已卸载", err, "请重试卸载。")
	}
	// 回国拦截（M6-11b）：已经回国时第 1 步不动 pf，锚点里的这一组和留下的启用引用在这里清掉、释放。标记先写：
	// 之后巡检、guard、应急程序都不再加载这一组；这一步失败可以重试（重入是空操作）。
	if err := d.m.DropReturnBlock(ctx); err != nil {
		add("解除 Claude 启动管控", false, "")
		return res, err
	}
	d.stopLaunchGuard()      // 第三层不再结束任何进程
	d.uninstallClaudeProxy() // 本机代理端口也不再需要（M6-5a 补审建议改）；第四道的开关记为关（v0.68）
	d.events.Add("uninstall", "launch_control_removed", "")
	add("解除 Claude 启动管控", true, strings.Join(notes, "；"))
	// 3. 安装器那一侧
	if u := d.opts.Sys.Uninstaller; u != nil {
		if err := u.RemoveInstall(ctx); err != nil {
			add("注销后台服务、删除专用用户和数据", false, "")
			return res, err
		}
		add("注销后台服务、删除专用用户和数据", true, "")
		res.Complete = true
	} else {
		add("注销后台服务、删除专用用户和数据", false, "需要安装器完成（开发安装请按各份脚本的 README 移除后台服务）")
	}
	return res, nil
}

// UninstallPreview 是卸载第一步的预览：要结束的 Claude 进程、正在运行的浏览器、要删除的东西。
type UninstallPreview struct {
	claudeproc.Preview
	Removes []string `json:"removes"`
}

func (d *Daemon) uninstallPreview(ctx context.Context) (UninstallPreview, error) {
	pv, err := d.preview(ctx)
	if err != nil {
		return UninstallPreview{}, err
	}
	out := UninstallPreview{Preview: pv, Removes: []string{"恢复本机网络、IPv6、时区和语言"}}
	if d.opts.Sys.ResolverLock != nil {
		out.Removes = append(out.Removes, "Claude 域名解析锁（卸载后 Claude 可以在本机网络下正常连接）")
	}
	if d.opts.Sys.ClaudeSettings != nil {
		out.Removes = append(out.Removes, "Claude Code 的托管设置")
	}
	if d.opts.Sys.FirstRun {
		out.Removes = append(out.Removes, "回国拦截（回国后拦 Anthropic 地址段的 pf 规则和它的启用引用）")
	}
	out.Removes = append(out.Removes, "启动管控", "后台服务、专用用户、日志和凭据")
	return out, nil
}

// handleUninstall 是 POST /v1/actions/uninstall：两步确认（同回国：第一次回 409 带预览和确认码）。
func (d *Daemon) handleUninstall(w http.ResponseWriter, r *http.Request) {
	var req ConfirmRequest
	b, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if len(strings.TrimSpace(string(b))) > 0 && json.Unmarshal(b, &req) != nil {
		writeJSON(w, http.StatusBadRequest, errBody{"请求不合法"})
		return
	}
	digest := digestOf(OpUninstall, browserChoice(req.KillBrowsers)) // 绑定“同时强制退出浏览器”（M6-5a 补审返工）
	if strings.TrimSpace(req.Confirm) == "" {
		pv, err := d.uninstallPreview(r.Context())
		pv.Preview = pv.Preview.WithBrowserChoice(req.KillBrowsers)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errBody{usererror.Wrap("列出卸载会做的事", err, "请再试一次。").Error()})
			return
		}
		raw, _ := json.Marshal(pv)
		writeJSON(w, http.StatusConflict, map[string]any{
			"code":         api.CodeNeedsConfirm,
			"error":        "卸载需要确认：会强制结束下面的 Claude 进程、恢复本机网络，并解除 Claude 启动管控（卸载后 Claude 可以在本机网络下正常启动）。确认后带上确认码再调用一次（60 秒内有效）。",
			"confirm":      d.confirms.issue(OpUninstall, digest),
			"expires_in_s": int(ConfirmTTL / time.Second),
			"preview":      json.RawMessage(raw),
		})
		return
	}
	held, err := d.confirms.take(req.Confirm, OpUninstall, digest)
	if err != nil {
		writeErr(w, http.StatusForbidden, api.ErrorBody{Code: api.CodeConfirmInvalid, Error: usererror.New("卸载", "确认码无效、已过期或已经用过", "什么都没有改动。请重新发起卸载，按新的预览确认。", err).Error()})
		return
	}
	ctx := context.WithoutCancel(r.Context())
	if req.KillBrowsers {
		ctx = claudeproc.WithKillBrowsers(ctx)
	}
	// 整段互斥：从开始到写完标记都占着（M6-5a 补审返工）。guard 里改动类请求看到它就回“忙”。
	if !d.uninstalling.CompareAndSwap(false, true) {
		d.confirms.giveBack(req.Confirm, held)
		writeErr(w, http.StatusConflict, api.ErrorBody{Code: api.CodeBusy, Error: usererror.New("卸载", "已经在卸载", "等它完成。", nil).Error()})
		return
	}
	res, err := d.runUninstall(ctx)
	d.uninstalling.Store(false)
	if err != nil {
		if errors.Is(err, state.ErrBusy) {
			d.confirms.giveBack(req.Confirm, held)
			writeErr(w, http.StatusConflict, api.ErrorBody{Code: api.CodeBusy, Error: err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": usererror.Wrap("卸载", err, "已完成的步骤不会重复出问题，请处理后重试卸载。").Error(), "steps": res.Steps})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// uninstallClaudeProxy 是卸载里第四道的收尾：停止监听，开关记为关（之后重启 abroadd 也不再监听）。分片已在前面按记录删过。
func (d *Daemon) uninstallClaudeProxy() {
	if !d.cpAvailable() {
		return
	}
	d.cpSwitchMu.Lock()
	defer d.cpSwitchMu.Unlock()
	d.stopCP()
	d.cpOn.Store(false)
	if err := d.saveClaudeSwitch(false); err != nil {
		d.events.Add("claude_proxy", "switch_off_failed", "记下开关："+err.Error()) // 已卸载时启动也不监听，不影响结果
	}
}

// UninstallPreviewRecheckNote 是只读卸载预览里固定的一句（v0.71c：应用确认后以 root 运行已安装的 uninstall.sh，
// 它再执行 abroad --yes uninstall，后台按当时的情况再走一遍两步确认）。
const UninstallPreviewRecheckNote = "执行时会按当时的情况重新列出；之后新启动的 Claude 也会被结束。"

// UninstallPreviewView 是 GET /v1/uninstall/preview 的应答：内容和卸载第一步 409 应答里的 preview 相同，加一句说明。
type UninstallPreviewView struct {
	UninstallPreview
	Note string `json:"note"`
}

// handleUninstallPreview 是 GET /v1/uninstall/preview（只读，v0.71c；顾问 2026-11-27 拍板）：不签发确认码、不改任何状态、
// 不拉起虚拟机。应用的卸载确认框用它；卸载后也能读（AllowedAfterUninstall 只管改动类接口）。
func (d *Daemon) handleUninstallPreview(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
	pv, err := d.uninstallPreview(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errBody{usererror.Wrap("列出卸载会做的事", err, "请再试一次。").Error()})
		return
	}
	pv.Preview = pv.Preview.WithBrowserChoice(r.URL.Query().Get("kill_browsers") == "true")
	writeJSON(w, http.StatusOK, UninstallPreviewView{UninstallPreview: pv, Note: UninstallPreviewRecheckNote})
}
