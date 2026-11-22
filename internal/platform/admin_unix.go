//go:build unix

package platform

import "os"

// IsAdmin 报告本进程是否有管理员权限（Mac 上是 root，即经 sudo 运行；M5-6 命令行的本地检查）。
func IsAdmin() bool { return os.Geteuid() == 0 }

// AdminHint 是需要管理员权限时给用户的说法。
const AdminHint = "需要 sudo：请用 sudo abroad … 运行"
