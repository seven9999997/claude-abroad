package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func mustOK(t *testing.T, x res, what string) res {
	t.Helper()
	if x.code != ExitOK {
		t.Fatalf("%s：退出码 %d\n输出：%s\n错误：%s", what, x.code, x.out, x.err)
	}
	return x
}

func mustJSON(t *testing.T, x res, what string) {
	t.Helper()
	mustOK(t, x, what)
	if !json.Valid([]byte(strings.TrimSpace(x.out))) {
		t.Fatalf("%s --json 输出不是 JSON：%s", what, x.out)
	}
}

const subNodes = `proxies:
  - {name: exit, type: ss, server: 8.8.4.20, port: 54321, cipher: aes-128-gcm, password: PWDMARK1}
  - {name: relay, type: trojan, server: relay.example.com, port: 443, password: KEYMARK4, sni: relay.example.com}
`
const lanNode = "name: lan\ntype: socks5\nserver: 192.168.1.20\nport: 7890\nusername: usermark\npassword: PWDMARK3\n"
const subURL = "https://sub.example.com/api?token=SUBMARK"

// 订阅抓到的内容（另一组节点）。
const fetchedNodes = `proxies:
  - {name: sub1, type: ss, server: srvmark.example.com, port: 8389, cipher: aes-128-gcm, password: PWDMARK2}
`

var markers = []string{"PWDMARK1", "PWDMARK2", "PWDMARK3", "KEYMARK4", "SUBMARK", "srvmark", "usermark", "54321", "8.8.4.20", "192.168.1.20", "PRIVKEYMARK", "vpn-secret-host"}

const wgConf = "[Interface]\nPrivateKey = YW5vdGhlcjMyYnl0ZXNsb25na2V5Zm9ydGVzdGluZzE=\nAddress = 10.66.0.2/32\n# PRIVKEYMARK\n\n[Peer]\nPublicKey = YW5vdGhlcjMyYnl0ZXNsb25na2V5Zm9ydGVzdGluZzE=\nEndpoint = vpn-secret-host.example.com:51820\nAllowedIPs = 0.0.0.0/0\n"

