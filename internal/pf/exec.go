package pf

import (
	"bytes"
	"context"
	"os/exec"
)

// pfctlPath 是唯一允许执行的程序（绝对路径，不经 PATH 查找）。
const pfctlPath = "/sbin/pfctl"

// ExecRunner 是真实的 Runner：执行 /sbin/pfctl。
//
// 注意：会话里不执行它（会改动宿主机的 pf，可能断网）。只有 abroadd（M2-9）和交给用户的脚本会用到；
// 有源码检查测试守着：整个仓库里除本文件外没有对 ExecRunner 的引用。
type ExecRunner struct{}

// Run 实现 Runner。
func (ExecRunner) Run(ctx context.Context, args []string, stdin []byte) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, pfctlPath, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	return so.Bytes(), se.Bytes(), err
}
