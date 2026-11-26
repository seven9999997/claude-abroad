package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"claude-abroad/internal/guard"
	"claude-abroad/internal/pf"
)

// 补齐三验收 a–d：“允许局域网”开关。
func TestAllowLANSwitch(t *testing.T) {
	var c callers
	e := newEnv(t)
	run := filepath.Join(filepath.Dir(e.dir), "run")
	_ = os.Mkdir(run, 0o700)
	start := func() *Daemon {
		o := e.opts()
		c.opts(&o)
		o.RunDir = run
		o.BootID = func() (string, error) { return "boot-1", nil }
		return e.start(o)
	}
	d := start()
	c.as(os.Getuid())
	if st := status(t, d); st.AllowLAN == nil || !*st.AllowLAN {
		t.Fatal("默认开")
	}
	post(t, d, "/v1/enable", 200)
	lan := pf.Packet{If: "en0", User: "alice", Proto: "tcp", DstPort: 445, Dst: netip.MustParseAddr("192.168.1.20")}
	if ok, _ := e.sys.PF.Eval(pf.DefaultAnchor, lan); !ok {
		t.Fatal("开着时局域网放行")
	}
	// c. 关是收紧：控制台用户直接通过；b. 一次加载、中间没有清空
	c.as(consoleTestUID)
	e.sys.PF.ResetCalls()
	code, b, _ := callRaw(d, "PUT", "/v1/settings/lan", []byte(`{"allow_lan":false}`))
	if code != 200 {
		t.Fatalf("关：%d %s", code, b)
	}
	if calls := e.sys.PF.Calls(); slices.Contains(calls, "flush") || !slices.Contains(calls, "load") {
		t.Fatalf("应一次加载、没有清空：%v", calls)
	}
	// a. 规则里没有局域网放行；到局域网地址被拦；专用用户、回环、DHCP 不受影响
	raw := string(e.sys.PF.Raw(pf.DefaultAnchor))
	if strings.Contains(raw, "192.168.1.0/24") || !strings.Contains(raw, "user _abroad") || !strings.Contains(raw, "on lo0") || !strings.Contains(raw, "port 67") {
		t.Fatalf("关掉后的规则：\n%s", raw)
	}
	if ok, _ := e.sys.PF.Eval(pf.DefaultAnchor, lan); ok {
		t.Fatal("关掉后到局域网地址应被拦")
	}
	vmLAN := lan
	vmLAN.User = "_abroad"
	if ok, _ := e.sys.PF.Eval(pf.DefaultAnchor, vmLAN); !ok {
		t.Fatal("专用用户到局域网地址仍应放行（主代理第一跳可能在局域网）")
	}
	if st := status(t, d); st.AllowLAN == nil || *st.AllowLAN {
		t.Fatal("状态里应是关")
	}
	// c. 从关到开是放松：控制台用户被拒，有原因
	code, b, _ = callRaw(d, "PUT", "/v1/settings/lan", []byte(`{"allow_lan":true}`))
	if code != http.StatusForbidden || codeOf(t, b).Code != "needs_authorization" || !strings.Contains(string(b), "允许局域网") {
		t.Fatalf("打开应要授权：%d %s", code, b)
	}
	// d. 持久化：重启后仍是关；另存给 guard 的参数里没有局域网段，guard 加载的规则里也没有
	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	d = start()
	if st := status(t, d); st.AllowLAN == nil || *st.AllowLAN {
		t.Fatal("重启后仍应是关")
	}
	cfg, err := guard.ReadParams(e.dir)
	if err != nil || len(cfg.LAN) != 0 {
		t.Fatalf("guard 的参数里不应有局域网段：%+v %v", cfg, err)
	}
	if err := d.Shutdown(); err != nil {
		t.Fatal(err)
	}
	e.sys.PF.Reset()
	_ = os.Remove(filepath.Join(run, guard.MarkerFile))
	if out, err := guard.Run(context.Background(), guard.Deps{StateDir: e.dir, RunDir: run, PF: &pf.Controller{R: e.sys.PF},
		BootID: func() (string, error) { return "boot-2", nil }}); err != nil || out != guard.Loaded {
		t.Fatalf("guard：%v %v", out, err)
	}
	want, _ := pf.Render(cfg)
	if got := e.sys.PF.Raw(pf.DefaultAnchor); !bytes.Equal(got, want) || bytes.Contains(got, []byte("192.168.1.0/24")) {
		t.Fatalf("guard 加载的规则：\n%s", got)
	}
	// 打开（属主）恢复
	d = start()
	c.as(os.Getuid())
	if code, b, _ := callRaw(d, "PUT", "/v1/settings/lan", []byte(`{"allow_lan":true}`)); code != 200 {
		t.Fatalf("属主打开：%d %s", code, b)
	}
	var v LANSetting
	_, gb := call(t, d, "GET", "/v1/settings/lan", nil)
	if json.Unmarshal(gb, &v) != nil || !v.AllowLAN {
		t.Fatalf("%s", gb)
	}
}

