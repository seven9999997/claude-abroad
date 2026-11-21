package pf

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

// recRunner 记录参数，按预设返回。
type recRunner struct {
	calls  [][]string
	stdins [][]byte
	so, se []byte
	err    error
}

func (r *recRunner) Run(_ context.Context, args []string, stdin []byte) ([]byte, []byte, error) {
	r.calls = append(r.calls, slices.Clone(args))
	r.stdins = append(r.stdins, stdin)
	return r.so, r.se, r.err
}

// c. 每个方法拼出的参数逐字核对；非法锚点、非法 token 在调用 Runner 之前就报错
func TestControllerArgs(t *testing.T) {
	ctx := context.Background()
	r := &recRunner{so: []byte("Token : 42\n")}
	c := &Controller{R: r, Anchor: "com.apple/100.abroad"}
	c.Load(ctx, []byte("block drop out quick all\n"))
	c.Flush(ctx)
	c.Enable(ctx)
	c.Release(ctx, "42")
	c.KillStates(ctx)
	r.so = []byte("Status: Enabled\n")
	c.Status(ctx)
	want := [][]string{
		{"-o", "none", "-a", "com.apple/100.abroad", "-f", "-"},
		{"-a", "com.apple/100.abroad", "-F", "all"},
		{"-E"},
		{"-X", "42"},
		{"-F", "states"},
		{"-s", "info"},
		{"-s", "rules"},
		{"-a", "com.apple/100.abroad", "-s", "rules"},
		{"-a", "com.apple/100.abroad", "-t", "abroad_lan", "-T", "show"},
	}
	if len(r.calls) != len(want) {
		t.Fatalf("调用次数：%v", r.calls)
	}
	for i := range want {
		if !slices.Equal(r.calls[i], want[i]) {
			t.Errorf("第 %d 次参数 %v，应为 %v", i, r.calls[i], want[i])
		}
	}
	if string(r.stdins[0]) != "block drop out quick all\n" {
		t.Error("Load 的规则应经 stdin")
	}
	if a, err := (&Controller{}).anchor(); a != DefaultAnchor || err != nil {
		t.Errorf("没设锚点时应用默认锚点：%q %v", a, err)
	}
	for _, bad := range []string{"com.apple/250", "com.apple/25.abroad", "com.apple/250.Abroad", "other/250.abroad", "com.apple/250.abroad; rm", "com.apple/*"} {
		br := &recRunner{}
		bc := &Controller{R: br, Anchor: bad}
		errs := []error{bc.Load(ctx, nil), bc.Flush(ctx)}
		_, e := bc.Status(ctx)
		errs = append(errs, e)
		for _, err := range errs {
			if !errors.Is(err, ErrBadAnchor) {
				t.Errorf("锚点 %q 应报 ErrBadAnchor：%v", bad, err)
			}
		}
		if len(br.calls) != 0 {
			t.Errorf("锚点 %q 不合法时不应调用 Runner", bad)
		}
	}
	for _, bad := range []string{"", "12a", "-1", "1 2", "12;"} {
		br := &recRunner{}
		if err := (&Controller{R: br}).Release(ctx, bad); !errors.Is(err, ErrBadToken) || len(br.calls) != 0 {
			t.Errorf("token %q 应在调用前报错：%v", bad, err)
		}
	}
}

// d. Enable 的解析
func TestEnableTokenParsing(t *testing.T) {
	cases := []struct {
		so, se string
		want   string
		err    error
	}{
		{"Token : 12345\n", "", "12345", nil},
		{"", "pf enabled\nToken : 67890\n", "67890", nil},
		{"", "pfctl: pf already enabled\nToken : 111\n", "111", nil},
		{"", "pf enabled\n", "", ErrNoToken},
		{"No ALTQ support in kernel\nALTQ related functions disabled\n", "pf enabled\nToken:   2222\nsomething else\n", "2222", nil},
		{"Token : abc\n", "", "", ErrNoToken},
	}
	for i, c := range cases {
		ctl := &Controller{R: &recRunner{so: []byte(c.so), se: []byte(c.se)}}
		got, err := ctl.Enable(context.Background())
		if got != c.want || !errors.Is(err, c.err) {
			t.Errorf("第 %d 例：%q %v，应为 %q %v", i, got, err, c.want, c.err)
		}
	}
	if _, err := (&Controller{R: &recRunner{err: errors.New("exit 1"), se: []byte("permission denied")}}).Enable(context.Background()); err == nil {
		t.Error("pfctl 出错时应报错")
	}
}