// 验收 a、d：每个子命令一条端到端（文本和 --json 各一次）；敏感内容经参数传入被拒、文件和标准输入成功；全部输出搜不到标记。
func TestEndToEnd(t *testing.T) {
	r := newRig(t, rigOpt{main: true})
	r.sub = []byte(fetchedNodes)
	var outputs []string
	keep := func(x res) res { outputs = append(outputs, x.all()); return x }
	both := func(what string, args ...string) (res, res) {
		t.Helper()
		txt := keep(mustOK(t, r.run(args...), what))
		js := keep(r.run(append([]string{"--json"}, args...)...))
		mustJSON(t, js, what)
		return txt, js
	}
	// split：会改状态的子命令，文本和 --json 各对一个不同的对象跑一次。
	split := func(what string, text, js []string) {
		t.Helper()
		keep(mustOK(t, r.run(text...), what))
		x := keep(r.run(append([]string{"--json"}, js...)...))
		mustJSON(t, x, what)
	}

	// 敏感内容经参数传入：一律拒绝、不发请求。
	for _, args := range [][]string{
		{"main", "add-source", "--url", subURL},
		{"main", "add-source", subURL},
		{"main", "add-source", "--url=" + subURL},
		{"main", "add-node", "--pass", "PWDMARK3", "--file", "x"},
		{"exit", "add", "wg", "--kind", "wireguard", "--conf", wgConf},
	} {
		x := keep(r.run(args...))
		if x.code != ExitUsage || x.reqs != 0 || !strings.Contains(x.err, "--file") {
			t.Fatalf("%v：应拒绝（参数错误、不发请求）：%+v", args, x)
		}
	}

	// 主代理：三种来源输入方式。
	x := keep(mustOK(t, r.run("main", "add-source", "--file", writeFile(t, "n.yaml", subNodes), "--name", "本地"), "add-source --file"))
	if !strings.Contains(x.out, "已添加来源") {
		t.Fatal(x.out)
	}
	keep(mustOK(t, r.run("main", "add-source", "--url-file", writeFile(t, "u.txt", subURL+"\n"), "--name", "订阅", "--via-host"), "add-source --url-file"))
	keep(mustOK(t, r.runWith(in{stdin: subNodes}, "--json", "main", "add-source", "--stdin", "--name", "标准输入"), "add-source --stdin"))
	keep(mustOK(t, r.run("main", "add-node", "--file", writeFile(t, "lan.yaml", lanNode)), "add-node --file"))
	r.mu.Lock()
	if len(r.urls) != 1 || r.urls[0] != subURL {
		t.Fatalf("订阅地址应原样交给 abroadd 抓取：%v", r.urls)
	}
	r.mu.Unlock()

	txt, js := both("main nodes", "main", "nodes")
	for _, name := range []string{"exit", "relay", "sub1", "lan"} {
		if !strings.Contains(txt.out, name) {
			t.Fatalf("main nodes 应列出 %s：%s", name, txt.out)
		}
	}
	var srcs []struct {
		ID, Name, Kind string
	}
	_ = json.Unmarshal([]byte(js.out), &srcs)
	subID := ""
	for _, s := range srcs {
		if s.Kind == "subscription" {
			subID = s.ID
		}
	}
	if len(srcs) != 4 || subID == "" {
		t.Fatalf("应有 4 个来源、其中一个订阅：%s", js.out)
	}
	both("main refresh", "main", "refresh", subID, "--via-host")

	// 选节点（名字重复时要求用 ID；带中转）。
	if x := keep(r.run("--yes", "main", "use", "exit")); x.code != ExitUsage || !strings.Contains(x.err, "节点 ID") {
		t.Fatalf("两个来源都有 exit：应要求用 ID：%+v", x)
	}
	if x := keep(r.run("--yes", "main", "use", "sub1", "--via", "lan")); x.code != ExitFail || !strings.Contains(x.err, "局域网") {
		t.Fatalf("局域网第一跳没有确认：%+v", x)
	}
	both("main confirm-lan", "main", "confirm-lan", "lan")
	x = keep(mustOK(t, r.run("--yes", "main", "use", "sub1", "--via", "lan"), "main use --via"))
	if !strings.Contains(x.out, "已保存") {
		t.Fatalf("未启用时应保存：%s", x.out)
	}
	txt, _ = both("main show", "main", "show")
	if !strings.Contains(txt.out, "sub1") || !strings.Contains(txt.out, "lan → sub1") {
		t.Fatalf("main show：%s", txt.out)
	}
	keep(mustOK(t, r.run("--yes", "main", "use", "sub1"), "main use"))
	both("main check", "main", "check")
	both("main apply", "--yes", "main", "apply")
	txt, _ = both("main exit", "main", "exit")
	if !strings.Contains(txt.out, "203.0.113.7") || !strings.Contains(txt.out, "US") {
		t.Fatalf("main exit 应显示出口 IP 和国家：%s", txt.out)
	}

	// 分流出口：文件和标准输入。
	keep(mustOK(t, r.run("exit", "add", "wg", "--kind", "wireguard", "--file", writeFile(t, "wg.conf", wgConf)), "exit add --file"))
	keep(mustOK(t, r.runWith(in{stdin: wgConf}, "exit", "add", "wg2", "--kind", "wireguard", "--stdin"), "exit add --stdin"))
	txt, _ = both("exit ls", "exit", "ls")
	if !strings.Contains(txt.out, "wg") || !strings.Contains(txt.out, "wg2") {
		t.Fatal(txt.out)
	}
	both("exit check", "exit", "check", "wg")
	keep(mustOK(t, r.runWith(in{stdin: wgConf}, "--json", "exit", "add", "wg3", "--kind", "wireguard", "--stdin"), "exit add --json"))
	split("exit rm", []string{"exit", "rm", "wg2"}, []string{"exit", "rm", "wg3"})

	// 应用分流。
	both("app set", "app", "set", "com.example.a", "block")
	keep(mustOK(t, r.run("app", "set", "com.example.b", "proxy:wg"), "app set proxy"))
	txt, js = both("app ls", "app", "ls")
	if !strings.Contains(txt.out, "com.example.a") || !strings.Contains(js.out, `"Action":"block"`) && !strings.Contains(js.out, `"action":"block"`) {
		t.Fatalf("app ls：%s / %s", txt.out, js.out)
	}
	split("app rm", []string{"app", "rm", "com.example.b"}, []string{"app", "rm", "com.example.a"})

	// 地区、启用、状态、环境检测、在联网的应用、引擎、停用、回国。
	both("region set", "--yes", "region", "set", "Asia/Tokyo", "ja-JP", "--locale", "ja_JP")
	txt, _ = both("region show", "region", "show")
	if !strings.Contains(txt.out, "Asia/Tokyo") {
		t.Fatal(txt.out)
	}
	x = keep(mustOK(t, r.run("--yes", "up"), "up"))
	if !strings.Contains(x.out, "完成") {
		t.Fatal(x.out)
	}
	txt, _ = both("status", "status")
	if !strings.Contains(txt.out, "状态：") || !strings.Contains(txt.out, "签名") {
		t.Fatalf("status 应显示签名校验未启用的说明：%s", txt.out)
	}
	both("apps", "apps")
	txt, _ = both("engine ls", "engine", "ls")
	if !strings.Contains(txt.out, "mihomo") {
		t.Fatal(txt.out)
	}
	both("region auto", "--yes", "region", "auto")
	// 假系统没有环境检测：如实说明（文本和 JSON 的正常输出见 TestStubOutputs）。
	if x := keep(r.run("check")); x.code != ExitFail || !strings.Contains(x.err, "没有环境检测") {
		t.Fatalf("check：%+v", x)
	}
	keep(mustOK(t, r.run("down"), "down"))
	keep(mustOK(t, r.run("--yes", "up"), "up 2"))
	mustJSON(t, keep(r.run("--json", "down")), "down")
	keep(mustOK(t, r.run("--yes", "up"), "up 3"))
	keep(mustOK(t, r.run("--yes", "restore"), "restore"))
	keep(mustOK(t, r.run("--yes", "up"), "up 4"))
	mustJSON(t, keep(r.run("--json", "--yes", "restore")), "restore")
	if x := keep(r.run("main", "rm-source", subID)); x.code != ExitBusy || !strings.Contains(x.err, "正在作为主代理") {
		t.Fatalf("正在用的来源不能删：%+v", x)
	}
	split("rm-source", []string{"main", "rm-source", srcs[2].ID}, []string{"main", "rm-source", srcs[0].ID})

	for _, o := range outputs {
		for _, m := range markers {
			if strings.Contains(o, m) {
				t.Errorf("输出里出现了 %q：%s", m, o)
			}
		}
	}
}

