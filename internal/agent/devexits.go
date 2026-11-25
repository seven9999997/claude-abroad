//go:build abroaddev

package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"

	"claude-abroad/internal/guestabi"
)

// DevExits 是开发用的多出口（m3-plan v2.8 决定 7）：只在内核命令行带 abroad.dev=1 时注册 PUT /v1/dev-exits。
// 每个出口是 sing-box 的一个 socks 出站，接本机回环上的开发上游（经网关的回环映射）。
// 收到新的出口表后重写 sing-box 配置（main 加这些出口）并重启 sing-box。
type DevExits struct {
	Set *SingBoxSet // sing-box 配置的唯一写入者

	mu   sync.Mutex
	list []SingBoxExit
}

type devExitsReq struct {
	Exits []SingBoxExit `json:"exits"`
}

// ParseDevCmdline 判断内核命令行里有没有开发标记 abroad.dev=1。
func ParseDevCmdline(cmdline string) bool {
	for _, f := range strings.Fields(cmdline) {
		if f == guestabi.CmdlineDev+"=1" {
			return true
		}
	}
	return false
}

func (d *DevExits) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "只支持 PUT", http.StatusMethodNotAllowed)
		return
	}
	var req devExitsReq
	dec := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || len(req.Exits) > 8 {
		http.Error(w, "请求不合法", http.StatusBadRequest)
		return
	}
	for _, e := range req.Exits {
		if e.Tag == guestabi.ExitMain {
			http.Error(w, "开发出口不能叫 main", http.StatusBadRequest)
			return
		}
		if e.Server != "" && e.Server != "127.0.0.1" && e.Server != guestabi.HostLoopbackIP {
			http.Error(w, "开发出口的服务器只能是回环地址", http.StatusBadRequest) // 出口命名空间的网段留给分流出口
			return
		}
	}
	if err := d.Set.SetDev(req.Exits); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	d.mu.Lock()
	d.list = append([]SingBoxExit(nil), req.Exits...)
	d.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "exits": len(req.Exits)})
}

// has 报告出口标签是否在开发出口表里。
func (d *DevExits) has(tag string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range d.list {
		if e.Tag == tag {
			return true
		}
	}
	return false
}

// devExitTable 是带开发出口的出口表：main 照旧，开发出口在 sing-box 运行时可用。
type devExitTable struct {
	MainExits
	dev *DevExits
}

func (t devExitTable) Lookup(tag string) (known, up bool) {
	if t.dev.has(tag) {
		return true, t.SingBox.Status().State == EngineRunning
	}
	return t.MainExits.Lookup(tag)
}
