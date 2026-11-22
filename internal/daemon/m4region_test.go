package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-abroad/internal/claudeproc"
	"claude-abroad/internal/exitcheck"
	"claude-abroad/internal/exitpolicy"
	"claude-abroad/internal/gatesm"
	"claude-abroad/internal/region"
	"claude-abroad/internal/sysnet"
)

// ---- M4-5：地区（m4-plan v2.4 验收 e、g、j）----

func putRegion(t *testing.T, d *Daemon, body string) (int, []byte) {
	t.Helper()
	return call(t, d, "PUT", "/v1/region", []byte(body))
}

func confirmOf(t *testing.T, b []byte) ConfirmNeeded {
	t.Helper()
	var cn ConfirmNeeded
	if json.Unmarshal(b, &cn) != nil || len(cn.Confirm) != 32 || cn.Region == nil {
		t.Fatalf("应带确认码和地区变化：%s", b)
	}
	return cn
}

func withBody(t *testing.T, method string, d *Daemon, path string, v any) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(v)
	return call(t, d, method, path, b)
}

func (e *env) locale() sysnet.MacLocale { return e.sys.Net.Snapshot().Locale }

func desired(e *env, t region.Target) sysnet.MacLocale {
	base := e.init.Locale
	base.User = "alice"
	d := region.Desired(base, t)
	d.User = ""
	return d
}

var tokyo = region.Target{Timezone: "Asia/Tokyo", Lang: "ja-JP", Locale: "ja_JP", Source: region.SourceManual}
var la = region.Target{Timezone: "America/Los_Angeles", Lang: "en-US", Locale: "en_US", Source: region.SourceManual}

// e. 启用：地区会变时不带确认码回 409（从 A 变为 B + 进程预览），没有改动、没有信号；带码执行，先结束 Claude 再改设置；
// 地区不变时一步完成、不结束任何进程。
func TestRegionEnableTwoStep(t *testing.T) {
	e, buf, o := confirmEnv(t)
	d := e.start(o)
	if code, b := putRegion(t, d, `{"mode":"manual","timezone":"Asia/Tokyo","lang":"ja-JP"}`); code != 200 {
		t.Fatalf("没有启用时切换地区只锁定：%d %s", code, b)
	}
	if !e.locale().Equal(e.init.Locale) || e.sys.VMRegion.Count() != 0 {
		t.Fatal("没有启用时不应改 Mac 和虚拟机")
	}
	addClaude(e, claudeproc.ExitOnTerm)
	code, b := call(t, d, "POST", "/v1/enable", nil)
	if code != http.StatusConflict {
		t.Fatalf("%d %s", code, b)
	}
	cn := confirmOf(t, b)
	if !strings.Contains(cn.Region.From, "Europe/Berlin") || !strings.Contains(cn.Region.To, "Asia/Tokyo") || len(cn.Preview.Processes) != 3 {
		t.Fatalf("%+v", cn)
	}
	if s := status(t, d); s.State != "RESTORED" || !e.sys.Net.Snapshot().Equal(e.init) || len(e.sys.Procs.SignalLog()) != 0 {
		t.Fatal("不带确认码时不应有任何改动")
	}
	if code, _ := withBody(t, "POST", d, "/v1/enable", ConfirmRequest{Confirm: strings.Repeat("a", 32)}); code != http.StatusForbidden {
		t.Fatalf("用错的码：%d", code)
	}
	var atKill sysnet.MacLocale
	e.sys.Procs.OnSignal = func(claudeproc.SignalRec) { atKill = e.locale() }
	if code, b := withBody(t, "POST", d, "/v1/enable", ConfirmRequest{Confirm: cn.Confirm}); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	if !atKill.Equal(e.init.Locale) {
		t.Fatal("结束 Claude 时还不应改设置")
	}
	if !e.locale().Equal(desired(e, tokyo)) || e.sys.VMRegion.Last.Timezone != "Asia/Tokyo" || e.sys.Procs.Alive(7200) {
		t.Fatalf("%+v", e.locale())
	}
	if s := status(t, d); s.Region == nil || s.Region.Target == nil || s.Region.Target.Timezone != "Asia/Tokyo" {
		t.Fatalf("status 应有目标地区：%+v", s.Region)
	}
	post(t, d, "/v1/restore", 200)
	if !e.sys.Net.Snapshot().Equal(e.init) {
		t.Fatalf("恢复后地区应逐项等于初始值：%+v", e.locale())
	}
	// j. 日志和事件里没有原值（原值只在 409 的应答里给确认框看）、没有确认码。
	var evs strings.Builder
	for _, r := range d.events.List() {
		evs.WriteString(r.Kind + " " + r.Detail + "\n")
	}
	all := buf.String() + evs.String()
	for _, x := range []string{cn.Confirm, "zh-Hans-CN", "zh-Hans_CN", "en-US\"", "Europe/Berlin"} {
		if strings.Contains(all, x) {
			t.Fatalf("日志或事件里出现了 %q", x)
		}
	}
	// 地区不变：目标就是此刻的设置 → 一步启用，不结束任何进程。
	if code, b := putRegion(t, d, `{"mode":"manual","timezone":"Europe/Berlin","lang":"zh-Hans-CN"}`); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	addClaude(e, claudeproc.ExitOnTerm)
	n := len(e.sys.Procs.SignalLog())
	if code, b := call(t, d, "POST", "/v1/enable", nil); code != 200 {
		t.Fatalf("地区不变时不需要确认：%d %s", code, b)
	}
	if len(e.sys.Procs.SignalLog()) != n {
		t.Fatal("地区不变时不应结束进程")
	}
}

