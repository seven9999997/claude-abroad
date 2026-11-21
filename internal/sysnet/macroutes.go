package sysnet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// ErrUnknownOutput 是命令输出的格式不认识（不当成空结果返回成功）。
var ErrUnknownOutput = errors.New("不认识的命令输出格式")

// MacRoutes 是 macOS 上的 Routes（M2-7d 决定 3）：只拼命令行、解析输出，经 Runner 执行。
type MacRoutes struct {
	R Runner
	// Iface 返回我们的 utun 名（Present 用；为 nil 时 Present 报错）。
	Iface func() string
}

func checkNet(dst netip.Prefix) error {
	if !dst.IsValid() || !dst.Addr().Is4() || dst != dst.Masked() {
		return fmt.Errorf("网段 %v：%w", dst, ErrBadArg)
	}
	return nil
}

// RouteAddArgs 是添加路由的参数。
func RouteAddArgs(dst netip.Prefix, ifName string) []string {
	return []string{"-n", "add", "-net", dst.String(), "-interface", ifName}
}

// RouteChangeArgs 是把已有路由改到 ifName 的参数。
func RouteChangeArgs(dst netip.Prefix, ifName string) []string {
	return []string{"-n", "change", "-net", dst.String(), "-interface", ifName}
}

// RouteDeleteArgs 是删除路由的参数。
func RouteDeleteArgs(dst netip.Prefix) []string {
	return []string{"-n", "delete", "-net", dst.String()}
}

func notInTable(stderr []byte) bool { return bytes.Contains(stderr, []byte("not in table")) }

// Add 实现 Routes：已存在时用 Via 判断，同接口返回 ErrExists，不同接口用 change 改过去。
func (m MacRoutes) Add(ctx context.Context, dst netip.Prefix, ifName string) error {
	if err := checkNet(dst); err != nil {
		return err
	}
	if err := ValidUtun(ifName); err != nil {
		return fmt.Errorf("接口名：%w", err)
	}
	_, se, err := m.R.Run(ctx, PathRoute, RouteAddArgs(dst, ifName), nil)
	if err == nil {
		return nil
	}
	if !bytes.Contains(se, []byte("File exists")) {
		return fmt.Errorf("添加路由 %s：%w（%s）", dst, err, firstLine(se))
	}
	// 已存在：看这条网段现在经哪个接口（查网段里的第一个地址）。
	via, verr := m.Via(ctx, dst.Addr().Next())
	if verr == nil && via == ifName {
		return fmt.Errorf("路由 %s：%w", dst, ErrExists)
	}
	if _, se, err := m.R.Run(ctx, PathRoute, RouteChangeArgs(dst, ifName), nil); err != nil {
		return fmt.Errorf("把路由 %s 改到 %s：%w（%s）", dst, ifName, err, firstLine(se))
	}
	return nil
}

// Delete 实现 Routes：先用 Via 确认这条网段经我们的 utun 才删（route delete 不分接口，
// 我们的路由被别的软件的 TUN 改写后，不能把别人的删掉）。不是我们的、不在表里、Iface 为空都返回 ErrNotFound。
func (m MacRoutes) Delete(ctx context.Context, dst netip.Prefix) error {
	if err := checkNet(dst); err != nil {
		return err
	}
	ours := ""
	if m.Iface != nil {
		ours = m.Iface()
	}
	if ours == "" {
		return fmt.Errorf("路由 %s（不知道我们的 utun）：%w", dst, ErrNotFound)
	}
	via, err := m.Via(ctx, dst.Addr().Next())
	if errors.Is(err, ErrNotFound) || (err == nil && via != ours) {
		return fmt.Errorf("路由 %s 不经我们的 utun：%w", dst, ErrNotFound)
	}
	if err != nil {
		return err
	}
	_, se, err := m.R.Run(ctx, PathRoute, RouteDeleteArgs(dst), nil)
	if err != nil {
		if notInTable(se) {
			return fmt.Errorf("路由 %s：%w", dst, ErrNotFound)
		}
		return fmt.Errorf("删除路由 %s：%w（%s）", dst, err, firstLine(se))
	}
	return nil
}

// Via 实现 Routes：route -n get <地址>，取 interface: 一行。
func (m MacRoutes) Via(ctx context.Context, addr netip.Addr) (string, error) {
	if !addr.IsValid() || !addr.Is4() {
		return "", fmt.Errorf("地址 %v：%w", addr, ErrBadArg)
	}
	so, se, err := m.R.Run(ctx, PathRoute, []string{"-n", "get", addr.String()}, nil)
	if err != nil {
		if notInTable(se) {
			return "", fmt.Errorf("到 %s 的路由：%w", addr, ErrNotFound)
		}
		return "", fmt.Errorf("查路由：%w（%s）", err, firstLine(se))
	}
	return ParseRouteGet(so)
}

// ParseRouteGet 解析 route -n get 的输出，返回 interface。
func ParseRouteGet(out []byte) (string, error) {
	if !bytes.Contains(out, []byte("route to:")) {
		return "", ErrUnknownOutput
	}
	for _, l := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), "interface:"); ok {
			if v = strings.TrimSpace(v); v != "" {
				return v, nil
			}
		}
	}
	return "", ErrUnknownOutput
}

// Present 实现 Routes：两条 /1 里各取一个地址，都经我们的 utun 才算在。
func (m MacRoutes) Present(ctx context.Context) (bool, error) {
	if m.Iface == nil {
		return false, errors.New("没有配置我们的 utun 名")
	}
	want := m.Iface()
	if want == "" {
		return false, nil
	}
	for _, a := range []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("129.0.0.1")} {
		via, err := m.Via(ctx, a)
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if via != want {
			return false, nil
		}
	}
	return true, nil
}

func firstLine(b []byte) string {
	l, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	if len(l) > 200 {
		l = l[:200]
	}
	return l
}
