package sniff

import (
	"bytes"
	"net"
	"strings"

	"claude-abroad/internal/rules"
)

// MaxHTTPHeader 是 HTTP 请求头的上限，超过按“没有 Host”处理。
const MaxHTTPHeader = 8 << 10

var httpMethods = []string{"GET", "HEAD", "POST", "PUT", "DELETE", "OPTIONS", "PATCH", "CONNECT", "TRACE"}

// maybeHTTP 判断 b 是否可能是某个白名单方法加空格的前缀（或以它开头）。
func maybeHTTP(b []byte) bool {
	for _, m := range httpMethods {
		p := m + " "
		n := min(len(b), len(p))
		if string(b[:n]) == p[:n] {
			return true
		}
	}
	return false
}

// HTTP 判定明文 HTTP 请求的 Host。不是 HTTP、没有 Host、头部超长、Host 不合法都返回 NotTLS
// （明文 HTTP 不会承载 Claude，m3-plan M3-2）。
func HTTP(b []byte) (r Result, needMore bool) {
	notTLS := Result{Kind: rules.NotTLS}
	if len(b) == 0 {
		return Result{}, true
	}
	if !maybeHTTP(b) {
		return notTLS, false
	}
	end := bytes.Index(b, []byte("\r\n\r\n"))
	if end < 0 {
		end = bytes.Index(b, []byte("\n\n"))
	}
	if end < 0 {
		if len(b) >= MaxHTTPHeader {
			return notTLS, false
		}
		return Result{}, true
	}
	if end > MaxHTTPHeader {
		return notTLS, false
	}
	lines := strings.Split(strings.ReplaceAll(string(b[:end]), "\r\n", "\n"), "\n")
	req := strings.Split(lines[0], " ")
	if len(req) != 3 || !strings.HasPrefix(req[2], "HTTP/1.") {
		return notTLS, false
	}
	if req[0] == "CONNECT" {
		return hostResult(req[1], notTLS), false
	}
	host := ""
	for _, l := range lines[1:] {
		k, v, ok := strings.Cut(l, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "host") {
			continue
		}
		if host != "" {
			return notTLS, false // 多个 Host：说不清
		}
		host = strings.TrimSpace(v)
		if host == "" {
			return notTLS, false
		}
	}
	if host == "" {
		return notTLS, false
	}
	return hostResult(host, notTLS), false
}

// hostResult 去掉端口后归一；IP 字面量和不合法的名字按没有 Host 处理。
func hostResult(hp string, bad Result) Result {
	h := hp
	if hh, _, err := net.SplitHostPort(hp); err == nil {
		h = hh
	} else if strings.HasPrefix(hp, "[") {
		return bad
	}
	return nameResult(h, rules.HTTPHost, bad)
}