// e. 切换地区（启用状态下）：会变时两步；结束不了时中止，地区、目标、网络都不变；改完补一轮复查。
func TestRegionSwitchWhileEnabled(t *testing.T) {
	e, _, o := confirmEnv(t)
	d := e.start(o)
	putRegion(t, d, `{"mode":"manual","timezone":"Asia/Tokyo","lang":"ja-JP"}`)
	post(t, d, "/v1/enable", 200)
	addClaude(e, claudeproc.Unkillable)
	code, b := putRegion(t, d, `{"mode":"manual","timezone":"America/Los_Angeles","lang":"en-US"}`)
	if code != http.StatusConflict {
		t.Fatalf("%d %s", code, b)
	}
	cn := confirmOf(t, b)
	snap := e.sys.Net.Snapshot()
	code, b = withBody(t, "PUT", d, "/v1/region", RegionRequest{Mode: "manual", Timezone: "America/Los_Angeles", Lang: "en-US", Confirm: cn.Confirm})
	if code == 200 || !strings.Contains(string(b), "结束不了") {
		t.Fatalf("结束不了应中止：%d %s", code, b)
	}
	if !e.sys.Net.Snapshot().Equal(snap) || status(t, d).Region.Target.Timezone != "Asia/Tokyo" || status(t, d).State != "ENABLED" {
		t.Fatal("中止时地区、目标、网络都不变")
	}
	// 确认码绑定参数：拿“东京 → 洛杉矶”的码去切到别的地方被拒。
	e.sys.Procs.Remove(7200)
	_, b = putRegion(t, d, `{"mode":"manual","timezone":"America/Los_Angeles","lang":"en-US"}`)
	cn = confirmOf(t, b)
	if code, _ := withBody(t, "PUT", d, "/v1/region", RegionRequest{Mode: "manual", Timezone: "Europe/Paris", Lang: "fr-FR", Confirm: cn.Confirm}); code != http.StatusForbidden {
		t.Fatalf("参数不符应被拒：%d", code)
	}
	_, b = putRegion(t, d, `{"mode":"manual","timezone":"America/Los_Angeles","lang":"en-US"}`)
	cn = confirmOf(t, b)
	rounds := d.re.Rounds()
	if code, b := withBody(t, "PUT", d, "/v1/region", RegionRequest{Mode: "manual", Timezone: "America/Los_Angeles", Lang: "en-US", Confirm: cn.Confirm}); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	if !e.locale().Equal(desired(e, la)) || status(t, d).Region.Target.Timezone != "America/Los_Angeles" {
		t.Fatalf("%+v", e.locale())
	}
	for i := 0; i < 300 && d.re.Rounds() == rounds; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if d.re.Rounds() == rounds {
		t.Fatal("切换地区后应补一轮复查")
	}
	if code, _ := putRegion(t, d, `{"mode":"manual","timezone":"Mars/Base","lang":"en-US"}`); code != http.StatusUnprocessableEntity {
		t.Fatal("不合法的时区应拒绝")
	}
	if code, _ := putRegion(t, d, `{"mode":"exit"}`); code != http.StatusUnprocessableEntity {
		t.Fatal("没有出口基准时不能回到跟随出口")
	}
	post(t, d, "/v1/restore", 200)
	if !e.sys.Net.Snapshot().Equal(e.init) {
		t.Fatal("恢复后应等于初始值")
	}
}

