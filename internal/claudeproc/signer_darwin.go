//go:build darwin

package claudeproc

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// CodesignSigner 只读执行 /usr/bin/codesign -dv 查签名。
type CodesignSigner struct{ Timeout time.Duration }

func (s CodesignSigner) TeamID(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", errors.New("路径不是绝对路径")
	}
	to := s.Timeout
	if to <= 0 {
		to = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), to)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/codesign", "-dv", p).CombinedOutput()
	id, perr := ParseCodesign(string(out))
	if perr != nil {
		if err != nil {
			return "", errors.New("查询签名失败")
		}
		return "", perr
	}
	return id, nil
}

// ProcTeamID 查正在运行的进程的签名（codesign -dv <进程号>，只读）：看的是内存里那个进程的签名，
// 进程启动后文件被替换也骗不过去。进程已经没了、超时、输出不认识都返回错误。
func (s CodesignSigner) ProcTeamID(pid int) (string, error) {
	if pid <= 1 {
		return "", errors.New("进程号不对")
	}
	to := s.Timeout
	if to <= 0 {
		to = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), to)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/codesign", "-dv", strconv.Itoa(pid)).CombinedOutput()
	if ctx.Err() != nil {
		return "", errors.New("查询签名超时")
	}
	id, perr := ParseCodesign(string(out))
	if perr != nil {
		if err != nil {
			return "", errors.New("查询签名失败")
		}
		return "", perr
	}
	return id, nil
}
