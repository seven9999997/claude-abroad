// Package preflight 是启用前的冲突检查（设计 10.4，M2-8）。
//
// 只读由代码保证：命令一律经 sysnet.ReadOnlyRunner（调用方传什么 Runner 都再包一层），文件经注入的 fs.FS 只读。
// 结果和日志里不出现代理地址、端口、PAC 地址、环境变量的值、进程的完整路径（只写“已配置”和软件名）。
// 任一检查的命令失败或输出不认识，产生一条“无法判断”的阻断项，不当成没有冲突（网络共享除外，只提示）。
package preflight

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"

	"claude-abroad/internal/sysnet"
	"claude-abroad/internal/usererror"
)

// Severity 是检查项的严重程度。
type Severity string

const (
	Block Severity = "block" // 阻断：解决后才能启用
	Warn  Severity = "warn"  // 只提示
)

// Finding 是一条检查结果。文字里不含地址、端口、值和完整路径。
type Finding struct {
	ID       string   // 检查项（system_proxy、pac、auto_discovery、takeover_route、ipv6_only、loopback_hop、proxy_app、shell_env、sharing、resolver_lock、claude_settings、claude_proxy_port）
	Severity Severity //
	Title    string   // 发现了什么
	Fix      string   // 怎么处理
	// Undecidable 表示这一项检查本身没能完成（环境检测据此给“无法判断”，M4-6a）。
	Undecidable bool
}

// Report 是一次检查的全部结果。
type Report struct{ Findings []Finding }

// Blocking 返回阻断项。
func (r Report) Blocking() []Finding {
	return slices.DeleteFunc(slices.Clone(r.Findings), func(f Finding) bool { return f.Severity != Block })
}

// Warnings 返回提示项。
func (r Report) Warnings() []Finding {
	return slices.DeleteFunc(slices.Clone(r.Findings), func(f Finding) bool { return f.Severity != Warn })
}

// NATPlist 是网络共享的配置文件（相对 Root）。
const NATPlist = "Library/Preferences/SystemConfiguration/com.apple.nat.plist"

// ShellFiles 是要看的 shell 配置（相对 Home）。
var ShellFiles = []string{".zshrc", ".zprofile", ".bash_profile", ".bashrc", ".profile"}

// MaxFileSize 是单个配置文件的读取上限。
const MaxFileSize = 1 << 20

// Checker 是冲突检查器，实现 state.Preflight。
type Checker struct {
	R    sysnet.Runner // 内部一律再包一层 ReadOnlyRunner
	Home fs.FS         // 用户主目录（读 shell 配置）；nil 时跳过这一项
	Root fs.FS         // 文件系统根（读网络共享的 plist）；nil 时跳过这一项
	// OurUtun 是我们自己的 utun 名（从 DISABLED 启用或 ENABLING 重试时非空）：经它的路由不算冲突。
	OurUtun func() string
	// FirstHop 返回主代理的第一跳主机（域名或地址）；nil 表示还没有主代理，跳过这一项。
	FirstHop func() (string, error)
	// ResolverConflicts 返回 Claude 域名解析锁有别人同名文件的域名（M6-3，设计 10.4）；nil 时跳过这一项。
	ResolverConflicts func() []string
	// ClaudeSettingsConflicts 返回别的托管设置文件定义了代理变量的“文件：变量”（M6-3b）；nil 时跳过。
	ClaudeSettingsConflicts func() []string
	// ClaudeProxyDown 报告第四层的本机代理端口没在听（被占用；M6-3b）；nil 时跳过。
	ClaudeProxyDown func() bool
	// IntegrityProblem 返回程序完整性校验不通过的说明（M6-7）；nil 或返回空时跳过。
	IntegrityProblem func() string

	mu   sync.Mutex
	last Report
}

func (c *Checker) runner() sysnet.Runner { return sysnet.ReadOnlyRunner{R: c.R} }

func undecidable(id, what string) Finding {
	return Finding{ID: id, Severity: Block, Title: "无法判断" + what, Fix: "检查本身没能完成。请稍后重试；仍不行请重启电脑后再试。", Undecidable: true}
}

