package vmmgr

import "claude-abroad/internal/platform"

// Launch 在 Windows 上尚未支持（Windows 版暂停）。
func (e *ExecLauncher) Launch() (Proc, error) {
	return nil, platform.Unsupported("拉起虚拟机")
}
