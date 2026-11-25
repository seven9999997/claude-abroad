package preflight

import (
	"context"
	"errors"
	"go/ast"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"claude-abroad/internal/srccheck"
	"claude-abroad/internal/sysnet"
)

var ctx = context.Background()

func sample(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeRunner 按“程序名 参数”应答；值为 "!fail" 时返回错误。记录收到的每条命令。
type fakeRunner struct {
	t     *testing.T
	resp  map[string]string
	calls []string
}

func (f *fakeRunner) Run(_ context.Context, p string, args []string, stdin []byte) ([]byte, []byte, error) {
	key := filepath.Base(p) + " " + strings.Join(args, " ")
	if stdin != nil {
		key += " <stdin>"
	}
	f.calls = append(f.calls, key)
	v, ok := f.resp[key]
	if !ok {
		f.t.Fatalf("没有准备应答：%q", key)
	}
	if v == "!fail" {
		return nil, []byte("boom"), errors.New("exit status 1")
	}
	return sample(f.t, v), nil, nil
}

// clean 是没有任何冲突的应答。
func clean() map[string]string {
	return map[string]string{
		"netstat -rn -f inet":     "netstat-clean.constructed.txt",
		"scutil --proxy":          "proxy-off.txt",
		"route -n get default":    "route-default-en0.txt",
		"route -n get 1.1.1.1":    "route-en0.constructed.txt",
		"route -n get 129.0.0.1":  "route-en0.constructed.txt",
		"route -n get 198.18.0.2": "route-en0.constructed.txt",
		"scutil --nwi":            "nwi-v4.txt",
		"ps -axo comm":            "ps-clean.constructed.txt",
	}
}

func checker(t *testing.T, over map[string]string) (*Checker, *fakeRunner) {
	resp := clean()
	for k, v := range over {
		resp[k] = v
	}
	r := &fakeRunner{t: t, resp: resp}
	return &Checker{R: r}, r
}

func ids(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.ID)
	}
	return out
}

func TestClean(t *testing.T) {
	c, _ := checker(t, nil)
	r := c.Run(ctx)
	if len(r.Findings) != 0 {
		t.Fatalf("%+v", r.Findings)
	}
	if err := c.Check(ctx); err != nil {
		t.Fatal(err)
	}
}

// b. 每个检查项：有冲突、没有冲突、命令失败或输出不认识。
func TestEachCheck(t *testing.T) {
	cases := []struct {
		name  string
		over  map[string]string
		id    string
		sev   Severity
		title string // 标题里应有的字样
	}{
		{"系统代理", map[string]string{"scutil --proxy": "proxy-http.constructed.txt"}, "system_proxy", Block, "系统代理已开启"},
		{"SOCKS 代理", map[string]string{"scutil --proxy": "proxy-socks.constructed.txt"}, "system_proxy", Block, "系统代理已开启"},
		{"PAC", map[string]string{"scutil --proxy": "proxy-pac.constructed.txt"}, "pac", Block, "PAC"},
		{"自动发现", map[string]string{"scutil --proxy": "proxy-wpad.constructed.txt"}, "auto_discovery", Block, "自动代理发现"},
		{"代理设置命令失败", map[string]string{"scutil --proxy": "!fail"}, "system_proxy", Block, "无法判断"},
		{"代理设置输出不认识", map[string]string{"scutil --proxy": "proxy-garbage.constructed.txt"}, "system_proxy", Block, "无法判断"},
		{"默认路由经别的 utun", map[string]string{"route -n get default": "route-default-utun.constructed.txt"}, "takeover_route", Block, "utun1024"},
		{"0.0.0.0/1 经别的 utun", map[string]string{"route -n get 1.1.1.1": "route-1.1.1.1-utun1024.txt"}, "takeover_route", Block, "utun1024"},
		{"fake-IP DNS 被抢", map[string]string{"route -n get 198.18.0.2": "route-1.1.1.1-utun1024.txt"}, "takeover_route", Block, "utun1024"},
		{"路由命令失败", map[string]string{"route -n get 129.0.0.1": "!fail"}, "takeover_route", Block, "无法判断"},
		{"只有 8/5 经别的 utun（地址探测抓不到）", map[string]string{"netstat -rn -f inet": "netstat-8-5.constructed.txt"}, "takeover_route", Block, "utun9"},
		{"本机实测的分段路由", map[string]string{"netstat -rn -f inet": "netstat-clash.txt"}, "takeover_route", Block, "utun1024"},
		{"路由表命令失败", map[string]string{"netstat -rn -f inet": "!fail"}, "takeover_route", Block, "无法判断"},
		{"路由表输出不认识", map[string]string{"netstat -rn -f inet": "proxy-off.txt"}, "takeover_route", Block, "无法判断"},
		{"路由输出不认识", map[string]string{"route -n get default": "proxy-off.txt"}, "takeover_route", Block, "无法判断"},
		{"只有 IPv6", map[string]string{"scutil --nwi": "nwi-v6only.constructed.txt"}, "ipv6_only", Block, "只有 IPv6"},
		{"网络信息命令失败", map[string]string{"scutil --nwi": "!fail"}, "ipv6_only", Block, "无法判断"},
		{"网络信息输出不认识", map[string]string{"scutil --nwi": "nwi-garbage.constructed.txt"}, "ipv6_only", Block, "无法判断"},
		{"代理软件在运行", map[string]string{"ps -axo comm": "ps-clash.txt"}, "proxy_app", Warn, "clash-verge"},
		{"进程列表命令失败", map[string]string{"ps -axo comm": "!fail"}, "proxy_app", Block, "无法判断"},
		{"进程列表输出不认识", map[string]string{"ps -axo comm": "proxy-off.txt"}, "proxy_app", Block, "无法判断"},
	}
	for _, cs := range cases {
		t.Run(cs.name, func(t *testing.T) {
			c, _ := checker(t, cs.over)
			r := c.Run(ctx)
			if len(r.Findings) != 1 {
				t.Fatalf("应恰好一条：%+v", r.Findings)
			}
			f := r.Findings[0]
			if f.ID != cs.id || f.Severity != cs.sev || !strings.Contains(f.Title, cs.title) || f.Fix == "" {
				t.Fatalf("%+v", f)
			}
		})
	}
}

