package daemon

import (
	"io"
	"net/http"

	"claude-abroad/internal/api"
	"claude-abroad/internal/claudeproc"
	"claude-abroad/internal/state"
)

// EnablePreviewRecheckNote 是启用预览里固定的一句（v0.71b：应用确认后以 root 运行 abroad --yes up，后台按当时的情况再走一遍）。
const EnablePreviewRecheckNote = "执行时会按当时的情况重新列出；之后新启动的 Claude 也会被结束。"

// EnablePreview 是 GET /v1/enable/preview（只读，v0.71b，顾问 2026-11-27 必须改）：内容和启用第一步一样——地区会不会变
// （从哪到哪）；会变时列出要结束的 Claude 进程、没能确认的进程、在运行的浏览器（和回国预览同一套）；现在能不能启用
// （不能时给原因代码和说明）。不签发确认码、不改任何状态、不拉起虚拟机。
type EnablePreview struct {
	// CanEnable：现在能不能启用（只看这几项：已卸载、正在进行别的操作、状态文件读不出、完整性校验不通过、没有出口基准）。
	CanEnable bool          `json:"can_enable"`
	Blocker   api.ErrorCode `json:"blocker,omitempty"`
	Reason    string        `json:"reason,omitempty"`
	// Enabled：已经是启用状态（再启用不会改地区）。
	Enabled bool `json:"enabled,omitempty"`
	// Region：地区会变时从哪到哪（和启用第一步 409 应答的 region 相同）；不会变时没有。
	Region *RegionChange `json:"region,omitempty"`
	// Preview：地区会变时要结束的 Claude 进程等（和启用第一步 409 应答的 preview 相同）；不会变时没有。
	Preview *claudeproc.Preview `json:"preview,omitempty"`
	Note    string              `json:"note"`
}

func (d *Daemon) handleEnablePreview(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
	pv := EnablePreview{CanEnable: true, Note: EnablePreviewRecheckNote}
	block := func(c api.ErrorCode, why string) {
		if pv.CanEnable {
			pv.CanEnable, pv.Blocker, pv.Reason = false, c, why
		}
	}
	f, err := d.store.Load()
	switch {
	case err != nil:
		block(api.CodeUnavailable, "状态文件读不出来，不能启用")
	case f.State == state.Enabled:
		pv.Enabled = true
	case f.State == state.Enabling || f.State == state.Restoring:
		block(api.CodeBusy, "正在启用或回国，等它完成")
	}
	if d.uninstalling.Load() || d.uninstalled() {
		block(api.CodeUninstalled, "Claude 出国已经一键卸载，要继续使用请重新安装")
	}
	if d.borrowing.Load() || d.m.Busy() {
		block(api.CodeBusy, "正在进行别的操作，等它完成")
	}
	if d.integrityBad() {
		block(api.CodeUnavailable, "程序完整性校验没有通过，不能启用")
	}
	if b := d.opts.Sys.Baseline; b != nil && !pv.Enabled {
		if err := b.Ready(); err != nil {
			block(api.CodeNoBaseline, "还没有出口基准：先选好主代理节点并应用")
		}
	}
	// 地区：和 handleEnable 的第一步同一个判断、同一套预览
	if err == nil && d.regionOn() && f.State != state.Enabled {
		t, ok, perr := d.regions.Peek()
		if perr != nil {
			block(api.CodeInvalid, "目标地区读不出来："+perr.Error())
		} else if ok {
			ch, cerr := d.regionCheck(r.Context(), t)
			if cerr != nil {
				block(api.CodeInvalid, cerr.Error())
			} else if ch != nil {
				p, perr := d.preview(r.Context())
				if perr != nil {
					writeJSON(w, http.StatusInternalServerError, errBody{"列出要结束的 Claude 进程失败：" + perr.Error()})
					return
				}
				pv.Region, pv.Preview = ch, &p
			}
		}
	}
	writeJSON(w, http.StatusOK, pv)
}
