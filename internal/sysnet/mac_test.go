package sysnet

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"claude-abroad/internal/pf"
)

type rcall struct {
	path  string
	args  []string
	stdin string
}

// recRunner 记录调用并按 resp 应答（resp 为 nil 时返回空输出、成功）。
type recRunner struct {
	calls []rcall
	resp  func(c rcall) ([]byte, []byte, error)
}

func (r *recRunner) Run(_ context.Context, path string, args []string, stdin []byte) ([]byte, []byte, error) {
	c := rcall{path, slices.Clone(args), string(stdin)}
	r.calls = append(r.calls, c)
	if r.resp == nil {
		return nil, nil, nil
	}
	return r.resp(c)
}

func sample(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func want(t *testing.T, got []rcall, exp ...rcall) {
	t.Helper()
	if len(got) != len(exp) {
		t.Fatalf("调用 %d 次，期望 %d 次：%+v", len(got), len(exp), got)
	}
	for i := range exp {
		if got[i].path != exp[i].path || !slices.Equal(got[i].args, exp[i].args) || got[i].stdin != exp[i].stdin {
			t.Fatalf("第 %d 次调用：\n%+v\n期望\n%+v", i+1, got[i], exp[i])
		}
	}
}

var (
	net01   = netip.MustParsePrefix("0.0.0.0/1")
	errExit = errors.New("exit status 1")
)

// b. Routes.Add 的三种走向、Delete 不在表里、Via。
func TestMacRoutesArgs(t *testing.T) {
	t.Run("成功", func(t *testing.T) {
		r := &recRunner{}
		if err := (MacRoutes{R: r}).Add(ctx, net01, "utun7"); err != nil {
			t.Fatal(err)
		}
		want(t, r.calls, rcall{PathRoute, []string{"-n", "add", "-net", "0.0.0.0/1", "-interface", "utun7"}, ""})
	})
	exists := func(via string) func(c rcall) ([]byte, []byte, error) {
		return func(c rcall) ([]byte, []byte, error) {
			switch c.args[1] {
			case "add":
				return nil, []byte("route: writing to routing socket: File exists\nadd net 0.0.0.0: gateway utun7: File exists\n"), errExit
			case "get":
				return []byte(strings.Replace(string(sample(t, "route-get-utun.txt")), "utun1024", via, 1)), nil, nil
			}
			return nil, nil, nil
		}
	}
	t.Run("已存在同接口", func(t *testing.T) {
		r := &recRunner{resp: exists("utun7")}
		if err := (MacRoutes{R: r}).Add(ctx, net01, "utun7"); !errors.Is(err, ErrExists) {
			t.Fatalf("%v", err)
		}
		want(t, r.calls,
			rcall{PathRoute, []string{"-n", "add", "-net", "0.0.0.0/1", "-interface", "utun7"}, ""},
			rcall{PathRoute, []string{"-n", "get", "0.0.0.1"}, ""})
	})
	t.Run("已存在不同接口改为 change", func(t *testing.T) {
		r := &recRunner{resp: exists("utun1024")}
		if err := (MacRoutes{R: r}).Add(ctx, net01, "utun7"); err != nil {
			t.Fatal(err)
		}
		want(t, r.calls,
			rcall{PathRoute, []string{"-n", "add", "-net", "0.0.0.0/1", "-interface", "utun7"}, ""},
			rcall{PathRoute, []string{"-n", "get", "0.0.0.1"}, ""},
			rcall{PathRoute, []string{"-n", "change", "-net", "0.0.0.0/1", "-interface", "utun7"}, ""})
	})
	t.Run("change 失败返回错误", func(t *testing.T) {
		r := &recRunner{resp: func(c rcall) ([]byte, []byte, error) {
			if c.args[1] == "change" {
				return nil, []byte("route: writing to routing socket: not in table\n"), errExit
			}
			return exists("utun1024")(c)
		}}
		if err := (MacRoutes{R: r}).Add(ctx, net01, "utun7"); err == nil || errors.Is(err, ErrExists) {
			t.Fatalf("%v", err)
		}
	})
	ours := func() string { return "utun7" }
	get := rcall{PathRoute, []string{"-n", "get", "0.0.0.1"}, ""}
	t.Run("Delete 经我们的 utun 才删", func(t *testing.T) {
		r := &recRunner{resp: exists("utun7")}
		if err := (MacRoutes{R: r, Iface: ours}).Delete(ctx, net01); err != nil {
			t.Fatal(err)
		}
		want(t, r.calls, get, rcall{PathRoute, []string{"-n", "delete", "-net", "0.0.0.0/1"}, ""})
	})
	t.Run("Delete 经别的接口不删", func(t *testing.T) {
		r := &recRunner{resp: exists("utun1024")}
		if err := (MacRoutes{R: r, Iface: ours}).Delete(ctx, net01); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%v", err)
		}
		want(t, r.calls, get)
	})
	t.Run("Delete 不知道我们的 utun 不删", func(t *testing.T) {
		for _, m := range []MacRoutes{{R: &recRunner{}}, {R: &recRunner{}, Iface: func() string { return "" }}} {
			if err := m.Delete(ctx, net01); !errors.Is(err, ErrNotFound) {
				t.Fatalf("%v", err)
			}
			if c := m.R.(*recRunner).calls; len(c) != 0 {
				t.Fatalf("不应有调用：%+v", c)
			}
		}
	})
	t.Run("Delete 不在表里", func(t *testing.T) {
		r := &recRunner{resp: func(rcall) ([]byte, []byte, error) {
			return nil, sample(t, "route-get-notintable.constructed.stderr.txt"), errExit
		}}
		if err := (MacRoutes{R: r, Iface: ours}).Delete(ctx, net01); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%v", err)
		}
		want(t, r.calls, get)
	})
	t.Run("Present", func(t *testing.T) {
		r := &recRunner{resp: exists("utun7")}
		ok, err := (MacRoutes{R: r, Iface: func() string { return "utun7" }}).Present(ctx)
		if err != nil || !ok {
			t.Fatalf("%v %v", ok, err)
		}
		ok, _ = (MacRoutes{R: r, Iface: func() string { return "utun8" }}).Present(ctx)
		if ok {
			t.Fatal("接口不同应为 false")
		}
	})
}

