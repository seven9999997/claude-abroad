package usernet

import (
	"context"
	"errors"
	"net/netip"

	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/miekg/dns"

	"claude-abroad/internal/guestabi"
)

const (
	// Subnet 是虚拟机网络（与 guestabi.DefaultIP、DefaultGW 一致）。
	Subnet = "192.168.127.0/24"
	// GatewayMAC 是网关的 MAC（gvisor-tap-vsock 的默认值）。
	GatewayMAC = "5a:94:ef:e4:0c:dd"
)

// NewConfiguration 组装 gvisor-tap-vsock 的配置：
//   - 子网 192.168.127.0/24，网关 .1，虚拟机 .2（静态地址，见 guestabi）；
//   - Forwards、DNS 区域一律为空：不开宿主端口转发；NAT 只在有回环白名单时有一条（见下）；
//   - Ec2MetadataAccess 关闭；
//   - DialContext（绑定网卡、失败即拒绝）、DNSExchange、Outbound（出站策略）必须非空。
//     DNSExchange 为 nil 时一律 REFUSED（开放模式）；收紧模式传 GatewayDNS.Exchange（只应答第一跳域名）。
//
// 注意：本包从不对外提供 gvisor-tap-vsock 的 services 接口（含 /forwarder/expose），
// 虚拟机无法让宿主开监听端口。
func NewConfiguration(d *Dialer, ref *PolicyRef, dnsx func(context.Context, *dns.Msg) (*dns.Msg, error)) (*types.Configuration, error) {
	if d == nil || ref == nil || ref.Load() == nil {
		return nil, errors.New("拨号器和出站策略都必须提供")
	}
	if d.Rules == nil {
		d.Rules = ref
	}
	// forwarder 先调 Outbound 再拨号；被策略拒绝的目标根本到不了拨号器，所以在这里计数，
	// 与 Dialer 里的拒绝合成同一个 policy_denied。
	outbound := func(network string, dst netip.AddrPort) bool {
		if ref.Allows(network, dst) {
			return true
		}
		d.denied.Add(1)
		return false
	}
	// 回环白名单：网关上挂一个虚拟地址 HostLoopbackIP，经 NAT 映射到本机 127.0.0.1，
	// 由拨号器（不绑网卡）代为连接。当且仅当白名单里有回环项时才加；白名单在 abroad-vm 启动时确定
	// （NAT 表在 virtualnetwork.New 时就固定了）。forwarder 先做 NAT 再查出站策略，
	// 所以 127.0.0.1 上白名单之外的端口仍被拒绝。
	if dnsx == nil {
		dnsx = RefuseDNS
	}
	nat := map[string]string{}
	var vips []string
	if hasLoopback(ref.Load().Allow) {
		nat[guestabi.HostLoopbackIP] = "127.0.0.1"
		vips = []string{guestabi.HostLoopbackIP}
	}
	return &types.Configuration{
		MTU:               1500,
		Subnet:            Subnet,
		GatewayIP:         guestabi.DefaultGW,
		GatewayMacAddress: GatewayMAC,
		DNS:               nil,
		Forwards:          map[string]string{},
		NAT:               nat,
		GatewayVirtualIPs: vips,
		DHCPStaticLeases:  map[string]string{},
		Protocol:          types.VfkitProtocol,
		Ec2MetadataAccess: false,
		DialContext:       d.DialContext,
		DNSExchange:       dnsx,
		Outbound:          outbound,
	}, nil
}
