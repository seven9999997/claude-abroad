package exitconf

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// 分享链接导入成分流出口（M6-6c）：SS、VMess、VLESS、Trojan、Hysteria2、TUIC 的分享链接，每行一个，转成只含
// proxies 的 Clash（mihomo）配置，再经 ParseClash 做和订阅出口一样的校验（节点字段白名单、端点合法）。之后按订阅出口
// 存储和下发（同一个引擎、同一套校验），原始链接不保存。
//
// 分享链接里有密码：调用方只从文件或标准输入读（不经命令行参数），这里的错误信息只说第几行、哪一项，不回显链接内容。

// ShareLink 是分享链接这一种来源（存储时转成 Clash）。
const ShareLink Kind = "sharelink"

// MaxShareLinks 是一次最多导入的链接数。
const MaxShareLinks = 50

// ShareLinksToClash 把若干行分享链接转成 Clash 配置（YAML），并校验。空行和 # 开头的行跳过。
func ShareLinksToClash(text string) ([]byte, error) {
	var proxies []map[string]any
	n := 0
	for i, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		n++
		if n > MaxShareLinks {
			return nil, lineErr(i+1, "", fmt.Sprintf("链接太多（最多 %d 个）", MaxShareLinks))
		}
		p, err := parseShareLink(line)
		if err != nil {
			return nil, lineErr(i+1, "", err.Error())
		}
		proxies = append(proxies, p)
	}
	if len(proxies) == 0 {
		return nil, lineErr(0, "", "没有分享链接")
	}
	b, err := yaml.Marshal(map[string]any{"proxies": proxies})
	if err != nil {
		return nil, lineErr(0, "", "生成配置失败")
	}
	if _, err := ParseClash(b); err != nil {
		return nil, err
	}
	return b, nil
}

func parseShareLink(s string) (map[string]any, error) {
	scheme, _, ok := strings.Cut(s, "://")
	if !ok {
		return nil, fmt.Errorf("不是分享链接")
	}
	switch strings.ToLower(scheme) {
	case "ss":
		return parseSS(s)
	case "vmess":
		return parseVMess(s)
	case "vless":
		return parseVLESS(s)
	case "trojan":
		return parseTrojan(s)
	case "hysteria2", "hy2":
		return parseHy2(s)
	case "tuic":
		return parseTUIC(s)
	}
	return nil, fmt.Errorf("不支持的链接种类（只支持 ss、vmess、vless、trojan、hysteria2、tuic）")
}

func b64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, fmt.Errorf("base64 解不开")
}

func hostPort(u *url.URL) (string, int, error) {
	h := u.Hostname()
	p, err := strconv.Atoi(u.Port())
	if h == "" || err != nil || p <= 0 || p > 65535 {
		return "", 0, fmt.Errorf("服务器地址或端口不对")
	}
	return h, p, nil
}

// 不认识的参数一律拒绝（M6-6c 补审）：映射不全时让用户看到导入失败，而不是连到配置不对的地方。
// 错误里只有参数名，不带值。
var (
	tlsParams       = []string{"sni", "peer", "alpn", "fp", "allowInsecure", "insecure"}
	transportParams = []string{"type", "path", "host", "serviceName"}
	// 取这些值时和没写一样（常见的链接会带上），别的值不认识。
	noopValues = map[string][]string{"headerType": {"none"}, "encryption": {"none"}}
)

func checkParams(q url.Values, allowed ...[]string) error {
	ok := map[string]bool{}
	for _, xs := range allowed {
		for _, x := range xs {
			ok[x] = true
		}
	}
	for k, vs := range q {
		if ok[k] {
			continue
		}
		v := ""
		if len(vs) > 0 {
			v = vs[0]
		}
		if v == "" {
			continue
		}
		known := false
		for _, nv := range noopValues[k] {
			known = known || v == nv
		}
		if !known {
			return fmt.Errorf("不认识的参数 %q（为免连到配置不对的地方，不导入）", k)
		}
	}
	return nil
}

func parseURL(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("链接格式不对")
	}
	return u, nil
}

// addTLS 按查询串加上 TLS 相关的字段。
func addTLS(p map[string]any, q url.Values, sniKey string) {
	if v := q.Get("sni"); v != "" {
		p[sniKey] = v
	} else if v := q.Get("peer"); v != "" {
		p[sniKey] = v
	}
	if v := q.Get("alpn"); v != "" {
		p["alpn"] = strings.Split(v, ",")
	}
	if v := q.Get("fp"); v != "" {
		p["client-fingerprint"] = v
	}
	if q.Get("allowInsecure") == "1" || q.Get("insecure") == "1" {
		p["skip-cert-verify"] = true
	}
}

