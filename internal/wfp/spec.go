// Package wfp 是 Windows 版断网保护的薄系统层（design-windows 0.5 决定 2）：建立我们的提供者和子层、
// 在一个事务里添加一组过滤器（动态或持久）、枚举、按提供者整批删除。
//
// 本文件不带构建约束：把 wfprules 的过滤器描述换算成 WFP 系统调用的参数（字段 GUID、匹配方式、数据类型、值），
// 以及把枚举到的条件换算回描述、把别家的过滤器汇总成“名字和数量”。这些都是纯函数，在 Mac 上测试；
// 真正的系统调用在 wfp_windows.go，只照这里算好的参数做，不自己决定任何参数。
package wfp

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"claude-abroad/internal/wfprules"
)

func mustGUID(s string) wfprules.GUID {
	g, err := wfprules.ParseGUID(s)
	if err != nil {
		panic(err)
	}
	return g
}

// 层和条件字段的 GUID（fwpmu.h）。
var (
	LayerConnectV4 = mustGUID("C38D57D1-05A7-4C33-904F-7FBCEEE60E82") // FWPM_LAYER_ALE_AUTH_CONNECT_V4
	LayerConnectV6 = mustGUID("4A72393B-319F-44BC-84C3-BA54DCB3B6B4") // FWPM_LAYER_ALE_AUTH_CONNECT_V6

	CondFlags          = mustGUID("632CE23B-5167-435C-86D7-E903684AA80C") // FWPM_CONDITION_FLAGS
	CondLocalInterface = mustGUID("4CD62A49-59C3-4969-B7F3-BDA5D32890A4") // FWPM_CONDITION_IP_LOCAL_INTERFACE
	CondUserID         = mustGUID("AF043A0A-B34D-4F86-979C-C90371AF6E66") // FWPM_CONDITION_ALE_USER_ID
	CondProtocol       = mustGUID("3971EF2B-623E-4F9A-8CB1-6E79B806B9A7") // FWPM_CONDITION_IP_PROTOCOL
	CondLocalPort      = mustGUID("0C1BA1AF-5765-453F-AF22-A8F791AC775B") // FWPM_CONDITION_IP_LOCAL_PORT
	CondRemotePort     = mustGUID("C35A604D-D22B-4E1A-91B4-68F674EE674B") // FWPM_CONDITION_IP_REMOTE_PORT
	CondRemoteAddress  = mustGUID("B235AE9A-1D64-49B8-A44C-5FF3D9095045") // FWPM_CONDITION_IP_REMOTE_ADDRESS
)

// FWP_DATA_TYPE、FWP_MATCH_TYPE、动作类型（fwptypes.h）。
const (
	TypeEmpty      uint32 = 0
	TypeUint8      uint32 = 1
	TypeUint16     uint32 = 2
	TypeUint32     uint32 = 3
	TypeUint64     uint32 = 4
	TypeSecDesc    uint32 = 14 // FWP_SECURITY_DESCRIPTOR_TYPE
	TypeV4AddrMask uint32 = 0x100

	MatchEqual       uint32 = 0
	MatchFlagsAllSet uint32 = 6

	ActionBlock  uint32 = 0x1001 // FWP_ACTION_BLOCK（带 FWP_ACTION_FLAG_TERMINATING）
	ActionPermit uint32 = 0x1002 // FWP_ACTION_PERMIT

	FlagIsLoopback uint32 = 0x1 // FWP_CONDITION_FLAG_IS_LOOPBACK

	ProtoTCP = 6
	ProtoUDP = 17
)

// CondSpec 是一个 FWPM_FILTER_CONDITION0 的参数。值按数据类型只用其中一个字段。
type CondSpec struct {
	Field uint32 // 只在测试和错误信息里用：wfprules.Field 的下标
	Key   wfprules.GUID
	Match uint32
	Type  uint32
	U64   uint64 // TypeUint8/16/32/64
	Addr  uint32 // TypeV4AddrMask：主机字节序
	Mask  uint32
	SDDL  string // TypeSecDesc：系统层用它生成自相关的安全描述符
	SID   string // TypeSecDesc 对应的 SID（换算回描述用）
}

// FilterSpec 是一个 FWPM_FILTER0 的参数。
type FilterSpec struct {
	Key      wfprules.GUID
	Name     string // 显示名：“Claude Abroad: <名字>”
	Layer    wfprules.GUID
	Sublayer wfprules.GUID
	Provider wfprules.GUID
	Weight   uint8
	Action   uint32
	Conds    []CondSpec
}

