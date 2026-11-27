// Package sysnet 是状态机改动本机网络的接口（设计第 10 节，M2-7）：路由、DNS、IPv6，以及只读的网卡信息。
//
// 7a 只有接口和内存假实现（Fake）；darwin 实现在 M2-7d，只做参数拼装和输出解析的单测，会话里不执行。
// 本包的非测试文件不出现执行外部命令的代码（源码检查测试保证）。
package sysnet

import (
	"context"
	"net/netip"

	"claude-abroad/internal/pf"
)

// TakeoverRoutes 是接管用的路由：1.0.0.0/8 起到 255.255.255.255，和 Clash、sing-box 的自动路由同形，**不含 0.0.0.0/8**。
// 2026-11-27 真机：用 0.0.0.0/1 时，按物理网卡限定的默认路由查找（IP_BOUND_IF 绑物理网卡的拨号要走它）拿 0.0.0.0 去匹配，
// 落到经我们 utun 的 0/1 上，答“not in table”，虚拟机对第一跳的拨号全部 ENETUNREACH、启用回滚。0.0.0.0/8 不是可路由的目的，
// 不经 utun 也没有出去的流量（pf 的最后一条照样拦下）。
var TakeoverRoutes = []netip.Prefix{
	netip.MustParsePrefix("1.0.0.0/8"), netip.MustParsePrefix("2.0.0.0/7"), netip.MustParsePrefix("4.0.0.0/6"),
	netip.MustParsePrefix("8.0.0.0/5"), netip.MustParsePrefix("16.0.0.0/4"), netip.MustParsePrefix("32.0.0.0/3"),
	netip.MustParsePrefix("64.0.0.0/2"), netip.MustParsePrefix("128.0.0.0/1"),
}

// LegacyTakeoverRoutes 是旧版本（v0.72e 之前）加的接管路由里、新集合没有的那条：恢复和应急程序顺带删掉
// （Delete 先确认它经我们的 utun 才删，不在或不是我们的直接跳过）。
var LegacyTakeoverRoutes = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/1")}

// Routes 管理接管用的路由（TakeoverRoutes → utun）。
type Routes interface {
	// Add 成功返回时，dst 必须经 ifName（M2-7e 约定）：
	//   - 已存在且接口相同：返回 ErrExists（调用方按成功处理）；
	//   - 已存在但接口不同（旧 utun、别的软件的 TUN）：改到 ifName，改不了就返回错误，不能按“已存在”算成功。
	Add(ctx context.Context, dst netip.Prefix, ifName string) error
	Delete(ctx context.Context, dst netip.Prefix) error
	// Present 报告我们加的路由是否还在。
	Present(ctx context.Context) (bool, error)
	// Via 返回到 addr 的流量走哪个接口（最长前缀匹配，等价于 route get）。
	Via(ctx context.Context, addr netip.Addr) (string, error)
	// Conflicts 看整张路由表，返回占着路由的别的虚拟网卡（M2-9a，定义见 ConflictIfaces）。
	Conflicts(ctx context.Context, ours string) ([]string, error)
}

// DNS 管理接管用的系统 DNS 设置。
type DNS interface {
	// Set 已设且服务器相同时返回 ErrExists（调用方按成功处理）；不同就改写。
	Set(ctx context.Context, server netip.Addr) error
	Remove(ctx context.Context) error
	// Present 报告我们设的 DNS 是否还在。
	Present(ctx context.Context) (bool, error)
	// System 只读地返回系统当前首选解析器（scutil --dns 的 resolver #1）的服务器，最多 4 个
	// （启用前记下，给直连拨号模块解析用，m3-plan v2.8 决定 2）。
	System(ctx context.Context) ([]netip.Addr, error)
}

// IPv6Mode 是一个网络服务的 IPv6 设置方式（networksetup 的四种）。
type IPv6Mode string

const (
	IPv6Automatic IPv6Mode = "automatic"
	IPv6Manual    IPv6Mode = "manual"
	IPv6LinkLocal IPv6Mode = "linklocal"
	IPv6Off       IPv6Mode = "off"
	// IPv6Unknown：读到了这个服务，但它的 IPv6 设置没法原样还原（手动配置缺前缀长度或路由器、不认识的模式或输出）。
	// 启用时不关它、回国时不还原（设计 4.1）。
	IPv6Unknown IPv6Mode = "unknown"
)

// IPv6Setting 是一个网络服务的 IPv6 设置。Manual 时 Address、PrefixLen 必填，Router 可选；其他模式三者都为空。
type IPv6Setting struct {
	Mode      IPv6Mode   `json:"mode"`
	Address   netip.Addr `json:"address,omitzero"`
	PrefixLen int        `json:"prefix_len,omitempty"`
	Router    netip.Addr `json:"router,omitzero"`
}

// Valid 报告设置是否合法。
func (s IPv6Setting) Valid() bool {
	switch s.Mode {
	case IPv6Automatic, IPv6LinkLocal, IPv6Off, IPv6Unknown:
		return !s.Address.IsValid() && s.PrefixLen == 0 && !s.Router.IsValid()
	case IPv6Manual:
		return s.Address.Is6() && s.PrefixLen > 0 && s.PrefixLen <= 128 && (!s.Router.IsValid() || s.Router.Is6())
	}
	return false
}

// IPv6 读取、关闭、还原各网络服务的 IPv6。
type IPv6 interface {
	// Snapshot 返回每个网络服务当前的 IPv6 设置（网络服务名 → 设置）。
	Snapshot(ctx context.Context) (map[string]IPv6Setting, error)
	Off(ctx context.Context, service string) error
	Restore(ctx context.Context, service string, s IPv6Setting) error
}

// NetInfo 是只读的网卡信息：物理网卡的直连网段和本机虚拟机网桥。
type NetInfo struct {
	Direct []netip.Prefix
	VMIfs  []string
}

// Interfaces 只读地查询网卡信息。
type Interfaces interface {
	Interfaces(ctx context.Context) (NetInfo, error)
}

// NetSource 把 Interfaces 适配成 pf.NetSource（给 pf.Patrol 用）。
func NetSource(i Interfaces) pf.NetSource { return netSource{i} }

type netSource struct{ i Interfaces }

func (n netSource) Snapshot(ctx context.Context) (pf.NetSnapshot, error) {
	ni, err := n.i.Interfaces(ctx)
	if err != nil {
		return pf.NetSnapshot{}, err
	}
	return pf.NetSnapshot{Direct: ni.Direct, VMIfs: ni.VMIfs}, nil
}
