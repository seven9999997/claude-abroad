package sysnet

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"claude-abroad/internal/pf"
)

// IfaceInfo 是一个网卡的只读信息（Interfaces 的输入，测试可注入）。
type IfaceInfo struct {
	Name     string
	Up       bool
	Loopback bool
	Addrs    []netip.Prefix
}

// MacInterfaces 是 Interfaces 的实现：用 Go 标准库读网卡，不执行命令（M2-7d 决定 3）。
type MacInterfaces struct {
	List func() ([]IfaceInfo, error) // 为 nil 时用 net.Interfaces
}

// StdInterfaces 用 net.Interfaces 读网卡。
func StdInterfaces() ([]IfaceInfo, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []IfaceInfo
	for _, i := range ifs {
		info := IfaceInfo{Name: i.Name, Up: i.Flags&net.FlagUp != 0, Loopback: i.Flags&net.FlagLoopback != 0}
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				ip, ok := netip.AddrFromSlice(n.IP)
				ones, _ := n.Mask.Size()
				if ok {
					info.Addrs = append(info.Addrs, netip.PrefixFrom(ip.Unmap(), ones))
				}
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// Interfaces 实现 Interfaces：直连网段取已启用、非回环、非 utun、非网桥接口的 IPv4 网段（公网网段照原样返回，
// 过滤由 pf.LANPrefixes 负责）；网桥取名字合规（pf.ValidVMIf）且已启用的接口。
func (m MacInterfaces) Interfaces(ctx context.Context) (NetInfo, error) {
	list := m.List
	if list == nil {
		list = StdInterfaces
	}
	ifs, err := list()
	if err != nil {
		return NetInfo{}, fmt.Errorf("读网卡：%w", err)
	}
	var ni NetInfo
	for _, i := range ifs {
		if !i.Up || i.Loopback || strings.HasPrefix(i.Name, "utun") {
			continue
		}
		if pf.ValidVMIf(i.Name) {
			ni.VMIfs = append(ni.VMIfs, i.Name)
			continue
		}
		if strings.HasPrefix(i.Name, "bridge") || strings.HasPrefix(i.Name, "vmenet") {
			continue // 名字不合规的网桥：既不当网桥，也不当直连网卡
		}
		for _, p := range i.Addrs {
			if p.Addr().Is4() && !p.Addr().IsLoopback() {
				ni.Direct = append(ni.Direct, p.Masked())
			}
		}
	}
	return ni, nil
}

// UtunConfigArgs 是配置 utun 地址和 MTU 的命令（接成 state.Tun 的真实实现在 M2-9）。
func UtunConfigArgs(name string, local, peer netip.Addr, mtu int) (path string, args []string, err error) {
	if err := ValidUtun(name); err != nil {
		return "", nil, fmt.Errorf("接口名：%w", err)
	}
	if !local.Is4() || !peer.Is4() || mtu < 576 || mtu > 65535 {
		return "", nil, fmt.Errorf("utun 配置：%w", ErrBadArg)
	}
	return PathIfconfig, []string{name, "inet", local.String(), peer.String(), "mtu", strconv.Itoa(mtu), "up"}, nil
}
