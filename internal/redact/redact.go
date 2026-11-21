// Package redact 去掉日志里的敏感内容（CLAUDE.md：日志里不写出口 IP、订阅地址、账号密码）。
// abroad-agent 的引擎日志和 abroad-vm 的第三方库日志（gvisor-tap-vsock 的 logrus）共用。
package redact

import (
	"bytes"
	"io"
	"net/netip"
	"regexp"
	"strings"
	"sync"
)

const redacted = "<已脱敏>"

// redactIPv6 处理一段“像 IPv6”的文本。匹配可能带着前缀（例如 "server:2001:db8::1"
// 被切成 ":2001:db8::1"，或时间 "12:34:56.2001:db8::1"）：整体解析不了时，
// 依次去掉开头的一段再试，找到地址就只替换地址部分。不是地址（例如时间 12:34:56）就原样保留。
func redactIPv6(m string) string {
	parse := func(c string) (netip.Addr, string, bool) {
		addr := strings.TrimRight(c, ":.")
		a, err := netip.ParseAddr(addr)
		return a, c[len(addr):], err == nil && a.Is6()
	}
	// 从左到右，在开头和每个分隔符（: 或 .）之后试，第一个能解析的就是最长的地址。
	for i := 0; i < len(m); i++ {
		if i > 0 && m[i-1] != ':' && m[i-1] != '.' {
			continue
		}
		a, tail, ok := parse(m[i:])
		if !ok {
			continue
		}
		if a.IsLoopback() || a.IsUnspecified() {
			return m
		}
		return m[:i] + redacted + tail
	}
	return m
}

var (
	reURLUser  = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/@\s]+@`)
	reURLPath  = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://[^/?#\s"']+)[/?#][^\s"']*`)
	reSecretKV = regexp.MustCompile(`(?i)\b(password|passwd|pass|secret|token|uuid|private[-_]?key|pre[-_]?shared[-_]?key|psk|auth|key)("?\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s,;&}\]]+)`)
	reIPv4     = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)
	reIPv6     = regexp.MustCompile(`[0-9A-Fa-f]*:[0-9A-Fa-f:]*:[0-9A-Fa-f:.]*`)
)

// Redact 去掉日志里的敏感内容（CLAUDE.md：日志里不写出口 IP、订阅地址、账号密码）：
// URL 里的 userinfo、路径和查询串（只留协议和主机）、password/secret/token/uuid/key 一类“键=值”“键: 值”的值、
// 非回环的 IPv4 和 IPv6 字面量。回环和未指定地址（127.0.0.1、::1、0.0.0.0）保留，便于排错。
func String(s string) string {
	s = reURLUser.ReplaceAllString(s, "${1}"+redacted+"@")
	s = reURLPath.ReplaceAllString(s, "${1}/"+redacted) // 订阅地址的口令常在路径或查询串里
	s = reSecretKV.ReplaceAllString(s, "${1}${2}"+redacted)
	s = reIPv4.ReplaceAllStringFunc(s, func(m string) string {
		if a, err := netip.ParseAddr(m); err == nil && (a.IsLoopback() || a.IsUnspecified()) {
			return m
		}
		return redacted
	})
	s = reIPv6.ReplaceAllStringFunc(s, redactIPv6)
	return s
}

// Writer 按行脱敏后写到 W（行内不完整的部分先缓存）。并发安全。
type Writer struct {
	W   io.Writer
	mu  sync.Mutex
	buf []byte
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			if len(w.buf) > 64<<10 { // 没有换行的超长输出：先按一行写出
				_, _ = io.WriteString(w.W, String(string(w.buf))+"\n")
				w.buf = w.buf[:0]
			}
			return n, nil
		}
		_, _ = io.WriteString(w.W, String(string(w.buf[:i]))+"\n")
		w.buf = w.buf[i+1:]
	}
}
