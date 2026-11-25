package claudeproc

import (
	"os"

	"claude-abroad/internal/procinfo"
)

// NewSystemService 用真实的进程表（sysctl）、真实的签名查询（只读 codesign）组装 Service。
// sig 为 nil 时是“不结束任何进程”的实现（--dev-skip-claude-kill）；真实模式由 cmd/abroadd 传入 NewRealSignaler 的结果。
func NewSystemService(sig Signaler, logf func(string, ...any)) *Service {
	procs := procinfo.SysProcs{}
	sc := &Scanner{Procs: procs, Signer: CodesignSigner{}, Bundle: (&procinfo.FileBundles{}).Read, Self: os.Getpid()}
	s := &Service{Scanner: sc, Logf: logf, Skip: sig == nil}
	if sig != nil {
		s.Killer = &Killer{Scanner: sc, Info: procs.Info, Sig: sig}
	}
	return s
}

// NewSystemLaunchGuard 用真实的进程表、真实的运行中进程签名查询（只读 codesign 查进程号）组装启动管控的第三层（M6-4b）。
// sig 必须是 NewRealSignaler 的结果（只有 cmd/abroadd 的真实模式入口构造）；为 nil 时返回 nil（不结束任何进程）。
func NewSystemLaunchGuard(sig Signaler) *LaunchGuard {
	if sig == nil {
		return nil
	}
	procs := procinfo.SysProcs{}
	return &LaunchGuard{Procs: procs, Info: procs.Info, Sig: sig, Self: os.Getpid(),
		Judge: &AutoJudge{Signer: CodesignSigner{}, Bundle: (&procinfo.FileBundles{}).Read}}
}
