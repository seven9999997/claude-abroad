//go:build windows

package region

import "claude-abroad/internal/platform"

// ConsoleUser 在 Windows 上一律失败（W-1 的占位）：取不到登录用户就不能当作“有人登录”。
// Windows 的登录用户取法（WTS 会话）见 design-windows。
func ConsoleUser() (string, int, error) {
	return "", 0, platform.Unsupported("取当前登录用户")
}
