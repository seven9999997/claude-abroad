package exitcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"claude-abroad/internal/exitpolicy"
	"claude-abroad/internal/srccheck"
	"claude-abroad/internal/wire"
)

// a. 出口变化规则：四条规则 × {IP 相同、同城换 IP、同国换城市、换国家、大陆段、基准为空}。
func TestRuleTable(t *testing.T) {
	cases := []struct{ name, ip, loc string }{
		{"IP 相同", "203.0.113.7", "US"},
		{"同城换 IP", "203.0.113.8", "US"},
		{"同国换城市", "203.0.113.9", "US"},
		{"换国家", "198.51.100.5", "JP"},
		{"大陆段", "1.0.1.1", "CN"},
		{"基准为空", "203.0.113.7", "US"},
	}
	want := map[exitpolicy.Rule][]Result{
		exitpolicy.RuleFixedIP:     {Pass, Fail, Fail, Fail, Fail, Unknown},
		exitpolicy.RuleSameCity:    {Pass, Pass, Fail, Fail, Fail, Unknown},
		exitpolicy.RuleSameCountry: {Pass, Pass, Pass, Fail, Fail, Unknown},
		exitpolicy.RuleAny:         {Pass, Pass, Pass, Pass, Fail, Unknown},
	}
	for rule, results := range want {
		for i, c := range cases {
			w := newWorld(t)
			w.set(func(w *world) { w.exitIP, w.exitLoc = c.ip, c.loc })
			o := Options{Base: base(rule, "203.0.113.7")}
			if c.name == "基准为空" {
				o.Base = nil
			}
			rep := w.checker().Check(context.Background(), "main", o)
			it, _ := rep.Item("rule")
			if it.Result != results[i] || rep.Verdict != results[i] {
				t.Errorf("%s / %s：规则一项 %s（%s），整轮 %s，期望 %s", rule, c.name, it.Result, it.Detail, rep.Verdict, results[i])
			}
			if c.name == "大陆段" {
				if m, _ := rep.Item("mainland"); m.Result != Fail || m.Cause != CauseMainland {
					t.Errorf("%s：大陆段应不通过：%+v", rule, m)
				}
			}
			if (rule == exitpolicy.RuleSameCountry || rule == exitpolicy.RuleAny) && w.geoCount() != 0 {
				t.Errorf("%s / %s：同一国家、不限两条规则不应做位置查询（%d 次）", rule, c.name, w.geoCount())
			}
		}
	}
}

// b. 判定表：每一行。
func TestCauseTable(t *testing.T) {
	want := map[Cause]Result{
		CauseMainland: Fail, CauseRuleViolation: Fail, CauseUnexpectedReply: Fail,
		CauseTimeout: Unknown, CauseConnect: Unknown, CauseRateLimited: Unknown, CauseBadFormat: Unknown, CauseChannelDown: Unknown,
		CauseNoBaseline: Unknown, CauseOK: Pass, CauseNotNeeded: Pass, Cause("新原因"): Unknown,
	}
	for c, r := range want {
		if ResultOf(c) != r {
			t.Errorf("%s 应是 %s，实际 %s", c, r, ResultOf(c))
		}
	}
	errs := map[Cause]error{
		CauseTimeout:     context.DeadlineExceeded,
		CauseConnect:     &wire.ReplyError{Code: wire.CodeDialFailed},
		CauseChannelDown: fmt.Errorf("%w：x", errChannel),
		CauseRateLimited: fail(CauseRateLimited, "429"),
	}
	for c, e := range errs {
		if causeOf(e) != c {
			t.Errorf("%v 应归为 %s，实际 %s", e, c, causeOf(e))
		}
	}
}

