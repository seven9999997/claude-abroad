package sysnet

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// ErrManualNoPrefix 是 -getinfo 说 IPv6 是 Manual，却没有给出前缀长度（不猜 64；真机样本待 M2-9 采集）。
var ErrManualNoPrefix = errors.New("IPv6 是手动配置，但读不到前缀长度")

// ErrManualNoRouter 是要还原的 Manual 设置没有路由器（networksetup -setv6manual 需要它）。
var ErrManualNoRouter = errors.New("IPv6 手动配置没有路由器，无法还原")

// MacIPv6 是 macOS 上的 IPv6（M2-7d 决定 3），经 networksetup。
type MacIPv6 struct{ R Runner }

// ParseServices 解析 -listallnetworkservices：跳过首行说明和带 * 的停用项；名字不合规的跳过。
func ParseServices(out []byte) ([]string, error) {
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) == 0 || !strings.Contains(lines[0], "asterisk") {
		return nil, ErrUnknownOutput
	}
	var svcs []string
	for _, l := range lines[1:] {
		l = strings.TrimRight(l, "\r")
		if l == "" || strings.HasPrefix(l, "*") || ValidService(l) != nil {
			continue
		}
		svcs = append(svcs, l)
	}
	return svcs, nil
}

// ParseGetinfo 解析 -getinfo 里的 IPv6 部分。
func ParseGetinfo(out []byte) (IPv6Setting, error) {
	kv := map[string]string{}
	for _, l := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(l, ":")
		if ok {
			kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	mode, ok := kv["IPv6"]
	if !ok {
		return IPv6Setting{}, ErrUnknownOutput
	}
	var s IPv6Setting
	switch m := strings.ToLower(mode); {
	case m == "automatic":
		s.Mode = IPv6Automatic
	case m == "off":
		s.Mode = IPv6Off
	case strings.Contains(m, "link") && strings.Contains(m, "local"):
		s.Mode = IPv6LinkLocal
	case m == "manual":
		s.Mode = IPv6Manual
		a, err := netip.ParseAddr(kv["IPv6 IP address"])
		if err != nil {
			return IPv6Setting{}, fmt.Errorf("手动配置的地址：%w", ErrUnknownOutput)
		}
		s.Address = a
		pl, ok := kv["IPv6 Prefix Length"]
		if !ok {
			return IPv6Setting{}, ErrManualNoPrefix
		}
		n, err := strconv.Atoi(pl)
		if err != nil {
			return IPv6Setting{}, ErrManualNoPrefix
		}
		s.PrefixLen = n
		if r := kv["IPv6 Router"]; r != "" && r != "none" {
			ra, err := netip.ParseAddr(r)
			if err != nil {
				return IPv6Setting{}, fmt.Errorf("手动配置的路由器：%w", ErrUnknownOutput)
			}
			s.Router = ra
		}
	default:
		return IPv6Setting{}, fmt.Errorf("IPv6 模式 %q：%w", mode, ErrUnknownOutput)
	}
	if !s.Valid() {
		return IPv6Setting{}, ErrUnknownOutput
	}
	return s, nil
}

// IPv6Args 返回把 service 设成 s 的 networksetup 参数。
func IPv6Args(service string, s IPv6Setting) ([]string, error) {
	if err := ValidService(service); err != nil {
		return nil, fmt.Errorf("网络服务名：%w", err)
	}
	if !s.Valid() || s.Mode == IPv6Unknown {
		return nil, fmt.Errorf("IPv6 设置：%w", ErrBadArg)
	}
	switch s.Mode {
	case IPv6Automatic:
		return []string{"-setv6automatic", service}, nil
	case IPv6LinkLocal:
		return []string{"-setv6LinkLocal", service}, nil
	case IPv6Off:
		return []string{"-setv6off", service}, nil
	default: // Manual
		if !s.Router.IsValid() {
			return nil, ErrManualNoRouter
		}
		return []string{"-setv6manual", service, s.Address.String(), strconv.Itoa(s.PrefixLen), s.Router.String()}, nil
	}
}

// networksetup 出错时常常退出码为 0、错误打在 stdout 上。
func nsErr(service string, so, se []byte, err error) error {
	out := string(so) + string(se)
	if strings.Contains(out, "is not a recognized network service") {
		return fmt.Errorf("网络服务：%w", ErrNotFound)
	}
	if err != nil || strings.Contains(out, "Error") {
		return fmt.Errorf("networksetup 失败：%v（%s）", err, firstLine([]byte(out)))
	}
	return nil
}

func (m MacIPv6) set(ctx context.Context, service string, s IPv6Setting) error {
	args, err := IPv6Args(service, s)
	if err != nil {
		return err
	}
	so, se, err := m.R.Run(ctx, PathNetworksetup, args, nil)
	return nsErr(service, so, se, err)
}

// Off 实现 IPv6。
func (m MacIPv6) Off(ctx context.Context, service string) error {
	return m.set(ctx, service, IPv6Setting{Mode: IPv6Off})
}

// Restore 实现 IPv6。
func (m MacIPv6) Restore(ctx context.Context, service string, s IPv6Setting) error {
	return m.set(ctx, service, s)
}

// Snapshot 实现 IPv6：-listallnetworkservices 加逐个 -getinfo。
func (m MacIPv6) Snapshot(ctx context.Context) (map[string]IPv6Setting, error) {
	so, se, err := m.R.Run(ctx, PathNetworksetup, []string{"-listallnetworkservices"}, nil)
	if err != nil {
		return nil, fmt.Errorf("列网络服务：%w（%s）", err, firstLine(se))
	}
	svcs, err := ParseServices(so)
	if err != nil {
		return nil, err
	}
	out := map[string]IPv6Setting{}
	for _, svc := range svcs {
		so, se, err := m.R.Run(ctx, PathNetworksetup, []string{"-getinfo", svc}, nil)
		if err := nsErr(svc, so, se, err); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue // 列出之后、读取之前被删掉了
			}
			return nil, err
		}
		s, err := ParseGetinfo(so)
		if err != nil || (s.Mode == IPv6Manual && !s.Router.IsValid()) {
			// 读到了服务但没法原样还原：记为 unknown，启用时不关它（不让整个快照失败）。
			s = IPv6Setting{Mode: IPv6Unknown}
		}
		out[svc] = s
	}
	return out, nil
}