// DisplayPrefix 是我们的过滤器显示名的前缀（换算回描述时去掉）。
const DisplayPrefix = "Claude Abroad: "

// SDDLForSID 是 ALE_USER_ID 条件要的安全描述符：只有一条允许 FWP_ACTRL_MATCH_FILTER（CC）的 ACE。
func SDDLForSID(sid string) string { return "D:(A;;CC;;;" + sid + ")" }

// SIDFromSDDL 是 SDDLForSID 的反函数；不是这个形式就报错。
func SIDFromSDDL(sddl string) (string, error) {
	s, ok := strings.CutPrefix(sddl, "D:(A;;CC;;;")
	if !ok {
		return "", errors.New("安全描述符不是我们生成的形式")
	}
	s, ok = strings.CutSuffix(s, ")")
	if !ok || !wfprules.ValidSID(s) {
		return "", errors.New("安全描述符不是我们生成的形式")
	}
	return s, nil
}

// Encode 把过滤器描述换算成系统调用的参数。描述不合法时报错。
func Encode(f wfprules.Filter) (FilterSpec, error) {
	if err := wfprules.Validate(f); err != nil {
		return FilterSpec{}, err
	}
	s := FilterSpec{Key: f.Key, Name: DisplayPrefix + f.Name, Sublayer: f.Sublayer, Provider: f.Provider, Weight: f.Weight}
	switch f.Layer {
	case wfprules.LayerConnectV4:
		s.Layer = LayerConnectV4
	case wfprules.LayerConnectV6:
		s.Layer = LayerConnectV6
	}
	if f.Action == wfprules.Block {
		s.Action = ActionBlock
	} else {
		s.Action = ActionPermit
	}
	for _, c := range f.Conds {
		cs, err := encodeCond(c)
		if err != nil {
			return FilterSpec{}, fmt.Errorf("%s：%w", f.Name, err)
		}
		s.Conds = append(s.Conds, cs)
	}
	return s, nil
}

func encodeCond(c wfprules.Cond) (CondSpec, error) {
	switch c.Field {
	case wfprules.FieldLoopback:
		return CondSpec{Key: CondFlags, Match: MatchFlagsAllSet, Type: TypeUint32, U64: uint64(FlagIsLoopback)}, nil
	case wfprules.FieldInterface:
		v, err := strconv.ParseUint(c.Value, 10, 64)
		return CondSpec{Key: CondLocalInterface, Match: MatchEqual, Type: TypeUint64, U64: v}, err
	case wfprules.FieldUser:
		return CondSpec{Key: CondUserID, Match: MatchEqual, Type: TypeSecDesc, SDDL: SDDLForSID(c.Value), SID: c.Value}, nil
	case wfprules.FieldProtocol:
		p := uint64(ProtoTCP)
		if c.Value == "udp" {
			p = ProtoUDP
		}
		return CondSpec{Key: CondProtocol, Match: MatchEqual, Type: TypeUint8, U64: p}, nil
	case wfprules.FieldLocalPort, wfprules.FieldRemotePort:
		v, err := strconv.ParseUint(c.Value, 10, 16)
		k := CondLocalPort
		if c.Field == wfprules.FieldRemotePort {
			k = CondRemotePort
		}
		return CondSpec{Key: k, Match: MatchEqual, Type: TypeUint16, U64: v}, err
	case wfprules.FieldRemoteAddr:
		p, err := netip.ParsePrefix(c.Value)
		if err != nil {
			return CondSpec{}, err
		}
		a := p.Addr().As4()
		var mask uint32
		if p.Bits() > 0 {
			mask = ^uint32(0) << (32 - p.Bits())
		}
		return CondSpec{Key: CondRemoteAddress, Match: MatchEqual, Type: TypeV4AddrMask,
			Addr: uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3]), Mask: mask}, nil
	}
	return CondSpec{}, fmt.Errorf("不认识的字段 %q", c.Field)
}