// b. 性质测试：只注入网络类故障（超时、连接失败、通道不可用、限流、格式异常）的任意组合，结果永远不是“不通过”。
func TestNetworkFaultsNeverFail(t *testing.T) {
	n := 5000
	if testing.Short() {
		n = 300
	}
	const workers = 8
	conn := []string{"", "connect", "channel", "timeout"}
	src := []string{"", "connect", "channel", "timeout", "429", "html", "nofield"}
	udp := []string{"ok", "connect", "silent"}
	rules := []exitpolicy.Rule{exitpolicy.RuleFixedIP, exitpolicy.RuleSameCity, exitpolicy.RuleSameCountry, exitpolicy.RuleAny}
	for wk := 0; wk < workers; wk++ {
		t.Run(fmt.Sprintf("组%d", wk), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			c := w.checker()
			// 时限调小：这里只断言“网络故障永远不会判成不通过”，时限短只会让更多探测变成“无法判断”，不影响结论。
			c.Total, c.HTTPS, c.IP, c.Geo, c.UDP, c.DoH = time.Second, 60*time.Millisecond, 30*time.Millisecond, 30*time.Millisecond, 15*time.Millisecond, 60*time.Millisecond
			r := rand.New(rand.NewSource(int64(wk) + 1))
			pick := func(list []string) string {
				v := list[r.Intn(len(list))]
				if v == "timeout" && r.Intn(4) != 0 { // 超时要等时限：少一些，测试快
					v = "connect"
				}
				return v
			}
			for i := 0; i < n/workers; i++ {
				w.set(func(w *world) {
					w.mode = map[string]string{
						"www.gstatic.com":    pick(conn),
						"cloudflare-dns.com": pick(conn),
						"www.cloudflare.com": pick(src),
						"ipwho.is":           pick(src),
						"api.ip.sb":          pick(src),
					}
					w.udpMode = udp[r.Intn(len(udp))]
				})
				c.mu.Lock()
				c.cache = nil
				c.mu.Unlock()
				rep := c.Check(context.Background(), "main", Options{Base: base(rules[r.Intn(4)], "203.0.113.7"), WantGeo: r.Intn(2) == 0})
				for _, it := range rep.Items {
					if it.Result == Fail || rep.Verdict == Fail {
						t.Fatalf("第 %d 组：只有网络故障却判了不通过：%+v（故障 %v / udp %s）", i, it, w.mode, w.udpMode)
					}
				}
			}
		})
	}
}