// —— Matcher ——

// sample 读真实样本，去掉 pfctl 打在 stderr 上的固定提示。
func sample(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "pfctl: Use of -f") || strings.HasPrefix(l, "present in the main") || strings.HasPrefix(l, "See /etc/pf.conf") {
			continue
		}
		out = append(out, l)
	}
	return []byte(strings.Join(out, "\n"))
}

// 采样时 pf-syntax.sh 用的三份配置。
var sampleCfgs = map[string]Config{
	"full":  {UtunName: "utun9", LAN: LANPrefixes(pfx("192.168.1.0/24")), VMIfs: []string{"bridge100", "vmenet0"}, User: "nobody"},
	"boot":  {LAN: LANPrefixes(pfx("192.168.1.0/24")), VMIfs: []string{"bridge100"}, User: "nobody"},
	"nolan": {UtunName: "utun9", VMIfs: []string{"bridge100"}, User: "nobody"},
}

func tableShow(rules []byte) []byte {
	var b strings.Builder
	for _, p := range expectedTable(rules) {
		b.WriteString("   " + p.String() + "\n")
	}
	return []byte(b.String())
}

// e. 真实样本判一致；各种差异判不同
func TestPrintMatcherSamples(t *testing.T) {
	m := PrintMatcher{User: "nobody", UID: "4294967294"}
	for name, c := range sampleCfgs {
		exp := mustRender(t, c)
		st := Status{AnchorRules: sample(t, "nvf-onone-"+name+".txt"), Table: tableShow(exp)}
		if !m.Present(st) || !m.Same(exp, st) {
			t.Errorf("%s：-o none 的真实样本应判一致", name)
		}
	}
	exp := mustRender(t, sampleCfgs["full"])
	good := sample(t, "nvf-onone-full.txt")
	tbl := tableShow(exp)
	if m.Same(exp, Status{AnchorRules: sample(t, "nvf-optimized-full.txt"), Table: tbl}) {
		t.Error("带优化的样本（自动表、重排）应判不同")
	}
	lines := strings.Split(strings.TrimSpace(string(good)), "\n")
	mut := func(f func([]string) []string) []byte {
		return []byte(strings.Join(f(slices.Clone(lines)), "\n"))
	}
	diffs := map[string][]byte{
		"少一行":          mut(func(l []string) []string { return slices.Delete(l, 3, 4) }),
		"多一行":          mut(func(l []string) []string { return append(l, l[len(l)-1]) }),
		"顺序调换":         mut(func(l []string) []string { l[4], l[5] = l[5], l[4]; return l }),
		"网段不同":         []byte(strings.Replace(string(good), "to 172.16.0.0/12 port", "to 172.17.0.0/16 port", 1)),
		"多一条全放行":       mut(func(l []string) []string { return slices.Insert(l, 2, "pass out quick all flags S/SA keep state") }),
		"UID 不对":       []byte(strings.ReplaceAll(string(good), "4294967294", "501")),
		"多出 automatic": []byte(strings.Replace(string(good), "to 10.0.0.0/8 port", "to <__automatic_0> port", 1)),
	}
	for name, rules := range diffs {
		if m.Same(exp, Status{AnchorRules: rules, Table: tbl}) {
			t.Errorf("%s 应判不同", name)
		}
	}
	if m.Same(exp, Status{AnchorRules: good, Table: tableShow(mustRender(t, Config{UtunName: "utun9", LAN: pfx("10.0.0.0/8"), User: "nobody"}))}) {
		t.Error("表内容不同应判不同")
	}
	if m.Same(exp, Status{AnchorRules: good}) {
		t.Error("期望有表而表不存在应判不同")
	}
	byName := []byte(strings.ReplaceAll(string(good), "user = 4294967294", "user = nobody"))
	if !m.Same(exp, Status{AnchorRules: byName, Table: tbl}) {
		t.Error("user 打成用户名也应判一致")
	}
	if m.Present(Status{}) || m.Present(Status{AnchorRules: []byte("\n")}) {
		t.Error("空锚点应判“不在”")
	}
}

