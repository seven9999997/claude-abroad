package daemon

import (
	"errors"
	"os"

	"claude-abroad/internal/pf"
	"claude-abroad/internal/sysnet"
	"claude-abroad/internal/usererror"
)

// ErrNotRoot 是不以 root 运行时要取生产执行器。
var ErrNotRoot = errors.New("真实模式需要 root")

// ProductionRunners 返回会真正执行系统命令的执行器（M2-9b-1 决定 3）。
// 全仓库只有这个文件可以引用 pf.ExecRunner 和 sysnet.ExecRunner（源码检查保证）；
// 9b-1 里没有任何代码路径调用它（cmd/abroadd 不带 --fake-system 时仍打印“真实模式尚未实现”）。
func ProductionRunners() (pf.Runner, sysnet.Runner, error) {
	if os.Geteuid() != 0 {
		return nil, nil, usererror.New("准备真实模式", "没有以 root 运行", "真实模式由安装后的后台服务以 root 启动；开发时请用开发构建的假系统模式。", ErrNotRoot)
	}
	return pf.ExecRunner{}, sysnet.ExecRunner{}, nil
}
