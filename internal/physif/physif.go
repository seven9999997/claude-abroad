// Package physif 识别当前的物理 IPv4 网卡（设计 4.5），给虚拟机出站的拨号器绑定用（IP_BOUND_IF）。
//
// Query 和 selectPhysicalInterface 取自 Claude Cage 的 internal/managedproxy/interface.go
// （MIT License，Copyright (c) 2026 Sylvain Zimmer，见 licenses/Claude-Cage-MIT.txt），
// 在此基础上加了“虚拟网卡一律当失败”和带后台刷新的 Watcher。
//
// 本包不 import managedproxy 或 usernet（有源码检查测试守着）。
package physif

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// QueryTimeout 是单次查询的上限（要起三个子进程）。
const QueryTimeout = 4 * time.Second

// virtualPrefixes 是绝不当成物理网卡的名字前缀：别的 VPN/代理的 TUN、回环、网桥、虚拟机网卡。
var virtualPrefixes = []string{"utun", "lo", "bridge", "vmenet"}

// ErrVirtual 是查询结果落在虚拟网卡上。
var ErrVirtual = errors.New("查到的是虚拟网卡，不是物理网卡")

// checkName 拒绝空名和虚拟网卡。
func checkName(name string) error {
	if name == "" {
		return errors.New("物理网卡查询结果为空")
	}
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(name, p) {
			return fmt.Errorf("%w（%s）", ErrVirtual, name)
		}
	}
	return nil
}

// Query 返回系统首选的、可达的物理 IPv4 硬件网卡（macOS）。默认路由经过 utun 时也不算物理上行。
// 结果是 utun、lo、bridge、vmenet 开头的网卡时返回错误。
func Query(ctx context.Context) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", errors.New("物理网卡识别当前支持 macOS")
	}
	ctx, cancel := context.WithTimeout(ctx, QueryTimeout)
	defer cancel()
	hardware, err := exec.CommandContext(ctx, "/usr/sbin/networksetup", "-listallhardwareports").Output()
	if err != nil {
		return "", errors.New("无法读取物理网卡，请检查主机网络后重试")
	}
	route, _ := exec.CommandContext(ctx, "/sbin/route", "-n", "get", "default").Output()
	nwi, _ := exec.CommandContext(ctx, "/usr/sbin/scutil", "--nwi").Output()
	usable := map[string]bool{}
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, _ := iface.Addrs()
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && ip.To4() != nil && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified() {
				usable[iface.Name] = true
			}
		}
	}
	name, err := selectPhysicalInterface(string(route), string(hardware), string(nwi), usable)
	if err != nil {
		return "", err
	}
	if err := checkName(name); err != nil {
		return "", err
	}
	return name, nil
}

func selectPhysicalInterface(route, hardware, nwi string, usable map[string]bool) (string, error) {
	physical := map[string]bool{}
	for _, line := range strings.Split(hardware, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "Device:" && usable[fields[1]] {
			physical[fields[1]] = true
		}
	}
	// scutil lists interfaces in network priority order. Only its reachable
	// IPv4 section is relevant to the proxy's IPv4 infrastructure transport.
	inIPv4, candidate := false, ""
	reachable := []string{}
	for _, line := range strings.Split(nwi, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "IPv4 network interface information") {
			inIPv4 = true
		} else if strings.HasPrefix(line, "IPv6 network interface information") {
			break
		}
		if !inIPv4 {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[1] == ":" && fields[2] == "flags" {
			candidate = fields[0]
		}
		if strings.HasPrefix(line, "reach") && strings.Contains(line, "(Reachable)") && physical[candidate] {
			reachable = append(reachable, candidate)
		}
	}
	for _, line := range strings.Split(route, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "interface:" {
			for _, name := range reachable {
				if fields[1] == name {
					return name, nil
				}
			}
		}
	}
	if len(reachable) > 0 {
		return reachable[0], nil
	}
	return "", errors.New("未找到可用的物理 IPv4 网卡；Cage 外网保持关闭，请连接网络后重试")
}
