package exitconf

import (
	"bufio"
	"bytes"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// OVPN 是校验过的 OpenVPN 客户端配置（m4-plan v2 建议改 8、v2.8 决定 3）。
//   - 只认白名单里的指令；脚本类（up、down、route-up…、script-security）、插件、管理接口、改运行环境的
//     （config、cd、chroot、daemon、log、status、user、setenv…）一律拒绝；
//   - 证书密钥只收内联形式（<ca>…</ca> 等），引用外部文件的指令一律拒绝；
//   - 影响路由和 DNS 的指令（redirect-gateway、route、dhcp-option、block-outside-dns…）去掉并记下：
//     默认路由由 agent 只在出口命名空间里设，推送的 DNS 由 agent 自带的固定脚本接收（R5 的结论）。
type OVPN struct {
	Proto   string     // udp 或 tcp
	Remotes []Endpoint // remote 指令
	Options [][]string // 白名单里的普通指令（原样的参数，已校验字符）
	Inline  map[string]string
	Dropped []string // 去掉了哪些指令（给用户看）
}

// 允许的普通指令 → 参数个数范围（-1 表示不限）。
var ovpnAllowed = map[string][2]int{
	"client": {0, 0}, "tls-client": {0, 0}, "nobind": {0, 0}, "persist-key": {0, 0}, "persist-tun": {0, 0},
	"remote-random": {0, 0}, "resolv-retry": {1, 1}, "remote-cert-tls": {1, 1}, "verify-x509-name": {1, 2},
	"cipher": {1, 1}, "data-ciphers": {1, 1}, "data-ciphers-fallback": {1, 1}, "ncp-ciphers": {1, 1}, "auth": {1, 1},
	"compress": {0, 1}, "comp-lzo": {0, 1}, "allow-compression": {1, 1}, "key-direction": {1, 1},
	"tls-version-min": {1, 2}, "tls-cipher": {1, 1}, "tls-ciphersuites": {1, 1}, "verb": {1, 1}, "mute": {1, 1},
	"mute-replay-warnings": {0, 0}, "reneg-sec": {1, 1}, "keepalive": {2, 2}, "ping": {1, 1}, "ping-restart": {1, 1},
	"sndbuf": {1, 1}, "rcvbuf": {1, 1}, "mssfix": {0, 1}, "tun-mtu": {1, 1}, "fragment": {1, 1},
	"auth-nocache": {0, 0}, "explicit-exit-notify": {0, 1}, "pull": {0, 0}, "connect-retry": {1, 2},
	"connect-retry-max": {1, 1}, "server-poll-timeout": {1, 1}, "hand-window": {1, 1}, "tls-timeout": {1, 1},
	"float": {0, 0}, "auth-retry": {1, 1}, "port": {1, 1},
}

// 去掉的指令（影响路由、DNS 或设备，由 agent 接管）。
var ovpnDropped = map[string]bool{
	"redirect-gateway": true, "route": true, "route-ipv6": true, "dhcp-option": true, "block-outside-dns": true,
	"route-nopull": true, "register-dns": true, "route-metric": true, "route-delay": true,
	"dev": true, "dev-type": true, "tun-ipv6": true, "route-method": true, "ip-win32": true,
}

// 只收内联形式的（作为指令出现即为引用外部文件，拒绝）。
var ovpnInline = map[string]bool{
	"ca": true, "cert": true, "key": true, "tls-auth": true, "tls-crypt": true, "tls-crypt-v2": true,
	"auth-user-pass": true, "extra-certs": true,
}

var ovpnArgRe = regexp.MustCompile(`^[A-Za-z0-9._:@=+/,-]{1,128}$`)

// ParseOVPN 解析并校验 OpenVPN 客户端配置。
func ParseOVPN(b []byte) (OVPN, error) {
	o := OVPN{Inline: map[string]string{}}
	if len(b) > 256<<10 {
		return o, lineErr(0, "", "配置太大")
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 4096), 256<<10)
	var block string
	var blockStart int
	var body strings.Builder
	isClient := false
	for n := 1; sc.Scan(); n++ {
		raw := sc.Text()
		line := strings.TrimSpace(raw)
		if block != "" {
			if line == "</"+block+">" {
				o.Inline[block] = body.String()
				block = ""
				body.Reset()
				continue
			}
			if strings.HasPrefix(line, "<") && !strings.HasPrefix(line, "-----") {
				return o, lineErr(n, block, "内联块没有结束")
			}
			body.WriteString(line + "\n")
			continue
		}
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if strings.HasPrefix(line, "<") && strings.HasSuffix(line, ">") && !strings.HasPrefix(line, "</") {
			tag := line[1 : len(line)-1]
			if !ovpnInline[tag] {
				return o, lineErr(n, tag, "不允许的内联块")
			}
			if _, dup := o.Inline[tag]; dup {
				return o, lineErr(n, tag, "内联块重复")
			}
			block, blockStart = tag, n
			continue
		}
		f := strings.Fields(line)
		d := strings.ToLower(strings.TrimPrefix(f[0], "--"))
		args := f[1:]
		switch {
		case d == "remote":
			if len(args) < 1 || len(args) > 3 {
				return o, lineErr(n, d, "参数个数不对")
			}
			port, proto := 1194, ""
			if len(args) >= 2 {
				p, err := strconv.Atoi(args[1])
				if err != nil {
					return o, lineErr(n, d, "端口不是数字")
				}
				port = p
			}
			if len(args) == 3 {
				proto = normProto(args[2])
				if proto == "" {
					return o, lineErr(n, d, "协议只能是 udp 或 tcp")
				}
			}
			ep, err := checkEndpoint(args[0], port, proto)
			if err != nil {
				return o, lineErr(n, d, err.Error())
			}
			o.Remotes = append(o.Remotes, ep)
		case d == "proto":
			if len(args) != 1 || normProto(args[0]) == "" {
				return o, lineErr(n, d, "只能是 udp 或 tcp（客户端）")
			}
			o.Proto = normProto(args[0])
		case d == "client":
			isClient = true
			o.Options = append(o.Options, []string{d})
		case ovpnDropped[d]:
			if d == "dev" && (len(args) != 1 || !strings.HasPrefix(args[0], "tun")) {
				return o, lineErr(n, d, "只支持 tun 设备")
			}
			if d == "dev-type" && (len(args) != 1 || args[0] != "tun") {
				return o, lineErr(n, d, "只支持 tun 设备")
			}
			if d != "dev" && d != "dev-type" {
				o.Dropped = append(o.Dropped, d)
			}
		case d == "pull-filter":
			// 推送项由 agent 统一过滤（design 7.3 v0.14）；pull-filter 先匹配者胜，用户的一条 accept 就能盖过我们的过滤。
			return o, lineErr(n, d, "不允许 pull-filter（服务端推送的选项由程序统一过滤）")
		case ovpnInline[d]:
			return o, lineErr(n, d, "证书、密钥、账号只收内联形式（<"+d+">…</"+d+">），不能引用外部文件")
		default:
			r, ok := ovpnAllowed[d]
			if !ok {
				return o, lineErr(n, d, "不允许的指令（脚本、插件、管理接口、改运行环境或引用外部文件的指令一律拒绝）")
			}
			if len(args) < r[0] || (r[1] >= 0 && len(args) > r[1]) {
				return o, lineErr(n, d, "参数个数不对")
			}
			for _, a := range args {
				if !ovpnArgRe.MatchString(a) {
					return o, lineErr(n, d, "参数里有不允许的字符")
				}
			}
			if d == "auth-retry" && args[0] != "none" && args[0] != "nointeract" {
				return o, lineErr(n, d, "只能是 none 或 nointeract")
			}
			o.Options = append(o.Options, append([]string{d}, args...))
		}
	}
	if block != "" {
		return o, lineErr(blockStart, block, "内联块没有结束")
	}
	if err := sc.Err(); err != nil {
		return o, lineErr(0, "", "读不出配置")
	}
	switch {
	case !isClient:
		return o, lineErr(0, "client", "缺少（只支持客户端配置）")
	case len(o.Remotes) == 0:
		return o, lineErr(0, "remote", "缺少服务器")
	case o.Inline["ca"] == "":
		return o, lineErr(0, "<ca>", "缺少内联的 CA 证书")
	}
	if o.Proto == "" {
		o.Proto = "udp"
	}
	for i := range o.Remotes {
		if o.Remotes[i].Proto == "" {
			o.Remotes[i].Proto = o.Proto
		}
	}
	return o, nil
}

