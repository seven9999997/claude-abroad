package main

import (
	"flag"
	"fmt"
	"io"

	"claude-abroad/internal/claudecfg"
)

// claudeSettingsCmd 是 abroad-dev claude-settings content [--port N]：打印 Claude Code 托管设置分片的内容
// （和 abroadd 首次准备写入的逐字相同；第四层的真机实测脚本用它）。退出码：0 成功；2 参数错。
func claudeSettingsCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "content" {
		fmt.Fprintln(stderr, "用法：abroad-dev claude-settings content [--port N]")
		return 2
	}
	fs := flag.NewFlagSet("claude-settings", flag.ContinueOnError)
	fs.SetOutput(stderr)
	port := fs.Uint("port", claudecfg.DefaultPort, "本机代理端口")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *port == 0 || *port > 65535 {
		fmt.Fprintln(stderr, "端口要在 1–65535")
		return 2
	}
	b, err := claudecfg.Content(uint16(*port))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	_, _ = stdout.Write(b)
	return 0
}
