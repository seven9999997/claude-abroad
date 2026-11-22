// Package console 取“当前控制台用户”（m5-plan v2 第 5 节 E3）：只读级别的调用方必须是它或 root。
//
// macOS 上取 /dev/console 的属主：有人在图形界面登录时它属于那个用户，登录窗口（没人登录）时属于 root。
// 不用 cgo、不起外部命令（scutil 读动态存储那条路需要起子进程、解析文本，作为备选记在探查结论里）。
package console

import "errors"

// ErrNoConsoleUser 是没有人在图形界面登录（/dev/console 属于 root）。
var ErrNoConsoleUser = errors.New("现在没有人在这台电脑的图形界面登录")

// FromOwner 把 /dev/console 的属主 uid 换成控制台用户：0（root，登录窗口）表示没有人登录。
func FromOwner(uid uint32) (int, error) {
	if uid == 0 {
		return 0, ErrNoConsoleUser
	}
	return int(uid), nil
}
