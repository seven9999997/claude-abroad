package pf

import (
	"bytes"
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "重写 testdata/*.golden")

func pfx(ss ...string) []netip.Prefix {
	var out []netip.Prefix
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

var fullCfg = Config{
	UtunName: "utun5",
	LAN:      pfx("192.168.1.0/24", "224.0.0.0/4", "169.254.0.0/16"),
	VMIfs:    []string{"vmenet0", "bridge100"},
	User:     "_abroad",
}

// a. golden：完整配置、开机（无 utun）、允许局域网关闭、无虚拟机网桥；输入顺序打乱后输出相同
func TestRenderGolden(t *testing.T) {
	cases := map[string]Config{
		"full":   fullCfg,
		"boot":   {LAN: fullCfg.LAN, VMIfs: fullCfg.VMIfs, User: "_abroad"},
		"nolan":  {UtunName: "utun5", VMIfs: fullCfg.VMIfs, User: "_abroad"},
		"novmif": {UtunName: "utun5", LAN: fullCfg.LAN, User: "_abroad"},
		// M6-11a：回国拦截那一组
		"returnblock": ReturnBlockConfig(),
	}
	for name, c := range cases {
		got, err := Render(c)
		if err != nil {
			t.Fatalf("%s：%v", name, err)
		}
		path := filepath.Join("testdata", name+".golden")
		if *update {
			os.MkdirAll("testdata", 0o755)
			os.WriteFile(path, got, 0o644)
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s：读 golden（用 -update 生成）：%v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s 与 golden 不同：\n%s", name, got)
		}
	}
	shuffled := Config{UtunName: "utun5", User: "_abroad",
		LAN:   pfx("169.254.0.0/16", "192.168.1.0/24", "224.0.0.0/4", "192.168.1.7/24"),
		VMIfs: []string{"bridge100", "vmenet0", "bridge100"}}
	a, _ := Render(fullCfg)
	b, _ := Render(shuffled)
	if !bytes.Equal(a, b) || Digest(a) != Digest(b) {
		t.Fatalf("顺序打乱、重复、未对齐的输入应得到相同输出：\n%s\n---\n%s", a, b)
	}
	if !strings.HasSuffix(string(a), "block drop out quick all\n") {
		t.Fatal("最后一条应是 block drop out quick all")
	}
	for _, kw := range []string{"$", "{ tcp", "{ udp"} {
		if strings.Contains(string(a), kw) {
			t.Fatalf("不应有宏或花括号列表：%q", kw)
		}
	}
}

// b. 拒绝用例
func TestRenderRejects(t *testing.T) {
	bad := []Config{
		{UtunName: "utun 5", User: "_abroad"},
		{UtunName: `utun"5`, User: "_abroad"},
		{UtunName: "utun5\npass out all", User: "_abroad"},
		{UtunName: "utun{5}", User: "_abroad"},
		{UtunName: "utun#5", User: "_abroad"},
		{UtunName: "Utun5", User: "_abroad"},
		{UtunName: "utun1234567890123", User: "_abroad"},
		{VMIfs: []string{"bridge 100"}, User: "_abroad"},
		{VMIfs: []string{"bridge100#"}, User: "_abroad"},
		{User: ""},
		{User: "_abroad all"},
		{User: "a;b"},
		{User: "root\n"},
		{LAN: pfx("160.79.104.0/23"), User: "_abroad"},
		{LAN: pfx("192.0.0.0/2"), User: "_abroad"},
		{LAN: pfx("0.0.0.0/0"), User: "_abroad"},
		{LAN: pfx("198.18.0.0/15"), User: "_abroad"},
		{LAN: pfx("fd00::/8"), User: "_abroad"},
		{LAN: pfx("192.168.0.0/15"), User: "_abroad"}, // 比 192.168/16 大
		// pf 的保留字和不合形式的名字
		{UtunName: "any", User: "_abroad"},
		{UtunName: "all", User: "_abroad"},
		{UtunName: "self", User: "_abroad"},
		{UtunName: "inet6", User: "_abroad"},
		{UtunName: "en0", User: "_abroad"},
		{UtunName: "utun", User: "_abroad"},
		{UtunName: "utun12345", User: "_abroad"},
		{VMIfs: []string{"any"}, User: "_abroad"},
		{VMIfs: []string{"en0"}, User: "_abroad"},
		{VMIfs: []string{"bridge"}, User: "_abroad"},
		{User: "unknown"},
		{User: "any"},
		{User: "0"},
		{User: "root"},
		{User: "_"},
		{User: "_Abroad"},
	}
	for i, c := range bad {
		out, err := Render(c)
		if err == nil || len(out) != 0 {
			t.Errorf("第 %d 个应被拒且输出为空：%+v", i, c)
		}
	}
}

// c. LANPrefixes
func TestLANPrefixes(t *testing.T) {
	got := LANPrefixes(pfx("160.79.104.0/23", "192.168.1.5/24", "10.0.0.0/8", "fd00::/8", "198.18.0.0/15"))
	has := func(s string) bool {
		for _, p := range got {
			if p == netip.MustParsePrefix(s) {
				return true
			}
		}
		return false
	}
	if has("160.79.104.0/23") || has("198.18.0.0/15") || !has("192.168.1.0/24") || !has("10.0.0.0/8") || !has("224.0.0.0/4") || !has("169.254.0.0/16") {
		t.Fatalf("LANPrefixes 不对：%v", got)
	}
	if _, err := Render(Config{LAN: got, User: "_abroad"}); err != nil {
		t.Fatalf("LANPrefixes 的结果应能直接 Render：%v", err)
	}
}

// M6-11a：第三组“回国拦截”——文本逐字固定（每段一行，立即拒绝，没有别的规则）；和别的字段同时给出时报错；
// 求值：到这两段的出站命中拦截，到别处的不命中（由系统的主规则集决定）。
func TestRenderReturnBlock(t *testing.T) {
	got, err := Render(Config{ReturnBlock: AnthropicNets})
	if err != nil {
		t.Fatal(err)
	}
	want := "block return out quick inet to 160.79.104.0/21\nblock return out quick inet6 to 2607:6bc0::/32\n"
	if string(got) != want {
		t.Fatalf("规则文本：\n%s", got)
	}
	for _, bad := range []Config{
		{ReturnBlock: AnthropicNets, User: "_abroad"},
		{ReturnBlock: AnthropicNets, Lockdown: true},
		{ReturnBlock: AnthropicNets, UtunName: "utun5"},
		{ReturnBlock: AnthropicNets, LAN: []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")}},
		{ReturnBlock: AnthropicNets, VMIfs: []string{"bridge100"}},
		{ReturnBlock: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}},
	} {
		if _, err := Render(bad); err == nil {
			t.Errorf("应报错：%+v", bad)
		}
	}
	for _, c := range []struct {
		dst     string
		matched bool
	}{
		{"160.79.104.10", true}, {"160.79.111.255", true}, {"2607:6bc0::1", true}, {"2607:6bc0:ffff::5", true},
		{"160.79.112.1", false}, {"1.1.1.1", false}, {"2606:4700::1111", false}, {"192.168.1.1", false},
	} {
		matched, pass, err := EvalMatch(got, Packet{If: "en0", Proto: "tcp", Dst: netip.MustParseAddr(c.dst), DstPort: 443})
		if err != nil || matched != c.matched || (matched && pass) {
			t.Errorf("%s：命中 %v 放行 %v（应命中 %v、命中时拦截）%v", c.dst, matched, pass, c.matched, err)
		}
	}
}
