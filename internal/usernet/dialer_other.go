//go:build !darwin

package usernet

import (
	"fmt"
	"syscall"
)

// bindControl 在非 macOS 上不可用：abroad-vm 只在 macOS 上运行，这里一律失败（失败即拒绝）。
func bindControl(ifname string) (func(network, address string, c syscall.RawConn) error, error) {
	return nil, fmt.Errorf("%w：网卡 %s：这个平台不支持 IP_BOUND_IF", errBind, ifname)
}