// b. 非法输入在调用 Runner 之前报错。
func TestMacBadInputs(t *testing.T) {
	r := &recRunner{}
	rt, v6 := MacRoutes{R: r}, MacIPv6{R: r}
	cases := map[string]error{
		"接口名不是 utun":  rt.Add(ctx, net01, "en0"),
		"接口名带空格":      rt.Add(ctx, net01, "utun7 x"),
		"网段无效":        rt.Add(ctx, netip.Prefix{}, "utun7"),
		"网段没对齐":       rt.Add(ctx, netip.MustParsePrefix("1.2.3.4/8"), "utun7"),
		"IPv6 网段":     rt.Delete(ctx, netip.MustParsePrefix("2001:db8::/32")),
		"Via 无效地址":    func() error { _, err := rt.Via(ctx, netip.Addr{}); return err }(),
		"服务名带换行":      v6.Off(ctx, "Wi-Fi\n-setv6off"),
		"服务名以 - 开头":   v6.Off(ctx, "-setv6off"),
		"服务名为空":       v6.Restore(ctx, "", IPv6Setting{Mode: IPv6Automatic}),
		"Manual 无路由器": v6.Restore(ctx, "Wi-Fi", IPv6Setting{Mode: IPv6Manual, Address: netip.MustParseAddr("2001:db8::5"), PrefixLen: 64}),
		"DNS 无效地址":    (MacDNS{R: r}).Set(ctx, netip.Addr{}),
	}
	for name, err := range cases {
		if err == nil {
			t.Errorf("%s：应报错", name)
		}
	}
	if _, _, err := UtunConfigArgs("en0", netip.MustParseAddr("198.18.0.1"), netip.MustParseAddr("198.18.0.1"), 1500); !errors.Is(err, ErrBadArg) {
		t.Errorf("utun 配置接口名：%v", err)
	}
	if len(r.calls) != 0 {
		t.Fatalf("非法输入不应调用 Runner：%+v", r.calls)
	}
}

