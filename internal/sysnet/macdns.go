package sysnet

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"strings"
)

// DNSKey 是我们写入的动态存储键（设计 4.1）。
const DNSKey = "State:/Network/Service/claude-abroad/DNS"

// MacDNS 是 macOS 上的 DNS（M2-7d 决定 3）：经 scutil 的 stdin 读写 DNSKey。
// 写入后系统是否把它当首选解析器需要实测（M2-9 用户脚本）。
type MacDNS struct {
	R      Runner
	Server netip.Addr // 我们设的服务器（Present 核对用）
}

// DNSSetScript 是写入 DNS 设置的 scutil 脚本（设计 4.1：ServerAddresses 加空的 SupplementalMatchDomains；
// “最高优先级”的实现和是否成为首选解析器待 M2-9 实测）。
func DNSSetScript(server netip.Addr) []byte {
	return []byte("d.init\nd.add ServerAddresses * " + server.String() + "\nd.add SupplementalMatchDomains * \"\"\nset " + DNSKey + "\nquit\n")
}

// DNSShowScript、DNSRemoveScript 是读取、删除的 scutil 脚本。
func DNSShowScript() []byte   { return []byte("show " + DNSKey + "\nquit\n") }
func DNSRemoveScript() []byte { return []byte("remove " + DNSKey + "\nquit\n") }

// show 返回键里的服务器地址；键不存在时 exists=false。
func (m MacDNS) show(ctx context.Context) (servers []netip.Addr, exists bool, err error) {
	so, se, err := m.R.Run(ctx, PathScutil, nil, DNSShowScript())
	if err != nil {
		return nil, false, fmt.Errorf("读 DNS 设置：%w（%s）", err, firstLine(se))
	}
	return ParseScutilShow(so)
}

// ParseScutilShow 解析 scutil 的 show 输出：No such key → 不存在；字典 → 取 ServerAddresses；其他 → ErrUnknownOutput。
func ParseScutilShow(out []byte) ([]netip.Addr, bool, error) {
	t := strings.TrimSpace(string(out))
	if t == "No such key" {
		return nil, false, nil
	}
	if !strings.HasPrefix(t, "<dictionary>") {
		return nil, false, ErrUnknownOutput
	}
	var servers []netip.Addr
	in := false
	for _, l := range strings.Split(t, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "ServerAddresses : <array>"):
			in = true
		case in && l == "}":
			in = false
		case in:
			_, v, ok := strings.Cut(l, " : ")
			a, err := netip.ParseAddr(strings.TrimSpace(v))
			if !ok || err != nil {
				return nil, true, ErrUnknownOutput
			}
			servers = append(servers, a)
		}
	}
	return servers, true, nil
}

// Set 实现 DNS：已设且服务器相同返回 ErrExists，不同就改写。
func (m MacDNS) Set(ctx context.Context, server netip.Addr) error {
	if !server.IsValid() {
		return fmt.Errorf("DNS 服务器：%w", ErrBadArg)
	}
	cur, exists, err := m.show(ctx)
	if err != nil {
		return err
	}
	if exists && len(cur) == 1 && cur[0] == server {
		return fmt.Errorf("DNS 设置：%w", ErrExists)
	}
	so, se, err := m.R.Run(ctx, PathScutil, nil, DNSSetScript(server))
	if err != nil || len(bytes.TrimSpace(so)) > 0 {
		return fmt.Errorf("写 DNS 设置：%v（%s%s）", err, firstLine(so), firstLine(se))
	}
	return nil
}

// Remove 实现 DNS：键不存在返回 ErrNotFound。
func (m MacDNS) Remove(ctx context.Context) error {
	_, exists, err := m.show(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("DNS 设置：%w", ErrNotFound)
	}
	so, se, err := m.R.Run(ctx, PathScutil, nil, DNSRemoveScript())
	if err != nil || len(bytes.TrimSpace(so)) > 0 {
		return fmt.Errorf("删 DNS 设置：%v（%s%s）", err, firstLine(so), firstLine(se))
	}
	return nil
}

// Present 实现 DNS：键存在且服务器是 Server。
func (m MacDNS) Present(ctx context.Context) (bool, error) {
	cur, exists, err := m.show(ctx)
	if err != nil || !exists {
		return false, err
	}
	return len(cur) == 1 && cur[0] == m.Server, nil
}

// MaxSystemDNS 是 System 返回的服务器个数上限。
const MaxSystemDNS = 4

// System 实现 DNS：只读地执行 scutil --dns，取首选解析器的服务器。
func (m MacDNS) System(ctx context.Context) ([]netip.Addr, error) {
	so, se, err := m.R.Run(ctx, PathScutil, []string{"--dns"}, nil)
	if err != nil {
		return nil, fmt.Errorf("读系统 DNS：%w（%s）", err, firstLine(se))
	}
	return ParseScutilDNS(so)
}

// ParseScutilDNS 解析 scutil --dns：只看第一部分（“DNS configuration”，不是“for scoped queries”）里的
// resolver #1，取它的 nameserver[i]，最多 4 个。没有 resolver #1 时返回空；格式不认识时返回 ErrUnknownOutput。
func ParseScutilDNS(out []byte) ([]netip.Addr, error) {
	lines := strings.Split(string(out), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "DNS configuration" {
		return nil, ErrUnknownOutput
	}
	var servers []netip.Addr
	in := false
	for _, l := range lines[1:] {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "DNS configuration"): // 第二部分（scoped），停
			return servers, nil
		case strings.HasPrefix(t, "resolver #"):
			if in {
				return servers, nil // resolver #1 结束
			}
			in = t == "resolver #1"
		case in && strings.HasPrefix(t, "nameserver["):
			_, v, ok := strings.Cut(t, ":")
			a, err := netip.ParseAddr(strings.TrimSpace(v))
			if !ok || err != nil {
				return nil, ErrUnknownOutput
			}
			if len(servers) < MaxSystemDNS {
				servers = append(servers, a.WithZone(""))
			}
		}
	}
	return servers, nil
}
