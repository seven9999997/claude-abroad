package sysnet

import (
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

func TestParseNetDest(t *testing.T) {
	cases := map[string]string{
		"1":                  "1.0.0.0/8",
		"10":                 "10.0.0.0/8",
		"169.254":            "169.254.0.0/16",
		"192.168.1":          "192.168.1.0/24",
		"2/7":                "2.0.0.0/7",
		"128.0/1":            "128.0.0.0/1",
		"192.168.77/24":        "192.168.77.0/24",
		"224.0.0/4":          "224.0.0.0/4",
		"127.0.0.1":          "127.0.0.1/32",
		"255.255.255.255/32": "255.255.255.255/32",
	}
	for in, want := range cases {
		p, def, err := ParseNetDest(in)
		if err != nil || def || p != netip.MustParsePrefix(want) {
			t.Errorf("%s：%v %v %v", in, p, def, err)
		}
	}
	if _, def, err := ParseNetDest("default"); err != nil || !def {
		t.Fatal("default")
	}
	for _, bad := range []string{"fe80::1", "1.2.3.4.5", "300", "a.b", "1/40", "", "link#12"} {
		if _, _, err := ParseNetDest(bad); err == nil {
			t.Errorf("%q 应解析失败", bad)
		}
	}
}

func rowsOf(t *testing.T, text string) []RouteRow {
	t.Helper()
	rows, err := ParseNetstat([]byte("Routing tables\n\nInternet:\nDestination        Gateway            Flags               Netif Expire\n" + text))
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// a. 实测样本：本机那组分段路由全部判为冲突（同一个接口只报一次）。
func TestConflictsRealSample(t *testing.T) {
	rows, err := ParseNetstat(sample(t, "netstat-rn-clash.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var clash []RouteRow
	for _, r := range rows {
		if r.Iface == "utun1024" {
			clash = append(clash, r)
			if !r.OK {
				t.Errorf("%s 没解析出来", r.Dest)
			}
			if got := ConflictIfaces([]RouteRow{r}, ""); !slices.Equal(got, []string{"utun1024"}) {
				t.Errorf("%s 应判为冲突", r.Dest)
			}
		}
	}
	if len(clash) != 9 {
		t.Fatalf("应认出 9 条经 utun1024 的路由（8 条分段 + 主机路由），实际 %d", len(clash))
	}
	if got := ConflictIfaces(rows, ""); !slices.Equal(got, []string{"utun1024"}) {
		t.Fatalf("%v", got)
	}
	// 我们自己就是 utun1024 时不算冲突（经 en0、bridge100、lo0 的从来不算）。
	if got := ConflictIfaces(rows, "utun1024"); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

// a. 构造样本。
func TestConflictsConstructed(t *testing.T) {
	cases := []struct {
		name, line string
		ours       string
		want       bool
	}{
		{"default 经 utun", "default            198.18.0.1         UGSc                 utun9", "", true},
		{"只有 8/5 经 utun", "8/5                198.18.0.1         UGSc                 utun9", "", true},
		{"10/8 经 utun 不算", "10                 10.8.0.1           UGSc                 utun9", "", false},
		{"172.16/12 经 utun 不算", "172.16/12          10.8.0.1           UGSc                 utun9", "", false},
		{"192.168.50 经 ppp 不算", "192.168.50         10.8.0.1           UGSc                  ppp0", "", false},
		{"198.18.0.0/16 经 utun", "198.18/16          198.18.0.1         UGSc                 utun9", "", true},
		{"198.19.1/24 经 utun（与 /15 重叠）", "198.19.1/24        198.18.0.1         UGSc                 utun9", "", true},
		{"目的不认识", "something!         198.18.0.1         UGSc                 utun9", "", true},
		{"公网主机路由经 wg", "203.0.114.5        10.8.0.1           UGHS                  wg0", "", true},
		{"经 en0 不算", "default            192.0.2.1          UGScg                  en0", "", false},
		{"公网经 en0 不算", "8/5                192.0.2.1          UGSc                   en0", "", false},
		{"经我们自己的 utun 不算", "8/5                198.18.0.1         UGSc                 utun7", "utun7", false},
		{"跨越私有和公网的网段（10/7）", "10/7               10.8.0.1           UGSc                 utun9", "", true},
	}
	for _, c := range cases {
		got := ConflictIfaces(rowsOf(t, c.line+"\n"), c.ours)
		if (len(got) > 0) != c.want {
			t.Errorf("%s：%v", c.name, got)
		}
	}
	if _, err := ParseNetstat([]byte("garbage\n")); !errors.Is(err, ErrUnknownOutput) {
		t.Fatal("不认识的输出应报错")
	}
	if _, err := ParseNetstat([]byte("Internet:\nDestination Gateway\nonly two\n")); !errors.Is(err, ErrUnknownOutput) {
		t.Fatal("列数不够应报错")
	}
}

// MacRoutes.Conflicts 走 netstat -rn -f inet。
func TestMacRoutesConflicts(t *testing.T) {
	r := &recRunner{resp: func(rcall) ([]byte, []byte, error) { return sample(t, "netstat-rn-clash.txt"), nil, nil }}
	got, err := (MacRoutes{R: r}).Conflicts(ctx, "utun7")
	if err != nil || !slices.Equal(got, []string{"utun1024"}) {
		t.Fatalf("%v %v", got, err)
	}
	want(t, r.calls, rcall{PathNetstat, []string{"-rn", "-f", "inet"}, ""})
}

// Fake 的 Conflicts 用同一个判断。
func TestFakeConflicts(t *testing.T) {
	st := initial()
	st.Default = "en0"
	f := NewFake(st)
	_ = f.Routes().Add(ctx, netip.MustParsePrefix("0.0.0.0/1"), "utun7")
	f.SetForeignRoute(netip.MustParsePrefix("10.0.0.0/8"), "utun9")
	if got, _ := f.Routes().Conflicts(ctx, "utun7"); len(got) != 0 {
		t.Fatalf("%v", got)
	}
	f.SetForeignRoute(netip.MustParsePrefix("8.0.0.0/5"), "utun9")
	if got, _ := f.Routes().Conflicts(ctx, "utun7"); !slices.Equal(got, []string{"utun9"}) {
		t.Fatalf("%v", got)
	}
	if !strings.Contains(strings.Join(f.Calls(), ","), OpRoutesConf) {
		t.Fatal("调用记录")
	}
}
