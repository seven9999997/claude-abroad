package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"strings"

	"claude-abroad/internal/daemon"
)

// ---- v0.68：第四道（Claude Code 的本机代理）的开关，默认关 ----
//
// claude-proxy show 只读；on、off 要 sudo（路由是敏感级别，本地先检查，不是 root 就提示并退出、不发请求）。打开前打印后果
// （daemon.ClaudeProxyConsequences，和后台同一份文字）并要求确认；取消、不是终端又没有 --yes 时不发请求。

func claudeProxyText(v daemon.ClaudeProxyView) []string {
	if !v.Available {
		return []string{"第四道（Claude Code 的本机代理）：" + v.Note}
	}
	var lines []string
	switch {
	case !v.Enabled:
		lines = append(lines, "第四道（Claude Code 的本机代理）："+daemon.ClaudeProxyOffNote+"。打开：sudo abroad claude-proxy on")
	case v.TestMode:
		lines = append(lines, "第四道（Claude Code 的本机代理）：已打开（"+daemon.ClaudeProxyTestNote+"）")
	case v.Status != nil && v.Status.Effective:
		lines = append(lines, "第四道（Claude Code 的本机代理）：已打开，生效中。关闭：sudo abroad claude-proxy off")
	default:
		lines = append(lines, "第四道（Claude Code 的本机代理）：已打开，但未生效（看 abroad launchguard 和 abroad status）。关闭：sudo abroad claude-proxy off")
	}
	if v.Enabled && v.Status != nil && !v.Status.Listening {
		lines = append(lines, "本机代理端口没在听（可能被别的程序占着）")
	}
	if v.Leftover {
		lines = append(lines, daemon.ClaudeProxyLeftoverNote)
	}
	lines = append(lines, v.Notes...)
	return lines
}

func cmdClaudeProxyShow(a *app, args []string) int {
	if len(args) != 0 {
		return usageErr(a, "claude-proxy show 不带参数")
	}
	resp, code := a.call("GET", "/v1/settings/claude-proxy", nil)
	if code != ExitOK {
		return code
	}
	return a.out(resp, func() {
		var v daemon.ClaudeProxyView
		_ = json.Unmarshal(resp.Body, &v)
		for _, l := range claudeProxyText(v) {
			fmt.Fprintln(a.stdout, l)
		}
	})
}

func cmdClaudeProxySet(on bool) func(a *app, args []string) int {
	return func(a *app, args []string) int {
		if len(args) != 0 {
			return usageErr(a, "claude-proxy on|off 不带参数")
		}
		if on && !a.confirmClaudeProxyOn() {
			return ExitConfirm
		}
		resp, code := a.call("PUT", "/v1/settings/claude-proxy", map[string]bool{"enabled": on})
		if code != ExitOK {
			return code
		}
		return a.out(resp, func() {
			var v daemon.ClaudeProxyView
			_ = json.Unmarshal(resp.Body, &v)
			for _, l := range claudeProxyText(v) {
				fmt.Fprintln(a.stdout, l)
			}
		})
	}
}

// confirmClaudeProxyOn 打印打开第四道的后果并要求确认；返回假时什么请求都不发。
func (a *app) confirmClaudeProxyOn() bool {
	pw := a.stdout
	if a.json {
		pw = a.stderr
	}
	fmt.Fprintln(pw, "打开第四道（Claude Code 的本机代理）的后果：")
	for _, l := range daemon.ClaudeProxyConsequences {
		fmt.Fprintln(pw, "  · "+l)
	}
	if a.yes {
		return true
	}
	if !a.isTerminal() {
		fmt.Fprintln(a.stderr, "需要确认：请在终端里运行并确认，或加 --yes；什么都没有改动")
		return false
	}
	fmt.Fprint(pw, "确认打开？输入 yes 继续：")
	line, _ := bufio.NewReader(a.stdin).ReadString('\n')
	s := strings.ToLower(strings.TrimSpace(line))
	if s != "yes" && s != "y" && s != "是" {
		fmt.Fprintln(a.stderr, "没有确认，什么都没有改动")
		return false
	}
	return true
}
