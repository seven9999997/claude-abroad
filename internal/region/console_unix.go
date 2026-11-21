//go:build unix

package region

import (
	"errors"
	"os"
	"os/user"
	"strconv"
	"syscall"

	"claude-abroad/internal/sysnet"
)

// ConsoleUser 读 /dev/console 的属主（标准库 os.Stat，不执行命令）。属主是 root 表示没有人登录（登录窗口）。
func ConsoleUser() (string, int, error) {
	fi, err := os.Stat("/dev/console")
	if err != nil {
		return "", 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", 0, errors.New("读不出 /dev/console 的属主")
	}
	uid := int(st.Uid)
	if uid == 0 {
		return "", 0, sysnet.ErrNoConsoleUser
	}
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return "", 0, err
	}
	return u.Username, uid, nil
}