// Decode 把枚举到的过滤器参数换算回描述（只认 Encode 能产生的形式），用于和期望的摘要比对。
func Decode(s FilterSpec) (wfprules.Filter, error) {
	f := wfprules.Filter{Key: s.Key, Sublayer: s.Sublayer, Provider: s.Provider, Weight: s.Weight}
	name, ok := strings.CutPrefix(s.Name, DisplayPrefix)
	if !ok {
		return f, errors.New("显示名不是我们的前缀")
	}
	f.Name = name
	switch s.Layer {
	case LayerConnectV4:
		f.Layer = wfprules.LayerConnectV4
	case LayerConnectV6:
		f.Layer = wfprules.LayerConnectV6
	default:
		return f, fmt.Errorf("不认识的层 %s", s.Layer)
	}
	switch s.Action {
	case ActionBlock:
		f.Action = wfprules.Block
	case ActionPermit:
		f.Action = wfprules.Permit
	default:
		return f, fmt.Errorf("不认识的动作 %#x", s.Action)
	}
	for _, c := range s.Conds {
		dc, err := decodeCond(c)
		if err != nil {
			return f, fmt.Errorf("%s：%w", f.Name, err)
		}
		f.Conds = append(f.Conds, dc)
	}
	return f, wfprules.Validate(f)
}

func decodeCond(c CondSpec) (wfprules.Cond, error) {
	bad := fmt.Errorf("条件 %s（匹配 %d、类型 %#x）不是我们生成的形式", c.Key, c.Match, c.Type)
	switch {
	case c.Key == CondFlags && c.Match == MatchFlagsAllSet && c.Type == TypeUint32 && c.U64 == uint64(FlagIsLoopback):
		return wfprules.Cond{Field: wfprules.FieldLoopback, Value: "1"}, nil
	case c.Key == CondLocalInterface && c.Match == MatchEqual && c.Type == TypeUint64:
		return wfprules.Cond{Field: wfprules.FieldInterface, Value: strconv.FormatUint(c.U64, 10)}, nil
	case c.Key == CondUserID && c.Match == MatchEqual && c.Type == TypeSecDesc:
		sid, err := SIDFromSDDL(c.SDDL)
		if err != nil {
			return wfprules.Cond{}, err
		}
		return wfprules.Cond{Field: wfprules.FieldUser, Value: sid}, nil
	case c.Key == CondProtocol && c.Match == MatchEqual && c.Type == TypeUint8 && (c.U64 == ProtoTCP || c.U64 == ProtoUDP):
		v := "tcp"
		if c.U64 == ProtoUDP {
			v = "udp"
		}
		return wfprules.Cond{Field: wfprules.FieldProtocol, Value: v}, nil
	case (c.Key == CondLocalPort || c.Key == CondRemotePort) && c.Match == MatchEqual && c.Type == TypeUint16 && c.U64 <= 0xffff:
		fld := wfprules.FieldLocalPort
		if c.Key == CondRemotePort {
			fld = wfprules.FieldRemotePort
		}
		return wfprules.Cond{Field: fld, Value: strconv.FormatUint(c.U64, 10)}, nil
	case c.Key == CondRemoteAddress && c.Match == MatchEqual && c.Type == TypeV4AddrMask:
		bits := 0
		for m := c.Mask; m&0x80000000 != 0; m <<= 1 {
			bits++
		}
		if bits < 32 && c.Mask<<bits != 0 {
			return wfprules.Cond{}, errors.New("掩码不连续")
		}
		a := netip.AddrFrom4([4]byte{byte(c.Addr >> 24), byte(c.Addr >> 16), byte(c.Addr >> 8), byte(c.Addr)})
		return wfprules.Cond{Field: wfprules.FieldRemoteAddr, Value: netip.PrefixFrom(a, bits).String()}, nil
	}
	return wfprules.Cond{}, bad
}

// ObjInfo 是枚举到的一个提供者或子层：只有键和名字。
type ObjInfo struct {
	Key  wfprules.GUID
	Name string
}

// RawFilter 是枚举到的一个过滤器的概要（别家的只用这些字段，不读条件）。
type RawFilter struct {
	Provider    wfprules.GUID // 没有提供者时为零值
	Sublayer    wfprules.GUID
	Layer       wfprules.GUID
	Action      uint32
	HasProvider bool
}

// OtherLine 是别家（不是我们的提供者）在出站连接层的过滤器汇总：只有名字和数量。
type OtherLine struct {
	Provider string // 提供者名字；没有提供者时是“（无提供者）”，查不到名字时是 GUID
	Sublayer string // 子层名字；查不到名字时是 GUID
	Layer    string // connect-v4、connect-v6
	Permit   int
	Block    int
	Other    int // 回调等其他动作
}