func normProto(p string) string {
	switch strings.ToLower(p) {
	case "udp", "udp4":
		return "udp"
	case "tcp", "tcp4", "tcp-client", "tcp4-client":
		return "tcp"
	}
	return ""
}

// Endpoints 返回全部服务器端点。
func (o OVPN) Endpoints() []Endpoint { return append([]Endpoint(nil), o.Remotes...) }

// GenParams 是重新生成 OpenVPN 配置时 agent 给的参数。
type GenParams struct {
	Dev       string            // 隧道设备名（tun 开头）
	DNSScript string            // agent 自带的固定脚本（接收推送的 DNS）
	ExitDir   string            // 出口自己的目录（脚本把 DNS 写到这里）
	Resolved  map[string]string // 服务器域名 → agent 在根命名空间解析出的 IP
}

// OVPNPullAccept 是接受的服务端推送项（前缀匹配，OpenVPN 的 pull-filter 先匹配者胜）；其余一律忽略（design 7.3 v0.14）。
// 带结尾空格的是为了不误配同前缀的其他选项（如 "ifconfig " 不配 ifconfig-ipv6）。
var OVPNPullAccept = []string{
	"ifconfig ",        // 隧道地址
	"topology ",        // 地址分配方式（subnet/net30），决定 ifconfig 的含义
	"tun-mtu ",         // 隧道 MTU
	"ping ",            // 保活
	"ping-restart ",    // 保活超时后重连
	"peer-id ",         // 数据通道的对端标识（2.4+ 服务端推送）
	"cipher ",          // 协商出的数据通道算法
	"protocol-flags ",  // 2.6 的协议特性协商
	"key-derivation ",  // 2.6 的密钥派生方式
	"auth-token ",      // 服务端下发的重连令牌（auth-user-pass 时用）
	"dhcp-option DNS ", // 推送的 DNS（旧式）
	"dns server ",      // 推送的 DNS（2.6 新式）
}

