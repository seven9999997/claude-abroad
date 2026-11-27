package usernet

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"syscall"
)

// 第一跳拨号失败的类别（2026-11-27 真机：启用期间虚拟机对第一跳没有一次成功的拨号，而原来的计数只记成功、策略拒绝和
// 绑定失败，连不上的那些一个都没记下）。只记类别和“第几个第一跳”，不记地址。
const (
	DialFailPerm    = "perm"    // 权限被拒（EPERM、EACCES：多半是 pf 拦下）
	DialFailUnreach = "unreach" // 主机或网络不可达（ENETUNREACH、EHOSTUNREACH、ENETDOWN、EHOSTDOWN）
	DialFailTimeout = "timeout" // 超时（ETIMEDOUT、拨号时限到）
	DialFailRefused = "refused" // 连接被拒（ECONNREFUSED）
	DialFailDNS     = "dns"     // 第一跳域名解析失败（网关 DNS 回 SERVFAIL）
	DialFailBind    = "bind"    // 识别或绑定物理网卡失败
	DialFailOther   = "other"   // 其他
)

// DialFailClasses 是全部类别（固定顺序，计数和日志按这个顺序）。
var DialFailClasses = []string{DialFailPerm, DialFailUnreach, DialFailTimeout, DialFailRefused, DialFailDNS, DialFailBind, DialFailOther}

// DialFailLabel 是类别的中文说明（日志用）。
func DialFailLabel(class string) string {
	switch class {
	case DialFailPerm:
		return "权限被拒"
	case DialFailUnreach:
		return "主机或网络不可达"
	case DialFailTimeout:
		return "超时"
	case DialFailRefused:
		return "连接被拒"
	case DialFailDNS:
		return "域名解析失败"
	case DialFailBind:
		return "绑定网卡失败"
	}
	return "其他错误"
}

// ClassifyDialError 把拨号错误归到一个类别（不看错误文字里的地址）。
func ClassifyDialError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errBind), errors.Is(err, errPhysical):
		return DialFailBind
	case errors.Is(err, syscall.EPERM), errors.Is(err, syscall.EACCES):
		return DialFailPerm
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH),
		errors.Is(err, syscall.ENETDOWN), errors.Is(err, syscall.EHOSTDOWN):
		return DialFailUnreach
	case errors.Is(err, syscall.ECONNREFUSED):
		return DialFailRefused
	case errors.Is(err, syscall.ETIMEDOUT), errors.Is(err, context.DeadlineExceeded):
		return DialFailTimeout
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return DialFailDNS
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return DialFailTimeout
	}
	return DialFailOther
}

// classIndex 是类别在 DialFailClasses 里的下标（不认识的按 other）。
func classIndex(class string) int {
	for i, c := range DialFailClasses {
		if c == class {
			return i
		}
	}
	return len(DialFailClasses) - 1
}

// dialFailCounts 是各类别的计数。
type dialFailCounts [7]atomic.Int64

// noteFail 计一次失败并调用钩子（dst 只交给钩子，由它决定怎么记——abroad-vm 只记第几个第一跳）。
func (d *Dialer) noteFail(network string, dst netip.AddrPort, class string) {
	d.fails[classIndex(class)].Add(1)
	if d.OnDialFailed != nil {
		d.OnDialFailed(network, dst, class)
	}
}

// NoteResolveFailed 记一次第一跳域名解析失败（网关 DNS 回 SERVFAIL 时由组装方调用）。
func (d *Dialer) NoteResolveFailed() { d.fails[classIndex(DialFailDNS)].Add(1) }

// DialFailCounts 返回各类别的计数（键是类别）。
func (d *Dialer) DialFailCounts() map[string]int64 {
	out := make(map[string]int64, len(DialFailClasses))
	for i, c := range DialFailClasses {
		out[c] = d.fails[i].Load()
	}
	return out
}