// TakeoverConflicts 是环境检测第 11 项复用的部分（m4-plan v2.6 决定 6）：系统代理、PAC、自动发现、别的软件占着路由
// （经我们自己 utun 的路由不算，见 OurUtun）。只读。
func (c *Checker) TakeoverConflicts(ctx context.Context) []Finding {
	return append(c.checkProxy(ctx), c.checkRoutes(ctx)...)
}

// Run 跑全部检查。
func (c *Checker) Run(ctx context.Context) Report {
	var r Report
	r.Findings = append(r.Findings, c.checkProxy(ctx)...)
	r.Findings = append(r.Findings, c.checkRoutes(ctx)...)
	r.Findings = append(r.Findings, c.checkIPv6Only(ctx)...)
	r.Findings = append(r.Findings, c.checkFirstHop()...)
	r.Findings = append(r.Findings, c.checkProxyApps(ctx)...)
	r.Findings = append(r.Findings, c.checkShell()...)
	r.Findings = append(r.Findings, c.checkSharing()...)
	r.Findings = append(r.Findings, c.checkResolverLock()...)
	r.Findings = append(r.Findings, c.checkClaudeCode()...)
	if c.IntegrityProblem != nil {
		if p := c.IntegrityProblem(); p != "" {
			r.Findings = append(r.Findings, Finding{ID: "integrity", Severity: Block, Title: p,
				Fix: "Claude 出国的程序或虚拟机镜像被改过或损坏：请重新安装 Claude 出国。在此之前只保留断网规则，不接管网络。"})
		}
	}
	c.mu.Lock()
	c.last = r
	c.mu.Unlock()
	return r
}

// Last 返回最近一次检查的全部结果（含提示项，给界面用）。
func (c *Checker) Last() Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

// ErrBlocked 是有阻断项。
var ErrBlocked = errors.New("启用前的冲突检查没有通过")

// Check 实现 state.Preflight：有阻断项时返回 usererror，列出每一项和处理办法；没有阻断项返回 nil。
func (c *Checker) Check(ctx context.Context) error {
	r := c.Run(ctx)
	blocks := r.Blocking()
	if len(blocks) == 0 {
		return nil
	}
	var reason, next []string
	for i, f := range blocks {
		reason = append(reason, f.Title)
		next = append(next, fmt.Sprintf("%d. %s", i+1, f.Fix))
	}
	return usererror.New("启用前的冲突检查", strings.Join(reason, "；"), "请按下面处理后再启用："+strings.Join(next, " "), ErrBlocked)
}

// ---- Claude 域名解析锁的冲突（M6-3）----

// checkResolverLock：/etc/resolver 下已有别人写的、和解析锁同名的文件（只提示：不影响启用，影响的是回国后的第一层）。
func (c *Checker) checkResolverLock() []Finding {
	if c.ResolverConflicts == nil {
		return nil
	}
	doms := c.ResolverConflicts()
	if len(doms) == 0 {
		return nil
	}
	return []Finding{{ID: "resolver_lock", Severity: Warn,
		Title: "Claude 域名解析锁有别的软件写的同名文件（" + strings.Join(doms, "、") + "）",
		Fix:   "回国后“Claude 连不上”的第一层对这些域名不生效。请确认这些文件是哪个软件写的；不再需要时移走它们，再在设置里重新写入解析锁。"}}
}

// ---- 第四层：Claude Code 的托管设置和本机代理端口（M6-3b）----

// checkClaudeCode：别的托管设置文件定义了代理变量（第四层被盖掉）、本机代理端口被占用（占用者如果是代理，
// Claude Code 的流量会交给它），都是必须处理的阻断项。
func (c *Checker) checkClaudeCode() []Finding {
	var out []Finding
	if c.ClaudeSettingsConflicts != nil {
		if cs := c.ClaudeSettingsConflicts(); len(cs) > 0 {
			out = append(out, Finding{ID: "claude_settings", Severity: Block,
				Title: "Claude Code 的托管设置里别的文件也定义了代理变量（" + strings.Join(cs, "、") + "）",
				Fix:   "第四层（回国后没拦住的 Claude Code 一律连不上）会被盖掉。请确认这些文件是谁写的，去掉其中的代理变量后再试；不会替你改别人的文件。"})
		}
	}
	if c.ClaudeProxyDown != nil && c.ClaudeProxyDown() {
		out = append(out, Finding{ID: "claude_proxy_port", Severity: Block,
			Title: "第四层的本机代理端口被别的程序占着",
			Fix:   "占用者如果是代理，Claude Code 的流量会交给它。请退出占用这个端口的程序后重启 Claude 出国的后台服务。"})
	}
	return out
}

