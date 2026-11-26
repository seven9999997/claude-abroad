package main

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"claude-abroad/internal/apiclient"
	"claude-abroad/internal/rules"
)

// 别处（另一个客户端）整份替换规则，不带条件头。
func (r *rig) putRulesRaw(yaml string) {
	r.t.Helper()
	cl := &apiclient.Client{Socket: r.d.SocketPath(), Timeout: 10 * time.Second}
	resp, err := cl.Do(context.Background(), "PUT", "/v1/rules", []byte(yaml))
	if err != nil || resp.Code != 200 {
		r.t.Fatalf("别处改规则：%v %d %s", err, resp.Code, resp.Body)
	}
}

func (r *rig) rules() *rules.Config {
	r.t.Helper()
	cl := &apiclient.Client{Socket: r.d.SocketPath(), Timeout: 10 * time.Second}
	resp, err := cl.Do(context.Background(), "GET", "/v1/rules", nil)
	if err != nil {
		r.t.Fatal(err)
	}
	c, err := rules.Parse(resp.Body)
	if err != nil {
		r.t.Fatal(err)
	}
	return c
}

// 带其他字段的规则：改一项时别的字段原样保留。
const otherFields = "default: proxy:main\nrules:\n  - {app: com.example.x, action: block}\nlocal_domains: [\"*.corp.example\"]\npinned_extra: [\"keep.example\"]\noffline_allow: [com.example.off]\nblock_udp_443: false\n"

// 补齐一验收 a：四项的查看、添加、删除；只改这一处；重复添加、删除不存在的项不发提交。
func TestCLIRuleLists(t *testing.T) {
	r := newRig(t, rigOpt{})
	r.asOwner()
	r.putRulesRaw(otherFields)
	var puts int
	r.trace = func(m, p string, h http.Header) {
		if m == "PUT" && strings.HasPrefix(p, "/v1/rules") {
			puts++
		}
	}
	base := r.rules()
	cases := []struct {
		kind, item string
		get        func(c *rules.Config) []string
		set        func(c *rules.Config)
	}{
		{"local", "intra.example", func(c *rules.Config) []string { return c.LocalDomains }, func(c *rules.Config) { c.LocalDomains = append(c.LocalDomains, "intra.example") }},
		{"pinned", "*.mine.example", func(c *rules.Config) []string { return c.PinnedExtra }, func(c *rules.Config) { c.PinnedExtra = append(c.PinnedExtra, "*.mine.example") }},
		{"offline", "com.example.nas", func(c *rules.Config) []string { return ruleLists["offline"].get(c) }, func(c *rules.Config) { c.OfflineAllow = append(c.OfflineAllow, "com.example.nas") }},
	}
	for _, c := range cases {
		x := r.run("rules", c.kind, "add", c.item)
		if x.code != ExitOK || !strings.Contains(x.out, "已加进") {
			t.Fatalf("%s add：%+v", c.kind, x)
		}
		want := r.rules()
		exp := *base
		exp.Rules = slices.Clone(base.Rules)
		exp.LocalDomains, exp.PinnedExtra, exp.OfflineAllow = slices.Clone(base.LocalDomains), slices.Clone(base.PinnedExtra), slices.Clone(base.OfflineAllow)
		c.set(&exp)
		wb, _ := want.Marshal()
		eb, _ := exp.Marshal()
		if string(wb) != string(eb) {
			t.Fatalf("%s add 应只改这一处：\n%s\n应为：\n%s", c.kind, wb, eb)
		}
		// 查看
		if x := r.run("rules", c.kind, "ls"); x.code != ExitOK || !strings.Contains(x.out, c.item) {
			t.Fatalf("%s ls：%+v", c.kind, x)
		}
		// 重复添加：不提交
		n := puts
		if x := r.run("rules", c.kind, "add", strings.ToUpper(c.item[:1])+c.item[1:]); x.code != ExitOK || !strings.Contains(x.out, "没有改动") || puts != n {
			t.Fatalf("%s 重复添加不应提交：%+v（提交 %d→%d）", c.kind, x, n, puts)
		}
		// 删除
		if x := r.run("rules", c.kind, "rm", c.item); x.code != ExitOK || !strings.Contains(x.out, "已从") {
			t.Fatalf("%s rm：%+v", c.kind, x)
		}
		if slices.Contains(c.get(r.rules()), c.item) {
			t.Fatalf("%s rm 后还在", c.kind)
		}
		gb, _ := r.rules().Marshal()
		bb, _ := base.Marshal()
		if string(gb) != string(bb) {
			t.Fatalf("%s 加了再删应回到原样：\n%s", c.kind, gb)
		}
		// 删除不存在的项：不提交
		n = puts
		if x := r.run("rules", c.kind, "rm", c.item); x.code != ExitOK || !strings.Contains(x.out, "没有改动") || puts != n {
			t.Fatalf("%s 删除不存在的项不应提交：%+v", c.kind, x)
		}
	}
	// UDP 443：开、再开（不提交）、查看、关
	if x := r.run("rules", "udp443", "on"); x.code != ExitOK || !r.rules().BlockUDP443 {
		t.Fatalf("udp443 on：%+v", x)
	}
	n := puts
	if x := r.run("rules", "udp443", "on"); x.code != ExitOK || puts != n {
		t.Fatalf("udp443 本来就开不应提交：%+v", x)
	}
	if x := r.run("rules", "udp443", "show"); !strings.Contains(x.out, "开") {
		t.Fatalf("udp443 show：%+v", x)
	}
	if x := r.run("rules", "udp443", "off"); x.code != ExitOK || r.rules().BlockUDP443 {
		t.Fatalf("udp443 off：%+v", x)
	}
	// 查看置顶域名：内置和追加的分开
	if x := r.run("rules", "pinned", "ls"); !strings.Contains(x.out, "内置") || !strings.Contains(x.out, "claude.ai") || !strings.Contains(x.out, "keep.example") {
		t.Fatalf("pinned ls：%+v", x)
	}
}

