package wfp

import (
	"bytes"
	"net/netip"
	"os"
	"strings"
	"testing"

	"claude-abroad/internal/wfprules"
)

const (
	sidSvc   = "S-1-5-80-1111111111-2222222222-3333333333-4444444444-555555555"
	sidLocal = "S-1-5-21-100-200-300-1001"
)

func rendered(t *testing.T) []wfprules.Filter {
	t.Helper()
	fs, err := wfprules.Render(wfprules.Config{TunLUID: 1689399632855040, AllowSIDs: []string{sidSvc, sidLocal},
		LAN: wfprules.LANPrefixes([]netip.Prefix{netip.MustParsePrefix("192.168.1.0/24"), netip.MustParsePrefix("10.20.0.0/16")})})
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

// 每个生成的过滤器都能换算成参数、再换算回来，摘要逐字节相同。
func TestEncodeDecodeRoundTrip(t *testing.T) {
	fs := rendered(t)
	var back []wfprules.Filter
	for _, f := range fs {
		s, err := Encode(f)
		if err != nil {
			t.Fatal(err)
		}
		if s.Provider != wfprules.ProviderKey || s.Sublayer != wfprules.SublayerKey || !strings.HasPrefix(s.Name, DisplayPrefix) {
			t.Fatalf("%s 的归属或显示名不对", f.Name)
		}
		d, err := Decode(s)
		if err != nil {
			t.Fatalf("%s：%v", f.Name, err)
		}
		back = append(back, d)
	}
	if !bytes.Equal(wfprules.Summary(fs), wfprules.Summary(back)) {
		t.Fatal("往返后摘要不同")
	}
}

// 具体的参数值：与 WFP 头文件的定义逐项对照。
func TestEncodeValues(t *testing.T) {
	enc := func(c wfprules.Cond) CondSpec {
		t.Helper()
		s, err := encodeCond(c)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if c := enc(wfprules.Cond{Field: wfprules.FieldLoopback, Value: "1"}); c.Key != CondFlags || c.Match != MatchFlagsAllSet || c.Type != TypeUint32 || c.U64 != 1 {
		t.Errorf("loopback：%+v", c)
	}
	if c := enc(wfprules.Cond{Field: wfprules.FieldInterface, Value: "1689399632855040"}); c.Key != CondLocalInterface || c.Type != TypeUint64 || c.U64 != 1689399632855040 {
		t.Errorf("interface：%+v", c)
	}
	if c := enc(wfprules.Cond{Field: wfprules.FieldUser, Value: sidLocal}); c.Key != CondUserID || c.Type != TypeSecDesc || c.SDDL != "D:(A;;CC;;;"+sidLocal+")" {
		t.Errorf("user：%+v", c)
	}
	if c := enc(wfprules.Cond{Field: wfprules.FieldProtocol, Value: "udp"}); c.Key != CondProtocol || c.Type != TypeUint8 || c.U64 != 17 {
		t.Errorf("udp：%+v", c)
	}
	if c := enc(wfprules.Cond{Field: wfprules.FieldProtocol, Value: "tcp"}); c.U64 != 6 {
		t.Errorf("tcp：%+v", c)
	}
	if c := enc(wfprules.Cond{Field: wfprules.FieldRemotePort, Value: "67"}); c.Key != CondRemotePort || c.Type != TypeUint16 || c.U64 != 67 {
		t.Errorf("remoteport：%+v", c)
	}
	if c := enc(wfprules.Cond{Field: wfprules.FieldLocalPort, Value: "68"}); c.Key != CondLocalPort || c.U64 != 68 {
		t.Errorf("localport：%+v", c)
	}
	for v, want := range map[string][2]uint32{
		"192.168.1.0/24":     {0xC0A80100, 0xFFFFFF00},
		"255.255.255.255/32": {0xFFFFFFFF, 0xFFFFFFFF},
		"10.0.0.0/8":         {0x0A000000, 0xFF000000},
		"224.0.0.0/4":        {0xE0000000, 0xF0000000},
	} {
		c := enc(wfprules.Cond{Field: wfprules.FieldRemoteAddr, Value: v})
		if c.Key != CondRemoteAddress || c.Type != TypeV4AddrMask || c.Addr != want[0] || c.Mask != want[1] {
			t.Errorf("%s：%+v", v, c)
		}
	}
	// GUID 文本与头文件一致（大写，固定）。
	for g, want := range map[wfprules.GUID]string{
		LayerConnectV4: "C38D57D1-05A7-4C33-904F-7FBCEEE60E82", LayerConnectV6: "4A72393B-319F-44BC-84C3-BA54DCB3B6B4",
		CondRemoteAddress: "B235AE9A-1D64-49B8-A44C-5FF3D9095045", CondUserID: "AF043A0A-B34D-4F86-979C-C90371AF6E66",
	} {
		if g.String() != want {
			t.Errorf("%s ≠ %s", g, want)
		}
	}
	if ActionBlock != 0x1001 || ActionPermit != 0x1002 {
		t.Error("动作值不对")
	}
}

// 枚举到的条件不是我们生成的形式时报错（例如别人改过我们的过滤器）。
func TestDecodeRejects(t *testing.T) {
	good, err := Encode(rendered(t)[0])
	if err != nil {
		t.Fatal(err)
	}
	mut := map[string]func(*FilterSpec){
		"显示名没有前缀": func(s *FilterSpec) { s.Name = "x" },
		"未知层":     func(s *FilterSpec) { s.Layer = wfprules.ProviderKey },
		"未知动作":    func(s *FilterSpec) { s.Action = 0x5003 },
		"权重越界":    func(s *FilterSpec) { s.Weight = 0xff },
		"未知字段":    func(s *FilterSpec) { s.Conds = []CondSpec{{Key: wfprules.SublayerKey, Type: TypeUint8}} },
		"loopback 用了别的匹配": func(s *FilterSpec) {
			s.Conds = []CondSpec{{Key: CondFlags, Match: MatchEqual, Type: TypeUint32, U64: 1}}
		},
		"协议是 icmp": func(s *FilterSpec) { s.Conds = []CondSpec{{Key: CondProtocol, Type: TypeUint8, U64: 1}} },
		"掩码不连续": func(s *FilterSpec) {
			s.Conds = []CondSpec{{Key: CondRemoteAddress, Type: TypeV4AddrMask, Addr: 0x0A000000, Mask: 0xFF00FF00}}
		},
		"地址不规范": func(s *FilterSpec) {
			s.Conds = []CondSpec{{Key: CondRemoteAddress, Type: TypeV4AddrMask, Addr: 0x0A010203, Mask: 0xFF000000}}
		},
		"安全描述符被改": func(s *FilterSpec) {
			s.Conds = []CondSpec{{Key: CondUserID, Type: TypeSecDesc, SDDL: "O:SYD:(A;;CC;;;" + sidLocal + ")"}}
		},
		"安全描述符放行所有人": func(s *FilterSpec) {
			s.Conds = []CondSpec{{Key: CondUserID, Type: TypeSecDesc, SDDL: "D:(A;;CC;;;S-1-1-0)"}}
		},
	}
	for name, m := range mut {
		s := good
		s.Conds = append([]CondSpec(nil), good.Conds...)
		m(&s)
		if _, err := Decode(s); err == nil {
			t.Errorf("%s：应报错", name)
		}
	}
}

// 别家的汇总：只有名字和数量；排除我们自己的提供者和子层；只看出站连接层；名字里的控制字符被清掉。
func TestSummarizeOthers(t *testing.T) {
	pA, _ := wfprules.ParseGUID("11111111-1111-1111-1111-111111111111")
	sB, _ := wfprules.ParseGUID("22222222-2222-2222-2222-222222222222")
	other, _ := wfprules.ParseGUID("33333333-3333-3333-3333-333333333333")
	filters := []RawFilter{
		{Provider: pA, HasProvider: true, Sublayer: sB, Layer: LayerConnectV4, Action: ActionBlock},
		{Provider: pA, HasProvider: true, Sublayer: sB, Layer: LayerConnectV4, Action: ActionPermit},
		{Provider: pA, HasProvider: true, Sublayer: sB, Layer: LayerConnectV4, Action: 0x5003},
		{Provider: pA, HasProvider: true, Sublayer: sB, Layer: LayerConnectV6, Action: ActionBlock},
		{Sublayer: sB, Layer: LayerConnectV4, Action: ActionPermit},                                                                     // 没有提供者
		{Provider: pA, HasProvider: true, Sublayer: sB, Layer: other, Action: ActionBlock},                                              // 别的层，不计
		{Provider: wfprules.ProviderKey, HasProvider: true, Sublayer: wfprules.SublayerKey, Layer: LayerConnectV4, Action: ActionBlock}, // 我们的
		{Sublayer: wfprules.SublayerKey, Layer: LayerConnectV4, Action: ActionBlock},                                                    // 在我们子层里的
	}
	got := SummarizeOthers(filters, []ObjInfo{{Key: pA, Name: "Proxy\x01Tool"}}, []ObjInfo{{Key: sB, Name: "Its sublayer"}})
	text := FormatOthers(got)
	want := "Proxy Tool | Its sublayer | connect-v4 | permit 1, block 1, other 1\n" +
		"Proxy Tool | Its sublayer | connect-v6 | permit 0, block 1, other 0\n" +
		"（无提供者） | Its sublayer | connect-v4 | permit 1, block 0, other 0\n"
	if text != want {
		t.Fatalf("汇总不对：\n%s", text)
	}
	if FormatOthers(nil) == "" {
		t.Fatal("空汇总应有说明")
	}
	// 查不到名字时写 GUID。
	got = SummarizeOthers(filters[:1], nil, nil)
	if got[0].Provider != pA.String() || got[0].Sublayer != sB.String() {
		t.Fatalf("%+v", got[0])
	}
}

func TestOurs(t *testing.T) {
	a, _ := Encode(rendered(t)[0])
	b := a
	b.Provider = wfprules.SublayerKey // 提供者被改，但仍在我们的子层：也算我们的
	c := a
	c.Provider, c.Sublayer = wfprules.GUID{1}, wfprules.GUID{2}
	if got := Ours([]FilterSpec{a, b, c}); len(got) != 2 {
		t.Fatalf("%d", len(got))
	}
}

func TestCleanPlan(t *testing.T) {
	if p := CleanPlan(Snapshot{}); len(p) != 0 || !NothingOurs(Snapshot{}) {
		t.Fatalf("空状态：%v", p)
	}
	a, _ := Encode(rendered(t)[0])
	snap := Snapshot{Ours: []FilterSpec{a}, OurProvider: true, OurSublayer: true}
	p := CleanPlan(snap)
	if len(p) != 3 || !strings.HasPrefix(p[0], "过滤器 "+a.Key.String()) || !strings.HasPrefix(p[1], "子层 ") || !strings.HasPrefix(p[2], "提供者 ") || NothingOurs(snap) {
		t.Fatalf("%v", p)
	}
	// 只剩提供者（例如上次删到一半）。
	if p := CleanPlan(Snapshot{OurProvider: true}); len(p) != 1 {
		t.Fatalf("%v", p)
	}
}

// 源码检查（design-windows 0.5 决定 2）：系统层不停用、不改防火墙服务，不执行命令，不按 ID 删除对象（只按我们固定的键
// 和我们枚举到的过滤器键删），删除只发生在 DeleteOurs 里。
func TestSystemLayerSource(t *testing.T) {
	b, err := os.ReadFile("wfp_windows.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, bad := range []string{"OpenSCManager", "ControlService", "ChangeServiceConfig", "DeleteService", "mpssvc", "MpsSvc",
		"netsh", "os/exec", "DeleteById", "FwpmEngineSetOption", "FwpmFilterSetSecurityInfo", "FwpmProviderSetSecurityInfo"} {
		if strings.Contains(src, bad) {
			t.Errorf("系统层出现了 %q", bad)
		}
	}
	i := strings.Index(src, "func (e *Engine) DeleteOurs(")
	if i < 0 {
		t.Fatal("找不到 DeleteOurs")
	}
	j := strings.Index(src[i+1:], "\nfunc ")
	body := src[i : i+1+j]
	for _, p := range []string{"procFilterDeleteByKey0.Call", "procSubLayerDeleteByKey0.Call", "procProviderDeleteByKey0.Call"} {
		if strings.Count(src, p) != 1 || !strings.Contains(body, p) {
			t.Errorf("%s 应只在 DeleteOurs 里出现一次", p)
		}
	}
	if !strings.Contains(body, "range snap.Ours") || !strings.Contains(body, "toWin(wfprules.SublayerKey)") || !strings.Contains(body, "toWin(wfprules.ProviderKey)") {
		t.Error("DeleteOurs 只能删我们枚举到的过滤器和我们固定键的子层、提供者")
	}
}
