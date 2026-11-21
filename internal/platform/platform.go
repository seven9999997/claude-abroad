// Package platform 放各平台共用的一点东西：Windows 版的占位实现统一返回的“本平台尚未支持”错误（W-1，design-windows v0.2）。
//
// 占位实现必须失败，不能假装成功：尤其是属主和权限校验、不跟随符号链接的打开、跨进程锁、对端身份、进程号和信号，
// 绝不能返回“校验通过”。源码检查（internal/srccheck）固定住这一点。
package platform

import (
	"errors"
	"fmt"
)

// ErrUnsupported 是“本平台尚未支持”。用 errors.Is(err, platform.ErrUnsupported) 判断（它同时也是 errors.ErrUnsupported）。
var ErrUnsupported = fmt.Errorf("本平台尚未支持：%w", errors.ErrUnsupported)

// Unsupported 返回带说明的“本平台尚未支持”错误，what 写是哪项功能（不含用户数据）。
func Unsupported(what string) error {
	return fmt.Errorf("%s：%w", what, ErrUnsupported)
}