// 验收 b：两层拒绝。
func TestTwoLayers(t *testing.T) {
	r := newRig(t, rigOpt{main: true, claude: true})
	mustOK(t, r.run("main", "add-source", "--file", writeFile(t, "n.yaml", subNodes)), "add-source")
	mustOK(t, r.run("--yes", "main", "apply"), "建立基准")
	mustOK(t, r.run("--yes", "region", "set", "Asia/Tokyo", "ja-JP"), "锁定地区")
	r.asConsole()
	// 第一层：敏感的子命令本地拦下、不发请求。
	for _, args := range [][]string{
		{"--yes", "restore"},
		{"--yes", "main", "use", "exit"},
		{"main", "add-source", "--file", "x"},
		{"main", "rm-source", "x"},
		{"main", "confirm-lan", "x"},
		{"exit", "add", "wg", "--kind", "wireguard", "--file", "x"},
		{"exit", "rm", "wg"},
	} {
		x := r.run(args...)
		if x.code != ExitDenied || x.reqs != 0 || !strings.Contains(x.err, "sudo") {
			t.Errorf("%v：本地应拦下（退出码 3、请求 0、提示 sudo）：%+v", args, x)
		}
	}
	// 第二层：绕过本地检查，abroadd 按级别拒绝；系统无改动。
	st0 := r.status()
	for _, args := range [][]string{{"--yes", "restore"}, {"exit", "rm", "wg"}, {"main", "add-source", "--file", writeFile(t, "n2.yaml", subNodes)}} {
		x := r.runWith(in{skip: true}, args...)
		if x.code != ExitDenied || x.reqs == 0 || !strings.Contains(x.err, "需要本人授权") {
			t.Errorf("%v：abroadd 应 403：%+v", args, x)
		}
	}
	if st := r.status(); st.State != st0.State || len(r.sys.Procs.Signals) != 0 {
		t.Fatal("被拒后系统不能有改动")
	}
	// 按本次调用决定的：启用会让地区变 → 403“这一次需要 sudo”和原因。
	if x := r.run("--yes", "up"); x.code != ExitDenied || !strings.Contains(x.err, "这一次需要 sudo") || !strings.Contains(x.err, "地区") {
		t.Fatalf("控制台用户启用（地区会变）：%+v", x)
	}
	if r.status().State == "ENABLED" {
		t.Fatal("被拒的启用不能生效")
	}
	r.asOwner()
	mustOK(t, r.run("--yes", "up"), "sudo 启用")
	r.asConsole()
	mustOK(t, r.run("--yes", "region", "set", "Asia/Tokyo", "ja-JP"), "地区不变的切换")
	if x := r.run("--yes", "region", "set", "Europe/Paris", "fr-FR"); x.code != ExitDenied || !strings.Contains(x.err, "这一次需要 sudo") || !strings.Contains(x.err, "地区会变") {
		t.Fatalf("地区会变的切换：%+v", x)
	}
	mustOK(t, r.run("app", "set", "com.example.a", "block"), "收紧的改规则")
	if x := r.run("app", "set", "com.example.a", "direct"); x.code != ExitDenied || !strings.Contains(x.err, "这一次需要 sudo") {
		t.Fatalf("放松保护的改规则：%+v", x)
	}
	mustOK(t, r.run("down"), "控制台用户停用")
}

