// Package exitcheck 是出口检查（design 8，m4-plan M4-2）：经指定出口做 HTTPS、UDP、DoH 探测，取出口 IP，
// 本地判断是否在大陆段，按出口变化规则与基准比对；位置按 IP 缓存。
//
// 所有网络访问都经传入的“按出口开流”的接口（stack.Opener），不直接拨号（源码检查守着）。
// 出口 IP、城市、运营商只在内存里的 Report 和基准文件（0600）里；Report 的字符串、JSON 形式只有摘要。
package exitcheck

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"os"

	"claude-abroad/internal/wire"
)

// Result 是检查结果：通过、不通过、无法判断。
type Result string

const (
	Pass    Result = "pass"
	Fail    Result = "fail"
	Unknown Result = "unknown"
)

// Cause 是一项检查得到这个结果的原因。
type Cause string

const (
	CauseOK              Cause = "ok"
	CauseMainland        Cause = "mainland"         // 明确测到出口在大陆地址段
	CauseRuleViolation   Cause = "rule_violation"   // 明确测到违反出口变化规则
	CauseUnexpectedReply Cause = "unexpected_reply" // 收到了应答但不是预期的（被劫持到别的页面、证书不对）
	CauseTimeout         Cause = "timeout"
	CauseConnect         Cause = "connect_failed"
	CauseRateLimited     Cause = "rate_limited" // 查询源限流或服务端出错（429、5xx）
	CauseBadFormat       Cause = "bad_format"   // 查询源的应答格式不对、缺字段
	CauseChannelDown     Cause = "channel_down" // 数据通道本身不可用
	CauseNoBaseline      Cause = "no_baseline"  // 没有基准（要重新“应用主代理”）
	CauseNotNeeded       Cause = "not_needed"   // 这条规则不需要这项（例如同一国家不查位置）
)

// causeResult 是“原因 → 结果”的判定表（m4-plan v2.1 决定 3）：只在这里定义一次。
// 连接失败归为“无法判断”而不是“不通过”：不因为一次抖动就关闸门，持续 10 分钟才按不通过处理（M4-3）。
var causeResult = map[Cause]Result{
	CauseOK:              Pass,
	CauseNotNeeded:       Pass,
	CauseMainland:        Fail,
	CauseRuleViolation:   Fail,
	CauseUnexpectedReply: Fail,
	CauseTimeout:         Unknown,
	CauseConnect:         Unknown,
	CauseRateLimited:     Unknown,
	CauseBadFormat:       Unknown,
	CauseChannelDown:     Unknown,
	CauseNoBaseline:      Unknown,
}

// ResultOf 按判定表给出结果；表里没有的原因按“无法判断”（偏向不误杀，同时不会放行：放行要求明确通过）。
func ResultOf(c Cause) Result {
	if r, ok := causeResult[c]; ok {
		return r
	}
	return Unknown
}

// probeError 是一项探测的失败：带原因。
type probeError struct {
	cause Cause
	msg   string
}

func (e *probeError) Error() string { return e.msg }

func fail(c Cause, msg string) error { return &probeError{cause: c, msg: msg} }

// causeOf 把探测的错误归类。网络类故障（超时、连接失败、通道不可用）一律不是“不通过”；
// 证书不对算“收到了不是预期的应答”。
func causeOf(err error) Cause {
	if err == nil {
		return CauseOK
	}
	var pe *probeError
	if errors.As(err, &pe) {
		return pe.cause
	}
	var cv *tls.CertificateVerificationError
	var ua x509.UnknownAuthorityError
	var he x509.HostnameError
	var ci x509.CertificateInvalidError
	if errors.As(err, &cv) || errors.As(err, &ua) || errors.As(err, &he) || errors.As(err, &ci) {
		return CauseUnexpectedReply
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return CauseTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return CauseTimeout
	}
	var re *wire.ReplyError
	if errors.As(err, &re) {
		return CauseConnect // 出口回了拨号失败等：对端或出口这一次不可达
	}
	if errors.Is(err, errChannel) {
		return CauseChannelDown
	}
	return CauseConnect
}

// errChannel 标记“数据通道本身打不开”（开流时不是出口的回应错误）。
var errChannel = errors.New("数据通道不可用")
