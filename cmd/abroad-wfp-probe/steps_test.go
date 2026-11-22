package main

import (
	"bytes"
	"net/netip"
	"runtime"
	"strings"
	"testing"
	"time"

	"claude-abroad/internal/wfprules"
)

const (
	selfSID  = "S-1-5-21-111-222-333-500" // 内置 Administrator
	otherSID = "S-1-5-21-111-222-333-1002"
)

func plan() Plan {
	return Plan{Duration: DefaultDuration, AllowSID: otherSID, SelfSID: selfSID,
		LAN:     []netip.Prefix{netip.MustParsePrefix("192.168.7.0/24")},
		Gateway: netip.MustParseAddr("192.168.7.1"),
		Public4: netip.MustParseAddrPort("1.1.1.1:443"), PublicDNS: netip.MustParseAddrPort("1.1.1.1:53"),
		Public6: netip.MustParseAddrPort("[2606:4700:4700::1111]:443"),
		Keep:    netip.MustParseAddr("192.168.7.30"), KeepPort: 22}
}

func TestParseArgs(t *testing.T) {
	o, err := parseArgs(nil)
	if err != nil || o.Yes || o.Duration != DefaultDuration {
		t.Fatalf("默认：%+v %v", o, err)
	}
	if o, err := parseArgs([]string{"--rehearse", "--hang", "--yes", "--seconds", "20"}); err != nil || !o.Hang || !o.Rehearse {
		t.Fatalf("演练停住：%+v %v", o, err)
	}
	if o, err := parseArgs([]string{"--rehearse", "--yes"}); err != nil || !o.Rehearse || !o.Yes {
		t.Fatalf("演练：%+v %v", o, err)
	}
	o, err = parseArgs([]string{"--yes", "--seconds", "60", "--allow-sid", otherSID, "--keep-host", "192.168.7.30", "--out", "x.txt"})
	if err != nil || !o.Yes || o.Duration != 60*time.Second || o.AllowSID != otherSID || o.Keep.String() != "192.168.7.30" || o.Out != "x.txt" {
		t.Fatalf("%+v %v", o, err)
	}
	for _, bad := range [][]string{
		{"--seconds"}, {"--seconds", "10"}, {"--seconds", "301"}, {"--seconds", "abc"}, {"--yess"}, {"-y"},
		{"--keep-host", "fe80::1"}, {"--keep-host", "host"}, {"--child", "f", "--yes"}, {"--child", "f", "--allow-sid", otherSID}, {"--child", "f", "--rehearse"}, {"--hang"}, {"--hang", "--yes"}, {"--child", "f", "--hang"},
	} {
		if _, err := parseArgs(bad); err == nil {
			t.Errorf("%v：应报错", bad)
		}
	}
	var out, errb bytes.Buffer
	if code := realMain([]string{"--bogus"}, &out, &errb); code != 2 {
		t.Fatalf("坏参数：%d", code)
	}
	if runtime.GOOS != "windows" {
		if code := realMain(nil, &out, &errb); code != 3 || !strings.Contains(errb.String(), "只在 Windows 上使用") {
			t.Fatalf("非 Windows：%d %s", code, errb.String())
		}
	}
}

func TestPlanValidate(t *testing.T) {
	if err := plan().Validate(); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(*Plan){
		"时限太短":               func(p *Plan) { p.Duration = 5 * time.Second },
		"时限太长":               func(p *Plan) { p.Duration = 10 * time.Minute },
		"放行 SYSTEM":          func(p *Plan) { p.AllowSID = "S-1-5-18" },
		"放行内置 Administrator": func(p *Plan) { p.AllowSID = selfSID },
		"放行自己": func(p *Plan) {
			p.SelfSID = otherSID
		},
		"开发机不在局域网": func(p *Plan) { p.Keep = netip.MustParseAddr("10.9.9.9") },
		"开发机是公网":   func(p *Plan) { p.Keep = netip.MustParseAddr("8.8.8.8") },
	}
	for name, m := range bad {
		p := plan()
		m(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s：应拒绝", name)
		}
	}
	p := plan()
	p.AllowSID = ""
	if err := p.Validate(); err != nil {
		t.Fatalf("不给 --allow-sid 也可以：%v", err)
	}
	if cfg := p.Config(); len(cfg.AllowSIDs) != 1 || cfg.AllowSIDs[0] != PlaceholderSID || cfg.TunLUID != 0 {
		t.Fatalf("%+v", cfg)
	}
}

