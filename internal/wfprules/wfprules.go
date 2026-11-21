// Package wfprules 生成 Windows 版断网保护的 WFP 过滤器描述（design-windows 0.5 决定 1），
// 并提供一个只认自己生成的描述的离线求值器，用来证明“不泄漏”。仿照 internal/pf：
//
//   - 规则是逐条展开的规范形式：每个过滤器在每个字段上最多一个条件（WFP 里同一字段的多个条件是“或”，
//     不同字段是“与”；我们只用“与”，同一字段要放行多个值就拆成多个过滤器）；
//   - 同样的输入逐字节相同的输出；
//   - 本包不带构建约束，不调用任何系统接口（有源码检查测试守着），系统层在 internal/wfp。
//
// 求值器对 WFP 仲裁的建模是假设，由第一级探查验证：同一子层内按权重从高到低取第一条命中的过滤器；
// 任何子层给出拦截都是最终结果；所有子层都没有命中时放行（WFP 在 ALE 连接层的默认动作）。
package wfprules

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// GUID 是 WFP 对象的键（按 Windows 的 GUID 文本格式输出，大写）。
type GUID [16]byte

func (g GUID) String() string {
	b := g[:]
	return strings.ToUpper(fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uint32(b[0])<<24|uint32(b[1])<<16|uint32(b[2])<<8|uint32(b[3]),
		uint16(b[4])<<8|uint16(b[5]), uint16(b[6])<<8|uint16(b[7]), b[8:10], b[10:16]))
}

// ParseGUID 解析 8-4-4-4-12 形式的 GUID（大小写都收）。
func ParseGUID(s string) (GUID, error) {
	var g GUID
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return g, errors.New("GUID 格式不对")
	}
	hex := strings.ReplaceAll(s, "-", "")
	for i := 0; i < 16; i++ {
		v, err := strconv.ParseUint(hex[2*i:2*i+2], 16, 8)
		if err != nil {
			return g, errors.New("GUID 格式不对")
		}
		g[i] = byte(v)
	}
	return g, nil
}

// derive 由名字算出确定的 GUID（sha256 截断，置成 RFC 4122 的变体、版本 8），同一个名字永远是同一个键。
func derive(name string) GUID {
	h := sha256.Sum256([]byte("claude-abroad/wfp/" + name))
	var g GUID
	copy(g[:], h[:16])
	g[6] = g[6]&0x0f | 0x80
	g[8] = g[8]&0x3f | 0x80
	return g
}

// 我们的提供者和子层的键是固定的：还原工具只凭这两个键找到并删除我们的全部对象。
var (
	ProviderKey = derive("provider")
	SublayerKey = derive("sublayer")
)

const (
	ProviderName = "Claude Abroad"
	SublayerName = "Claude Abroad leak protection"
	// SublayerWeight 是子层权重（0–65535），取高值让我们的子层先被求值；
	// 求值器的建模里拦截在任何子层都是最终结果，所以权重只影响顺序、不影响结论（由探查验证）。
	SublayerWeight = 0xF000
)

// Layer 是过滤器所在的层。只用 ALE 连接授权层（出站连接、UDP 的第一个包）。
type Layer string

const (
	LayerConnectV4 Layer = "ALE_AUTH_CONNECT_V4"
	LayerConnectV6 Layer = "ALE_AUTH_CONNECT_V6"
)

// Action 是过滤器的动作。
type Action string

const (
	Permit Action = "permit"
	Block  Action = "block"
)

// Field 是条件的字段。
type Field string

const (
	FieldLoopback   Field = "loopback"   // FWPM_CONDITION_FLAGS 含 FWP_CONDITION_FLAG_IS_LOOPBACK；值固定为 "1"
	FieldInterface  Field = "interface"  // FWPM_CONDITION_IP_LOCAL_INTERFACE；值是十进制的接口 LUID
	FieldUser       Field = "user"       // FWPM_CONDITION_ALE_USER_ID；值是 SID 文本
	FieldProtocol   Field = "protocol"   // FWPM_CONDITION_IP_PROTOCOL；值是 tcp、udp
	FieldLocalPort  Field = "localport"  // FWPM_CONDITION_IP_LOCAL_PORT
	FieldRemotePort Field = "remoteport" // FWPM_CONDITION_IP_REMOTE_PORT
	FieldRemoteAddr Field = "remoteaddr" // FWPM_CONDITION_IP_REMOTE_ADDRESS；值是 IPv4 网段（/32 是单个地址）
)

