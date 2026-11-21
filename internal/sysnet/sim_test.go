package sysnet

import (
	"errors"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

// 错误类别：nil、ErrExists、ErrNotFound、其他。
func errClass(err error) string {
	switch {
	case err == nil:
		return "nil"
	case errors.Is(err, ErrExists):
		return "exists"
	case errors.Is(err, ErrNotFound):
		return "notfound"
	}
	return "other"
}

type views struct {
	r Routes
	d DNS
	v IPv6
}

// M2-9b-1 a：同一串操作对“Fake 的视图”和“真实实现 + SimRunner”执行，错误类别和快照逐步一致。
// 有意的不同不在序列里：真实的 Delete 不删不经我们 utun 的路由（Fake 的视图会删）。
func TestSimConsistency(t *testing.T) {
	mk := func() *Fake {
		st := initial()
		st.Default = "en0"
		st.IPv6["Odd"] = IPv6Setting{Mode: IPv6Unknown}
		return NewFake(st)
	}
	fa, fb := mk(), mk()
	sim := NewSimRunner(fb)
	ours := "utun7"
	a := views{fa.Routes(), fa.DNS(), fa.IPv6()}
	b := views{MacRoutes{R: sim, Iface: func() string { return ours }}, MacDNS{R: sim, Server: netip.MustParseAddr("198.18.0.2")}, MacIPv6{R: sim}}
	n01, n128 := netip.MustParsePrefix("0.0.0.0/1"), netip.MustParsePrefix("128.0.0.0/1")
	d2, d3 := netip.MustParseAddr("198.18.0.2"), netip.MustParseAddr("198.18.0.3")
	manual := IPv6Setting{Mode: IPv6Manual, Address: netip.MustParseAddr("2001:db8::5"), PrefixLen: 64, Router: netip.MustParseAddr("2001:db8::1")}
	type op struct {
		name string
		do   func(v views) (string, error)
	}
	via := func(addr string) func(v views) (string, error) {
		return func(v views) (string, error) { return v.r.Via(ctx, netip.MustParseAddr(addr)) }
	}
	ops := []op{
		{"加路由", func(v views) (string, error) { return "", v.r.Add(ctx, n01, ours) }},
		{"重复加", func(v views) (string, error) { return "", v.r.Add(ctx, n01, ours) }},
		{"加第二条", func(v views) (string, error) { return "", v.r.Add(ctx, n128, ours) }},
		{"Via", via("1.1.1.1")},
		{"Via 默认", via("10.1.1.1")},
		{"改接口", func(v views) (string, error) { return "", v.r.Add(ctx, n01, "utun9") }},
		{"Conflicts（改到 utun9 后）", func(v views) (string, error) { c, err := v.r.Conflicts(ctx, ours); return strings.Join(c, ","), err }},
		{"改回", func(v views) (string, error) { return "", v.r.Add(ctx, n01, ours) }},
		{"Conflicts", func(v views) (string, error) { c, err := v.r.Conflicts(ctx, ours); return strings.Join(c, ","), err }},
		{"删路由", func(v views) (string, error) { return "", v.r.Delete(ctx, n01) }},
		{"删不存在的", func(v views) (string, error) { return "", v.r.Delete(ctx, n01) }},
		{"设 DNS", func(v views) (string, error) { return "", v.d.Set(ctx, d2) }},
		{"重复设", func(v views) (string, error) { return "", v.d.Set(ctx, d2) }},
		{"DNS 在", func(v views) (string, error) {
			ok, err := v.d.Present(ctx)
			return map[bool]string{true: "y", false: "n"}[ok], err
		}},
		{"改写", func(v views) (string, error) { return "", v.d.Set(ctx, d3) }},
		{"删 DNS", func(v views) (string, error) { return "", v.d.Remove(ctx) }},
		{"删不存在的 DNS", func(v views) (string, error) { return "", v.d.Remove(ctx) }},
		{"IPv6 快照", func(v views) (string, error) {
			s, err := v.v.Snapshot(ctx)
			var parts []string
			for _, k := range slices.Sorted(maps.Keys(s)) {
				parts = append(parts, k+"="+string(s[k].Mode))
			}
			return strings.Join(parts, ";"), err
		}},
		{"关 Wi-Fi", func(v views) (string, error) { return "", v.v.Off(ctx, "Wi-Fi") }},
		{"还原 Automatic", func(v views) (string, error) { return "", v.v.Restore(ctx, "Wi-Fi", IPv6Setting{Mode: IPv6Automatic}) }},
		{"还原 LinkLocal", func(v views) (string, error) { return "", v.v.Restore(ctx, "Wi-Fi", IPv6Setting{Mode: IPv6LinkLocal}) }},
		{"还原 Off", func(v views) (string, error) { return "", v.v.Restore(ctx, "Wi-Fi", IPv6Setting{Mode: IPv6Off}) }},
		{"还原 Manual", func(v views) (string, error) { return "", v.v.Restore(ctx, "Wi-Fi", manual) }},
		{"快照里读回 Manual", func(v views) (string, error) {
			s, err := v.v.Snapshot(ctx)
			return string(s["Wi-Fi"].Mode) + " " + s["Wi-Fi"].Address.String(), err
		}},
		{"关不存在的服务", func(v views) (string, error) { return "", v.v.Off(ctx, "Nope") }},
		{"还原不存在的服务", func(v views) (string, error) { return "", v.v.Restore(ctx, "Nope", IPv6Setting{Mode: IPv6Automatic}) }},
	}
	for _, o := range ops {
		ra, ea := o.do(a)
		rb, eb := o.do(b)
		if errClass(ea) != errClass(eb) || ra != rb {
			t.Fatalf("%s：Fake 视图 (%q, %v)，真实实现 + 模拟器 (%q, %v)", o.name, ra, ea, rb, eb)
		}
		if !fa.Snapshot().Equal(fb.Snapshot()) {
			t.Fatalf("%s 之后快照不同：\n%+v\n%+v", o.name, fa.Snapshot(), fb.Snapshot())
		}
	}
	if u := sim.Unknown(); len(u) != 0 {
		t.Fatalf("不应有不认识的命令：%v", u)
	}
}

// 不认识的命令返回错误并记下来。
func TestSimUnknown(t *testing.T) {
	sim := NewSimRunner(NewFake(initial()))
	cases := []struct {
		path  string
		args  []string
		stdin string
	}{
		{PathRoute, []string{"-n", "flush"}, ""},
		{PathRoute, []string{"-n", "add", "-net", "0.0.0.0/1", "-gateway", "x"}, ""},
		{PathNetworksetup, []string{"-setairportpower", "en0", "off"}, ""},
		{PathScutil, nil, "set State:/Network/Global/DNS\n"},
		{PathIfconfig, []string{"en0", "down"}, ""},
		{"/sbin/pfctl", []string{"-d"}, ""},
	}
	for _, c := range cases {
		var stdin []byte
		if c.stdin != "" {
			stdin = []byte(c.stdin)
		}
		if _, _, err := sim.Run(ctx, c.path, c.args, stdin); !errors.Is(err, ErrSimUnknown) {
			t.Errorf("%s %v：%v", c.path, c.args, err)
		}
	}
	if len(sim.Unknown()) != len(cases) {
		t.Fatalf("%v", sim.Unknown())
	}
}