// b. 只有 10/8 经别的 utun（公司 VPN 的内网分段）：不阻断。
func TestPrivateSplitNotConflict(t *testing.T) {
	c, _ := checker(t, map[string]string{"netstat -rn -f inet": "netstat-10-8.constructed.txt"})
	if f := c.Run(ctx).Findings; len(f) != 0 {
		t.Fatalf("%+v", f)
	}
}

// b. 自己的 utun 不算冲突。
func TestOurUtunNotConflict(t *testing.T) {
	over := map[string]string{
		"netstat -rn -f inet":     "netstat-ours-utun7.constructed.txt",
		"route -n get default":    "route-default-utun7.constructed.txt",
		"route -n get 1.1.1.1":    "route-1.1.1.1-utun7.constructed.txt",
		"route -n get 129.0.0.1":  "route-1.1.1.1-utun7.constructed.txt",
		"route -n get 198.18.0.2": "route-1.1.1.1-utun7.constructed.txt",
	}
	c, _ := checker(t, over)
	if r := c.Run(ctx); !slices.Equal(ids(r.Findings), []string{"takeover_route"}) {
		t.Fatalf("不传自己的 utun 时应判冲突：%+v", r.Findings)
	}
	c.OurUtun = func() string { return "utun7" }
	if r := c.Run(ctx); len(r.Findings) != 0 {
		t.Fatalf("经自己的 utun 不算冲突：%+v", r.Findings)
	}
	// 自己的 utun 名不能把别人的放过去。
	c2, _ := checker(t, map[string]string{"route -n get 1.1.1.1": "route-1.1.1.1-utun1024.txt"})
	c2.OurUtun = func() string { return "utun7" }
	if r := c2.Run(ctx); !slices.Equal(ids(r.Findings), []string{"takeover_route"}) {
		t.Fatalf("%+v", r.Findings)
	}
}

// b. 主代理第一跳是回环。
func TestFirstHop(t *testing.T) {
	for host, block := range map[string]bool{
		"127.0.0.1": true, "localhost": true, "LOCALHOST.": true, "::1": true, "[::1]": true, "0.0.0.0": true, "a.localhost": true,
		"203.0.113.5": false, "node.example.com": false, "192.168.1.5": false,
	} {
		c, _ := checker(t, nil)
		c.FirstHop = func() (string, error) { return host, nil }
		got := ids(c.Run(ctx).Blocking())
		if block != slices.Contains(got, "loopback_hop") {
			t.Errorf("%s：%v", host, got)
		}
	}
	c, _ := checker(t, nil)
	c.FirstHop = func() (string, error) { return "", errors.New("没读到") }
	if f := c.Run(ctx).Blocking(); len(f) != 1 || !strings.Contains(f[0].Title, "无法判断") {
		t.Fatalf("%+v", f)
	}
}