// Cond 是一个“等于”条件（网段条件是“属于”）。
type Cond struct {
	Field Field
	Value string
}

// Filter 是一个过滤器的完整描述。系统层只照它建对象，不自己决定任何参数。
type Filter struct {
	Key      GUID
	Name     string // 稳定的名字，键由它算出
	Layer    Layer
	Sublayer GUID
	Provider GUID
	Weight   uint8 // 0–15，同一子层内越大越先求值
	Action   Action
	Conds    []Cond
}

// Config 是生成规则的输入。
type Config struct {
	// TunLUID 是我们的虚拟网卡（Wintun）的接口 LUID；0 表示还没有网卡（开机时），不输出那一条。
	TunLUID uint64
	// AllowSIDs 是唯一放行去公网的账户（专用服务账户；探查时是用户的另一个本地账户）。至少一个。
	AllowSIDs []string
	// LAN 是允许直连的局域网段；“允许局域网”关闭时为空。只收私有范围，用 LANPrefixes 从网卡的直连网段算出。
	LAN []netip.Prefix
}

// 过滤器的权重：同一子层内越大越先求值。
const (
	wLoopback = 15
	wTun      = 14
	wUser     = 13
	wDHCP     = 12
	wLAN      = 11
	wBlock    = 0
)

// sidRe 只收两类账户：本机或域里的普通账户（S-1-5-21-x-y-z-RID，RID ≥ 1000，挡住 Administrator、Guest 等内置账户）
// 和服务的虚拟账户（S-1-5-80-a-b-c-d-e）。SYSTEM、LOCAL SERVICE、Everyone、Administrators 这类范围太大的一律拒绝：
// 放行它们等于放行大部分进程。
var (
	sid21Re = regexp.MustCompile(`^S-1-5-21-[0-9]{1,10}-[0-9]{1,10}-[0-9]{1,10}-([0-9]{1,10})$`)
	sid80Re = regexp.MustCompile(`^S-1-5-80-[0-9]{1,10}-[0-9]{1,10}-[0-9]{1,10}-[0-9]{1,10}-[0-9]{1,10}$`)
)

// ValidSID 报告 s 是否是允许放行的账户 SID。
func ValidSID(s string) bool {
	if m := sid21Re.FindStringSubmatch(s); m != nil {
		rid, err := strconv.ParseUint(m[1], 10, 32)
		return err == nil && rid >= 1000
	}
	return sid80Re.MatchString(s)
}

// lanAllowed 是局域网段必须整个落入的范围：RFC1918、CGNAT、链路本地、组播（与 internal/pf 相同）。
// 直连网段是公网地址时（例如恶意 Wi-Fi 的 DHCP 把 Anthropic 的地址段发成直连网段）不能放行。
var lanAllowed = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/4"),
}

// dhcpDst 是 DHCP（udp 68 → 67）允许的目的地址：广播和私有范围（与 internal/pf 相同）。
var dhcpDst = []netip.Prefix{
	netip.MustParsePrefix("255.255.255.255/32"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
}

// lanOK 报告 p 是否是整个落在 lanAllowed 某一项里的 IPv4 网段。
func lanOK(p netip.Prefix) bool {
	if !p.IsValid() || !p.Addr().Is4() {
		return false
	}
	p = p.Masked()
	for _, a := range lanAllowed {
		if a.Bits() <= p.Bits() && a.Contains(p.Addr()) {
			return true
		}
	}
	return false
}

// LANPrefixes 由网卡的直连网段算出放行的局域网段：丢掉不在私有范围里的，加上组播 224/4 和链路本地 169.254/16。
// 结果已排序、去重。
func LANPrefixes(direct []netip.Prefix) []netip.Prefix {
	out := []netip.Prefix{netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("169.254.0.0/16")}
	for _, p := range direct {
		if lanOK(p) {
			out = append(out, p.Masked())
		}
	}
	return sortPrefixes(out)
}

func sortPrefixes(in []netip.Prefix) []netip.Prefix {
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b netip.Prefix) int {
		if c := a.Addr().Compare(b.Addr()); c != 0 {
			return c
		}
		return a.Bits() - b.Bits()
	})
	return slices.Compact(out)
}

