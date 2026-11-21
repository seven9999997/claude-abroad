// abroad-wfp-probe 是 WR1 的第一级探查（design-windows 0.3、0.5 决定 4、第 7 条）：只用动态过滤器（程序一退出，
// 系统自动删除），自带时限（默认 120 秒，最长 300 秒）。验证并记录：
//
//   - 默认拦截是否生效（公网 TCP、UDP）；回环、局域网是否照常；
//   - IPv6 是否被拦；装过滤器之前建立的连接之后还能不能用；
//   - 以另一个本地账户运行的进程能否按 SID 被放行（用户按 README 用另一个账户执行一条命令；没执行就记“未执行”）；
//   - 枚举到的我们的过滤器是否与期望的摘要一致；
//   - 探查前后别家在出站连接层的过滤器（只记名字和数量）。
//
// 每一项同时记下离线求值器的预测和实际结果，不一致的单独标出。启动时先做还原工具的空跑：已经有我们的对象就拒绝开始。
//
// 本文件是纯逻辑（步骤表、预测、判定、报告、脱敏），在 Mac 上测试；真正的连接和 WFP 调用在 probe_windows.go。
package main

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"claude-abroad/internal/wfprules"
)

// 时限。
const (
	DefaultDuration = 120 * time.Second
	MaxDuration     = 300 * time.Second
	MinDuration     = 20 * time.Second
)

// PlaceholderSID 是没有给 --allow-sid 时放进规则的账户：没有任何进程以它运行（规则至少要有一个放行账户）。
const PlaceholderSID = "S-1-5-80-0-0-0-0-1"

// Outcome 是一次实际尝试的结果。
type Outcome string

const (
	Allowed    Outcome = "放行"   // 连上了，或者被对端拒绝/重置（包已经出了本机）
	Blocked    Outcome = "拦截"   // 本机拒绝（WSAEACCES 10013：被 WFP 拦下）
	Unknown    Outcome = "无法判断" // 超时、没有路由等，分不清是谁挡的
	NotRun     Outcome = "未执行"
	Mismatch   Outcome = "不一致"
	Consistent Outcome = "一致"
)

// Kind 是步骤的类别。
type Kind string

const (
	KindTCP       Kind = "tcp"         // 新建 TCP 连接
	KindUDP       Kind = "udp"         // 发一个 UDP 包并等回应（DNS 查询）
	KindEstablish Kind = "established" // 过滤器装上之前建立的连接，装上之后再收发
	KindChild     Kind = "child"       // 另一个账户运行的子命令（结果文件）
	KindEnum      Kind = "enum"        // 枚举比对
)

// Step 是一项检查。
type Step struct {
	ID     string
	Title  string
	Kind   Kind
	Target netip.AddrPort // TCP/UDP 的目标
	// Loopback、SID 用于离线预测。
	Loopback bool
	SID      string
}

// Plan 是探查的输入（由命令行参数和本机信息算出）。
type Plan struct {
	Duration  time.Duration
	AllowSID  string         // 另一个本地账户的 SID；为空时用 PlaceholderSID，子命令那一项记“未执行”
	SelfSID   string         // 探查进程自己的账户
	LAN       []netip.Prefix // 本机网卡的直连网段（算放行的局域网段用）
	Gateway   netip.Addr     // 默认网关（局域网那一项的目标）；无效时那一项记“无法判断”
	Public4   netip.AddrPort // 公网 IPv4 TCP 目标
	PublicDNS netip.AddrPort // 公网 DNS（UDP 53）
	Public6   netip.AddrPort // 公网 IPv6 TCP 目标
	Keep      netip.Addr     // 远程执行时开发机的地址（SSH 回包的预测）；无效时不做那一项预测
	KeepPort  uint16         // 本机 SSH 端口
}

// Config 是交给 wfprules 的规则配置：没有虚拟网卡（W-2a 不建网卡），允许局域网。
func (p Plan) Config() wfprules.Config {
	sid := p.AllowSID
	if sid == "" {
		sid = PlaceholderSID
	}
	return wfprules.Config{AllowSIDs: []string{sid}, LAN: wfprules.LANPrefixes(p.LAN)}
}

// Validate 检查计划。
func (p Plan) Validate() error {
	if p.Duration < MinDuration || p.Duration > MaxDuration {
		return fmt.Errorf("时限要在 %v 到 %v 之间", MinDuration, MaxDuration)
	}
	if p.AllowSID != "" && !wfprules.ValidSID(p.AllowSID) {
		return errors.New("--allow-sid 不是允许的账户 SID（只收本机普通账户 S-1-5-21-…-RID（RID ≥ 1000）或服务虚拟账户）")
	}
	if p.AllowSID != "" && p.AllowSID == p.SelfSID {
		return errors.New("--allow-sid 不能是运行探查的这个账户：那样整个探查都会被放行，结果没有意义")
	}
	if p.Keep.IsValid() {
		in := false
		for _, l := range wfprules.LANPrefixes(p.LAN) {
			if l.Contains(p.Keep) {
				in = true
			}
		}
		if !in {
			return errors.New("远程执行时开发机的地址不在放行的局域网段里：装上过滤器后远程连接会断，拒绝开始")
		}
	}
	return nil
}