// addTransport 按 type（tcp、ws、grpc、http、h2）加上传输层的选项。
func addTransport(p map[string]any, q url.Values) error {
	switch t := q.Get("type"); t {
	case "", "tcp":
	case "ws":
		p["network"] = "ws"
		opts := map[string]any{}
		if v := q.Get("path"); v != "" {
			opts["path"] = v
		}
		if v := q.Get("host"); v != "" {
			opts["headers"] = map[string]any{"Host": v}
		}
		p["ws-opts"] = opts
	case "grpc":
		p["network"] = "grpc"
		p["grpc-opts"] = map[string]any{"grpc-service-name": q.Get("serviceName")}
	case "http", "h2":
		p["network"] = "h2"
		opts := map[string]any{}
		if v := q.Get("path"); v != "" {
			opts["path"] = v
		}
		if v := q.Get("host"); v != "" {
			opts["host"] = []string{v}
		}
		p["h2-opts"] = opts
	default:
		return fmt.Errorf("不支持的传输方式 %q", t)
	}
	return nil
}

func name(u *url.URL, def string) string {
	if u.Fragment != "" {
		return u.Fragment
	}
	return def
}

// ss://base64(method:password)@host:port#name，或 ss://base64(method:password@host:port)#name（SIP002 和旧格式）。
func parseSS(s string) (map[string]any, error) {
	rest := strings.TrimPrefix(s[strings.Index(s, "://")+3:], "")
	frag := ""
	if i := strings.Index(rest, "#"); i >= 0 {
		rest, frag = rest[:i], rest[i+1:]
	}
	if i := strings.Index(rest, "?"); i >= 0 {
		q, _ := url.ParseQuery(rest[i+1:])
		if q.Get("plugin") != "" {
			return nil, fmt.Errorf("不支持带插件的 SS 链接")
		}
		if err := checkParams(q, []string{"plugin"}); err != nil {
			return nil, err
		}
		rest = rest[:i]
	}
	var userinfo, hostport string
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		userinfo, hostport = rest[:at], rest[at+1:]
		if dec, err := b64(userinfo); err == nil && strings.Contains(string(dec), ":") {
			userinfo = string(dec)
		} else if u, err := url.PathUnescape(userinfo); err == nil {
			userinfo = u
		}
	} else {
		dec, err := b64(rest)
		if err != nil {
			return nil, err
		}
		at := strings.LastIndex(string(dec), "@")
		if at < 0 {
			return nil, fmt.Errorf("链接格式不对")
		}
		userinfo, hostport = string(dec[:at]), string(dec[at+1:])
	}
	method, password, ok := strings.Cut(userinfo, ":")
	if !ok || method == "" || password == "" {
		return nil, fmt.Errorf("加密方式或密码缺失")
	}
	u, err := parseURL("ss://" + hostport)
	if err != nil {
		return nil, err
	}
	h, p, err := hostPort(u)
	if err != nil {
		return nil, err
	}
	n, _ := url.PathUnescape(frag)
	if n == "" {
		n = "ss"
	}
	return map[string]any{"name": n, "type": "ss", "server": h, "port": p, "cipher": method, "password": password, "udp": true}, nil
}

