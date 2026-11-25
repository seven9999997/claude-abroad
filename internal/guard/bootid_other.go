//go:build !darwin && !linux

package guard

import "claude-abroad/internal/platform"

// BootID 在本平台尚未支持（Windows 版暂停）。
func BootID() (string, error) { return "", platform.Unsupported("开机标识") }