// Steps 由计划生成步骤表（顺序固定）。
func (p Plan) Steps() []Step {
	s := []Step{
		{ID: "loopback", Title: "回环上的 TCP 连接", Kind: KindTCP, Loopback: true, SID: p.SelfSID},
		{ID: "public-tcp", Title: "公网 IPv4 TCP（默认拦截）", Kind: KindTCP, Target: p.Public4, SID: p.SelfSID},
		{ID: "public-udp", Title: "公网 DNS 查询（UDP 53，默认拦截）", Kind: KindUDP, Target: p.PublicDNS, SID: p.SelfSID},
		{ID: "lan-tcp", Title: "到默认网关的 TCP（允许局域网）", Kind: KindTCP, Target: netip.AddrPortFrom(p.Gateway, 80), SID: p.SelfSID},
		{ID: "ipv6-tcp", Title: "公网 IPv6 TCP（一律拦截）", Kind: KindTCP, Target: p.Public6, SID: p.SelfSID},
		{ID: "established", Title: "装过滤器之前建立的公网连接，装上之后再收发", Kind: KindEstablish, Target: p.Public4, SID: p.SelfSID},
		{ID: "child-sid", Title: "另一个本地账户的进程到公网（按 SID 放行）", Kind: KindChild, Target: p.Public4, SID: p.AllowSID},
		{ID: "enum", Title: "枚举到的我们的过滤器与期望的摘要一致", Kind: KindEnum},
	}
	return s
}

// Predict 用离线求值器算一项的预测。established 和 enum 没有求值器的预测（返回 Unknown 和说明）。
func Predict(fs []wfprules.Filter, st Step) (Outcome, string) {
	switch st.Kind {
	case KindEstablish:
		return Unknown, "求值器只建模新连接（ALE 连接授权层），已建立的连接是否被重新评估正是要实测的"
	case KindEnum:
		return Allowed, "期望一致"
	}
	if st.Kind == KindChild && st.SID == "" {
		return NotRun, "没有给 --allow-sid"
	}
	c := wfprules.Conn{Loopback: st.Loopback, SID: st.SID, Proto: "tcp", Remote: st.Target.Addr(), RemotePort: st.Target.Port()}
	if st.Kind == KindUDP {
		c.Proto = "udp"
	}
	if st.Loopback {
		c.Remote = netip.MustParseAddr("127.0.0.1")
	}
	if !c.Remote.IsValid() {
		return Unknown, "没有目标地址"
	}
	c.Remote = c.Remote.Unmap()
	c.V6 = c.Remote.Is6()
	v, err := wfprules.Eval(fs, c)
	if err != nil {
		return Unknown, "求值失败：" + err.Error()
	}
	if v.Permit {
		return Allowed, "命中 " + orDefault(v.Matched)
	}
	return Blocked, "命中 " + v.Matched
}

func orDefault(s string) string {
	if s == "" {
		return "（没有命中，默认放行）"
	}
	return s
}

// KeepPrediction 预测远程执行时开发机的 SSH 连接（本机发出的回包）在过滤器装上后是否放行。
// 回包在 ALE 连接层不重新授权（连接是对端发起的，授权发生在接收层），这里按“本机向开发机发起连接”保守地预测：
// 放行才允许远程执行。
func KeepPrediction(fs []wfprules.Filter, p Plan, selfSID string) (bool, string) {
	if !p.Keep.IsValid() {
		return true, "不是远程执行"
	}
	v, err := wfprules.Eval(fs, wfprules.Conn{SID: selfSID, Proto: "tcp", LocalPort: p.KeepPort, RemotePort: 50000, Remote: p.Keep})
	if err != nil {
		return false, err.Error()
	}
	return v.Permit, orDefault(v.Matched)
}

// Result 是一项的结果。
type Result struct {
	Step      Step
	Predicted Outcome
	Why       string
	Actual    Outcome
	Detail    string // 实际结果的细节（错误码、耗时）
}

// Verdict 判定预测与实际是否一致：任何一边是“无法判断”或“未执行”时不算不一致。
func Verdict(r Result) Outcome {
	switch {
	case r.Actual == NotRun || r.Predicted == NotRun:
		return NotRun
	case r.Actual == Unknown || r.Predicted == Unknown:
		return Unknown
	case r.Actual == r.Predicted:
		return Consistent
	}
	return Mismatch
}

