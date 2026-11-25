//go:build !darwin

package claudeproc

import (
	"errors"

	"claude-abroad/internal/platform"
)

// CodesignSigner 在非 macOS 上不可用。
type CodesignSigner struct{}

func (CodesignSigner) TeamID(string) (string, error) { return "", errors.New("只支持 macOS") }

// ProcTeamID 在本平台尚未支持（M6-4a 新增，不是和 Linux 共用的旧替身）。
func (CodesignSigner) ProcTeamID(int) (string, error) {
	return "", platform.Unsupported("查询运行中进程的签名")
}
