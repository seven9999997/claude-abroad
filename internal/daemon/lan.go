package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"claude-abroad/internal/api"
)

// “允许局域网”开关（补齐三，architect 2026-11-27，设计 6.1）：默认开，和以前的行为相同。关掉之后断网规则和接管规则里都
// 不再放行局域网段（abroad_lan 表为空）；回环、DHCP、虚拟机专用用户的出站和回国拦截那一组不受影响。关是收紧（不需要授权），
// 从关到开是放松（敏感）。存在状态目录里（不进规则文档：它改的是 pf 的参数，不是分流规则）。改动后巡检按新参数立即整体重载
// （一次加载，没有空窗）；已回国时巡检暂停或在回国拦截模式，只记下，下次启用生效。另存给开机 guard 的参数随之变化。

// LANSettingFile 是开关的文件（状态目录里，0600）。
const LANSettingFile = "allow-lan.json"

// LANSetting 是 GET/PUT /v1/settings/lan。
type LANSetting struct {
	AllowLAN bool `json:"allow_lan"`
}

// LANOffNote 是关掉时的后果（状态、接口用；界面和命令行的文字在各自的文字表里）。
const LANOffNote = "局域网里的打印机、NAS、投屏、别的电脑的共享都连不上"

// loadAllowLAN 读开关；文件不在时默认开，读不出、格式不对时按关（宁严勿宽）并记事件。
func (d *Daemon) loadAllowLAN() bool {
	b, err := os.ReadFile(filepath.Join(d.opts.StateDir, LANSettingFile))
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	var s LANSetting
	if err == nil {
		err = json.Unmarshal(b, &s)
	}
	if err != nil {
		d.events.Add("settings", "allow_lan_invalid", "“允许局域网”的设置读不出来，按关处理")
		return false
	}
	return s.AllowLAN
}

func (d *Daemon) handleLANGet(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
	writeJSON(w, http.StatusOK, LANSetting{AllowLAN: d.allowLAN.Load()})
}

func (d *Daemon) handleLANPut(w http.ResponseWriter, r *http.Request) {
	var req LANSetting
	b, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if json.Unmarshal(b, &req) != nil {
		writeErr(w, http.StatusBadRequest, api.ErrorBody{Error: "请求体不合法：只认 {\"allow_lan\": true|false}", Field: "allow_lan"})
		return
	}
	cur := d.allowLAN.Load()
	if req.AllowLAN == cur {
		writeJSON(w, http.StatusOK, LANSetting{AllowLAN: cur})
		return
	}
	if req.AllowLAN && !requireSensitive(w, r, "打开“允许局域网”是放松保护：局域网段不经代理") {
		return
	}
	out, _ := json.Marshal(req)
	if err := writeFileAtomic(filepath.Join(d.opts.StateDir, LANSettingFile), append(out, '\n')); err != nil {
		writeErr(w, http.StatusInternalServerError, api.ErrorBody{Error: "保存“允许局域网”失败：" + err.Error()})
		return
	}
	d.allowLAN.Store(req.AllowLAN)
	if req.AllowLAN {
		d.events.Add("settings", "allow_lan_on", "")
	} else {
		d.events.Add("settings", "allow_lan_off", LANOffNote)
	}
	// 立即按新参数重载（巡检暂停时什么都不做：已回国，下次启用生效）
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	_ = d.ppf.P.Step(ctx)
	writeJSON(w, http.StatusOK, LANSetting{AllowLAN: req.AllowLAN})
}