func newFilter(name string, layer Layer, w uint8, act Action, conds ...Cond) Filter {
	return Filter{Key: derive("filter/" + name), Name: name, Layer: layer, Sublayer: SublayerKey, Provider: ProviderKey, Weight: w, Action: act, Conds: conds}
}

// Render 生成过滤器描述。输入不合法时返回错误，结果为空。
func Render(c Config) ([]Filter, error) {
	if len(c.AllowSIDs) == 0 {
		return nil, errors.New("至少要有一个放行的账户")
	}
	sids := slices.Clone(c.AllowSIDs)
	for _, s := range sids {
		if !ValidSID(s) {
			return nil, errors.New("放行账户的 SID 不合法或范围太大")
		}
	}
	slices.Sort(sids)
	sids = slices.Compact(sids)
	var lan []netip.Prefix
	for _, p := range c.LAN {
		if !lanOK(p) {
			return nil, fmt.Errorf("局域网段 %s 不在允许的私有范围里", p)
		}
		lan = append(lan, p.Masked())
	}
	lan = sortPrefixes(lan)

	var out []Filter
	// IPv6：回环放行，其余一律拦截（包括我们的虚拟网卡和放行账户：虚拟网卡只配 IPv4）。
	out = append(out,
		newFilter("v6/permit-loopback", LayerConnectV6, wLoopback, Permit, Cond{FieldLoopback, "1"}),
		newFilter("v6/block-all", LayerConnectV6, wBlock, Block))
	out = append(out, newFilter("v4/permit-loopback", LayerConnectV4, wLoopback, Permit, Cond{FieldLoopback, "1"}))
	if c.TunLUID != 0 {
		out = append(out, newFilter("v4/permit-tun", LayerConnectV4, wTun, Permit, Cond{FieldInterface, strconv.FormatUint(c.TunLUID, 10)}))
	}
	for _, s := range sids {
		for _, proto := range []string{"tcp", "udp"} {
			out = append(out, newFilter("v4/permit-user/"+s+"/"+proto, LayerConnectV4, wUser, Permit,
				Cond{FieldUser, s}, Cond{FieldProtocol, proto}))
		}
	}
	for _, d := range dhcpDst {
		out = append(out, newFilter("v4/permit-dhcp/"+d.String(), LayerConnectV4, wDHCP, Permit,
			Cond{FieldProtocol, "udp"}, Cond{FieldLocalPort, "68"}, Cond{FieldRemotePort, "67"}, Cond{FieldRemoteAddr, d.String()}))
	}
	for _, p := range lan {
		out = append(out, newFilter("v4/permit-lan/"+p.String(), LayerConnectV4, wLAN, Permit, Cond{FieldRemoteAddr, p.String()}))
	}
	out = append(out, newFilter("v4/block-all", LayerConnectV4, wBlock, Block))
	return out, nil
}

// Summary 把过滤器描述写成规范文本，一个过滤器一行，按（层、权重从高到低、名字）排序：
//
//	<键> <层> w<权重> <动作> sublayer=<子层键> provider=<提供者键> [字段=值 ...] name=<名字>
//
// 条件按字段名排序。同样的过滤器集合（不论顺序）逐字节相同。系统层把枚举到的过滤器转成同样的格式，
// 与期望的摘要比对。
func Summary(fs []Filter) []byte {
	fs = sorted(fs)
	var b bytes.Buffer
	for _, f := range fs {
		fmt.Fprintf(&b, "%s %s w%d %s sublayer=%s provider=%s", f.Key, f.Layer, f.Weight, f.Action, f.Sublayer, f.Provider)
		cs := slices.Clone(f.Conds)
		slices.SortFunc(cs, func(a, b Cond) int { return strings.Compare(string(a.Field), string(b.Field)) })
		for _, c := range cs {
			fmt.Fprintf(&b, " %s=%s", c.Field, c.Value)
		}
		fmt.Fprintf(&b, " name=%s\n", f.Name)
	}
	return b.Bytes()
}

