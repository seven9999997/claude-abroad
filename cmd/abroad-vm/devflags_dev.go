//go:build abroaddev

package main

import (
	"flag"

	"claude-abroad/internal/guestabi"
)

// 开发构建（go build -tags abroaddev）才有：--dev-agent 给内核命令行加 abroad.dev=1，agent 据此注册开发用的多出口接口
// （m3-plan v2.8 决定 7）；--outbound-open 恢复 M1 的“公网全放行”（M6-1）。发布构建没有这个文件。
func init() {
	addDevFlags = func(fs *flag.FlagSet, o *options) {
		fs.BoolVar(&o.devAgent, "dev-agent", false, "开发构建：内核命令行加 abroad.dev=1（agent 注册 /v1/dev-exits）")
		fs.BoolVar(&o.outboundOpen, "outbound-open", false, "开发构建：公网一律放行（M1 的行为），与 --first-hop 互斥")
	}
	devCmdline = func(o options, cmdline string, logf func(string, ...any)) string {
		if !o.devAgent {
			return cmdline
		}
		logf("警告：开发构建，内核命令行带 %s=1（agent 注册开发用的多出口接口）", guestabi.CmdlineDev)
		return cmdline + " " + guestabi.CmdlineDev + "=1"
	}
}
