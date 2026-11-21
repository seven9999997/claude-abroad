//go:build darwin

package claudeproc

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
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
