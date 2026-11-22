//go:build !darwin

package console

import "claude-abroad/internal/platform"

// UID 在 Mac 以外的平台还没有实现（Windows 的“当前交互会话用户”由 W 系列另做）。
func UID() (int, error) { return 0, platform.Unsupported("取当前控制台用户") }
