package daemon

import (
	"os"

	"claude-abroad/internal/pf"
	"claude-abroad/internal/state"
)

// 回国拦截（M6-11a，设计 10.2）：回国后 pf 锚点里只留一组规则，立即拒绝到 Anthropic 登记地址段的出站，并保留一个
// pf 启用引用；只有卸载才解除。开关跟“首次准备做过”走：打开了首次准备（--launch-control）、完成标记在、没在卸载
// 也没卸载过。全新安装、M2–M5 的真机脚本、假系统模式都不满足，回国行为和以前完全一样。

// ReturnBlockStatus 是状态里的回国拦截。
type ReturnBlockStatus struct {
	On     bool     `json:"on"`             // 回国后会保留这一组（开关）
	Active bool     `json:"active"`         // 现在锚点里就是这一组、pf 启用着（已回国时）
	Nets   []string `json:"nets"`           // 拦哪几段
	Test   bool     `json:"test,omitempty"` // 测试段（开发构建只管测试目标时，M6-11b）
	Note   string   `json:"note"`
}

// ReturnBlockNote 是回国拦截的说明（状态、界面用）。
const ReturnBlockNote = "回国后，本机任何程序（包括浏览器）都连不上 Anthropic 登记的地址段；本机另有代理软件接管时拦不住。只有卸载才解除。"

// returnBlockOn 是状态机的开关（每次回国、恢复时读一次）：还要选择文件给出一套可加载的地址段（M6-11b）。
func (d *Daemon) returnBlockOn() bool {
	if !d.opts.Sys.FirstRun || d.uninstalling.Load() || d.uninstalled() {
		return false
	}
	if _, err := os.Stat(d.preparedMarker()); err != nil {
		return false
	}
	return d.rbChoice().Load
}

// rbChoice 是当前回国拦截用哪一套地址段（按状态目录里的选择文件；规则见 pf.ChooseReturnBlock）。
func (d *Daemon) rbChoice() pf.ReturnBlockChoice { return state.ChooseReturnBlock(d.opts.StateDir) }

// writeReturnBlockSet 写选择文件（首次准备完成时；fresh 为假时只在文件不在时补写）：只管测试目标（开发构建的
// --dev-resolver-test-domain）时写“测试”，否则写“真实”。开发构建写“真实”时打一条醒目的日志和事件。
func (d *Daemon) writeReturnBlockSet(fresh bool) {
	if !fresh {
		if _, err := state.ReadReturnBlockSet(d.opts.StateDir); err == nil {
			return
		}
	}
	set := pf.SetReal
	if d.opts.Sys.PrepareTestOnly {
		set = pf.SetTest
	}
	if err := state.WriteReturnBlockSet(d.opts.StateDir, set); err != nil {
		d.events.Add("prepare", "return_block_set_failed", "回国拦截的选择文件没能写下")
		return
	}
	if set == pf.SetReal && pf.DevBuild {
		d.opts.logf("注意：开发构建打开了启动管控、没有只管测试目标——回国拦截用真实的 Anthropic 地址段，本机直连 Anthropic 会被拒，直到卸载")
		d.events.Add("prepare", "return_block_real_in_dev", "开发构建在用真实的 Anthropic 地址段")
	}
}

// ReturnBlockTestNote 是测试段时状态里的标注。
const ReturnBlockTestNote = "测试段（开发构建）：只拦文档保留的地址，不影响真实的 Claude。"

func (d *Daemon) returnBlockStatus() *ReturnBlockStatus {
	c := d.rbChoice()
	rs := &ReturnBlockStatus{On: d.returnBlockOn(), Active: d.ppf.P.ReturnBlockActive(), Nets: []string{}, Note: ReturnBlockNote, Test: c.Set == pf.SetTest}
	if rs.Test {
		rs.Note = ReturnBlockTestNote
	}
	if !c.Load && c.Note != "" {
		rs.Note = c.Note
	}
	for _, p := range c.Nets {
		rs.Nets = append(rs.Nets, p.String())
	}
	return rs
}