// 验收 b：读和写之间别处改了规则：重读重做，保留别人的改动；连续 3 次冲突时报错，规则里没有半截改动。
func TestCLIRulesConflictRetry(t *testing.T) {
	r := newRig(t, rigOpt{})
	r.asOwner()
	r.putRulesRaw(otherFields)
	other := strings.Replace(otherFields, "com.example.x", "com.example.y", 1)
	once := true
	r.trace = func(m, p string, h http.Header) {
		if m == "PUT" && strings.HasPrefix(p, "/v1/rules") && once {
			once = false
			r.putRulesRaw(other) // 别处在我们读之后、写之前改了
		}
	}
	if x := r.run("rules", "local", "add", "intra.example"); x.code != ExitOK {
		t.Fatalf("应重读重做后成功：%+v", x)
	}
	c := r.rules()
	if !slices.Contains(c.LocalDomains, "intra.example") || c.Rules[0].App != "com.example.y" {
		t.Fatalf("应保留别人的改动并加上自己的：%+v", c)
	}
	// 一直冲突：3 次后放弃
	r.putRulesRaw(otherFields)
	flip := false
	tries := 0
	r.trace = func(m, p string, h http.Header) {
		if m == "PUT" && strings.HasPrefix(p, "/v1/rules") {
			tries++
			flip = !flip
			if flip {
				r.putRulesRaw(other)
			} else {
				r.putRulesRaw(otherFields)
			}
		}
	}
	x := r.run("rules", "local", "add", "never.example")
	if x.code != ExitBusy || tries != RulesRetries || !strings.Contains(x.err, "没有改动") {
		t.Fatalf("连续冲突应在 %d 次后放弃：%+v（试了 %d 次）", RulesRetries, x, tries)
	}
	if slices.Contains(r.rules().LocalDomains, "never.example") {
		t.Fatal("放弃后规则里不应有它的改动")
	}
}