// ---- 系统代理、PAC、自动发现 ----

func (c *Checker) checkProxy(ctx context.Context) []Finding {
	so, _, err := c.runner().Run(ctx, sysnet.PathScutil, []string{"--proxy"}, nil)
	if err != nil {
		return []Finding{undecidable("system_proxy", "系统代理的设置")}
	}
	kv, ok := parseScutilDict(so)
	if !ok {
		return []Finding{undecidable("system_proxy", "系统代理的设置")}
	}
	var out []Finding
	if kv["HTTPEnable"] == "1" || kv["HTTPSEnable"] == "1" || kv["SOCKSEnable"] == "1" {
		out = append(out, Finding{ID: "system_proxy", Severity: Block, Title: "系统代理已开启（已配置）",
			Fix: "在“系统设置 → 网络 → 详细信息 → 代理”里关闭网页代理、安全网页代理和 SOCKS 代理，或在代理软件里关掉“设为系统代理”。"})
	}
	if kv["ProxyAutoConfigEnable"] == "1" {
		out = append(out, Finding{ID: "pac", Severity: Block, Title: "自动代理配置（PAC）已开启（已配置）",
			Fix: "在“系统设置 → 网络 → 详细信息 → 代理”里关闭“自动代理配置”。"})
	}
	if kv["ProxyAutoDiscoveryEnable"] == "1" {
		out = append(out, Finding{ID: "auto_discovery", Severity: Block, Title: "自动代理发现已开启",
			Fix: "在“系统设置 → 网络 → 详细信息 → 代理”里关闭“自动代理发现”。"})
	}
	return out
}

// parseScutilDict 解析 scutil 打印的顶层字典（只取 “键 : 值” 的简单行）。
func parseScutilDict(out []byte) (map[string]string, bool) {
	t := strings.TrimSpace(string(out))
	if !strings.HasPrefix(t, "<dictionary>") {
		return nil, false
	}
	kv := map[string]string{}
	for _, l := range strings.Split(t, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(l), " : ")
		if ok {
			kv[k] = strings.TrimSpace(v)
		}
	}
	return kv, true
}

// ---- 别的虚拟网卡占着路由 ----

// RouteProbes 是要看走哪个接口的目的地：默认路由、两条 /1 各一个地址、fake-IP DNS。
// 别的 TUN 常用比 /1 更具体的一组路由（本机实测 Clash 用 1/8、2/7 … 64/2 和 128.0/1），所以按地址探，不只看 /1 在不在。
var RouteProbes = []string{"default", "1.1.1.1", "129.0.0.1", "198.18.0.2"}

// checkRoutes 先看整张路由表（sysnet.ConflictIfaces，M2-9a），再按地址探测实际出口作复核；任一发现都阻断。
func (c *Checker) checkRoutes(ctx context.Context) []Finding {
	ours := ""
	if c.OurUtun != nil {
		ours = c.OurUtun()
	}
	taken, err := sysnet.MacRoutes{R: c.runner()}.Conflicts(ctx, ours)
	if err != nil {
		return []Finding{undecidable("takeover_route", "路由表")}
	}
	for _, dst := range RouteProbes {
		so, _, err := c.runner().Run(ctx, sysnet.PathRoute, []string{"-n", "get", dst}, nil)
		if err != nil {
			return []Finding{undecidable("takeover_route", "路由的去向")}
		}
		ifName, err := sysnet.ParseRouteGet(so)
		if err != nil {
			return []Finding{undecidable("takeover_route", "路由的去向")}
		}
		if ifName != ours && sysnet.IsVirtualIf(ifName) && !slices.Contains(taken, ifName) {
			taken = append(taken, ifName)
		}
	}
	if len(taken) == 0 {
		return nil
	}
	return []Finding{{ID: "takeover_route", Severity: Block, Title: "别的软件的虚拟网卡占着路由（接口 " + strings.Join(taken, "、") + "）",
		Fix: "先退出它，或关掉它的接管模式（Clash 的 TUN 模式、Surge 的增强模式、其他 VPN）。"}}
}

