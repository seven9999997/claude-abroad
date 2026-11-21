// m0-r2-probe 是 M0 风险 R2 的探测程序：验证 pf 的 `user` 规则能否匹配
// IP_BOUND_IF 绑定物理网卡的连接、经 SCM_RIGHTS 传递的连接，以及"默认拦截
// 所有接口"的规则对系统的影响。它本身不改系统状态，由 scripts/m0/r2/run.sh
// 以不同身份调用。
//
// 每个探测子命令输出一行：
//
//	RESULT ok=true|false kind=<结果分类> detail=<说明>
//
// kind 取值：ok（通了）、refused（对方回了 RST，说明包到了对方）、
// timeout（没有回应，通常是被拦截或线路丢包）、error（本机出错）。
package main

import (
	"claude-abroad/internal/platform"
	"fmt"
	"os"
	"runtime"
)

const usage = `用法：m0-r2-probe <子命令> [参数]

探测（输出 RESULT 行）：
  dial      -net tcp|udp|udp-sendto -to ip:port[,ip:port...] [-iface en0] [-sni name]
  fdsend    -sock path -net tcp|udp -to ip:port [-iface en0] [-presend]
  fdrecv    -sock path [-sni name] [-timeout 3s]
  listen    -addr [::1]:0

辅助（供脚本使用）：
  rules     -user name [-lan cidr,...] [-vmif if,...]   输出 pf 规则文本
  lan       -iface en0                                 输出网卡所在 IPv4 网段
  vmifs                                                列出本机虚拟机网桥接口
  labeldiff before.txt after.txt                       比较两次 pfctl -sl 输出
`

func main() {
	if msg, code := platform.MacOnlyEntry("m0-r2-probe", runtime.GOOS); code != 0 { // W-1：Windows 上只打印说明
		fmt.Fprintln(os.Stderr, msg)
		os.Exit(code)
	}
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "dial":
		err = cmdDial(args)
	case "fdsend":
		err = cmdFDSend(args)
	case "fdrecv":
		err = cmdFDRecv(args)
	case "listen":
		err = cmdListen(args)
	case "rules":
		err = cmdRules(args)
	case "lan":
		err = cmdLAN(args)
	case "vmifs":
		err = cmdVMIfs(args)
	case "labeldiff":
		err = cmdLabelDiff(args)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "m0-r2-probe:", err)
		os.Exit(1)
	}
}
