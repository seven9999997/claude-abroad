package usernet

import (
	"context"

	"github.com/miekg/dns"
)

// RefuseDNS 是虚拟机网关的 DNS：对任何查询都回 REFUSED（保留 ID 和问题段），不调用任何解析器。
// 设计 4.5：虚拟机不使用本机解析器，只有引擎自己用 DoH 解析节点域名。
func RefuseDNS(_ context.Context, q *dns.Msg) (*dns.Msg, error) {
	m := new(dns.Msg)
	m.SetRcode(q, dns.RcodeRefused)
	m.RecursionAvailable = false
	return m, nil
}
