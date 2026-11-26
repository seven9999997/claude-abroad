package daemon

import (
	"os"

	"claude-abroad/internal/pf"
)

// 回国拦截（M6-11a，设计 10.2）：回国后 pf 锚点里只留一组规则，立即拒绝到 Anthropic 登记地址段的出站，并保留一个
// pf 启用引用；只有卸载才解除。开关跟“首次准备做过”走：打开了首次准备（--launch-control）、完成标记在、没在卸载
// 也没卸载过。全新安装、M2–M5 的真机脚本、假系统模式都不满足，回国行为和以前完全一样。

// ReturnBlockStatus 是状态里的回国拦截。
type ReturnBlockStatus struct {
	On     bool     `json:"on"`     // 回国后会保留这一组（开关）
	Active bool     `json:"active"` // 现在锚点里就是这一组、pf 启用着（已回国时）
	Nets   []string `json:"nets"`   // 拦哪几段
	Note   string   `json:"note"`
}

// ReturnBlockNote 是回国拦截的说明（状态、界面用）。
const ReturnBlockNote = "回国后，本机任何程序（包括浏览器）都连不上 Anthropic 登记的地址段；本机另有代理软件接管时拦不住。只有卸载才解除。"

// returnBlockOn 是状态机的开关（每次回国、恢复时读一次）。
func (d *Daemon) returnBlockOn() bool {
	if !d.opts.Sys.FirstRun || d.uninstalling.Load() || d.uninstalled() {
		return false
	}
	_, err := os.Stat(d.preparedMarker())
	return err == nil
}

func (d *Daemon) returnBlockStatus() *ReturnBlockStatus {
	rs := &ReturnBlockStatus{On: d.returnBlockOn(), Active: d.ppf.P.ReturnBlockActive(), Nets: []string{}, Note: ReturnBlockNote}
	for _, p := range pf.AnthropicNets {
		rs.Nets = append(rs.Nets, p.String())
	}
	return rs
}