// SummarizeOthers 汇总别家在出站连接层（ALE_AUTH_CONNECT_V4/V6）的过滤器。结果按（提供者、子层、层）排序。
// 我们自己的提供者不计入。只输出名字和数量，不输出任何条件。
func SummarizeOthers(filters []RawFilter, providers, sublayers []ObjInfo) []OtherLine {
	pname := map[wfprules.GUID]string{}
	for _, p := range providers {
		pname[p.Key] = clean(p.Name)
	}
	sname := map[wfprules.GUID]string{}
	for _, s := range sublayers {
		sname[s.Key] = clean(s.Name)
	}
	type key struct{ p, s, l string }
	agg := map[key]*OtherLine{}
	for _, f := range filters {
		var layer string
		switch f.Layer {
		case LayerConnectV4:
			layer = "connect-v4"
		case LayerConnectV6:
			layer = "connect-v6"
		default:
			continue
		}
		if f.HasProvider && f.Provider == wfprules.ProviderKey {
			continue
		}
		if f.Sublayer == wfprules.SublayerKey {
			continue
		}
		p := "（无提供者）"
		if f.HasProvider {
			if p = pname[f.Provider]; p == "" {
				p = f.Provider.String()
			}
		}
		s := sname[f.Sublayer]
		if s == "" {
			s = f.Sublayer.String()
		}
		k := key{p, s, layer}
		l := agg[k]
		if l == nil {
			l = &OtherLine{Provider: p, Sublayer: s, Layer: layer}
			agg[k] = l
		}
		switch f.Action {
		case ActionPermit:
			l.Permit++
		case ActionBlock:
			l.Block++
		default:
			l.Other++
		}
	}
	out := make([]OtherLine, 0, len(agg))
	for _, l := range agg {
		out = append(out, *l)
	}
	slices.SortFunc(out, func(a, b OtherLine) int {
		if c := strings.Compare(a.Provider, b.Provider); c != 0 {
			return c
		}
		if c := strings.Compare(a.Sublayer, b.Sublayer); c != 0 {
			return c
		}
		return strings.Compare(a.Layer, b.Layer)
	})
	return out
}

// clean 把名字里的控制字符换成空格并截断，免得别家的名字弄乱输出。
func clean(s string) string {
	r := []rune(s)
	if len(r) > 80 {
		r = r[:80]
	}
	for i, c := range r {
		if c < 0x20 || c == 0x7f {
			r[i] = ' '
		}
	}
	return strings.TrimSpace(string(r))
}

// FormatOthers 把汇总写成文本，一行一组。
func FormatOthers(lines []OtherLine) string {
	if len(lines) == 0 {
		return "（出站连接层没有别家的过滤器）\n"
	}
	var b strings.Builder
	for _, l := range lines {
		fmt.Fprintf(&b, "%s | %s | %s | permit %d, block %d, other %d\n", l.Provider, l.Sublayer, l.Layer, l.Permit, l.Block, l.Other)
	}
	return b.String()
}

// Ours 从枚举结果里挑出我们的过滤器（提供者是我们的，或者在我们的子层里）。
func Ours(fs []FilterSpec) []FilterSpec {
	var out []FilterSpec
	for _, f := range fs {
		if f.Provider == wfprules.ProviderKey || f.Sublayer == wfprules.SublayerKey {
			out = append(out, f)
		}
	}
	return out
}

// Snapshot 是一次枚举的结果。
type Snapshot struct {
	Ours      []FilterSpec // 我们的过滤器（提供者或子层是我们的），条件已读出
	Others    []RawFilter  // 全部过滤器的概要（别家的汇总用；含我们的，SummarizeOthers 会排除）
	Providers []ObjInfo
	Sublayers []ObjInfo
	// OurProvider、OurSublayer 报告我们的提供者、子层对象是否存在。
	OurProvider bool
	OurSublayer bool
}

// NothingOurs 报告枚举结果里是否已经没有我们的任何对象。
func NothingOurs(s Snapshot) bool { return len(s.Ours) == 0 && !s.OurProvider && !s.OurSublayer }

// CleanPlan 列出还原工具将要删除的对象（空跑时打印；真删前也先打印）：先过滤器，再子层，最后提供者。
func CleanPlan(s Snapshot) []string {
	var out []string
	fs := slices.Clone(s.Ours)
	slices.SortFunc(fs, func(a, b FilterSpec) int { return strings.Compare(a.Key.String(), b.Key.String()) })
	for _, f := range fs {
		out = append(out, fmt.Sprintf("过滤器 %s %s", f.Key, clean(f.Name)))
	}
	if s.OurSublayer {
		out = append(out, fmt.Sprintf("子层 %s %s", wfprules.SublayerKey, wfprules.SublayerName))
	}
	if s.OurProvider {
		out = append(out, fmt.Sprintf("提供者 %s %s", wfprules.ProviderKey, wfprules.ProviderName))
	}
	return out
}
