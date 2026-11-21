package sysnet

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"

	"go/ast"
	"io/fs"
	"path/filepath"
	"slices"

	"claude-abroad/internal/pf"
	"claude-abroad/internal/srccheck"
)

var ctx = context.Background()

func initial() FakeState {
	return FakeState{
		IPv6: map[string]IPv6Setting{
			"Wi-Fi":      {Mode: IPv6Automatic},
			"Ethernet":   {Mode: IPv6Manual, Address: netip.MustParseAddr("fd00::5"), PrefixLen: 64, Router: netip.MustParseAddr("fd00::1")},
			"Thunder":    {Mode: IPv6LinkLocal},
			"USB 10/100": {Mode: IPv6Off},
		},
		Net: NetInfo{Direct: []netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")}, VMIfs: []string{"bridge100"}},
	}
}

func TestFakeCRUDAndPresent(t *testing.T) {
	f := NewFake(initial())
	r, d, v := f.Routes(), f.DNS(), f.IPv6()
	if ok, _ := r.Present(ctx); ok {
		t.Fatal("初始不应有路由")
	}
	dst := netip.MustParsePrefix("198.18.0.0/15")
	if err := r.Add(ctx, dst, "utun7"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := r.Present(ctx); !ok {
		t.Fatal("加路由后 Present 应为 true")
	}
	if f.Snapshot().Routes[dst] != "utun7" {
		t.Fatal("路由没记下")
	}
	if err := r.Add(ctx, dst, "utun7"); !errors.Is(err, ErrExists) || f.Snapshot().Routes[dst] != "utun7" {
		t.Fatalf("同接口重复加路由：%v", err)
	}
	if err := r.Delete(ctx, dst); err != nil {
		t.Fatal(err)
	}
	if ok, _ := r.Present(ctx); ok {
		t.Fatal("删路由后 Present 应为 false")
	}
	if err := r.Delete(ctx, dst); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删不存在的路由：%v", err)
	}

	srv := netip.MustParseAddr("198.18.0.2")
	if err := d.Set(ctx, srv); err != nil {
		t.Fatal(err)
	}
	if ok, _ := d.Present(ctx); !ok || f.Snapshot().DNS != srv {
		t.Fatal("DNS 没设上")
	}
	if err := d.Set(ctx, srv); !errors.Is(err, ErrExists) {
		t.Fatalf("重复设 DNS：%v", err)
	}
	if err := d.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _ := d.Present(ctx); ok {
		t.Fatal("DNS 没删掉")
	}
	if err := d.Remove(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删 DNS：%v", err)
	}

	snap, err := v.Snapshot(ctx)
	if err != nil || len(snap) != 4 || snap["Ethernet"].PrefixLen != 64 {
		t.Fatalf("IPv6 快照：%v %v", snap, err)
	}
	if err := v.Off(ctx, "Ethernet"); err != nil {
		t.Fatal(err)
	}
	if f.Snapshot().IPv6["Ethernet"].Mode != IPv6Off {
		t.Fatal("没关掉")
	}
	if err := v.Restore(ctx, "Ethernet", snap["Ethernet"]); err != nil {
		t.Fatal(err)
	}
	if f.Snapshot().IPv6["Ethernet"] != snap["Ethernet"] {
		t.Fatal("没还原")
	}
	if err := v.Off(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("关不存在的服务：%v", err)
	}
	if err := v.Restore(ctx, "Wi-Fi", IPv6Setting{Mode: IPv6Manual}); err == nil {
		t.Fatal("不合法的设置应报错")
	}
	// 返回的快照是副本，改它不影响 Fake。
	snap["Wi-Fi"] = IPv6Setting{Mode: IPv6Off}
	if f.Snapshot().IPv6["Wi-Fi"].Mode != IPv6Automatic {
		t.Fatal("快照不是副本")
	}
	if c := f.Calls(); len(c) == 0 || c[0] != "routes.present" || c[1] != "routes.add 198.18.0.0/15 utun7" {
		t.Fatalf("调用记录：%v", c)
	}
}

func TestFakeFailNext(t *testing.T) {
	f := NewFake(initial())
	before := f.Snapshot()
	boom := errors.New("boom")
	changes := 0
	f.OnChange = func(string) { changes++ }
	cases := []struct {
		op string
		do func() error
	}{
		{OpRoutesAdd, func() error { return f.Routes().Add(ctx, netip.MustParsePrefix("198.18.0.0/15"), "utun7") }},
		{OpDNSSet, func() error { return f.DNS().Set(ctx, netip.MustParseAddr("198.18.0.2")) }},
		{OpIPv6Off, func() error { return f.IPv6().Off(ctx, "Wi-Fi") }},
		{OpIPv6Restore, func() error { return f.IPv6().Restore(ctx, "Wi-Fi", IPv6Setting{Mode: IPv6Off}) }},
	}
	for _, c := range cases {
		f.FailNext(c.op, boom)
		if err := c.do(); !errors.Is(err, boom) {
			t.Fatalf("%s：%v", c.op, err)
		}
		if !f.Snapshot().Equal(before) {
			t.Fatalf("%s 失败后状态变了", c.op)
		}
	}
	if changes != 0 {
		t.Fatalf("失败的调用不应回调：%d", changes)
	}
	f.FailNext(OpInterfaces, boom)
	if _, err := f.Interfaces(ctx); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	// 只失败一次。
	if err := cases[0].do(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Interfaces(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestFakeOnChangeExactlyOnce(t *testing.T) {
	f := NewFake(initial())
	var got []string
	f.OnChange = func(op string) {
		// 回调里能读状态（不持锁）。
		_ = f.Snapshot()
		got = append(got, op)
	}
	dst := netip.MustParsePrefix("198.18.0.0/15")
	_ = f.Routes().Add(ctx, dst, "utun7")
	_, _ = f.Routes().Present(ctx) // 只读，不回调
	_ = f.DNS().Set(ctx, netip.MustParseAddr("198.18.0.2"))
	_, _ = f.IPv6().Snapshot(ctx)
	_ = f.IPv6().Off(ctx, "Wi-Fi")
	_ = f.IPv6().Restore(ctx, "Wi-Fi", IPv6Setting{Mode: IPv6Automatic})
	_ = f.DNS().Remove(ctx)
	_ = f.Routes().Delete(ctx, dst)
	_ = f.Routes().Delete(ctx, dst) // 失败，不回调
	_, _ = f.Interfaces(ctx)
	want := []string{OpRoutesAdd, OpDNSSet, OpIPv6Off, OpIPv6Restore, OpDNSRemove, OpRoutesDelete}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("回调序列 %v，期望 %v", got, want)
	}
}

func TestFakeReverseRestoresSnapshot(t *testing.T) {
	init := initial()
	f := NewFake(init)
	orig, _ := f.IPv6().Snapshot(ctx)
	dst := netip.MustParsePrefix("198.18.0.0/15")
	apply := func() {
		_ = f.Routes().Add(ctx, dst, "utun7")
		_ = f.DNS().Set(ctx, netip.MustParseAddr("198.18.0.2"))
		for _, s := range []string{"Wi-Fi", "Ethernet", "Thunder"} {
			if err := f.IPv6().Off(ctx, s); err != nil {
				t.Fatal(err)
			}
		}
	}
	apply()
	if f.Snapshot().Equal(init) {
		t.Fatal("改动后不应与初始相等")
	}
	_ = f.IPv6().Restore(ctx, "Thunder", orig["Thunder"])
	_ = f.IPv6().Restore(ctx, "Ethernet", orig["Ethernet"])
	_ = f.IPv6().Restore(ctx, "Wi-Fi", orig["Wi-Fi"])
	_ = f.DNS().Remove(ctx)
	_ = f.Routes().Delete(ctx, dst)
	if !f.Snapshot().Equal(init) {
		t.Fatalf("逆向还原后应与初始相等：%+v", f.Snapshot())
	}

	// 少还原一项（每一项分别试）就不相等。
	skips := []string{"route", "dns", "Wi-Fi", "Ethernet", "Thunder"}
	for _, skip := range skips {
		f := NewFake(init)
		_ = f.Routes().Add(ctx, dst, "utun7")
		_ = f.DNS().Set(ctx, netip.MustParseAddr("198.18.0.2"))
		for _, s := range []string{"Wi-Fi", "Ethernet", "Thunder"} {
			_ = f.IPv6().Off(ctx, s)
		}
		for _, s := range []string{"Thunder", "Ethernet", "Wi-Fi"} {
			if s != skip {
				_ = f.IPv6().Restore(ctx, s, orig[s])
			}
		}
		if skip != "dns" {
			_ = f.DNS().Remove(ctx)
		}
		if skip != "route" {
			_ = f.Routes().Delete(ctx, dst)
		}
		if f.Snapshot().Equal(init) {
			t.Fatalf("少还原 %s 仍判相等", skip)
		}
	}
}

// NetSource 适配直接喂给 pf.Patrol 跑一轮 Step：规则按 Fake 的网卡信息生成，LAN 网段放行、公网被拦。
func TestNetSourceFeedsPatrol(t *testing.T) {
	f := NewFake(initial())
	fpf := pf.NewFakePF()
	fpf.UIDs = map[string]string{"_abroad": "250"}
	var tokens []string
	p := &pf.Patrol{
		C:       &pf.Controller{R: fpf, Matcher: pf.PrintMatcher{User: "_abroad", UID: "250"}},
		Net:     NetSource(f),
		User:    "_abroad",
		Utun:    func() string { return "utun7" },
		OnToken: func(tok string) { tokens = append(tokens, tok) },
	}
	if err := p.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if !p.Protected() || len(tokens) != 1 {
		t.Fatalf("protected=%v tokens=%v", p.Protected(), tokens)
	}
	ok, err := fpf.Eval(pf.DefaultAnchor, pf.Packet{If: "en0", Proto: "tcp", DstPort: 80, Dst: netip.MustParseAddr("192.168.1.20")})
	if err != nil || !ok {
		t.Fatalf("LAN 应放行：%v %v", ok, err)
	}
	ok, _ = fpf.Eval(pf.DefaultAnchor, pf.Packet{If: "en0", Proto: "tcp", DstPort: 443, Dst: netip.MustParseAddr("160.79.104.10")})
	if ok {
		t.Fatal("公网应被拦")
	}
	ok, _ = fpf.Eval(pf.DefaultAnchor, pf.Packet{If: "bridge100", Proto: "udp", DstPort: 53, Dst: netip.MustParseAddr("192.168.64.1")})
	if !ok {
		t.Fatal("网桥到私网应放行")
	}
	if c := f.Calls(); len(c) != 1 || c[0] != OpInterfaces {
		t.Fatalf("调用记录：%v", c)
	}
	// 读不到网卡信息时，适配把错误交给 Patrol（沿用上一次的期望，保护不失效）。
	f.FailNext(OpInterfaces, errors.New("boom"))
	if err := p.Step(ctx); err != nil || !p.Protected() {
		t.Fatalf("err=%v protected=%v", err, p.Protected())
	}
}

// 源码检查：非测试文件不执行外部命令（7a 不写 darwin 实现）。
// M2-7d 决定 5：只有 exec.go 可以导入 os/exec，其中以 / 开头的字符串只能是允许的路径；
// darwin 实现文件（mac*.go）放开命令名的字符串检查；其余文件照常全部检查。
func TestNoExecInSources(t *testing.T) {
	rules := map[string]srccheck.Rule{"exec.go": {AllowExec: true, AllowCmdName: true}}
	all, _ := filepath.Glob("mac*.go")
	mac := slices.DeleteFunc(all, func(f string) bool { return strings.HasSuffix(f, "_test.go") })
	for _, f := range mac {
		rules[filepath.Base(f)] = srccheck.Rule{AllowCmdName: true}
	}
	// 命令模拟器（M2-9b-1）按真实命令的输出格式作答，字符串里有命令名；它不执行任何命令（导入和调用检查照常）。
	rules["sim.go"] = srccheck.Rule{AllowCmdName: true}
	rules["sim_locale.go"] = srccheck.Rule{AllowCmdName: true}
	srccheck.NoExecWith(t, ".", rules)
	allowed := []string{"/sbin/route", "/usr/sbin/networksetup", "/usr/sbin/scutil", "/sbin/ifconfig", "/usr/sbin/netstat", "/bin/ps",
		"/usr/sbin/systemsetup", "/usr/bin/defaults", "/bin/launchctl", "/usr/bin/sudo"} // 后 4 个是 M4-5 的地区（m4-plan v2.4 决定 7）
	var paths []string
	for _, s := range srccheck.StringLits(srccheck.Parse(t, "exec.go")) {
		if strings.HasPrefix(s, "/") {
			if !slices.Contains(allowed, s) {
				t.Errorf("exec.go 里有白名单外的路径：%q", s)
			}
			paths = append(paths, s)
		}
	}
	if len(paths) != len(allowed) {
		t.Errorf("exec.go 的路径：%v", paths)
	}
	// mac*.go 里不得出现以 / 开头的路径字面量（路径只经 exec.go 的常量）。
	for _, f := range mac {
		for _, s := range srccheck.StringLits(srccheck.Parse(t, f)) {
			if strings.HasPrefix(s, "/") {
				t.Errorf("%s 里有路径字面量：%q", f, s)
			}
		}
	}
}

// M2-7d 决定 5、M2-9b-1 决定 3：全仓库非测试代码除 sysnet/exec.go 和 daemon/realexec.go 外没有对 ExecRunner 的引用。
func TestExecRunnerReferences(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "third_party" || d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) && p != root {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		abs, _ := filepath.Abs(p)
		self, _ := filepath.Abs("exec.go")
		realexec, _ := filepath.Abs("../daemon/realexec.go") // M2-9b-1 决定 3：生产执行器只在这里取得
		if abs == self || abs == realexec {
			return nil
		}
		inSysnet := filepath.Dir(abs) == filepath.Dir(self)
		ast.Inspect(srccheck.Parse(t, p), func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && id.Name == "sysnet" && x.Sel.Name == "ExecRunner" {
					t.Errorf("%s 引用了 sysnet.ExecRunner", p)
				}
			case *ast.Ident:
				if inSysnet && x.Name == "ExecRunner" {
					t.Errorf("%s 引用了 ExecRunner", p)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFakeTestHelpers(t *testing.T) {
	f := NewFake(initial())
	changes := 0
	f.OnChange = func(string) { changes++ }
	_ = f.Routes().Add(ctx, netip.MustParsePrefix("0.0.0.0/1"), "utun7")
	_ = f.Routes().Add(ctx, netip.MustParsePrefix("10.9.0.0/16"), "utun9")
	_ = f.DNS().Set(ctx, netip.MustParseAddr("198.18.0.2"))
	changes = 0
	f.RemoveRoutesVia("utun7")
	if st := f.Snapshot(); len(st.Routes) != 1 || st.Routes[netip.MustParsePrefix("10.9.0.0/16")] != "utun9" {
		t.Fatalf("%v", st.Routes)
	}
	f.ClearRoutesAndDNS()
	if st := f.Snapshot(); len(st.Routes) != 0 || st.DNS.IsValid() {
		t.Fatal("没清掉")
	}
	if changes != 0 {
		t.Fatal("测试辅助不应回调")
	}
}

// 7e 第 0 步：Add 同接口 ErrExists 且不算改动，不同接口改写并算一次改动；DNS.Set 同理。
func TestFakeAddSetContract(t *testing.T) {
	f := NewFake(initial())
	var changes []string
	f.OnChange = func(op string) { changes = append(changes, op) }
	dst := netip.MustParsePrefix("0.0.0.0/1")
	if err := f.Routes().Add(ctx, dst, "utun7"); err != nil {
		t.Fatal(err)
	}
	if err := f.Routes().Add(ctx, dst, "utun7"); !errors.Is(err, ErrExists) {
		t.Fatalf("%v", err)
	}
	if err := f.Routes().Add(ctx, dst, "utun8"); err != nil {
		t.Fatalf("不同接口应改写：%v", err)
	}
	if f.Snapshot().Routes[dst] != "utun8" {
		t.Fatal("没改写")
	}
	a, b := netip.MustParseAddr("198.18.0.2"), netip.MustParseAddr("198.18.0.3")
	if err := f.DNS().Set(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := f.DNS().Set(ctx, a); !errors.Is(err, ErrExists) {
		t.Fatalf("%v", err)
	}
	if err := f.DNS().Set(ctx, b); err != nil || f.Snapshot().DNS != b {
		t.Fatalf("不同服务器应改写：%v", err)
	}
	want := []string{OpRoutesAdd, OpRoutesAdd, OpDNSSet, OpDNSSet}
	if strings.Join(changes, ",") != strings.Join(want, ",") {
		t.Fatalf("改动 %v，期望 %v（ErrExists 不算改动）", changes, want)
	}
}

// 7e 第 0 步：Via 的最长前缀匹配。
func TestFakeViaLongestPrefix(t *testing.T) {
	st := initial()
	st.Default = "en0"
	f := NewFake(st)
	r := f.Routes()
	via := func(a string) string {
		t.Helper()
		v, err := r.Via(ctx, netip.MustParseAddr(a))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if via("1.1.1.1") != "en0" {
		t.Fatal("默认路由")
	}
	_ = r.Add(ctx, netip.MustParsePrefix("0.0.0.0/1"), "utun7")
	_ = r.Add(ctx, netip.MustParsePrefix("128.0.0.0/1"), "utun7")
	if via("1.1.1.1") != "utun7" || via("129.0.0.1") != "utun7" || via("198.18.0.2") != "utun7" {
		t.Fatal("两条 /1")
	}
	f.SetForeignRoute(netip.MustParsePrefix("198.18.0.0/16"), "utun9")
	if via("198.18.0.2") != "utun9" || via("1.1.1.1") != "utun7" {
		t.Fatal("更具体的 /16")
	}
	if _, err := NewFake(initial()).Routes().Via(ctx, netip.MustParseAddr("1.1.1.1")); !errors.Is(err, ErrNotFound) {
		t.Fatal("没有默认路由时应 ErrNotFound")
	}
}