func sorted(fs []Filter) []Filter {
	out := slices.Clone(fs)
	slices.SortStableFunc(out, func(a, b Filter) int {
		if c := strings.Compare(string(a.Layer), string(b.Layer)); c != 0 {
			return c
		}
		if a.Weight != b.Weight {
			return int(b.Weight) - int(a.Weight)
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

var knownFields = map[Field]bool{FieldLoopback: true, FieldInterface: true, FieldUser: true, FieldProtocol: true, FieldLocalPort: true, FieldRemotePort: true, FieldRemoteAddr: true}

// ParseSummary 解析 Summary 的输出。不认识的层、动作、字段，格式不对的值，同一字段出现两次，都是错误：
// 求值器只对自己认识的描述下结论。
func ParseSummary(text []byte) ([]Filter, error) {
	var out []Filter
	for i, line := range strings.Split(strings.TrimRight(string(text), "\n"), "\n") {
		if line == "" {
			if len(text) == 0 {
				break
			}
			return nil, fmt.Errorf("第 %d 行是空行", i+1)
		}
		f, err := parseLine(line)
		if err != nil {
			return nil, fmt.Errorf("第 %d 行：%w", i+1, err)
		}
		out = append(out, f)
	}
	return out, nil
}

func parseLine(line string) (Filter, error) {
	var f Filter
	parts := strings.Split(line, " ")
	if len(parts) < 7 {
		return f, errors.New("字段太少")
	}
	var err error
	if f.Key, err = ParseGUID(parts[0]); err != nil {
		return f, err
	}
	f.Layer = Layer(parts[1])
	w, err := strconv.ParseUint(strings.TrimPrefix(parts[2], "w"), 10, 8)
	if err != nil || !strings.HasPrefix(parts[2], "w") {
		return f, errors.New("权重格式不对")
	}
	f.Weight = uint8(w)
	f.Action = Action(parts[3])
	sub, ok := strings.CutPrefix(parts[4], "sublayer=")
	if !ok {
		return f, errors.New("缺子层")
	}
	if f.Sublayer, err = ParseGUID(sub); err != nil {
		return f, err
	}
	prov, ok := strings.CutPrefix(parts[5], "provider=")
	if !ok {
		return f, errors.New("缺提供者")
	}
	if f.Provider, err = ParseGUID(prov); err != nil {
		return f, err
	}
	name, ok := strings.CutPrefix(parts[len(parts)-1], "name=")
	if !ok || name == "" {
		return f, errors.New("缺名字")
	}
	f.Name = name
	for _, kv := range parts[6 : len(parts)-1] {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return f, fmt.Errorf("条件 %q 格式不对", kv)
		}
		f.Conds = append(f.Conds, Cond{Field(k), v})
	}
	return f, Validate(f)
}

// Validate 检查一个过滤器描述是否是求值器认识的形式。
func Validate(f Filter) error {
	if f.Layer != LayerConnectV4 && f.Layer != LayerConnectV6 {
		return fmt.Errorf("不认识的层 %q", f.Layer)
	}
	if f.Action != Permit && f.Action != Block {
		return fmt.Errorf("不认识的动作 %q", f.Action)
	}
	if f.Weight > 15 {
		return errors.New("权重超出 0–15")
	}
	seen := map[Field]bool{}
	for _, c := range f.Conds {
		if !knownFields[c.Field] {
			return fmt.Errorf("不认识的字段 %q", c.Field)
		}
		if seen[c.Field] {
			return fmt.Errorf("字段 %q 出现了两次", c.Field)
		}
		seen[c.Field] = true
		if err := checkValue(f.Layer, c); err != nil {
			return err
		}
	}
	return nil
}

func checkValue(l Layer, c Cond) error {
	switch c.Field {
	case FieldLoopback:
		if c.Value != "1" {
			return errors.New("loopback 的值只能是 1")
		}
	case FieldInterface:
		v, err := strconv.ParseUint(c.Value, 10, 64)
		if err != nil || v == 0 || strconv.FormatUint(v, 10) != c.Value {
			return errors.New("接口 LUID 格式不对")
		}
	case FieldUser:
		if !ValidSID(c.Value) {
			return errors.New("SID 不合法")
		}
	case FieldProtocol:
		if c.Value != "tcp" && c.Value != "udp" {
			return errors.New("协议只认 tcp、udp")
		}
	case FieldLocalPort, FieldRemotePort:
		v, err := strconv.ParseUint(c.Value, 10, 16)
		if err != nil || strconv.FormatUint(v, 10) != c.Value {
			return errors.New("端口格式不对")
		}
	case FieldRemoteAddr:
		p, err := netip.ParsePrefix(c.Value)
		if err != nil || !p.Addr().Is4() || p.Masked() != p || p.String() != c.Value || l != LayerConnectV4 {
			return errors.New("远端地址只认 IPv4 层上的规范 IPv4 网段")
		}
	}
	return nil
}

// Conn 是一个出站连接的属性（离线求值的输入）。
type Conn struct {
	V6         bool       // 地址族
	Loopback   bool       // 走回环
	IfLUID     uint64     // 出口接口的 LUID
	SID        string     // 发起进程的账户
	Proto      string     // tcp、udp、icmp …
	LocalPort  uint16     //
	RemotePort uint16     //
	Remote     netip.Addr // 目的地址
}

// Verdict 是求值结果。
type Verdict struct {
	Permit bool
	// Matched 是决定结果的过滤器名字；没有任何过滤器命中时为空（按 WFP 默认放行）。
	Matched string
}

// Eval 按建模的 WFP 仲裁规则求值（见包注释）。描述不合法时返回错误。
func Eval(fs []Filter, c Conn) (Verdict, error) {
	layer := LayerConnectV4
	if c.V6 {
		layer = LayerConnectV6
	}
	if c.Remote.IsValid() && c.Remote.Unmap().Is4() == c.V6 {
		return Verdict{}, errors.New("目的地址与地址族不一致")
	}
	bySub := map[GUID][]Filter{}
	var subs []GUID
	for _, f := range fs {
		if err := Validate(f); err != nil {
			return Verdict{}, fmt.Errorf("%s：%w", f.Name, err)
		}
		if f.Layer != layer {
			continue
		}
		if _, ok := bySub[f.Sublayer]; !ok {
			subs = append(subs, f.Sublayer)
		}
		bySub[f.Sublayer] = append(bySub[f.Sublayer], f)
	}
	slices.SortFunc(subs, func(a, b GUID) int { return bytes.Compare(a[:], b[:]) })
	var permitBy string
	for _, s := range subs {
		list := bySub[s]
		slices.SortStableFunc(list, func(a, b Filter) int {
			if a.Weight != b.Weight {
				return int(b.Weight) - int(a.Weight)
			}
			return strings.Compare(a.Name, b.Name)
		})
		for _, f := range list {
			if !match(f, c) {
				continue
			}
			if f.Action == Block {
				return Verdict{Permit: false, Matched: f.Name}, nil
			}
			if permitBy == "" {
				permitBy = f.Name
			}
			break
		}
	}
	return Verdict{Permit: true, Matched: permitBy}, nil
}

func match(f Filter, c Conn) bool {
	for _, k := range f.Conds {
		switch k.Field {
		case FieldLoopback:
			if !c.Loopback {
				return false
			}
		case FieldInterface:
			if strconv.FormatUint(c.IfLUID, 10) != k.Value {
				return false
			}
		case FieldUser:
			if c.SID != k.Value {
				return false
			}
		case FieldProtocol:
			if c.Proto != k.Value {
				return false
			}
		case FieldLocalPort:
			if strconv.FormatUint(uint64(c.LocalPort), 10) != k.Value {
				return false
			}
		case FieldRemotePort:
			if strconv.FormatUint(uint64(c.RemotePort), 10) != k.Value {
				return false
			}
		case FieldRemoteAddr:
			p := netip.MustParsePrefix(k.Value)
			if !c.Remote.IsValid() || !p.Contains(c.Remote.Unmap()) {
				return false
			}
		default:
			return false
		}
	}
	return true
}
