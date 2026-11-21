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
	o, err = parseArgs([]string{"--yes", "--seconds", "60", "--allow-sid", otherSID, "--keep-host", "192.168.7.30", "--out", "x.txt"})
	if err != nil || !o.Yes || o.Duration != 60*time.Second || o.AllowSID != otherSID || o.Keep.String() != "192.168.7.30" || o.Out != "x.txt" {
		t.Fatalf("%+v %v", o, err)
	}
	for _, bad := range [][]string{
		{"--seconds"}, {"--seconds", "10"}, {"--seconds", "301"}, {"--seconds", "abc"}, {"--yess"}, {"-y"},
		{"--keep-host", "fe80::1"}, {"--keep-host", "host"}, {"--child", "f", "--yes"}, {"--child", "f", "--allow-sid", otherSID},
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
		"established": Unknown, "child-sid": Allowed, "enum": Allowed,
	}
	steps := p.Steps()
	if len(steps) != len(want) {
		t.Fatalf("步骤数 %d", len(steps))
	}
	for _, st := range steps {
		got, why := Predict(fs, st)
		if got != want[st.ID] || why == "" {
			t.Errorf("%s：预测 %s（%s），应为 %s", st.ID, got, why, want[st.ID])
		}
	}
	p.AllowSID = ""
	fs, _ = wfprules.Render(p.Config())
	for _, st := range p.Steps() {
		if st.ID == "child-sid" {
			if got, _ := Predict(fs, st); got != NotRun {
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
			if got, _ := Predict(fs, st); got != Unknown {
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