// 验收 c：--yes。
func TestYes(t *testing.T) {
	r := newRig(t, rigOpt{claude: true})
	mustOK(t, r.run("--yes", "up"), "启用")
	// 不带 --yes、终端里输入“否”：打印预览、不执行。
	x := r.runWith(in{tty: true, stdin: "否\n"}, "restore")
	if x.code != ExitConfirm || !strings.Contains(x.out, "会结束的 Claude 进程：1 个") || !strings.Contains(x.out, "7100") || !strings.Contains(x.out, "输入 yes") {
		t.Fatalf("输入“否”：%+v", x)
	}
	// 不是终端、没有 --yes：打印预览、不执行。
	x = r.runWith(in{stdin: "yes\n"}, "restore")
	if x.code != ExitConfirm || !strings.Contains(x.out, "7100") || !strings.Contains(x.err, "--yes") {
		t.Fatalf("非终端无 --yes：%+v", x)
	}
	if r.status().State != "ENABLED" || len(r.sys.Procs.Signals) != 0 {
		t.Fatal("没有确认时不能执行")
	}
	// 控制台用户带 --yes 回国：仍被拒（--yes 不能代替 sudo）。
	r.asConsole()
	if x := r.run("--yes", "restore"); x.code != ExitDenied || x.reqs != 0 {
		t.Fatalf("--yes 不能代替 sudo：%+v", x)
	}
	r.asOwner()
	// 带 --yes：同样打印预览，自动提交确认码。
	x = r.run("--yes", "restore")
	if x.code != ExitOK || !strings.Contains(x.out, "7100") || strings.Contains(x.out, "输入 yes") || x.reqs != 2 {
		t.Fatalf("--yes：%+v", x)
	}
	if r.status().State == "ENABLED" {
		t.Fatal("回国应已执行")
	}
	// 终端里输入 yes：执行。地区会变的切换打印“从什么变成什么”。
	mustOK(t, r.run("--yes", "region", "set", "Asia/Tokyo", "ja-JP"), "锁定")
	mustOK(t, r.run("--yes", "up"), "启用")
	x = r.runWith(in{tty: true, stdin: "yes\n"}, "region", "set", "Europe/Paris", "fr-FR")
	if x.code != ExitOK || !strings.Contains(x.out, "地区：") || !strings.Contains(x.out, "→") {
		t.Fatalf("地区切换的预览：%+v", x)
	}
}

// 验收 g：尚未提供的子命令说明“以后的版本提供”、退出码 7、不发请求。
func TestLater(t *testing.T) {
	r := newRig(t, rigOpt{})
	for _, args := range [][]string{
		{"engine", "install", "sing-box"}, {"engine", "rollback", "sing-box"}, {"check", "--device"},
		{"restore", "--force"}, {"uninstall"}, // --kill-browsers 在 M6-4b-3 提供了
	} {
		x := r.run(args...)
		if x.code != ExitLater || x.reqs != 0 || !strings.Contains(x.out, "以后的版本") {
			t.Errorf("%v：%+v", args, x)
		}
	}
}

// 验收 i：status --watch。Ctrl+C（ctx 结束）干净退出；后台服务断开时有提示。
func TestWatch(t *testing.T) {
	r := newRig(t, rigOpt{})
	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuf{}
	done := make(chan res, 1)
	go func() { done <- r.runWith(in{ctx: ctx, out: out}, "status", "--watch") }()
	waitFor(t, func() bool { return strings.Contains(out.String(), "Ctrl+C") }, out)
	time.Sleep(100 * time.Millisecond) // 等订阅建立
	mustOK(t, r.run("--yes", "up"), "启用")
	waitFor(t, func() bool { return strings.Contains(out.String(), "gate/") }, out)
	cancel()
	select {
	case x := <-done:
		if x.code != ExitOK || x.err != "" {
			t.Fatalf("Ctrl+C 应干净退出：%+v", x)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Ctrl+C 后没有退出")
	}
	// 断线。
	out2 := &syncBuf{}
	go func() { done <- r.runWith(in{out: out2}, "status", "--watch") }()
	waitFor(t, func() bool { return strings.Contains(out2.String(), "Ctrl+C") }, out2)
	time.Sleep(100 * time.Millisecond)
	r.d.Shutdown()
	select {
	case x := <-done:
		if x.code != ExitNoDaemon || !strings.Contains(x.err, "断开") || strings.Contains(x.err, "goroutine") {
			t.Fatalf("断线：%+v", x)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("断线后没有退出")
	}
}

func waitFor(t *testing.T, f func() bool, out *syncBuf) {
	t.Helper()
	for i := 0; i < 300; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等不到；输出：%s", out.String())
}
