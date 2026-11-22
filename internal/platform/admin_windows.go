//go:build windows

package platform

// IsAdmin 在 Windows 上还没有实现（W 系列做“是否以管理员身份运行”的判断）：一律按没有管理员权限处理，
// 敏感子命令因此在本地就被拦下（不能假装有权限）。
func IsAdmin() bool { return false }

// AdminHint 是需要管理员权限时给用户的说法。
const AdminHint = "需要以管理员身份运行：请在“以管理员身份运行”的终端里执行"