// Generate 从校验过的结构重新生成客户端配置：固定的设备、route-noexec 加推送项白名单、我们的 DNS 脚本，加上白名单里的指令和内联块。
func (o OVPN) Generate(p GenParams) string {
	var b strings.Builder
	w := func(s ...string) { b.WriteString(strings.Join(s, " ") + "\n") }
	w("client")
	w("dev", p.Dev)
	w("dev-type", "tun")
	w("nobind")
	// 不用 route-nopull：它连推送的 DNS 一起丢（M4-7 端到端发现）。route-noexec 是主保障：OpenVPN 不加、不删任何路由，
	// 默认路由由 agent 只在出口命名空间里设；推送项再按白名单过滤，末尾一条兜底忽略其余。
	w("route-noexec")
	for _, a := range OVPNPullAccept {
		w("pull-filter", "accept", `"`+a+`"`)
	}
	w("pull-filter", "ignore", `""`)
	w("script-security", "2")
	w("setenv", "ABROAD_EXIT_DIR", p.ExitDir)
	w("up", p.DNSScript)
	for _, r := range o.Remotes {
		host := r.Host
		if ip, ok := p.Resolved[host]; ok {
			host = ip
		}
		w("remote", host, strconv.Itoa(r.Port), r.Proto)
	}
	w("proto", o.Proto)
	for _, opt := range o.Options {
		if opt[0] == "client" {
			continue
		}
		w(opt...)
	}
	tags := make([]string, 0, len(o.Inline))
	for t := range o.Inline {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	for _, t := range tags {
		b.WriteString("<" + t + ">\n" + o.Inline[t] + "</" + t + ">\n")
	}
	return b.String()
}