// 补齐三验收 f、g、h：不分配假地址的名单经规则文档（带条件写入、按内容分级）；新加的项和追加的置顶域名重叠时拒绝；已在名单里、
// 之后被加进置顶的不拒绝（fakedns 按置顶处理）；规则替换后 DNS 模块立即用上新名单。
func TestBypassExtraRules(t *testing.T) {
	e := newM3Daemon(t, "")
	if code, msg := putRules(t, e.d, "pinned_extra: [\"*.mine.example\"]\nbypass_extra: [x.mine.example]\n", false); code != 422 || !strings.Contains(msg, "置顶域名") {
		t.Fatalf("新加的项和追加的置顶重叠应拒绝：%d %s", code, msg)
	}
	if code, msg := putRules(t, e.d, "bypass_extra: [nas.home.example, x.mine.example]\n", false); code != 200 {
		t.Fatalf("%d %s", code, msg)
	}
	if !e.dp.srv.Bypass.Match("nas.home.example") || e.dp.srv.Bypass.Match("plain.example.org") {
		t.Fatal("DNS 模块应立即用上新名单")
	}
	// 之后把 *.mine.example 加进置顶：不拒绝，x.mine.example 按置顶处理
	if code, msg := putRules(t, e.d, "pinned_extra: [\"*.mine.example\"]\nbypass_extra: [nas.home.example, x.mine.example]\n", false); code != 200 {
		t.Fatalf("已在名单里的不拒绝：%d %s", code, msg)
	}
	if e.dp.srv.Bypass.Match("x.mine.example") || !e.dp.srv.Bypass.Match("nas.home.example") {
		t.Fatal("后加进置顶的按置顶处理")
	}
	if code, msg := putRules(t, e.d, "bypass_extra: [claude.ai]\n", false); code != 422 {
		t.Fatalf("Claude 域名应拒绝：%d %s", code, msg)
	}
	if code, _ := putRules(t, e.d, "", false); code != 200 || e.dp.srv.Bypass.Match("nas.home.example") {
		t.Fatal("删掉后恢复分配假地址")
	}
}

// 补齐四验收 f：在联网的应用和只有规则的应用都带种类；共用的系统进程带说明；状态里仍然没有应用 ID（原有测试守着）。
func TestAppKinds(t *testing.T) {
	e := newM3Daemon(t, "rules:\n  - {app: system:apsd, action: block}\n  - {app: \"path:/usr/local/bin/x\", action: block}\n  - {app: com.example.only, action: block}\n")
	plain := netip.AddrPortFrom(e.lookup(t, "plain.example.org"), 443)
	held, err := e.dial(20001, plain) // com.app.a（m3 的假识别）；在途的才算在联网
	if err != nil || echoConn(held) != nil {
		t.Fatal(err)
	}
	defer held.Close()
	_, b := call(t, e.d, "GET", "/v1/apps", nil)
	var v AppsView
	if json.Unmarshal(b, &v) != nil {
		t.Fatalf("%s", b)
	}
	kinds := map[string]AppKindView{}
	for _, a := range v.Apps {
		kinds[a.App] = AppKindView{App: a.App, Kind: a.Kind, Shared: a.Shared}
	}
	for _, k := range v.RuleKinds {
		kinds[k.App] = k
	}
	want := map[string]string{"system:apsd": "system", "path:/usr/local/bin/x": "path", "com.example.only": "app"}
	for app, k := range want {
		if kinds[app].Kind != k {
			t.Errorf("%s 的种类：%+v，应为 %s", app, kinds[app], k)
		}
	}
	if kinds["system:apsd"].Shared == "" || kinds["com.example.only"].Shared != "" {
		t.Fatalf("共用的系统进程才有说明：%+v", kinds)
	}
	if len(v.Apps) == 0 || v.Apps[0].Kind == "" {
		t.Fatalf("在联网的应用也要有种类：%s", b)
	}
}