// b. DNS 的 Set、Present、Remove。
func TestMacDNSArgs(t *testing.T) {
	srv := netip.MustParseAddr("198.18.0.2")
	show := rcall{PathScutil, nil, "show " + DNSKey + "\nquit\n"}
	resp := func(showOut string) func(c rcall) ([]byte, []byte, error) {
		return func(c rcall) ([]byte, []byte, error) {
			if strings.HasPrefix(c.stdin, "show") {
				return sample(t, showOut), nil, nil
			}
			return nil, nil, nil
		}
	}
	t.Run("Set 不存在时写入", func(t *testing.T) {
		r := &recRunner{resp: resp("scutil-show-missing.txt")}
		if err := (MacDNS{R: r, Server: srv}).Set(ctx, srv); err != nil {
			t.Fatal(err)
		}
		want(t, r.calls, show, rcall{PathScutil, nil, "d.init\nd.add ServerAddresses * 198.18.0.2\nd.add SupplementalMatchDomains * \"\"\nset " + DNSKey + "\nquit\n"})
	})
	t.Run("Set 相同返回 ErrExists", func(t *testing.T) {
		r := &recRunner{resp: resp("scutil-show-present.constructed.txt")}
		if err := (MacDNS{R: r, Server: srv}).Set(ctx, srv); !errors.Is(err, ErrExists) {
			t.Fatalf("%v", err)
		}
		want(t, r.calls, show)
	})
	t.Run("Set 不同就改写", func(t *testing.T) {
		r := &recRunner{resp: resp("scutil-show-other.constructed.txt")}
		if err := (MacDNS{R: r, Server: srv}).Set(ctx, srv); err != nil {
			t.Fatal(err)
		}
		if len(r.calls) != 2 || !strings.HasPrefix(r.calls[1].stdin, "d.init") {
			t.Fatalf("%+v", r.calls)
		}
	})
	t.Run("Present", func(t *testing.T) {
		for f, exp := range map[string]bool{"scutil-show-present.constructed.txt": true, "scutil-show-other.constructed.txt": false, "scutil-show-missing.txt": false} {
			ok, err := (MacDNS{R: &recRunner{resp: resp(f)}, Server: srv}).Present(ctx)
			if err != nil || ok != exp {
				t.Fatalf("%s：%v %v", f, ok, err)
			}
		}
	})
	t.Run("Remove", func(t *testing.T) {
		r := &recRunner{resp: resp("scutil-show-present.constructed.txt")}
		if err := (MacDNS{R: r, Server: srv}).Remove(ctx); err != nil {
			t.Fatal(err)
		}
		want(t, r.calls, show, rcall{PathScutil, nil, "remove " + DNSKey + "\nquit\n"})
		r = &recRunner{resp: resp("scutil-show-missing.txt")}
		if err := (MacDNS{R: r}).Remove(ctx); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%v", err)
		}
		want(t, r.calls, show)
	})
}

// b. IPv6 四种模式的 Restore 和 Off；utun 配置。
func TestMacIPv6Args(t *testing.T) {
	r := &recRunner{}
	v6 := MacIPv6{R: r}
	svc := "USB 10/100/1000 LAN"
	if err := v6.Off(ctx, svc); err != nil {
		t.Fatal(err)
	}
	_ = v6.Restore(ctx, svc, IPv6Setting{Mode: IPv6Automatic})
	_ = v6.Restore(ctx, svc, IPv6Setting{Mode: IPv6LinkLocal})
	_ = v6.Restore(ctx, svc, IPv6Setting{Mode: IPv6Off})
	_ = v6.Restore(ctx, svc, IPv6Setting{Mode: IPv6Manual, Address: netip.MustParseAddr("2001:db8::5"), PrefixLen: 64, Router: netip.MustParseAddr("2001:db8::1")})
	want(t, r.calls,
		rcall{PathNetworksetup, []string{"-setv6off", svc}, ""},
		rcall{PathNetworksetup, []string{"-setv6automatic", svc}, ""},
		rcall{PathNetworksetup, []string{"-setv6LinkLocal", svc}, ""},
		rcall{PathNetworksetup, []string{"-setv6off", svc}, ""},
		rcall{PathNetworksetup, []string{"-setv6manual", svc, "2001:db8::5", "64", "2001:db8::1"}, ""})
	// networksetup 退出码 0、错误打在 stdout 上。
	r2 := &recRunner{resp: func(rcall) ([]byte, []byte, error) {
		return []byte("Nope is not a recognized network service.\n** Error: The parameters were not valid.\n"), nil, nil
	}}
	if err := (MacIPv6{R: r2}).Off(ctx, "Nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("%v", err)
	}
	p, args, err := UtunConfigArgs("utun7", netip.MustParseAddr("198.18.0.1"), netip.MustParseAddr("198.18.0.1"), 1500)
	if err != nil || p != PathIfconfig || !slices.Equal(args, []string{"utun7", "inet", "198.18.0.1", "198.18.0.1", "mtu", "1500", "up"}) {
		t.Fatalf("%s %v %v", p, args, err)
	}
}

