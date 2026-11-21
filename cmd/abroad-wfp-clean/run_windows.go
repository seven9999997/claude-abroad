//go:build windows

package main

import (
	"fmt"
	"io"

	"claude-abroad/internal/wfp"
)

func run(o options, stdout, stderr io.Writer) int {
	e, err := wfp.Open(false)
	if err != nil {
		fmt.Fprintf(stderr, "打开 Windows 防火墙（WFP）失败：%v\n请在“以管理员身份运行”的 PowerShell 里执行。\n", err)
		return 4
	}
	defer e.Close()
	snap, err := e.Enumerate()
	if err != nil {
		fmt.Fprintf(stderr, "列出 WFP 对象失败：%v\n", err)
		return 4
	}
	plan := wfp.CleanPlan(snap)
	var delErr error
	var after []string
	if o.Delete && len(plan) > 0 {
		_, delErr = e.DeleteOurs()
		again, err := e.Enumerate()
		if err != nil {
			fmt.Fprintf(stderr, "删除后再次检查失败：%v\n", err)
			return 4
		}
		after = wfp.CleanPlan(again)
	}
	text, code := report(plan, o.Delete, delErr, after)
	fmt.Fprint(stdout, text)
	return code
}