// Report 是整次探查的输出。
type Report struct {
	Started, Finished time.Time
	Duration          time.Duration
	Rules             string // 期望的摘要
	OthersBefore      string
	OthersAfter       string
	Results           []Result
	CleanBefore       []string // 开始前还原工具空跑的计划
	LeftAfter         []string // 结束后（会话关闭后）我们的对象
	Notes             []string
}

// Format 把报告写成文本（脱敏前）。
func (r Report) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# abroad-wfp-probe 第一级（只用动态过滤器）\n开始 %s，结束 %s，时限 %v\n\n", r.Started.Format(time.RFC3339), r.Finished.Format(time.RFC3339), r.Duration)
	fmt.Fprintf(&b, "## 开始前还原工具空跑\n")
	if len(r.CleanBefore) == 0 {
		b.WriteString("没有我们的对象\n")
	} else {
		for _, l := range r.CleanBefore {
			b.WriteString(l + "\n")
		}
	}
	mism := 0
	b.WriteString("\n## 各项结果（预测 / 实际 / 判定）\n")
	for _, x := range r.Results {
		v := Verdict(x)
		mark := ""
		if v == Mismatch {
			mark = "  ←←← 不一致"
			mism++
		}
		fmt.Fprintf(&b, "- [%s] %s：预测 %s（%s）；实际 %s（%s）；%s%s\n", x.Step.ID, x.Step.Title, x.Predicted, x.Why, x.Actual, x.Detail, v, mark)
	}
	fmt.Fprintf(&b, "\n不一致 %d 项。\n", mism)
	b.WriteString("\n## 结束后（动态会话关闭后）我们的对象\n")
	if len(r.LeftAfter) == 0 {
		b.WriteString("没有残留\n")
	} else {
		for _, l := range r.LeftAfter {
			b.WriteString(l + "\n")
		}
	}
	b.WriteString("\n## 开始前别家在出站连接层的过滤器（名字和数量）\n" + r.OthersBefore)
	b.WriteString("\n## 结束后别家在出站连接层的过滤器（名字和数量）\n" + r.OthersAfter)
	b.WriteString("\n## 期望的规则摘要\n" + r.Rules)
	if len(r.Notes) > 0 {
		b.WriteString("\n## 备注\n")
		for _, n := range r.Notes {
			b.WriteString("- " + n + "\n")
		}
	}
	return b.String()
}

var (
	ipv4Re = regexp.MustCompile(`\b(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})\b`)
	sidRe  = regexp.MustCompile(`S-1-5-21-\d+-\d+-\d+-(\d+)`)
	ipv6Re = regexp.MustCompile(`\b[0-9a-fA-F]{1,4}(:[0-9a-fA-F]{0,4}){2,7}\b`)
)

// Redact 给要贴回的输出脱敏：本机账户名、机器名换成占位；私有 IPv4 只保留前两段；SID 只保留最后的 RID；
// 公网 IPv6 只保留前两组。公网 IPv4 目标（探查用的固定公共地址）保留，便于对照。secrets 是要整串替换的词。
func Redact(text string, secrets []string) string {
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, s := range secrets {
		if len(s) >= 2 {
			text = replaceFold(text, s, "<本机名>")
		}
	}
	text = sidRe.ReplaceAllString(text, "S-1-5-21-x-x-x-$1")
	text = ipv4Re.ReplaceAllStringFunc(text, func(m string) string {
		a, err := netip.ParseAddr(m)
		if err != nil || !(a.IsPrivate() || a.IsLinkLocalUnicast() || isCGNAT(a)) {
			return m
		}
		f := strings.Split(m, ".")
		return f[0] + "." + f[1] + ".x.x"
	})
	text = ipv6Re.ReplaceAllStringFunc(text, func(m string) string {
		a, err := netip.ParseAddr(m)
		if err != nil || a.Is4() || a.IsLoopback() {
			return m
		}
		f := strings.Split(m, ":")
		if len(f) < 3 {
			return m
		}
		return f[0] + ":" + f[1] + ":x:x"
	})
	return text
}

func isCGNAT(a netip.Addr) bool { return netip.MustParsePrefix("100.64.0.0/10").Contains(a) }

func replaceFold(s, old, repl string) string {
	lo := strings.ToLower(s)
	ol := strings.ToLower(old)
	var b strings.Builder
	for {
		i := strings.Index(lo, ol)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i] + repl)
		s, lo = s[i+len(old):], lo[i+len(old):]
	}
}

// ParseDuration 解析 --seconds。
func ParseDuration(v string) (time.Duration, error) {
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, errors.New("--seconds 要是整数")
	}
	return time.Duration(n) * time.Second, nil
}