// c. 输出解析，全部用 testdata 样本。
func TestParseSamples(t *testing.T) {
	for f, exp := range map[string]string{"route-get-utun.txt": "utun1024", "route-get-en0.constructed.txt": "en0", "route-get-lo0.txt": "lo0"} {
		if got, err := ParseRouteGet(sample(t, f)); err != nil || got != exp {
			t.Fatalf("%s：%q %v", f, got, err)
		}
	}
	r := &recRunner{resp: func(rcall) ([]byte, []byte, error) {
		return nil, sample(t, "route-get-notintable.constructed.stderr.txt"), errExit
	}}
	if _, err := (MacRoutes{R: r}).Via(ctx, netip.MustParseAddr("1.1.1.1")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("没有路由：%v", err)
	}
	if _, err := ParseRouteGet([]byte("something else\n")); !errors.Is(err, ErrUnknownOutput) {
		t.Fatal("不认识的 route 输出")
	}

	svcs, err := ParseServices(sample(t, "listallnetworkservices.txt"))
	if err != nil || len(svcs) != 7 || svcs[0] != "AX88179A" || !slices.Contains(svcs, "203.0.113.10:443") {
		t.Fatalf("%v %v", svcs, err)
	}
	svcs, err = ParseServices(sample(t, "listallnetworkservices-disabled.constructed.txt"))
	if err != nil || slices.Contains(svcs, "Thunderbolt Bridge") || slices.Contains(svcs, "*Thunderbolt Bridge") || len(svcs) != 7 {
		t.Fatalf("停用项应跳过：%v %v", svcs, err)
	}
	if _, err := ParseServices([]byte("Wi-Fi\n")); !errors.Is(err, ErrUnknownOutput) {
		t.Fatal("没有首行说明应报错")
	}

	for f, exp := range map[string]IPv6Setting{
		"getinfo-automatic-wifi.txt":        {Mode: IPv6Automatic},
		"getinfo-automatic-vpn.txt":         {Mode: IPv6Automatic},
		"getinfo-automatic-ethernet.txt":    {Mode: IPv6Automatic},
		"getinfo-off.constructed.txt":       {Mode: IPv6Off},
		"getinfo-linklocal.constructed.txt": {Mode: IPv6LinkLocal},
		"getinfo-manual.constructed.txt":    {Mode: IPv6Manual, Address: netip.MustParseAddr("2001:db8::5"), PrefixLen: 64, Router: netip.MustParseAddr("2001:db8::1")},
	} {
		if got, err := ParseGetinfo(sample(t, f)); err != nil || got != exp {
			t.Fatalf("%s：%+v %v", f, got, err)
		}
	}
	if _, err := ParseGetinfo(sample(t, "getinfo-manual-noprefix.constructed.txt")); !errors.Is(err, ErrManualNoPrefix) {
		t.Fatalf("Manual 缺前缀：%v", err)
	}
	if _, err := ParseGetinfo(sample(t, "getinfo-unknown.constructed.txt")); !errors.Is(err, ErrUnknownOutput) {
		t.Fatalf("不认识的模式：%v", err)
	}
	if _, err := ParseGetinfo([]byte("DHCP Configuration\n")); !errors.Is(err, ErrUnknownOutput) {
		t.Fatal("没有 IPv6 行应报错")
	}

	if s, ok, err := ParseScutilShow(sample(t, "scutil-show-missing.txt")); err != nil || ok || s != nil {
		t.Fatal("键不存在")
	}
	if s, ok, err := ParseScutilShow(sample(t, "scutil-show-present.constructed.txt")); err != nil || !ok || len(s) != 1 || s[0] != netip.MustParseAddr("198.18.0.2") {
		t.Fatalf("键存在：%v %v %v", s, ok, err)
	}
	if _, _, err := ParseScutilShow([]byte("garbage")); !errors.Is(err, ErrUnknownOutput) {
		t.Fatal("不认识的 scutil 输出")
	}

	// Snapshot 用样本跑一遍。
	snapR := &recRunner{resp: func(c rcall) ([]byte, []byte, error) {
		if c.args[0] == "-listallnetworkservices" {
			return sample(t, "listallnetworkservices.txt"), nil, nil
		}
		return sample(t, "getinfo-automatic-wifi.txt"), nil, nil
	}}
	snap, err := (MacIPv6{R: snapR}).Snapshot(ctx)
	if err != nil || len(snap) != 7 || snap["Wi-Fi"].Mode != IPv6Automatic {
		t.Fatalf("%v %v", snap, err)
	}
}