func geoReport(country, tz string) exitcheck.Report {
	r := passReport()
	r.Country, r.Timezone = country, tz
	r.Geo.Country, r.Geo.Timezone = country, tz
	return r
}

// e. 应用主代理：首次（未启用）只保存基准、锁定按出口确定的目标；启用后出口换到别处再应用 → 两步，
// 第一次调用什么都不保存；确认后保存基准、同步地区。
func TestApplyMainProxyRegion(t *testing.T) {
	e, _, o := confirmEnv(t)
	pr := &fakeProber{rep: geoReport("US", "America/Los_Angeles")}
	sys := e.sys.System()
	sys.Baseline = NewBaselineService(pr, e.dir)
	o.Sys = sys
	d := e.start(o)
	code, b := call(t, d, "POST", "/v1/main-proxy/apply", nil)
	var res MainProxyResult
	if code != 200 || json.Unmarshal(b, &res) != nil || res.Region == nil || res.Region.Timezone != "America/Los_Angeles" || res.Region.Source != region.SourceExit || res.Synced {
		t.Fatalf("%d %s", code, b)
	}
	if !e.locale().Equal(e.init.Locale) {
		t.Fatal("没有启用时不应改地区")
	}
	post(t, d, "/v1/enable", 200)
	if !e.locale().Equal(desired(e, region.Target{Timezone: "America/Los_Angeles", Lang: "en-US", Locale: "en_US"})) {
		t.Fatalf("%+v", e.locale())
	}
	baseBefore, _ := os.ReadFile(filepath.Join(e.dir, BaselineFile))
	pr.mu.Lock()
	pr.rep = geoReport("JP", "Asia/Tokyo")
	pr.mu.Unlock()
	code, b = call(t, d, "POST", "/v1/main-proxy/apply", nil)
	if code != http.StatusConflict {
		t.Fatalf("%d %s", code, b)
	}
	cn := confirmOf(t, b)
	if after, _ := os.ReadFile(filepath.Join(e.dir, BaselineFile)); string(after) != string(baseBefore) {
		t.Fatal("第一次调用不应保存基准")
	}
	if code, b := withBody(t, "POST", d, "/v1/main-proxy/apply", MainProxyRequest{Confirm: cn.Confirm}); code != 200 || !strings.Contains(string(b), `"synced":true`) {
		t.Fatalf("%d %s", code, b)
	}
	jp := region.Target{Timezone: "Asia/Tokyo", Lang: "ja-JP", Locale: "ja_JP"}
	if !e.locale().Equal(desired(e, jp)) || status(t, d).Region.Target.Country != "JP" {
		t.Fatalf("%+v", e.locale())
	}
	// /v1/baseline 是别名。
	if code, _ := call(t, d, "POST", "/v1/baseline?rule=same_country", nil); code != 200 {
		t.Fatalf("别名：%d", code)
	}
	// 手动指定后再应用主代理：目标保持手动的。
	putRegion(t, d, `{"mode":"manual","timezone":"Asia/Tokyo","lang":"en-US"}`) // 会变：409，没带码，不改
	post(t, d, "/v1/restore", 200)
	if code, _ := putRegion(t, d, `{"mode":"manual","timezone":"Europe/Paris","lang":"fr-FR"}`); code != 200 {
		t.Fatal("回国状态下切换地区只锁定")
	}
	_, b = call(t, d, "POST", "/v1/main-proxy/apply", nil)
	if json.Unmarshal(b, &res) != nil || res.Region == nil || res.Region.Timezone != "Europe/Paris" || res.Region.Source != region.SourceManual {
		t.Fatalf("手动的目标应保持：%s", b)
	}
	if !e.sys.Net.Snapshot().Equal(e.init) {
		t.Fatal("回国后应等于初始值")
	}
}

// geoExit 是能给出国家和时区的出口检查（复查用）。
type geoExit struct {
	FakeErr
	country, tz string
}

func (g *geoExit) CheckResultGeo(context.Context) (gatesm.Result, string, string, string) {
	return gatesm.Pass, "", g.country, g.tz
}

