package daemon

import (
	"io"
	"net/http"
	"time"

	"claude-abroad/internal/exitcheck"
)

// MainExitView 是 GET /v1/main-proxy/exit：主代理出口的 IP、国家、城市、时区（来自出口基准），给菜单栏和首页显示。
//
// 这是“应答里不含出口 IP”的唯一例外（design 11.1 v0.26、m5-plan v2.5 决定 3）：界面要显示出口 IP，而状态接口、事件、
// 日志、任务查询会被用户整段贴出去求助，所以出口 IP 只放在这一个要单独调用的接口里；处理时不写日志、不发事件。
type MainExitView struct {
	Available bool      `json:"available"` // 有可用的出口基准
	IP        string    `json:"ip,omitempty"`
	Country   string    `json:"country,omitempty"`
	Region    string    `json:"region,omitempty"`
	City      string    `json:"city,omitempty"`
	Timezone  string    `json:"timezone,omitempty"`
	At        time.Time `json:"at,omitempty"` // 基准建立的时间
	Note      string    `json:"note,omitempty"`
}

func (d *Daemon) handleMainExit(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
	bs := d.opts.Sys.Baseline
	if bs == nil {
		writeJSON(w, http.StatusOK, MainExitView{Note: "这个模式没有出口基准"})
		return
	}
	base, err := exitcheck.LoadBaseline(bs.Path)
	if err != nil {
		writeJSON(w, http.StatusOK, MainExitView{Note: "还没有：应用主代理后显示"})
		return
	}
	writeJSON(w, http.StatusOK, MainExitView{Available: true, IP: base.IP, Country: base.Country, Region: base.Region, City: base.City, Timezone: base.Timezone, At: base.At})
}