// 预测表：每一项的预测都是离线求值器按规则算出的。
func TestPredictions(t *testing.T) {
	p := plan()
	fs, err := wfprules.Render(p.Config())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Outcome{
		"loopback": Allowed, "public-tcp": Blocked, "public-udp": Blocked, "lan-tcp": Allowed, "ipv6-tcp": Blocked,
		"established": Unknown, "child-sid": Allowed, "rehearse-hit": Allowed, "wsl": Unknown, "enum": Allowed,
	}
	steps := p.Steps()
	if len(steps) != len(want) {
		t.Fatalf("步骤数 %d", len(steps))
	}
	for _, st := range steps {
		got, why := Predict(fs, st, false)
		if got != want[st.ID] || why == "" {
			t.Errorf("%s：预测 %s（%s），应为 %s", st.ID, got, why, want[st.ID])
		}
	}
	p.AllowSID = ""
	fs, _ = wfprules.Render(p.Config())
	for _, st := range p.Steps() {
		if st.ID == "child-sid" {
			if got, _ := Predict(fs, st, false); got != NotRun {
				t.Errorf("没给 --allow-sid 时子命令应是未执行：%s", got)
			}
		}
	}
	// 没有网关时局域网那一项无法判断。
	p = plan()
	p.Gateway = netip.Addr{}
	fs, _ = wfprules.Render(p.Config())
	for _, st := range p.Steps() {
		if st.ID == "lan-tcp" {
			if got, _ := Predict(fs, st, false); got != Unknown {
				t.Errorf("没有网关：%s", got)
			}
		}
	}
}

func TestKeepPrediction(t *testing.T) {
	p := plan()
	fs, _ := wfprules.Render(p.Config())
	if ok, why := KeepPrediction(fs, p, selfSID); !ok {
		t.Fatalf("局域网里的开发机应放行：%s", why)
	}
	// 局域网关闭（规则里没有这个网段）时预测会被拦。
	p2 := p
	p2.LAN = nil
	fs2, _ := wfprules.Render(p2.Config())
	if ok, _ := KeepPrediction(fs2, p, selfSID); ok {
		t.Fatal("开发机网段不在规则里时应预测会被拦")
	}
	p.Keep = netip.Addr{}
	if ok, _ := KeepPrediction(fs, p, selfSID); !ok {
		t.Fatal("不是远程执行时不限制")
	}
}

func TestVerdict(t *testing.T) {
	for _, c := range []struct {
		pred, act, want Outcome
	}{
		{Allowed, Allowed, Consistent}, {Blocked, Blocked, Consistent}, {Blocked, Allowed, Mismatch}, {Allowed, Blocked, Mismatch},
		{Unknown, Allowed, Unknown}, {Blocked, Unknown, Unknown}, {Allowed, NotRun, NotRun}, {NotRun, Allowed, NotRun},
	} {
		if got := Verdict(Result{Predicted: c.pred, Actual: c.act}); got != c.want {
			t.Errorf("%s/%s：%s，应为 %s", c.pred, c.act, got, c.want)
		}
	}
}

func TestReportFormat(t *testing.T) {
	r := Report{Started: time.Unix(0, 0), Finished: time.Unix(60, 0), Duration: DefaultDuration, Rules: "R\n",
		OthersBefore: "A\n", OthersAfter: "B\n",
		Results: []Result{
			{Step: Step{ID: "x", Title: "X"}, Predicted: Blocked, Why: "w", Actual: Allowed, Detail: "d"},
			{Step: Step{ID: "y", Title: "Y"}, Predicted: Allowed, Why: "w", Actual: Allowed, Detail: "d"},
		}}
	text := r.Format()
	if !strings.Contains(text, "[x] X：预测 拦截（w）；实际 放行（d）；不一致  ←←← 不一致") || !strings.Contains(text, "不一致 1 项") ||
		!strings.Contains(text, "没有残留") || !strings.Contains(text, "没有我们的对象") {
		t.Fatalf("%s", text)
	}
	r.LeftAfter = []string{"提供者 P"}
	if !strings.Contains(r.Format(), "提供者 P") {
		t.Fatal("残留要列出")
	}
}

