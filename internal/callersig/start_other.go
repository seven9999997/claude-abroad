//go:build !darwin

package callersig

import "claude-abroad/internal/platform"

// ProcStart 只在 macOS 上有（别的平台不缓存）。
func ProcStart(int) (int64, error) { return 0, platform.Unsupported("取进程的启动时间") }