// g. 出口漂移不改地区：复查得到同国不同时区、或不同国家，Mac 的命令记录没有新增，目标不变，有事件，闸门不关。
func TestRegionDriftOnlyEvents(t *testing.T) {
	e, _, o := confirmEnv(t)
	ex := &geoExit{country: "JP", tz: "Asia/Tokyo"}
	sys := e.sys.System()
	sys.Exit = ex
	o.Sys = sys
	d := e.start(o)
	putRegion(t, d, `{"mode":"manual","timezone":"Asia/Tokyo","lang":"ja-JP"}`)
	post(t, d, "/v1/enable", 200)
	waitGateOpen(t, d)
	// 手动目标没有国家：只比时区。换成跟随出口的目标来测国家。
	t.Run("同国不同时区", func(t *testing.T) {
		calls := len(e.sys.Net.Calls())
		snap := e.locale()
		ex.tz = "Asia/Osaka_Not_Real"
		ex.tz = "Asia/Seoul"
		d.re.RunOnce(context.Background())
		if !hasEvent(d, "region", "region_drift") {
			t.Fatal("应有漂移事件")
		}
		if !e.locale().Equal(snap) || status(t, d).Region.Target.Timezone != "Asia/Tokyo" || !status(t, d).GateOpen {
			t.Fatal("漂移不应改地区、不改目标、不关闸门")
		}
		for _, c := range e.sys.Net.Calls()[calls:] {
			if strings.HasPrefix(c, "locale.") && c != sysnet.OpLocaleRead {
				t.Fatalf("Mac 的命令记录不应新增：%s", c)
			}
		}
	})
	t.Run("不同国家", func(t *testing.T) {
		if err := d.regions.Lock(region.Target{Timezone: "Asia/Tokyo", Lang: "ja-JP", Locale: "ja_JP", Country: "JP", Source: region.SourceExit}); err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, r := range d.events.List() {
			if r.Kind == "region_drift" {
				n++
			}
		}
		ex.country, ex.tz = "KR", "Asia/Tokyo"
		d.re.RunOnce(context.Background())
		m := 0
		for _, r := range d.events.List() {
			if r.Kind == "region_drift" {
				m++
			}
			if strings.Contains(r.Detail, "KR") || strings.Contains(r.Detail, "Seoul") {
				t.Fatalf("事件里不写具体的国家和时区：%s", r.Detail)
			}
		}
		if m != n+1 || !status(t, d).GateOpen {
			t.Fatalf("国家不同：一条新事件、闸门不关：%d → %d", n, m)
		}
		d.re.RunOnce(context.Background()) // 同样的漂移不重复发
		k := 0
		for _, r := range d.events.List() {
			if r.Kind == "region_drift" {
				k++
			}
		}
		if k != m {
			t.Fatal("同样的漂移不应重复发事件")
		}
	})
	post(t, d, "/v1/restore", 200)
}

var (
	_ = netip.Addr{}
	_ = exitpolicy.RuleAny
)

// a. 锁定之后只有三个操作能改变目标：源码里写 region.json 的只有 Regions.Lock，调用它的只有
// Regions.Target（首次启用）和 applyTarget（应用主代理、切换地区）；停用、恢复、复查、监护、再启用都不改它。
func TestRegionLockOnlyByThreeOps(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	callers := map[string]int{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, ".Lock(t)") || strings.Contains(line, "regions.Lock(") {
				callers[f]++
			}
		}
	}
	if callers["region_api.go"] != 2 || len(callers) != 1 {
		t.Fatalf("锁定目标的调用处：%v（应只有 Regions.Target 和 applyTarget）", callers)
	}
	e, _, o := confirmEnv(t)
	o.SuperviseInterval = 20 * time.Millisecond
	d := e.start(o)
	putRegion(t, d, `{"mode":"manual","timezone":"Asia/Tokyo","lang":"ja-JP"}`)
	p := filepath.Join(e.dir, RegionFileName)
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	post(t, d, "/v1/enable", 200)
	d.re.RunOnce(context.Background())
	time.Sleep(60 * time.Millisecond) // 监护跑几轮
	post(t, d, "/v1/disable", 200)
	post(t, d, "/v1/enable", 200)
	post(t, d, "/v1/restore", 200)
	post(t, d, "/v1/enable", 200)
	post(t, d, "/v1/restore", 200)
	if after, _ := os.ReadFile(p); string(after) != string(before) {
		t.Fatalf("目标被改了：%s → %s", before, after)
	}
}

