package daemon

import (
	"context"

	"claude-abroad/internal/state"
	"claude-abroad/internal/vmmgr"
)

// startVMManager：Sys.VM 是 *vmmgr.Manager（虚拟机由 abroadd 管理，M6-2a）时接上闸门和复查、按状态文件决定
// 要不要虚拟机，再开始管理循环。虚拟机由外部启动（ExternalVM）时什么都不做，行为和以前相同。
//
// 要不要虚拟机：状态文件是回国（含全新安装）时不常驻（应用主代理、检查主代理在一次互斥的操作内部临时借用，
// 结束时停掉，见 vmborrow.go）；其余（启用、启用中、停用、恢复中）以及读不出状态文件时都要——
// 启用中、恢复中由 Recover 接着走完，走到回国时状态机会调用 Stop。
func (d *Daemon) startVMManager() {
	m, ok := d.opts.Sys.VM.(*vmmgr.Manager)
	if !ok || m == nil {
		return
	}
	d.vmm = m
	m.Gate, m.Kick = d.gate, d.re
	if m.Logf == nil {
		m.Logf = d.opts.logf
	}
	m.OnEvent = func(kind, detail string) { d.events.Add("vm", kind, detail) }
	f, err := d.store.Load()
	m.SetWanted(err != nil || f.State != state.Restored)
	ctx, cancel := context.WithCancel(context.Background())
	d.vmStop, d.vmDone = cancel, make(chan struct{})
	go func() { defer close(d.vmDone); m.Run(ctx) }()
}