// b. shell 配置：提示、文件名和行号；注释行和名字只是包含这些字样的不报；大小写都报。
func TestShellEnv(t *testing.T) {
	home := fstest.MapFS{
		".zshrc":   {Data: []byte("# export HTTPS_PROXY=http://10.66.77.88:7890\nexport MY_HTTP_PROXY_NOTE=1\nalias x=y\nexport https_proxy=http://10.66.77.88:7890\n")},
		".bashrc":  {Data: []byte("  ALL_PROXY=socks5://secret-user:secret-pass@10.66.77.88:7891\nHTTP_PROXY_HOST=x\n")},
		".profile": {Data: []byte("export http_proxy_x=1\n")},
	}
	c, _ := checker(t, nil)
	c.Home = home
	r := c.Run(ctx)
	if len(r.Findings) != 1 || r.Findings[0].ID != "shell_env" || r.Findings[0].Severity != Warn {
		t.Fatalf("%+v", r.Findings)
	}
	title := r.Findings[0].Title
	if !strings.Contains(title, "~/.zshrc 第 4 行") || !strings.Contains(title, "~/.bashrc 第 1 行") || strings.Count(title, "第") != 2 {
		t.Fatalf("%s", title)
	}
	// 超过 1 MiB：无法检查的提示。
	big := fstest.MapFS{".zshrc": {Data: make([]byte, MaxFileSize+1)}}
	c.Home = big
	if f := c.Run(ctx).Warnings(); len(f) != 1 || !strings.Contains(f[0].Title, "无法检查") {
		t.Fatalf("%+v", f)
	}
	c.Home = fstest.MapFS{}
	if f := c.Run(ctx).Findings; len(f) != 0 {
		t.Fatalf("没有配置文件时不报：%+v", f)
	}
}

// b. 网络共享。
func TestSharing(t *testing.T) {
	for file, want := range map[string]string{
		"nat-off.plist":                "",
		"nat-on.constructed.plist":     "已开启",
		"nat-binary.constructed.plist": "无法判断",
		"garbage":                      "无法判断",
	} {
		data := []byte("<plist><dict><key>NAT</key><oops")
		if file != "garbage" {
			data = sample(t, file)
		}
		c, _ := checker(t, nil)
		c.Root = fstest.MapFS{NATPlist: {Data: data}}
		f := c.Run(ctx).Findings
		if want == "" {
			if len(f) != 0 {
				t.Fatalf("%s：%+v", file, f)
			}
			continue
		}
		if len(f) != 1 || f[0].ID != "sharing" || f[0].Severity != Warn || !strings.Contains(f[0].Title, want) {
			t.Fatalf("%s：%+v", file, f)
		}
	}
	c, _ := checker(t, nil)
	c.Root = fstest.MapFS{}
	if f := c.Run(ctx).Findings; len(f) != 0 {
		t.Fatalf("没有 plist 时不报：%+v", f)
	}
}

// c. 脱敏：所有结果文字和 Check 的错误里都不含地址、端口、PAC 地址、环境变量值、完整路径。
func TestRedaction(t *testing.T) {
	c, _ := checker(t, map[string]string{
		"scutil --proxy":       "proxy-http.constructed.txt",
		"ps -axo comm":         "ps-secretpath.constructed.txt",
		"route -n get default": "route-default-utun.constructed.txt",
	})
	c.Home = fstest.MapFS{".zshrc": {Data: []byte("export ALL_PROXY=socks5://secret-user:secret-pass@10.66.77.88:7891\n")}}
	c.FirstHop = func() (string, error) { return "127.0.0.1", nil }
	var texts []string
	for _, f := range c.Run(ctx).Findings {
		texts = append(texts, f.Title, f.Fix)
	}
	err := c.Check(ctx)
	if err == nil {
		t.Fatal("应阻断")
	}
	texts = append(texts, err.Error())
	c2, _ := checker(t, map[string]string{"scutil --proxy": "proxy-pac.constructed.txt"})
	for _, f := range c2.Run(ctx).Findings {
		texts = append(texts, f.Title, f.Fix)
	}
	secrets := []string{"10.66.77.88", "7897", "7891", "7890", "pac.secret-corp.example", "8080", "proxy.pac", "secret-user", "secret-pass", "socks5://",
		"/Users/secret-person", "secret-person", "/opt/homebrew", "198.18.0.1"}
	all := strings.Join(texts, "\n")
	for _, s := range secrets {
		if strings.Contains(all, s) {
			t.Errorf("结果里出现了 %q：\n%s", s, all)
		}
	}
	if !strings.Contains(all, "sing-box") || !strings.Contains(all, "xray") {
		t.Fatalf("应只写软件名：%s", all)
	}
}

