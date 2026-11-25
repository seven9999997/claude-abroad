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
const UninstalledFile = "uninstalled"

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
	// 2. 解除启动管控
	var notes []string
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
	if cs := d.opts.Sys.ClaudeSettings; cs != nil {
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
	if d.lgStop != nil { // 第三层不再结束任何进程
		d.lgStop()
		<-d.lgDone
		d.lgStop = nil
	}
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
	digest := digestOf(OpUninstall)
	if strings.TrimSpace(req.Confirm) == "" {
		pv, err := d.uninstallPreview(r.Context())
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
	res, err := d.runUninstall(ctx)
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