// 输出脱敏：本机名、账户名、私有地址、SID 的机器部分、公网 IPv6 的后半段。
func TestRedact(t *testing.T) {
	in := "DESKTOP-ABC123\\administrator 在 desktop-abc123 上；网关 192.168.7.1，网段 10.20.30.0/24，100.64.1.2，169.254.3.4；" +
		"账户 S-1-5-21-1234567890-987654321-555555555-1002；目标 1.1.1.1:443；IPv6 2408:8207:1234:5678::9；回环 ::1；127.0.0.1"
	out := Redact(in, []string{"DESKTOP-ABC123", "Administrator"})
	for _, leak := range []string{"ABC123", "abc123", "administrator", "192.168.7.1", "10.20.30.0", "100.64.1.2", "169.254.3.4", "1234567890", "987654321", "1234:5678"} {
		if strings.Contains(out, leak) {
			t.Errorf("没脱敏：%s\n%s", leak, out)
		}
	}
	for _, keep := range []string{"192.168.x.x", "10.20.x.x", "S-1-5-21-x-x-x-1002", "1.1.1.1:443", "2408:8207:x:x", "::1", "127.0.0.1"} {
		if !strings.Contains(out, keep) {
			t.Errorf("不该去掉：%s\n%s", keep, out)
		}
	}
	if Redact("abc", []string{"", "a"}) != "abc" {
		t.Error("太短的词不替换")
	}
}

// a. 装哪一组过滤器只由 Plan.Filters 决定：演练模式只有那一条回环过滤器（带我们的提供者和子层），
// 不管计划里其他字段是什么都不可能得到正式的规则；正式模式与 wfprules.Render 完全相同。
func TestFilters(t *testing.T) {
	p := plan()
	formal, err := p.Filters()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := wfprules.Render(p.Config())
	if string(wfprules.Summary(formal)) != string(wfprules.Summary(want)) || len(formal) < 10 {
		t.Fatalf("正式模式应是 wfprules 生成的那组：%d 条", len(formal))
	}
	for _, mut := range []func(*Plan){
		func(*Plan) {},
		func(p *Plan) { p.AllowSID = "" },
		func(p *Plan) { p.LAN = nil; p.Keep = netip.Addr{} },
		func(p *Plan) { p.Gateway = netip.Addr{} },
	} {
		p := plan()
		p.Rehearse = true
		mut(&p)
		fs, err := p.Filters()
		if err != nil {
			t.Fatal(err)
		}
		if len(fs) != 1 {
			t.Fatalf("演练模式应只有 1 条，得到 %d 条", len(fs))
		}
		f := fs[0]
		if f.Provider != wfprules.ProviderKey || f.Sublayer != wfprules.SublayerKey || f.Action != wfprules.Block ||
			f.Layer != wfprules.LayerConnectV4 || f.Key.String() != RehearsalKey {
			t.Fatalf("演练过滤器：%+v", f)
		}
		conds := map[wfprules.Field]string{}
		for _, c := range f.Conds {
			conds[c.Field] = c.Value
		}
		if len(conds) != 3 || conds[wfprules.FieldLoopback] != "1" || conds[wfprules.FieldProtocol] != "tcp" || conds[wfprules.FieldRemotePort] != "1" {
			t.Fatalf("演练过滤器的条件只能是“回环 + TCP + 端口 1”：%+v", f.Conds)
		}
		for _, g := range formal {
			if g.Key == f.Key {
				t.Fatal("演练过滤器的键不能与正式规则重复")
			}
		}
	}
}

// b. 演练模式下各项的预测都是“照常”（只有 rehearse-hit 预测拦截，用来证明演练过滤器生效）；判定沿用 Verdict。
func TestRehearsalPredictions(t *testing.T) {
	p := plan()
	p.Rehearse = true
	fs, err := p.Filters()
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range p.Steps() {
		got, why := Predict(fs, st, true)
		want := Allowed
		if st.ID == "rehearse-hit" {
			want = Blocked
		}
		if got != want || why == "" {
			t.Errorf("%s：演练预测 %s（%s），应为 %s", st.ID, got, why, want)
		}
	}
	p.AllowSID = ""
	for _, st := range p.Steps() {
		if st.ID == "child-sid" {
			if got, _ := Predict(fs, st, true); got != NotRun {
				t.Errorf("没给 --allow-sid 时子命令应是未执行：%s", got)
			}
		}
	}
	if ok, why := KeepPrediction(fs, plan(), selfSID); !ok {
		t.Fatalf("演练时远程连接应放行：%s", why)
	}
	if !strings.Contains(p.ModeName(), "演练") || strings.Contains(plan().ModeName(), "演练") {
		t.Fatal("模式名")
	}
	if text := (Report{Mode: p.ModeName()}).Format(); !strings.HasPrefix(text, "# abroad-wfp-probe 演练") {
		t.Fatalf("报告标题：%s", text)
	}
}