// 验收 c、d：放松不带 sudo 被拒（提示这一次需要 sudo 和后台的原因），带 sudo 通过；收紧不带 sudo 直接通过。后台拒绝的
// 三种原样显示原因；删除内置置顶域名由命令行按状态里的名单拦下，不发提交。
func TestCLIRuleListsAuthAndRejects(t *testing.T) {
	r := newRig(t, rigOpt{})
	r.asOwner()
	r.putRulesRaw(otherFields)
	var puts int
	r.trace = func(m, p string, h http.Header) {
		if m == "PUT" && strings.HasPrefix(p, "/v1/rules") {
			puts++
		}
	}
	r.asConsole()
	if x := r.run("rules", "local", "add", "intra.example"); x.code != ExitDenied || !strings.Contains(x.err, "这一次需要 sudo") {
		t.Fatalf("放松不带 sudo 应被拒：%+v", x)
	}
	if x := r.run("rules", "udp443", "on"); x.code != ExitOK {
		t.Fatalf("收紧不带 sudo 应通过：%+v", x)
	}
	if x := r.run("rules", "pinned", "add", "more.example"); x.code != ExitOK {
		t.Fatalf("追加置顶域名是收紧，应通过：%+v", x)
	}
	r.asOwner()
	if x := r.run("rules", "local", "add", "intra.example"); x.code != ExitOK {
		t.Fatalf("带 sudo 应通过：%+v", x)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"rules", "local", "add", "api.anthropic.com"}, "Claude 相关的域名不能走本机网络"},
		{[]string{"rules", "offline", "add", "com.anthropic.claudefordesktop"}, "offline_allow"},
		{[]string{"rules", "local", "add", "bad..example"}, "不是合法的域名"},
	} {
		x := r.run(c.args...)
		if x.code == ExitOK || !strings.Contains(x.err, c.want) {
			t.Fatalf("%v 应原样显示后台的原因 %q：%+v", c.args, c.want, x)
		}
	}
	n := puts
	if x := r.run("rules", "pinned", "rm", "claude.ai"); x.code != ExitDenied || !strings.Contains(x.err, "内置的置顶域名") || puts != n {
		t.Fatalf("删除内置置顶域名应在本地拦下、不提交：%+v", x)
	}
	if x := r.run("rules", "pinned", "rm", "*.anthropic.com"); x.code != ExitDenied || puts != n {
		t.Fatalf("删除内置置顶域名的子域名写法也应拦下：%+v", x)
	}
	if x := r.run("rules", "pinned", "rm", "nothere.example"); x.code != ExitOK || puts != n {
		t.Fatalf("删除不存在的项不报错、不提交：%+v", x)
	}
}

// 验收 k：命令行发出的每一个规则 PUT 都带 If-Match（含按应用设规则、删规则）。
func TestCLIRulesPutsCarryIfMatch(t *testing.T) {
	r := newRig(t, rigOpt{})
	r.asOwner()
	var puts, without int
	r.trace = func(m, p string, h http.Header) {
		if m == "PUT" && strings.HasPrefix(p, "/v1/rules") {
			puts++
			if h.Get("If-Match") == "" {
				without++
			}
		}
	}
	for _, args := range [][]string{
		{"app", "set", "com.example.a", "block"}, {"app", "rm", "com.example.a"},
		{"rules", "local", "add", "a.example"}, {"rules", "local", "rm", "a.example"},
		{"rules", "pinned", "add", "b.example"}, {"rules", "pinned", "rm", "b.example"},
		{"rules", "offline", "add", "com.example.c"}, {"rules", "offline", "rm", "com.example.c"},
		{"rules", "udp443", "on"}, {"rules", "udp443", "off"},
	} {
		if x := r.run(args...); x.code != ExitOK {
			t.Fatalf("%v：%+v", args, x)
		}
	}
	if puts != 10 || without != 0 {
		t.Fatalf("每个规则提交都应带 If-Match：提交 %d 次，没带的 %d 次", puts, without)
	}
}