// vmess://base64(JSON)（v2rayN 格式）。
func parseVMess(s string) (map[string]any, error) {
	b, err := b64(s[len("vmess://"):])
	if err != nil {
		return nil, err
	}
	var v map[string]any
	if json.Unmarshal(b, &v) != nil {
		return nil, fmt.Errorf("内容不是 JSON")
	}
	for k, x := range v {
		switch k {
		case "v", "ps", "add", "port", "id", "aid", "scy", "net", "host", "path", "tls", "sni":
			continue
		}
		if x == nil || x == "" || (k == "type" && x == "none") {
			continue
		}
		return nil, fmt.Errorf("不认识的参数 %q（为免连到配置不对的地方，不导入）", k)
	}
	str := func(k string) string {
		switch x := v[k].(type) {
		case string:
			return x
		case float64:
			return strconv.Itoa(int(x))
		}
		return ""
	}
	port, err := strconv.Atoi(str("port"))
	if err != nil || str("add") == "" || port <= 0 || port > 65535 {
		return nil, fmt.Errorf("服务器地址或端口不对")
	}
	if str("id") == "" {
		return nil, fmt.Errorf("缺少 id")
	}
	aid, _ := strconv.Atoi(str("aid"))
	cipher := str("scy")
	if cipher == "" {
		cipher = "auto"
	}
	p := map[string]any{"name": firstNonEmpty(str("ps"), "vmess"), "type": "vmess", "server": str("add"), "port": port,
		"uuid": str("id"), "alterId": aid, "cipher": cipher, "udp": true}
	q := url.Values{}
	q.Set("type", str("net"))
	q.Set("path", str("path"))
	q.Set("host", str("host"))
	if str("net") == "grpc" {
		q.Set("serviceName", str("path"))
	}
	if err := addTransport(p, q); err != nil {
		return nil, err
	}
	if t := str("tls"); t != "" && t != "none" && t != "tls" {
		return nil, fmt.Errorf("不支持的 tls %q", t)
	}
	if str("tls") == "tls" {
		p["tls"] = true
		if sni := firstNonEmpty(str("sni"), str("host")); sni != "" {
			p["servername"] = sni
		}
	}
	return p, nil
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

// vless://uuid@host:port?encryption=none&security=tls|reality&sni=&fp=&pbk=&sid=&type=&path=&host=&flow=#name
func parseVLESS(s string) (map[string]any, error) {
	u, err := parseURL(s)
	if err != nil {
		return nil, err
	}
	h, port, err := hostPort(u)
	if err != nil {
		return nil, err
	}
	if u.User == nil || u.User.Username() == "" {
		return nil, fmt.Errorf("缺少 uuid")
	}
	q := u.Query()
	if err := checkParams(q, tlsParams, transportParams, []string{"flow", "security", "pbk", "sid"}); err != nil {
		return nil, err
	}
	p := map[string]any{"name": name(u, "vless"), "type": "vless", "server": h, "port": port, "uuid": u.User.Username(), "udp": true}
	if v := q.Get("flow"); v != "" {
		p["flow"] = v
	}
	switch q.Get("security") {
	case "tls":
		p["tls"] = true
		addTLS(p, q, "servername")
	case "reality":
		p["tls"] = true
		addTLS(p, q, "servername")
		ro := map[string]any{"public-key": q.Get("pbk")}
		if v := q.Get("sid"); v != "" {
			ro["short-id"] = v
		}
		p["reality-opts"] = ro
	case "", "none":
	default:
		return nil, fmt.Errorf("不支持的 security %q", q.Get("security"))
	}
	if err := addTransport(p, q); err != nil {
		return nil, err
	}
	return p, nil
}

// trojan://password@host:port?sni=&type=&path=&host=&allowInsecure=#name
func parseTrojan(s string) (map[string]any, error) {
	u, err := parseURL(s)
	if err != nil {
		return nil, err
	}
	h, port, err := hostPort(u)
	if err != nil {
		return nil, err
	}
	if u.User == nil || u.User.Username() == "" {
		return nil, fmt.Errorf("缺少密码")
	}
	q := u.Query()
	if err := checkParams(q, tlsParams, transportParams, []string{"security"}); err != nil {
		return nil, err
	}
	if sec := q.Get("security"); sec != "" && sec != "tls" {
		return nil, fmt.Errorf("不支持的 security %q", sec)
	}
	p := map[string]any{"name": name(u, "trojan"), "type": "trojan", "server": h, "port": port, "password": u.User.Username(), "udp": true}
	addTLS(p, q, "sni")
	if err := addTransport(p, q); err != nil {
		return nil, err
	}
	return p, nil
}

// hysteria2://auth@host:port?sni=&insecure=&obfs=salamander&obfs-password=#name（hy2:// 同）
func parseHy2(s string) (map[string]any, error) {
	u, err := parseURL(s)
	if err != nil {
		return nil, err
	}
	h, port, err := hostPort(u)
	if err != nil {
		return nil, err
	}
	if u.User == nil {
		return nil, fmt.Errorf("缺少密码")
	}
	pw := u.User.Username()
	if p2, ok := u.User.Password(); ok {
		pw += ":" + p2
	}
	q := u.Query()
	if err := checkParams(q, tlsParams, []string{"obfs", "obfs-password"}); err != nil {
		return nil, err
	}
	p := map[string]any{"name": name(u, "hysteria2"), "type": "hysteria2", "server": h, "port": port, "password": pw}
	addTLS(p, q, "sni")
	if v := q.Get("obfs"); v != "" {
		p["obfs"] = v
		p["obfs-password"] = q.Get("obfs-password")
	}
	return p, nil
}

// tuic://uuid:password@host:port?sni=&congestion_control=&alpn=&udp_relay_mode=#name
func parseTUIC(s string) (map[string]any, error) {
	u, err := parseURL(s)
	if err != nil {
		return nil, err
	}
	h, port, err := hostPort(u)
	if err != nil {
		return nil, err
	}
	if u.User == nil || u.User.Username() == "" {
		return nil, fmt.Errorf("缺少 uuid")
	}
	pw, _ := u.User.Password()
	q := u.Query()
	if err := checkParams(q, tlsParams, []string{"congestion_control", "udp_relay_mode"}); err != nil {
		return nil, err
	}
	p := map[string]any{"name": name(u, "tuic"), "type": "tuic", "server": h, "port": port, "uuid": u.User.Username(), "password": pw}
	addTLS(p, q, "sni")
	if v := q.Get("congestion_control"); v != "" {
		p["congestion-controller"] = v
	}
	if v := q.Get("udp_relay_mode"); v != "" {
		p["udp-relay-mode"] = v
	}
	return p, nil
}
