//go:build !windows

package main

import (
	"fmt"
	"io"
)

func run(_ options, _, stderr io.Writer) int {
	fmt.Fprintln(stderr, "abroad-wfp-probe 只在 Windows 上使用：它在 Windows 防火墙（WFP）里做断网规则的第一级探查")
	return 3
}
