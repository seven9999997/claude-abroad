package fakedns

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/miekg/dns"
)

func corpMatcher(name string) bool { return strings.HasSuffix(name, ".corp.example") }

// f. local_domains：分配假地址，映射带本地标记；落盘读回标记还在；名单变化后标记随之更新。
func TestLocalMarker(t *testing.T) {
	p := DefaultPool()
	p.SetLocalMatcher(corpMatcher)
	a := mustAssign(t, p, "git.corp.example", t0)
	b := mustAssign(t, p, "www.example.org", t0)
	for addr, want := range map[netip.Addr]bool{a: true, b: false} {
		m, release, ok := p.AcquireMapping(addr, t0)
		if !ok || m.Local != want {
			t.Fatalf("%v：%+v ok=%v，期望本地=%v", addr, m, ok, want)
		}
		release()
	}
	// 落盘再读回（读回时没有设名单：用文件里的标记）。
	var buf bytes.Buffer
	if err := p.Save(&buf); err != nil {
		t.Fatal(err)
	}
	q := DefaultPool()
	if err := q.Load(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}
	if m, rel, ok := q.AcquireMapping(a, t0); !ok || !m.Local || m.Name != "git.corp.example" {
		t.Fatalf("读回后本地标记应还在：%+v", m)
	} else {
		rel()
	}
	// 读回时已有名单：以当前名单为准。
	r := DefaultPool()
	r.SetLocalMatcher(func(string) bool { return false })
	_ = r.Load(bytes.NewReader(buf.Bytes()))
	if m, rel, _ := r.AcquireMapping(a, t0); m.Local {
		t.Fatal("当前名单里没有它，读回后不应带本地标记")
	} else {
		rel()
	}
	// 名单变化：已有映射的标记随之更新。
	p.SetLocalMatcher(func(n string) bool { return n == "www.example.org" })
	ma, ra, _ := p.AcquireMapping(a, t0)
	mb, rb, _ := p.AcquireMapping(b, t0)
	ra()
	rb()
	if ma.Local || !mb.Local {
		t.Fatalf("名单变化后标记应更新：%+v %+v", ma, mb)
	}
	p.SetLocalMatcher(nil)
	if m, rel, _ := p.AcquireMapping(b, t0); m.Local {
		t.Fatal("清空名单后不应带本地标记")
	} else {
		rel()
	}
}

// 版本 1 的旧文件仍能读入，标记都为假；版本 2 的标志字节不认识时整份拒绝。
func TestLoadVersion1AndBadFlags(t *testing.T) {
	recs := []record{{netip.MustParseAddr("198.18.1.1"), t0, "git.corp.example", true}}
	v1 := build(1, 1, recs, fileMagic)
	p := DefaultPool()
	if err := p.Load(bytes.NewReader(v1)); err != nil {
		t.Fatalf("版本 1 应能读入：%v", err)
	}
	if m, rel, ok := p.AcquireMapping(netip.MustParseAddr("198.18.1.1"), t0); !ok || m.Local {
		t.Fatalf("版本 1 读入的标记应为假：%+v", m)
	} else {
		rel()
	}
	v2 := build(fileVersion, 1, recs, fileMagic)
	bad := append([]byte(nil), v2[:len(v2)-sha256.Size]...)
	bad[len(bad)-1] = 0x02 // 不认识的标志位
	sum := sha256.Sum256(bad)
	bad = append(bad, sum[:]...)
	if err := DefaultPool().Load(bytes.NewReader(bad)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("标志不认识应报 ErrCorrupt：%v", err)
	}
}

// 本地域名优先于“不使用假地址”的名单：*.local 在名单里，但设成本地域名后分配假地址、不交上游。
func TestLocalBeatsBypass(t *testing.T) {
	up := &fakeUp{realA: []string{"192.168.1.20"}, ttl: 60}
	s := newServer(up)
	s.Pool.SetLocalMatcher(func(n string) bool { return strings.HasSuffix(n, ".local") })
	m := s.Handle(context.Background(), query("printer.local", dns.TypeA))
	if len(m.Answer) != 1 || up.calls.Load() != 0 {
		t.Fatalf("本地域名应分配假地址、不交上游：%v，上游 %d 次", m.Answer, up.calls.Load())
	}
	a, _ := netip.AddrFromSlice(m.Answer[0].(*dns.A).A)
	if !s.Pool.Contains(a.Unmap()) {
		t.Fatalf("应是假地址：%v", a)
	}
	// 不是本地域名的名单内域名照旧交上游。
	s.Handle(context.Background(), query("time.apple.com", dns.TypeA))
	if up.calls.Load() != 1 {
		t.Fatal("名单内的非本地域名应交上游")
	}
}

// 私有网段的 PTR 在本地回 NXDOMAIN，上游调用数为 0；其他 PTR 行为不变。
func TestPrivatePTR(t *testing.T) {
	up := &fakeUp{}
	s := newServer(up)
	private := []string{
		"4.3.2.10.in-addr.arpa", "1.0.16.172.in-addr.arpa", "1.0.31.172.in-addr.arpa", "20.1.168.192.in-addr.arpa",
		"1.1.254.169.in-addr.arpa", "168.192.in-addr.arpa", "10.in-addr.arpa", "16.172.in-addr.arpa.",
		"4.3.2.10.IN-ADDR.ARPA.",
	}
	for _, n := range private {
		if m := s.Handle(context.Background(), query(n, dns.TypePTR)); m.Rcode != dns.RcodeNameError {
			t.Errorf("%s：应在本地回 NXDOMAIN，得到 %s", n, dns.RcodeToString[m.Rcode])
		}
	}
	if up.calls.Load() != 0 || s.Stats().PTRLocal != int64(len(private)) {
		t.Fatalf("私有网段的 PTR 不应交上游：上游 %d 次，ptr_local %d", up.calls.Load(), s.Stats().PTRLocal)
	}
	public := []string{"8.8.8.8.in-addr.arpa", "1.0.15.172.in-addr.arpa", "1.0.32.172.in-addr.arpa", "172.in-addr.arpa",
		"1.1.1.11.in-addr.arpa", "1.168.193.in-addr.arpa", "01.2.3.10.in-addr.arpa"}
	for _, n := range public {
		s.Handle(context.Background(), query(n, dns.TypePTR))
	}
	if up.calls.Load() != int32(len(public)) {
		t.Fatalf("其他 PTR 应照常交上游：%d/%d", up.calls.Load(), len(public))
	}
}