// ---- 只有 IPv6 ----

func (c *Checker) checkIPv6Only(ctx context.Context) []Finding {
	so, _, err := c.runner().Run(ctx, sysnet.PathScutil, []string{"--nwi"}, nil)
	if err != nil {
		return []Finding{undecidable("ipv6_only", "当前网络是否有 IPv4")}
	}
	t := string(so)
	i4 := strings.Index(t, "IPv4 network interface information")
	i6 := strings.Index(t, "IPv6 network interface information")
	if i4 < 0 || i6 < i4 {
		return []Finding{undecidable("ipv6_only", "当前网络是否有 IPv4")}
	}
	v4, v6 := t[i4:i6], t[i6:]
	has := func(sec string) bool { return strings.Contains(sec, "address") && !strings.Contains(sec, "No IPv") }
	if !has(v4) && has(v6) {
		return []Finding{{ID: "ipv6_only", Severity: Block, Title: "当前网络只有 IPv6",
			Fix: "Claude 出国会关闭 IPv6，这个网络下无法使用。请换一个有 IPv4 的网络。"}}
	}
	if !has(v4) {
		return []Finding{{ID: "ipv6_only", Severity: Block, Title: "当前没有可用的 IPv4 网络", Fix: "请先连上网络再启用。"}}
	}
	return nil
}

// ---- 主代理第一跳是本机回环 ----

func (c *Checker) checkFirstHop() []Finding {
	if c.FirstHop == nil {
		return nil
	}
	host, err := c.FirstHop()
	if err != nil {
		return []Finding{undecidable("loopback_hop", "主代理的第一跳")}
	}
	h := strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	a, perr := netip.ParseAddr(h)
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || (perr == nil && (a.IsLoopback() || a.IsUnspecified())) {
		return []Finding{{ID: "loopback_hop", Severity: Block, Title: "主代理的第一跳是本机回环",
			Fix: "本机的代理软件当主代理会形成回环。请改用远端或局域网里的节点作为主代理。"}}
	}
	return nil
}

// ---- 代理软件在运行 ----

// ProxyApps 是认得出来的代理软件（进程名里包含这些字样，不分大小写）。
var ProxyApps = []string{"clash", "mihomo", "surge", "sing-box", "v2ray", "xray", "shadowsocks", "ss-local", "trojan",
	"flclash", "stash", "quantumult", "loon", "hysteria", "naive", "tuic", "shadowrocket"}

