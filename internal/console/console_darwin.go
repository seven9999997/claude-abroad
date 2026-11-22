//go:build darwin

package console

import (
	"fmt"
	"os"
	"syscall"
)

// DevConsole 是控制台设备。
const DevConsole = "/dev/console"

// UID 返回当前控制台用户的 uid；没有人登录时返回 ErrNoConsoleUser。只读：一次 stat。
func UID() (int, error) {
	fi, err := os.Stat(DevConsole)
	if err != nil {
		return 0, fmt.Errorf("读控制台设备：%w", err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("读控制台设备：拿不到属主")
	}
	return FromOwner(st.Uid)
}