// d. 只读：底层 Runner 收到的每条命令都在只读白名单内；传入的 Runner 不能绕过。
func TestReadOnly(t *testing.T) {
	c, r := checker(t, nil)
	c.Run(ctx)
	allowed := []string{"netstat -rn -f inet", "scutil --proxy", "route -n get default", "route -n get 1.1.1.1", "route -n get 129.0.0.1", "route -n get 198.18.0.2", "scutil --nwi", "ps -axo comm"}
	for _, call := range r.calls {
		if !slices.Contains(allowed, call) {
			t.Errorf("白名单外的命令：%q", call)
		}
	}
	if len(r.calls) != len(allowed) {
		t.Fatalf("%v", r.calls)
	}
	// 内部一律包 ReadOnlyRunner：即使传入的是会执行一切的 Runner，改动类命令也到不了它。
	if _, _, err := c.runner().Run(ctx, sysnet.PathRoute, []string{"-n", "add", "-net", "0.0.0.0/1", "-interface", "utun7"}, nil); !errors.Is(err, sysnet.ErrNotReadOnly) {
		t.Fatalf("%v", err)
	}
	if len(r.calls) != len(allowed) {
		t.Fatal("改动类命令到了底层")
	}
}

// d. 源码检查：没有 os/exec、没有写文件的调用。
func TestNoExecNoWrite(t *testing.T) {
	srccheck.NoExec(t, ".")
	files, _ := filepath.Glob("*.go")
	writes := []string{"os.WriteFile", "os.Create", "os.OpenFile", "os.Remove", "os.RemoveAll", "os.Mkdir", "os.MkdirAll", "os.Rename", "os.Chmod", "os.Chown", "os.Symlink", "os.Truncate"}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		ast.Inspect(srccheck.Parse(t, f), func(n ast.Node) bool {
			if x, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := x.X.(*ast.Ident); ok && slices.Contains(writes, id.Name+"."+x.Sel.Name) {
					t.Errorf("%s 调用了 %s.%s", f, id.Name, x.Sel.Name)
				}
			}
			return true
		})
	}
}

// e. 有阻断项时 Check 返回 usererror 并列出处理办法；只有提示项时返回 nil，提示项可另行取出。
func TestCheckError(t *testing.T) {
	c, _ := checker(t, map[string]string{"scutil --proxy": "proxy-pac.constructed.txt", "route -n get 1.1.1.1": "route-1.1.1.1-utun1024.txt", "ps -axo comm": "ps-clash.txt"})
	err := c.Check(ctx)
	if !errors.Is(err, ErrBlocked) || !strings.HasPrefix(err.Error(), "启用前的冲突检查失败：") {
		t.Fatalf("%v", err)
	}
	for _, f := range c.Last().Blocking() {
		if !strings.Contains(err.Error(), f.Fix) {
			t.Errorf("错误信息里没有处理办法：%s", f.Fix)
		}
	}
	if strings.Contains(err.Error(), "继续运行不拦") {
		t.Fatal("提示项不应进阻断的错误信息")
	}
	c2, _ := checker(t, map[string]string{"ps -axo comm": "ps-clash.txt"})
	if err := c2.Check(ctx); err != nil {
		t.Fatalf("只有提示项时应返回 nil：%v", err)
	}
	if w := c2.Last().Warnings(); len(w) != 1 || w[0].ID != "proxy_app" {
		t.Fatalf("%+v", w)
	}
}

// M6-3：解析锁有别人的同名文件：只提示（不阻断启用），写明域名；没有冲突或没接时没有这一项。
func TestResolverLockConflictFinding(t *testing.T) {
	c := &Checker{ResolverConflicts: func() []string { return []string{"claude.com"} }}
	f := c.checkResolverLock()
	if len(f) != 1 || f[0].ID != "resolver_lock" || f[0].Severity != Warn || !strings.Contains(f[0].Title, "claude.com") {
		t.Fatalf("%+v", f)
	}
	if got := (&Checker{ResolverConflicts: func() []string { return nil }}).checkResolverLock(); len(got) != 0 {
		t.Fatal("没有冲突时不应有这一项")
	}
	if got := (&Checker{}).checkResolverLock(); len(got) != 0 {
		t.Fatal("没接时不应有这一项")
	}
}