// M2-8 第 0 步：Snapshot 混合样本——还原不了的服务记为 unknown，不让整个快照失败。
func TestSnapshotUnknown(t *testing.T) {
	list := "An asterisk (*) denotes that a network service is disabled.\nWi-Fi\nManual NoRouter\nOdd\nManual NoPrefix\n"
	files := map[string]string{
		"Wi-Fi":           "getinfo-automatic-wifi.txt",
		"Manual NoRouter": "getinfo-manual-norouter.constructed.txt",
		"Odd":             "getinfo-unknown.constructed.txt",
		"Manual NoPrefix": "getinfo-manual-noprefix.constructed.txt",
	}
	r := &recRunner{resp: func(c rcall) ([]byte, []byte, error) {
		if c.args[0] == "-listallnetworkservices" {
			return []byte(list), nil, nil
		}
		return sample(t, files[c.args[1]]), nil, nil
	}}
	snap, err := (MacIPv6{R: r}).Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantModes := map[string]IPv6Mode{"Wi-Fi": IPv6Automatic, "Manual NoRouter": IPv6Unknown, "Odd": IPv6Unknown, "Manual NoPrefix": IPv6Unknown}
	for svc, m := range wantModes {
		if snap[svc].Mode != m || !snap[svc].Valid() {
			t.Fatalf("%s：%+v", svc, snap[svc])
		}
	}
	// 列表本身不认识：整体报错。
	bad := &recRunner{resp: func(rcall) ([]byte, []byte, error) { return []byte("garbage\n"), nil, nil }}
	if _, err := (MacIPv6{R: bad}).Snapshot(ctx); !errors.Is(err, ErrUnknownOutput) {
		t.Fatalf("%v", err)
	}
	// unknown 不能拿去还原。
	if _, err := IPv6Args("Odd", IPv6Setting{Mode: IPv6Unknown}); !errors.Is(err, ErrBadArg) {
		t.Fatalf("%v", err)
	}
}

// d. ExecRunner 拒绝白名单外的路径且不执行。
func TestExecRunnerPaths(t *testing.T) {
	for _, p := range []string{"route", "/bin/sh", "/sbin/../bin/sh", "/sbin/route/../../bin/sh", "/usr/bin/true"} {
		if _, _, err := (ExecRunner{}).Run(ctx, p, nil, nil); !errors.Is(err, ErrPathNotAllowed) {
			t.Errorf("%s：%v", p, err)
		}
	}
}