// c. 查询源的解析：成功（会话里真实采样、脱敏后的样本）、字段缺失、网页。
func TestSourceParsers(t *testing.T) {
	read := func(n string) []byte {
		b, err := os.ReadFile(filepath.Join("..", "exitpolicy", "testdata", n))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	ip := mustAddr("203.0.113.7")
	if info, err := (cfTrace{}).Parse(read("cf-trace-ok.txt")); err != nil || info.IP != ip || info.Country != "US" {
		t.Fatalf("Cloudflare trace：%+v %v", info, err)
	}
	if g, err := (ipwhoGeo{}).Parse(read("ipwho-ok.json"), ip); err != nil || g.City != "Exampleville" || g.Timezone != "America/Los_Angeles" || g.ASN != 64500 {
		t.Fatalf("ipwho.is：%+v %v", g, err)
	}
	if g, err := (ipsbGeo{}).Parse(read("ipsb-ok.json"), ip); err != nil || g.Region != "Example State" || g.Country != "US" {
		t.Fatalf("ip.sb：%+v %v", g, err)
	}
	if info, err := (ipwhoSelf{}).Parse(read("ipwho-ok.json")); err != nil || info.IP != ip {
		t.Fatalf("ipwho.is 查自己：%+v %v", info, err)
	}
	// 按 IP 查时 IP 不符：格式不对。
	if _, err := (ipwhoGeo{}).Parse(read("ipwho-ok.json"), mustAddr("203.0.113.8")); err == nil {
		t.Fatal("IP 不符应报错")
	}
	// 网页、字段缺失。
	for name, p := range map[string]func([]byte) error{
		"cf":    func(b []byte) error { _, err := (cfTrace{}).Parse(b); return err },
		"ipwho": func(b []byte) error { _, err := (ipwhoGeo{}).Parse(b, ip); return err },
		"ipsb":  func(b []byte) error { _, err := (ipsbGeo{}).Parse(b, ip); return err },
	} {
		if p([]byte("<html>blocked</html>")) == nil {
			t.Errorf("%s：网页应报错", name)
		}
	}
	if _, err := (cfTrace{}).Parse([]byte("fl=1\nloc=US\n")); err == nil {
		t.Error("cf 缺 ip 应报错")
	}
	// 字段缺失只影响依赖它的项：Cloudflare 没给国家 → 去查位置补国家，结果仍通过。
	info, err := (cfTrace{}).Parse([]byte("ip=203.0.113.7\n"))
	if err != nil || info.Country != "" {
		t.Fatalf("缺 loc 时 IP 仍可用：%+v %v", info, err)
	}
	g, err := (ipwhoGeo{}).Parse([]byte(`{"success":true,"ip":"203.0.113.7","country_code":"US"}`), ip)
	if err != nil || g.City != "" || g.Country != "US" {
		t.Fatalf("缺城市时其余字段仍可用：%+v %v", g, err)
	}
}

// c. 查询源经 HTTP 的限流、超时、网页、字段缺失（每个源各一例）以及“只影响依赖该字段的检查项”。
func TestSourceFailuresViaHTTP(t *testing.T) {
	for _, host := range []string{"www.cloudflare.com", "ipwho.is", "api.ip.sb"} {
		for _, m := range []string{"429", "timeout", "html", "nofield"} {
			w := newWorld(t)
			w.set(func(w *world) { w.mode[host] = m })
			rep := w.checker().Check(context.Background(), "main", Options{Base: base(exitpolicy.RuleSameCity, "203.0.113.7"), WantGeo: true})
			if rep.Verdict == Fail {
				t.Errorf("%s %s：不应不通过：%v", host, m, rep)
			}
		}
	}
	// Cloudflare 没给国家：用 ipwho.is 的位置补国家，同一国家规则仍通过。
	w := newWorld(t)
	w.set(func(w *world) { w.exitLoc = "" })
	rep := w.checker().Check(context.Background(), "main", Options{Base: base(exitpolicy.RuleSameCountry, "203.0.113.7")})
	if rep.Verdict != Pass || rep.Country != "US" {
		t.Fatalf("缺国家时应查位置补上：%v", rep)
	}
	// 位置源缺城市（nofield）：同一城市规则无法判断，同一国家规则不受影响。
	w = newWorld(t)
	w.set(func(w *world) { w.mode["ipwho.is"], w.mode["api.ip.sb"] = "nofield", "429"; w.exitIP = "203.0.113.8" })
	rep = w.checker().Check(context.Background(), "main", Options{Base: base(exitpolicy.RuleSameCity, "203.0.113.7")})
	if it, _ := rep.Item("rule"); it.Result != Unknown {
		t.Fatalf("缺城市时同一城市规则应无法判断：%+v", it)
	}
	rep = w.checker().Check(context.Background(), "main", Options{Base: base(exitpolicy.RuleSameCountry, "203.0.113.7")})
	if rep.Verdict != Pass {
		t.Fatalf("同一国家规则不受位置源的影响：%v", rep)
	}
}

// d. 换源与缓存。
func TestFallbackAndCache(t *testing.T) {
	w := newWorld(t)
	w.set(func(w *world) { w.mode["www.cloudflare.com"] = "timeout" })
	rep := w.checker().Check(context.Background(), "main", Options{Base: base(exitpolicy.RuleSameCountry, "203.0.113.7")})
	if rep.Verdict != Pass || w.ipHits["ipwho-self"] != 1 {
		t.Fatalf("第一个源超时应换第二个：%v %v", rep, w.ipHits)
	}
	w.set(func(w *world) {
		w.mode = map[string]string{"www.cloudflare.com": "connect", "ipwho.is": "429", "api.ip.sb": "html"}
	})
	rep = w.checker().Check(context.Background(), "main", Options{Base: base(exitpolicy.RuleSameCountry, "203.0.113.7")})
	if it, _ := rep.Item("ip"); it.Result != Unknown || rep.Verdict != Unknown {
		t.Fatalf("全部失败应无法判断：%v", rep)
	}
	// 同一城市规则：出口 IP 不变时，100 轮里位置查询 0 次（基准里的位置当缓存）。
	w = newWorld(t)
	now := time.Unix(1_900_000_000, 0)
	c := w.checker()
	c.Now = func() time.Time { return now }
	b := base(exitpolicy.RuleSameCity, "203.0.113.7")
	b.At = now
	for i := 0; i < 100; i++ {
		if rep := c.Check(context.Background(), "main", Options{Base: b}); rep.Verdict != Pass {
			t.Fatalf("第 %d 轮：%v", i, rep)
		}
	}
	if w.geoCount() != 0 {
		t.Fatalf("IP 不变时不应查位置：%d", w.geoCount())
	}
	// IP 变了（同城）：只查一次，之后命中缓存。
	w.set(func(w *world) { w.exitIP = "203.0.113.8" })
	for i := 0; i < 20; i++ {
		c.Check(context.Background(), "main", Options{Base: b})
	}
	if w.geoCount() != 1 {
		t.Fatalf("IP 变化后应只查一次：%d", w.geoCount())
	}
	// 24 小时后过期，重新查。
	now = now.Add(GeoCacheTTL + time.Second)
	c.Check(context.Background(), "main", Options{Base: b})
	if w.geoCount() != 2 {
		t.Fatalf("缓存过期后应重新查：%d", w.geoCount())
	}
}

// e. HTTPS 探测以收到预期的应答为准。
func TestHTTPSProbe(t *testing.T) {
	cases := []struct {
		mode string
		want Result
	}{{"", Pass}, {"hang", Unknown}, {"200", Fail}, {"badcert", Fail}, {"connect", Unknown}, {"channel", Unknown}}
	for _, c := range cases {
		w := newWorld(t)
		w.set(func(w *world) { w.mode["www.gstatic.com"] = c.mode })
		ch := w.checker()
		ch.HTTPS = 300 * time.Millisecond
		rep := ch.Check(context.Background(), "main", Options{Base: base(exitpolicy.RuleAny, "203.0.113.7")})
		if it, _ := rep.Item("https"); it.Result != c.want {
			t.Errorf("HTTPS %q：%s（%s），期望 %s", c.mode, it.Result, it.Detail, c.want)
		}
	}
}

// f. UDP 探测：合法的 DNS 应答才算通过；只记录，不影响整轮。
func TestUDPProbe(t *testing.T) {
	for mode, want := range map[string]Result{"ok": Pass, "silent": Unknown, "badid": Fail, "badq": Fail, "connect": Unknown} {
		w := newWorld(t)
		w.set(func(w *world) { w.udpMode = mode })
		rep := w.checker().Check(context.Background(), "main", Options{Base: base(exitpolicy.RuleAny, "203.0.113.7")})
		it, _ := rep.Item("udp")
		if it.Result != want {
			t.Errorf("UDP %s：%s（%s），期望 %s", mode, it.Result, it.Detail, want)
		}
		if rep.Verdict != Pass {
			t.Errorf("UDP %s：主代理的 UDP 不通过只记录，不影响整轮：%v", mode, rep)
		}
	}
}

// g. 时限：所有源都卡住时整轮在时限内返回“无法判断”；ctx 取消时及时返回；没有泄漏 goroutine。
func TestTimeoutsAndNoLeak(t *testing.T) {
	w := newWorld(t)
	w.set(func(w *world) {
		for _, h := range testHosts {
			w.mode[h] = "timeout"
		}
		w.udpMode = "silent"
	})
	before := runtime.NumGoroutine()
	c := w.checker()
	c.Total, c.HTTPS, c.IP, c.Geo, c.UDP, c.DoH = 400*time.Millisecond, 5*time.Second, 5*time.Second, 5*time.Second, 5*time.Second, 5*time.Second
	start := time.Now()
	rep := c.Check(context.Background(), "main", Options{Base: base(exitpolicy.RuleSameCity, "203.0.113.7")})
	if el := time.Since(start); el > 2*time.Second || rep.Verdict != Unknown {
		t.Fatalf("总时限内应返回无法判断：%s %v", el, rep)
	}
	// 默认总时限不超过 20 秒。
	if TotalTimeout > 20*time.Second {
		t.Fatal("一轮总时限不能超过 20 秒")
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.Total = time.Minute
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start = time.Now()
	rep = c.Check(ctx, "main", Options{Base: base(exitpolicy.RuleAny, "203.0.113.7")})
	if el := time.Since(start); el > 2*time.Second || rep.Verdict == Pass {
		t.Fatalf("ctx 取消后应及时返回：%s %v", el, rep)
	}
	for i := 0; i < 50 && runtime.NumGoroutine() > before+2; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if g := runtime.NumGoroutine(); g > before+2 {
		t.Fatalf("goroutine 泄漏：%d → %d", before, g)
	}
}

// h. 基准文件。
func TestBaselineFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "baseline.json")
	if _, err := LoadBaseline(p); !errors.Is(err, ErrNoBaseline) {
		t.Fatalf("不存在时应是 ErrNoBaseline：%v", err)
	}
	w := newWorld(t)
	rep := w.checker().Check(context.Background(), "main", Options{WantGeo: true})
	b, err := BaselineFromReport(rep, exitpolicy.RuleSameCity)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveBaseline(p, b); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("权限应是 0600：%o", fi.Mode().Perm())
	}
	got, err := LoadBaseline(p)
	if err != nil || got.IP != b.IP || got.City != b.City || got.Rule != b.Rule || !got.At.Equal(b.At) {
		t.Fatalf("读回不等：%+v %v", got, err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".baseline-*")); len(left) != 0 {
		t.Fatalf("不应留下临时文件：%v", left)
	}
	bad := func(name string, prep func()) {
		t.Helper()
		prep()
		if _, err := LoadBaseline(p); !errors.Is(err, ErrNoBaseline) {
			t.Errorf("%s：应当作没有基准：%v", name, err)
		}
	}
	bad("损坏", func() { os.WriteFile(p, []byte("{bad"), 0o600) })
	bad("字段不合法", func() {
		os.WriteFile(p, []byte(`{"v":1,"rule":"same_country","ip":"10.0.0.1","country":"US","at":"2026-01-01T00:00:00Z"}`), 0o600)
	})
	bad("规则不认识", func() {
		os.WriteFile(p, []byte(`{"v":1,"rule":"x","ip":"203.0.113.7","country":"US","at":"2026-01-01T00:00:00Z"}`), 0o600)
	})
	bad("未知字段", func() {
		os.WriteFile(p, []byte(`{"v":1,"rule":"any","ip":"203.0.113.7","country":"US","at":"2026-01-01T00:00:00Z","x":1}`), 0o600)
	})
	bad("别人可读", func() { SaveBaseline(p, b); os.Chmod(p, 0o644) })
	bad("符号链接", func() {
		os.Remove(p)
		real := filepath.Join(dir, "real.json")
		SaveBaseline(real, b)
		os.Symlink(real, p)
	})
	// 大陆出口不能当基准。
	w.set(func(w *world) { w.exitIP, w.exitLoc = "1.0.1.1", "CN" })
	rep = w.checker().Check(context.Background(), "main", Options{WantGeo: true})
	if _, err := BaselineFromReport(rep, exitpolicy.RuleSameCountry); err == nil {
		t.Fatal("大陆出口不能当基准")
	}
}

