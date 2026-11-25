package daemon

import (
	"reflect"

	"claude-abroad/internal/guard"
	"claude-abroad/internal/pf"
)

// wireGuard 接上和开机时的 guard 共用的文件（M6-2b，m6-plan v2.3 决定 3、5）：
//   - 每次巡检成功应用规则后，参数（不带 utun）有变化就原子另存到状态目录；
//   - 本进程第一次成功应用后，在运行时目录写“本次开机已加载”的标记（内容是开机标识）；
//   - guard 取得的 pf 启用引用由 PatrolPF.AdoptExternal 收养。
//
// 都在巡检持锁时调用：只写文件，不调用巡检的方法。写失败只记事件（保护已经生效，影响的只是下一次开机）。
func (d *Daemon) wireGuard() {
	run := d.opts.RunDir
	if run == "" {
		return
	}
	bootID := d.opts.BootID
	if bootID == nil {
		bootID = guard.BootID
	}
	d.ppf.ExternalTokens = func() ([]string, error) { return guard.TakeTokens(run) }
	var last *guard.Params
	marked := false
	d.ppf.P.OnApplied = func(c pf.Config) {
		p := guard.ParamsFrom(c)
		if last == nil || !reflect.DeepEqual(*last, p) {
			if err := guard.WriteParams(d.opts.StateDir, p); err != nil {
				d.events.Add("guard", "params_failed", "开机时用的参数没能另存")
			} else {
				last = &p
			}
		}
		if !marked {
			b, err := bootID()
			if err == nil {
				err = guard.WriteMarker(run, b)
			}
			if err != nil {
				d.events.Add("guard", "marker_failed", "“本次开机已加载”的标记没能写下")
				return
			}
			marked = true
		}
	}
}