// v2.5 必须改 1（daemon 一侧）：恢复时时区写回失败 → 恢复照样成功、网络干净，status 有“地区待还原”（只有项目名）；
// 重试间隔不小于 5 分钟（时钟注入）；持续失败不影响其他功能；成功后文件删除并有事件。
func TestRegionPendingRetry(t *testing.T) {
	clk := &stepClock{now: time.Now()}
	e, buf, o := confirmEnv(t, func(o *Options) { o.Clock = clk })
	d := e.start(o)
	putRegion(t, d, `{"mode":"manual","timezone":"Asia/Tokyo","lang":"ja-JP"}`)
	post(t, d, "/v1/enable", 200)
	e.sys.Net.FailNext(sysnet.OpLocaleTZ, errBoomD)
	b := post(t, d, "/v1/restore", 200)
	var st Status
	if json.Unmarshal(b, &st) != nil || st.State != "RESTORED" || st.Region == nil || len(st.Region.Pending) != 1 || st.Region.Pending[0] != "timezone" ||
		!strings.Contains(st.Region.Notice, "网络已恢复") {
		t.Fatalf("恢复应成功并带提示：%s", b)
	}
	snap := e.sys.Net.Snapshot()
	snap.Locale = e.init.Locale
	if !snap.Equal(e.init) || e.locale().Timezone != "Asia/Tokyo" {
		t.Fatal("网络应还原干净、时区还没还原")
	}
	tzWrites := func() int {
		n := 0
		for _, c := range e.sys.Net.Calls() {
			if strings.HasPrefix(c, sysnet.OpLocaleTZ) {
				n++
			}
		}
		return n
	}
	n0 := tzWrites()
	clk.Advance(RegionRetryInterval - time.Second)
	time.Sleep(30 * time.Millisecond)
	if tzWrites() != n0 {
		t.Fatal("不到 5 分钟不应重试")
	}
	e.sys.Net.FailNext(sysnet.OpLocaleTZ, errBoomD) // 下一次重试也失败
	clk.Advance(time.Second)
	waitFor(t, func() bool { return tzWrites() == n0+1 })
	if s := status(t, d); s.Region == nil || len(s.Region.Pending) != 1 {
		t.Fatal("持续失败时文件保留")
	}
	post(t, d, "/v1/enable", 200) // 其他功能不受影响（原值以待还原文件为准）
	post(t, d, "/v1/restore", 200)
	if !e.sys.Net.Snapshot().Equal(e.init) {
		t.Fatalf("再启用、恢复后应回到初始值：%+v", e.locale())
	}
	if s := status(t, d); s.Region != nil && len(s.Region.Pending) != 0 {
		t.Fatal("全部还原后不应再有待还原")
	}
	// 再造一次待还原，让重试来清。
	post(t, d, "/v1/enable", 200)
	e.sys.Net.FailNext(sysnet.OpLocaleTZ, errBoomD)
	post(t, d, "/v1/restore", 200)
	clk.Advance(RegionRetryInterval)
	waitFor(t, func() bool { return hasEvent(d, "state", "region_pending_cleared") }) // 先删文件、再发事件
	if s := status(t, d); s.Region == nil || len(s.Region.Pending) != 0 || !e.sys.Net.Snapshot().Equal(e.init) {
		t.Fatal("重试成功后应删除文件、等于初始值")
	}
	if fi, err := os.Stat(filepath.Join(e.dir, "region-pending.json")); err == nil {
		t.Fatalf("文件应已删除：%v", fi.Mode())
	}
	if strings.Contains(buf.String(), "Europe/Berlin") {
		t.Fatal("日志里不应有原值")
	}
}

var errBoomD = errors.New("命令失败")

func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("等待超时")
}

// v2.5 建议改 3（验收 e）：不接地区同步（--dev-skip-region 时虚拟机一侧为 nil）：应用主代理、启用、恢复都不读也不写
// Mac 的时区和语言，命令记录里没有这些命令；应用主代理不需要确认码。
func TestRegionSkipNoCommands(t *testing.T) {
	e := newEnv(t)
	k := &realKit{log: &cmdLog{}}
	p := e.realParams(k)
	p.VMRegion = nil
	p.Baseline = NewBaselineService(&fakeProber{rep: geoReport("JP", "Asia/Tokyo")}, e.dir)
	sys, err := NewRealSystem(p)
	if err != nil {
		t.Fatal(err)
	}
	if sys.Locale != nil {
		t.Fatal("不接虚拟机一侧时，Mac 一侧也不接")
	}
	e.system = &sys
	d := e.start(e.opts())
	if code, b := call(t, d, "POST", "/v1/main-proxy/apply", nil); code != 200 || strings.Contains(string(b), `"region"`) {
		t.Fatalf("%d %s", code, b)
	}
	if code, b := call(t, d, "POST", "/v1/enable", nil); code != 200 {
		t.Fatalf("不需要确认码：%d %s", code, b)
	}
	if code, _ := putRegion(t, d, `{"mode":"manual","timezone":"Asia/Tokyo","lang":"ja-JP"}`); code != http.StatusNotImplemented {
		t.Fatal("没有地区同步时切换地区应回 501")
	}
	post(t, d, "/v1/restore", 200)
	for _, c := range k.log.all() {
		for _, x := range []string{sysnet.PathSystemsetup, sysnet.PathDefaults, sysnet.PathLaunchctl, sysnet.PathSudo} {
			if strings.HasPrefix(c, x) {
				t.Fatalf("不应有地区命令：%s", c)
			}
		}
	}
	if !e.sys.Net.Snapshot().Equal(e.init) || status(t, d).Region != nil {
		t.Fatal("系统应还原干净，status 没有地区")
	}
}