func (c *Checker) checkProxyApps(ctx context.Context) []Finding {
	so, _, err := c.runner().Run(ctx, sysnet.PathPs, []string{"-axo", "comm"}, nil)
	if err != nil {
		return []Finding{undecidable("proxy_app", "有没有代理软件在运行")}
	}
	lines := strings.Split(strings.TrimSpace(string(so)), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "COMM" {
		return []Finding{undecidable("proxy_app", "有没有代理软件在运行")}
	}
	var names []string
	for _, l := range lines[1:] {
		base := path.Base(strings.TrimSpace(l)) // 只留程序名，不出现完整路径
		lb := strings.ToLower(base)
		for _, p := range ProxyApps {
			if strings.Contains(lb, p) && !slices.Contains(names, base) {
				names = append(names, base)
				break
			}
		}
	}
	if len(names) == 0 {
		return nil
	}
	slices.Sort(names)
	return []Finding{{ID: "proxy_app", Severity: Warn, Title: "有代理软件在运行：" + strings.Join(names, "、"),
		Fix: "继续运行不拦。它的节点可以导入成主代理或分流出口；它在分流规则里不能设为“本机网络”。"}}
}

// ---- shell 配置里的代理环境变量 ----

var envRe = regexp.MustCompile(`(?i)^\s*(export\s+)?(http_proxy|https_proxy|all_proxy)\s*=`)

func (c *Checker) checkShell() []Finding {
	if c.Home == nil {
		return nil
	}
	var hits []string
	var unread []string
	for _, name := range ShellFiles {
		f, err := c.Home.Open(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			unread = append(unread, name)
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
		f.Close()
		if err != nil || len(data) > MaxFileSize {
			unread = append(unread, name)
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(data))
		sc.Buffer(make([]byte, 64*1024), MaxFileSize)
		for n := 1; sc.Scan(); n++ {
			if envRe.MatchString(sc.Text()) {
				hits = append(hits, fmt.Sprintf("~/%s 第 %d 行", name, n))
			}
		}
	}
	var out []Finding
	if len(hits) > 0 {
		out = append(out, Finding{ID: "shell_env", Severity: Warn, Title: "Shell 配置里写了代理环境变量：" + strings.Join(hits, "、"),
			Fix: "请删掉或注释这些行。Claude 带着代理环境变量启动会被拦下。"})
	}
	if len(unread) > 0 {
		out = append(out, Finding{ID: "shell_env", Severity: Warn, Title: "无法检查 Shell 配置：" + strings.Join(unread, "、"),
			Fix: "文件读不出来或超过 1 MiB，请自己确认里面没有 HTTP_PROXY、HTTPS_PROXY、ALL_PROXY。"})
	}
	return out
}

// ---- 网络共享 ----

func (c *Checker) checkSharing() []Finding {
	if c.Root == nil {
		return nil
	}
	unknown := []Finding{{ID: "sharing", Severity: Warn, Title: "无法判断网络共享是否开启",
		Fix: "如果开着“互联网共享”，启用后共享出去的设备会断网。"}}
	f, err := c.Root.Open(NATPlist)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // 从没配置过网络共享
	}
	if err != nil {
		return unknown
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	f.Close()
	if err != nil || len(data) > MaxFileSize || bytes.HasPrefix(data, []byte("bplist")) {
		return unknown
	}
	root, err := parsePlist(data)
	if err != nil {
		return unknown
	}
	top, _ := root.(map[string]any)
	nat, ok := top["NAT"].(map[string]any)
	if !ok {
		return nil
	}
	switch v := nat["Enabled"].(type) {
	case int64:
		if v != 0 {
			return sharingOn()
		}
	case bool:
		if v {
			return sharingOn()
		}
	case nil:
	default:
		return unknown
	}
	return nil
}

func sharingOn() []Finding {
	return []Finding{{ID: "sharing", Severity: Warn, Title: "网络共享（互联网共享）已开启",
		Fix: "启用后共享出去的设备会断网。不需要的话在“系统设置 → 通用 → 共享”里关掉。"}}
}

// parsePlist 解析 XML plist 的 dict、array、integer、true、false、string、data、real（只为读网络共享，不引入依赖）。
func parsePlist(data []byte) (any, error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	d.Strict = false
	for {
		tok, err := d.Token()
		if err != nil {
			return nil, err
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "plist" {
			return plistValue(d, nil)
		}
	}
}

// plistValue 读下一个值；start 非 nil 时它就是该值的起始元素。
func plistValue(d *xml.Decoder, start *xml.StartElement) (any, error) {
	if start == nil {
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			if se, ok := tok.(xml.StartElement); ok {
				start = &se
				break
			}
		}
	}
	switch start.Name.Local {
	case "dict":
		m := map[string]any{}
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.EndElement:
				return m, nil
			case xml.StartElement:
				if t.Name.Local != "key" {
					return nil, errors.New("plist：dict 里缺 key")
				}
				var k string
				if err := d.DecodeElement(&k, &t); err != nil {
					return nil, err
				}
				v, err := plistValue(d, nil)
				if err != nil {
					return nil, err
				}
				m[k] = v
			}
		}
	case "array":
		var a []any
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.EndElement:
				return a, nil
			case xml.StartElement:
				v, err := plistValue(d, &t)
				if err != nil {
					return nil, err
				}
				a = append(a, v)
			}
		}
	case "true", "false":
		if err := d.Skip(); err != nil {
			return nil, err
		}
		return start.Name.Local == "true", nil
	case "integer":
		var s string
		if err := d.DecodeElement(&s, start); err != nil {
			return nil, err
		}
		var n int64
		if _, err := fmt.Sscan(strings.TrimSpace(s), &n); err != nil {
			return nil, err
		}
		return n, nil
	case "string", "data", "real", "date":
		var s string
		if err := d.DecodeElement(&s, start); err != nil {
			return nil, err
		}
		return s, nil
	}
	return nil, fmt.Errorf("plist：不认识的元素 %s", start.Name.Local)
}