// d. ReadOnlyRunner 拒绝改动类命令，底层 0 次调用；白名单各放行一例。
func TestReadOnlyRunner(t *testing.T) {
	deny := []rcall{
		{PathRoute, []string{"-n", "add", "-net", "0.0.0.0/1", "-interface", "utun7"}, ""},
		{PathRoute, []string{"-n", "delete", "-net", "0.0.0.0/1"}, ""},
		{PathRoute, []string{"-n", "change", "-net", "0.0.0.0/1", "-interface", "utun7"}, ""},
		{PathRoute, []string{"-n", "get", "1.1.1.1", "x"}, ""},
		{PathRoute, []string{"get", "1.1.1.1"}, ""},
		{PathNetworksetup, []string{"-setv6off", "Wi-Fi"}, ""},
		{PathNetworksetup, []string{"-setv6automatic", "Wi-Fi"}, ""},
		{PathNetworksetup, []string{"-getinfo", "-setv6off"}, ""},
		{PathScutil, nil, "d.init\nd.add ServerAddresses * 198.18.0.2\nset " + DNSKey + "\n"},
		{PathScutil, nil, "show " + DNSKey + "\nremove " + DNSKey + "\n"},
		{PathScutil, nil, "set " + DNSKey + "\n"},
		{PathScutil, []string{"--set", "HostName", "x"}, ""},
		{PathIfconfig, []string{"utun7"}, ""},
		{PathIfconfig, []string{"utun7", "inet", "198.18.0.1", "198.18.0.1", "mtu", "1500", "up"}, ""},
		{PathNetstat, []string{"-rn"}, ""},
		{PathScutil, []string{"--proxy"}, "show x\n"},
		{PathRoute, []string{"-n", "get", "default", "x"}, ""},
		{PathRoute, []string{"-n", "get", "notanaddr"}, ""},
		{PathPs, []string{"-axo", "comm,args"}, ""},
		{PathPs, []string{"-ef"}, ""},
		{PathPs, []string{"-axo", "comm"}, "x"},
		{"/bin/sh", []string{"-c", "true"}, ""},
	}
	under := &recRunner{}
	ro := ReadOnlyRunner{R: under}
	for _, c := range deny {
		var stdin []byte
		if c.stdin != "" {
			stdin = []byte(c.stdin)
		}
		if _, _, err := ro.Run(ctx, c.path, c.args, stdin); !errors.Is(err, ErrNotReadOnly) {
			t.Errorf("应拒绝 %s %v %q：%v", c.path, c.args, c.stdin, err)
		}
	}
	if len(under.calls) != 0 {
		t.Fatalf("被拒绝的命令到了底层：%+v", under.calls)
	}
	allow := []rcall{
		{PathRoute, []string{"-n", "get", "1.1.1.1"}, ""},
		{PathNetstat, []string{"-rn", "-f", "inet"}, ""},
		{PathNetworksetup, []string{"-listallnetworkservices"}, ""},
		{PathNetworksetup, []string{"-getinfo", "Wi-Fi"}, ""},
		{PathScutil, []string{"--dns"}, ""},
		{PathScutil, []string{"--nwi"}, ""},
		{PathScutil, []string{"--proxy"}, ""},
		{PathRoute, []string{"-n", "get", "default"}, ""},
		{PathPs, []string{"-axo", "comm"}, ""},
		{PathScutil, nil, "show " + DNSKey + "\nlist State:/Network/Service/.*/DNS\nquit\n"},
	}
	for _, c := range allow {
		var stdin []byte
		if c.stdin != "" {
			stdin = []byte(c.stdin)
		}
		if _, _, err := ro.Run(ctx, c.path, c.args, stdin); err != nil {
			t.Errorf("应放行 %s %v：%v", c.path, c.args, err)
		}
	}
	if len(under.calls) != len(allow) {
		t.Fatalf("放行 %d 次", len(under.calls))
	}
}

// e. Interfaces 用注入的接口列表。
func TestMacInterfaces(t *testing.T) {
	list := []IfaceInfo{
		{Name: "lo0", Up: true, Loopback: true, Addrs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/8")}},
		{Name: "utun1024", Up: true, Addrs: []netip.Prefix{netip.MustParsePrefix("198.18.0.1/30")}},
		{Name: "bridge100", Up: true, Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.64.1/24")}},
		{Name: "bridge1x", Up: true, Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.65.1/24")}},
		{Name: "en0", Up: true, Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.1.23/24"), netip.MustParsePrefix("fe80::1/64"), netip.MustParsePrefix("2001:db8::23/64")}},
		{Name: "en5", Up: false, Addrs: []netip.Prefix{netip.MustParsePrefix("10.9.0.2/16")}},
		{Name: "en7", Up: true, Addrs: []netip.Prefix{netip.MustParsePrefix("160.79.104.23/23")}},
	}
	ni, err := (MacInterfaces{List: func() ([]IfaceInfo, error) { return list, nil }}).Interfaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantDirect := []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24"), netip.MustParsePrefix("160.79.104.0/23")}
	if !slices.Equal(ni.Direct, wantDirect) || !slices.Equal(ni.VMIfs, []string{"bridge100"}) {
		t.Fatalf("%+v", ni)
	}
	lan := pf.LANPrefixes(ni.Direct)
	if slices.Contains(lan, netip.MustParsePrefix("160.79.104.0/23")) || !slices.Contains(lan, netip.MustParsePrefix("192.168.1.0/24")) {
		t.Fatalf("LANPrefixes 应丢掉公网网段：%v", lan)
	}
}