// —— Ensure（用 FakePF）——

func newCtl(f *FakePF) *Controller {
	return &Controller{R: f, Matcher: PrintMatcher{User: "_abroad", UID: "250"}}
}

var ensureCfg = Config{UtunName: "utun5", LAN: LANPrefixes(pfx("192.168.1.0/24")), User: "_abroad"}

func publicPkt() Packet { return pk("en0", "alice", "tcp", 1000, 443, "1.1.1.1") }

// f、g
func TestEnsureWithFakePF(t *testing.T) {
	ctx := context.Background()
	f := NewFakePF()
	f.UIDs = map[string]string{"_abroad": "250"}
	c := newCtl(f)
	exp := mustRender(t, ensureCfg)
	pass := func() bool {
		ok, err := f.Eval(DefaultAnchor, publicPkt())
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !pass() {
		t.Fatal("前提：没有规则时假 pf 不拦")
	}

	// 首次：Load 紧跟 KillStates，再 Enable
	r, err := c.Ensure(ctx, exp, nil)
	if err != nil || !r.Loaded || !r.WasAbsent || !r.KilledStates || r.NewToken == "" {
		t.Fatalf("首次：%+v %v", r, err)
	}
	calls := f.Calls()
	li := slices.Index(calls, "load")
	if li < 0 || li+1 >= len(calls) || calls[li+1] != "kill" || slices.Index(calls, "enable") < li {
		t.Fatalf("首次的调用序列应是 load 紧跟 kill，再 enable：%v", calls)
	}
	if pass() {
		t.Fatal("Ensure 之后 en0 上普通用户到公网应被拦")
	}

	// 已一致：只有只读调用
	f.ResetCalls()
	if r, err := c.Ensure(ctx, exp, nil); err != nil || r.Loaded || r.NewToken != "" {
		t.Fatalf("已一致：%+v %v", r, err)
	}
	for _, op := range f.Calls() {
		if op != "info" && op != "mainrules" && op != "anchorrules" && op != "table" {
			t.Fatalf("已一致时应只有只读调用：%v", f.Calls())
		}
	}

	// ClearAnchor 后：Load + Kill；之前放行
	f.ClearAnchor()
	if !pass() {
		t.Fatal("锚点被清空后假 pf 不拦（这就是要巡检的原因）")
	}
	k := f.Kills()
	if r, err := c.Ensure(ctx, exp, nil); err != nil || !r.Loaded || !r.KilledStates || f.Kills() != k+1 {
		t.Fatalf("ClearAnchor 后：%+v %v", r, err)
	}
	if pass() {
		t.Fatal("恢复后应拦")
	}

	// 内容不同（换网段）：只 Load 不 Kill；之后旧网段不放行
	exp2 := mustRender(t, Config{UtunName: "utun5", LAN: LANPrefixes(pfx("10.9.0.0/16")), User: "_abroad"})
	k = f.Kills()
	if r, err := c.Ensure(ctx, exp2, exp); err != nil || !r.Loaded || r.KilledStates || r.Foreign || f.Kills() != k {
		t.Fatalf("换网段（previous 等于锚点）：%+v %v", r, err)
	}
	if ok, _ := f.Eval(DefaultAnchor, pk("en0", "alice", "tcp", 1000, 631, "192.168.1.20")); ok {
		t.Fatal("换网段后旧网段不应放行")
	}
	if ok, _ := f.Eval(DefaultAnchor, pk("en0", "alice", "tcp", 1000, 631, "10.9.1.1")); !ok {
		t.Fatal("新网段应放行")
	}

	// Disable 后：返回新 token
	f.Disable()
	if !pass() {
		t.Fatal("pf 被关后假 pf 不拦")
	}
	if r, err := c.Ensure(ctx, exp2, nil); err != nil || r.NewToken == "" || r.Loaded {
		t.Fatalf("Disable 后应只重新启用：%+v %v", r, err)
	}
	if pass() {
		t.Fatal("重新启用后应拦")
	}

	// DropMainRef：返回 ErrAnchorNotReferenced
	f.DropMainRef()
	if !pass() {
		t.Fatal("主规则集不引用锚点时假 pf 不拦")
	}
	if _, err := c.Ensure(ctx, exp2, nil); !errors.Is(err, ErrAnchorNotReferenced) {
		t.Fatalf("应返回 ErrAnchorNotReferenced：%v", err)
	}
	if !pass() {
		t.Fatal("Ensure 不改主规则集，仍不拦（处置在 6c）")
	}
}

func TestEnsureFailures(t *testing.T) {
	ctx := context.Background()
	exp := mustRender(t, ensureCfg)
	exp2 := mustRender(t, Config{UtunName: "utun5", LAN: LANPrefixes(pfx("10.9.0.0/16")), User: "_abroad"})

	// Load 本身失败：旧规则保留
	f := NewFakePF()
	f.UIDs = map[string]string{"_abroad": "250"}
	c := newCtl(f)
	if _, err := c.Ensure(ctx, exp, nil); err != nil {
		t.Fatal(err)
	}
	f.FailNext("load", errors.New("boom"))
	if _, err := c.Ensure(ctx, exp2, nil); err == nil {
		t.Fatal("Load 失败应报错")
	}
	if ok, _ := f.Eval(DefaultAnchor, pk("en0", "alice", "tcp", 1000, 631, "192.168.1.20")); !ok {
		t.Fatal("Load 失败时旧规则应原样保留")
	}

	// Status 出错直接返回
	f.FailNext("info", errors.New("boom"))
	if r, err := c.Ensure(ctx, exp2, nil); err == nil || r.Loaded {
		t.Fatalf("Status 出错应直接返回：%+v %v", r, err)
	}

	// 复核不一致（Matcher 的 UID 不对）：新规则保留，ErrVerifyFailed
	f2 := NewFakePF()
	f2.UIDs = map[string]string{"_abroad": "999"}
	c2 := newCtl(f2)
	if _, err := c2.Ensure(ctx, exp, nil); !errors.Is(err, ErrVerifyFailed) {
		t.Fatalf("复核不一致应 ErrVerifyFailed：%v", err)
	}
	if ok, _ := f2.Eval(DefaultAnchor, publicPkt()); ok {
		t.Fatal("复核不一致时新规则保留（仍拦）")
	}

	// 漏掉 -o none（假 pf 按优化后的形式打印）：ErrVerifyFailed
	f3 := NewFakePF()
	f3.UIDs = map[string]string{"_abroad": "250"}
	opt := &noOptimizerOff{f3}
	c3 := &Controller{R: opt, Matcher: PrintMatcher{User: "_abroad", UID: "250"}}
	if _, err := c3.Ensure(ctx, exp, nil); !errors.Is(err, ErrVerifyFailed) {
		t.Fatalf("加载时没关优化器应 ErrVerifyFailed：%v", err)
	}
}

// noOptimizerOff 去掉 Load 参数里的 -o none（模拟漏掉关闭优化器）。
type noOptimizerOff struct{ *FakePF }

func (n *noOptimizerOff) Run(ctx context.Context, args []string, stdin []byte) ([]byte, []byte, error) {
	if len(args) >= 2 && args[0] == "-o" && args[1] == "none" {
		args = args[2:]
	}
	return n.FakePF.Run(ctx, args, stdin)
}

// 必须改 1：主规则集是否引用锚点按行判断，scrub-anchor 等不算。
func TestReferencesAnchor(t *testing.T) {
	cases := map[string]bool{
		`scrub-anchor "com.apple/*" all fragment reassemble`:                               false,
		"nat-anchor \"com.apple/*\" all\nrdr-anchor \"com.apple/*\" all":                   false,
		`dummynet-anchor "com.apple/*" all`:                                                false,
		"scrub-anchor \"com.apple/*\" all fragment reassemble\nanchor \"com.apple/*\" all": true,
		`  anchor "com.apple/*" all`:                                                       true,
		"pass all flags S/SA keep state":                                                   false,
		"":                                                                                 false,
	}
	for in, want := range cases {
		if got := referencesAnchor([]byte(in)); got != want {
			t.Errorf("%q：%v，应为 %v", in, got, want)
		}
	}
	// 经 Status：FakePF 换掉主规则集后只留 scrub-anchor 行，Referenced 应为 false。
	f := NewFakePF()
	c := &Controller{R: f}
	st, _ := c.Status(context.Background())
	if !st.Referenced {
		t.Fatal("默认主规则集应引用锚点")
	}
	f.DropMainRef()
	st, _ = c.Status(context.Background())
	if st.Referenced {
		t.Fatal("只剩 scrub-anchor 时不算引用")
	}
}

// 建议改 3：期望里没有 <abroad_lan> 时不比表（旧表残留无害）。
func TestMatcherIgnoresLeftoverTable(t *testing.T) {
	m := PrintMatcher{User: "nobody", UID: "4294967294"}
	exp := mustRender(t, sampleCfgs["nolan"])
	st := Status{AnchorRules: sample(t, "nvf-onone-nolan.txt"), Table: []byte("   192.168.1.0/24\n   224.0.0.0/4\n")}
	if !m.Same(exp, st) {
		t.Fatal("期望里没有局域网表时，残留的旧表不影响判一致")
	}
}

// a. 锚点被别的软件改成别的合法规则：previous 为空或不等时 Load + Kill，Foreign。
func TestEnsureForeignAnchor(t *testing.T) {
	ctx := context.Background()
	exp := mustRender(t, ensureCfg)
	other := mustRender(t, Config{UtunName: "utun7", User: "_abroad"})
	for name, prev := range map[string][]byte{"previous 为空": nil, "previous 不等": mustRender(t, Config{UtunName: "utun6", User: "_abroad"})} {
		f := NewFakePF()
		f.UIDs = map[string]string{"_abroad": "250"}
		c := newCtl(f)
		f.Run(ctx, LoadArgs(DefaultAnchor), other) // 别的软件往锚点里放了别的规则
		k := f.Kills()
		r, err := c.Ensure(ctx, exp, prev)
		if err != nil || !r.Loaded || !r.Foreign || !r.KilledStates || f.Kills() != k+1 {
			t.Errorf("%s：应 Load + Kill，Foreign：%+v %v", name, r, err)
		}
	}
}

func TestReleaseInvalidToken(t *testing.T) {
	f := NewFakePF()
	c := &Controller{R: f}
	tok, err := c.Enable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Release(context.Background(), tok); err != nil {
		t.Fatal(err)
	}
	// 第二次释放：token 已失效。
	if err := c.Release(context.Background(), tok); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("应返回 ErrTokenInvalid：%v", err)
	}
	// 其他失败不带 ErrTokenInvalid。
	f.FailNext("release", errors.New("boom"))
	if err := c.Release(context.Background(), "77"); err == nil || errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("%v", err)
	}
	if len(f.Tokens()) != 0 || f.Active(DefaultAnchor) {
		t.Fatal("不应还有 token 或生效的锚点")
	}
}
