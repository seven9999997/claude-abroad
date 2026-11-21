package exitcheck

import "testing"

// M4-8：分流出口的判定只看 https、ip、mainland、doh；rule（没有基准时是无法判断）、geo、udp 不计入。
func TestSplitVerdict(t *testing.T) {
	mk := func(items ...Item) Report { return Report{Items: items} }
	pass := func(n string) Item { return Item{Name: n, Result: Pass} }
	unk := func(n string) Item { return Item{Name: n, Result: Unknown} }
	fail := func(n string) Item { return Item{Name: n, Result: Fail} }
	base := []Item{pass("https"), pass("ip"), pass("mainland"), pass("doh")}
	r := mk(append(base, unk("rule"), unk("geo"), fail("udp"))...)
	if r.SplitVerdict() != Pass || r.UDPResult() != Fail {
		t.Fatal("没有基准、位置查不到、UDP 不通时分流出口仍可以通过")
	}
	if r.Verdict == Pass {
		t.Fatal("这里没有算主代理的判定")
	}
	if mk(pass("https"), fail("mainland"), pass("ip"), pass("doh")).SplitVerdict() != Fail {
		t.Fatal("大陆段不通过")
	}
	if mk(pass("https"), unk("ip"), pass("mainland"), pass("doh")).SplitVerdict() != Unknown {
		t.Fatal("取不到 IP 是无法判断")
	}
	if mk(unk("rule"), pass("udp")).SplitVerdict() != Unknown {
		t.Fatal("四项都没有时不能当作通过")
	}
	if mk(base...).UDPResult() != Unknown {
		t.Fatal("没有 UDP 一项时是无法判断")
	}
}