// i. 不泄漏敏感字段：日志、报告的字符串和 JSON 形式里没有出口 IP、城市、运营商。
func TestNoSensitiveLeak(t *testing.T) {
	w := newWorld(t)
	var logs []string
	c := w.checker()
	c.Logf = func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }
	rep := c.Check(context.Background(), "main", Options{Base: base(exitpolicy.RuleSameCity, "203.0.113.7"), WantGeo: true})
	if rep.IP.String() != "203.0.113.7" || rep.Geo == nil || rep.Geo.City != "Exampleville" {
		t.Fatalf("内存里的报告应有完整信息：%+v", rep.Geo)
	}
	js, _ := json.Marshal(rep)
	all := strings.Join(append(logs, rep.String(), fmt.Sprintf("%v %+v %#v %s", rep, rep, rep, rep), string(js)), "\n")
	for _, s := range []string{"203.0.113.7", "Exampleville", "Example State", "Example Hosting", "64500"} {
		if strings.Contains(all, s) {
			t.Fatalf("日志或报告的文字形式里出现了敏感字段 %q：\n%s", s, all)
		}
	}
	if !strings.Contains(all, `"country":"US"`) || !strings.Contains(all, "America/Los_Angeles") {
		t.Fatalf("摘要里应有国家和时区：%s", all)
	}
	// 仓库里的样本文件只有文档用的地址。
	files, _ := filepath.Glob(filepath.Join("..", "exitpolicy", "testdata", "*"))
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, ip := range ipRe.FindAllString(string(b), -1) {
			if !strings.HasPrefix(ip, "203.0.113.") && !strings.HasPrefix(ip, "198.51.100.") && !strings.HasPrefix(ip, "192.0.2.") {
				t.Errorf("%s 里有不是文档用的地址 %s", f, ip)
			}
		}
	}
}

// j. 没有直连：所有网络访问都经开流接口；构造 HTTP 客户端只在 transport.go。
func TestNoDirectDial(t *testing.T) {
	srccheck.NoDial(t, ".", map[string][]string{"transport.go": {"http.Transport", "http.Client"}})
}