func utf16le(s string, bom bool) []byte {
	var b []byte
	if bom {
		b = append(b, 0xFF, 0xFE)
	}
	for _, r := range s {
		b = append(b, byte(r), byte(r>>8))
	}
	return b
}

func TestDecodeWSLList(t *testing.T) {
	for _, c := range []struct {
		in   []byte
		want string
	}{
		{utf16le("Ubuntu\r\ndocker-desktop\r\n", true), "Ubuntu|docker-desktop"},
		{utf16le("Ubuntu-22.04\r\n\r\n", false), "Ubuntu-22.04"},
		{[]byte("Debian\n"), "Debian"},
		{nil, ""},
		{utf16le("\r\n", true), ""},
	} {
		if got := strings.Join(DecodeWSLList(c.in), "|"); got != c.want {
			t.Errorf("%q：%q，应为 %q", c.in, got, c.want)
		}
	}
}

// c. WSL 观察项的分类：没装、没有发行版、调用失败、通、不通。
func TestClassifyWSL(t *testing.T) {
	list := utf16le("Ubuntu\r\n", true)
	for _, c := range []struct {
		name string
		o    WSLObservation
		want Outcome
	}{
		{"没装 WSL", WSLObservation{EntryMissing: true}, NotRun},
		{"没有发行版", WSLObservation{ListFailed: true}, NotRun},
		{"列表为空", WSLObservation{ListOut: utf16le("\r\n", true)}, NotRun},
		{"调用超时", WSLObservation{ListOut: list, ProbeErr: "45 秒内没有结束"}, Unknown},
		{"没有标记", WSLObservation{ListOut: list, ProbeOut: "sh: bash: not found"}, Unknown},
		{"通", WSLObservation{ListOut: list, ProbeOut: WSLMarkerOK + "\n"}, Allowed},
		{"不通", WSLObservation{ListOut: list, ProbeOut: WSLMarkerFail + "\n"}, Blocked},
	} {
		got, why := ClassifyWSL(c.o)
		if got != c.want || why == "" {
			t.Errorf("%s：%s（%s），应为 %s", c.name, got, why, c.want)
		}
	}
	// 基线：装过滤器之前就不通（或调用失败）时，过滤器期间的通/不通都记“无法判断”；基线通时照常判。
	okBase := &WSLObservation{ProbeOut: WSLMarkerOK}
	failBase := &WSLObservation{ProbeOut: WSLMarkerFail}
	errBase := &WSLObservation{ProbeErr: "45 秒内没有结束"}
	for _, c := range []struct {
		name string
		o    WSLObservation
		want Outcome
	}{
		{"基线通、期间不通", WSLObservation{ListOut: list, ProbeOut: WSLMarkerFail, Baseline: okBase}, Blocked},
		{"基线通、期间通", WSLObservation{ListOut: list, ProbeOut: WSLMarkerOK, Baseline: okBase}, Allowed},
		{"基线不通、期间不通", WSLObservation{ListOut: list, ProbeOut: WSLMarkerFail, Baseline: failBase}, Unknown},
		{"基线不通、期间通", WSLObservation{ListOut: list, ProbeOut: WSLMarkerOK, Baseline: failBase}, Unknown},
		{"基线调用失败", WSLObservation{ListOut: list, ProbeOut: WSLMarkerFail, Baseline: errBase}, Unknown},
		{"基线有、但没装 WSL", WSLObservation{EntryMissing: true, Baseline: failBase}, NotRun},
	} {
		got, why := ClassifyWSL(c.o)
		if got != c.want || why == "" {
			t.Errorf("%s：%s（%s），应为 %s", c.name, got, why, c.want)
		}
	}
	s := WSLProbeScript("1.1.1.1", "443")
	if !strings.Contains(s, "/dev/tcp/1.1.1.1/443") || !strings.Contains(s, WSLMarkerOK) || !strings.Contains(s, WSLMarkerFail) {
		t.Fatal(s)
	}
}

// a. “停住”开关只在演练模式下允许；硬时限 = 时限 + 5 秒，退出码 6。
func TestHang(t *testing.T) {
	p := plan()
	p.Hang = true
	if err := p.Validate(); err == nil {
		t.Fatal("正式模式下“停住”应被拒")
	}
	p.Rehearse = true
	if err := p.Validate(); err != nil {
		t.Fatalf("演练模式下允许：%v", err)
	}
	p.Duration = MinDuration
	if p.HardDeadline() != 25*time.Second || ExitHardTimeout != 6 {
		t.Fatalf("硬时限 %v，退出码 %d", p.HardDeadline(), ExitHardTimeout)
	}
}