// m4-plan v2.14 验收 l：手动地区的可选地区格式。给了字段（原样使用）、没给（按语言推导）、非法值（拒绝、不改动）、
// 与当前值相同（不会变，一步完成）；地区格式进入确认码绑定的参数摘要，换了值旧确认码失效。
func TestRegionManualLocale(t *testing.T) {
	e, _, o := confirmEnv(t)
	d := e.start(o)
	// 非法值：拒绝，什么都没改。
	snap := e.sys.Net.Snapshot()
	if code, b := putRegion(t, d, `{"mode":"manual","timezone":"Asia/Tokyo","lang":"zh-Hans-CN","locale":"zh CN"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("非法地区格式应拒绝：%d %s", code, b)
	}
	if code, _ := putRegion(t, d, `{"mode":"exit","locale":"zh_CN"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("跟随出口时不能带地区格式：%d", code)
	}
	if !e.sys.Net.Snapshot().Equal(snap) {
		t.Fatal("被拒时不能有改动")
	}
	// 给了字段：锁定的目标用给的值。
	if code, b := putRegion(t, d, `{"mode":"manual","timezone":"Asia/Tokyo","lang":"zh-Hans-CN","locale":"zh_CN"}`); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	if st := status(t, d); st.Region.Target.Locale != "zh_CN" || st.Region.Target.Lang != "zh-Hans-CN" {
		t.Fatalf("应原样用给的地区格式：%+v", st.Region.Target)
	}
	post(t, d, "/v1/enable", 200)
	want := region.Target{Timezone: "Asia/Tokyo", Lang: "zh-Hans-CN", Locale: "zh_CN", Source: region.SourceManual}
	if !e.locale().Equal(desired(e, want)) {
		t.Fatalf("Mac 应按给的地区格式写：%+v", e.locale())
	}
	// 与当前值相同：不会变，启用状态下也不要确认码。
	if code, b := putRegion(t, d, `{"mode":"manual","timezone":"Asia/Tokyo","lang":"zh-Hans-CN","locale":"zh_CN"}`); code != 200 {
		t.Fatalf("与当前相同应一步完成：%d %s", code, b)
	}
	// 没给字段：按语言推导成 zh-Hans_CN，和当前的 zh_CN 不同 → 地区会变，要确认。
	code, b := putRegion(t, d, `{"mode":"manual","timezone":"Asia/Tokyo","lang":"zh-Hans-CN"}`)
	if code != http.StatusConflict {
		t.Fatalf("不给字段时按推导，会变应要确认：%d %s", code, b)
	}
	cn := confirmOf(t, b)
	// 确认码绑定地区格式：拿“推导值”的码去提交另一个地区格式被拒。
	if code, _ := withBody(t, "PUT", d, "/v1/region", RegionRequest{Mode: "manual", Timezone: "Asia/Tokyo", Lang: "zh-Hans-CN", Locale: "zh_TW", Confirm: cn.Confirm}); code != http.StatusForbidden {
		t.Fatalf("换了地区格式，旧确认码应失效：%d", code)
	}
	_, b = putRegion(t, d, `{"mode":"manual","timezone":"Asia/Tokyo","lang":"zh-Hans-CN"}`)
	cn = confirmOf(t, b)
	if code, b := withBody(t, "PUT", d, "/v1/region", RegionRequest{Mode: "manual", Timezone: "Asia/Tokyo", Lang: "zh-Hans-CN", Confirm: cn.Confirm}); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	if st := status(t, d); st.Region.Target.Locale != "zh-Hans_CN" {
		t.Fatalf("不给字段时应按语言推导：%+v", st.Region.Target)
	}
}
