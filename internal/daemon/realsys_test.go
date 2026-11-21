package daemon

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"claude-abroad/internal/pf"
	"claude-abroad/internal/state"
	"claude-abroad/internal/sysnet"
	"claude-abroad/internal/tun"
)

// cmdLog 记下真实系统发出的每一条命令（pfctl 和 sysnet 两路），给命令审计用。
type cmdLog struct {
	mu    sync.Mutex
	cmds  []string
	loads [][]byte // pfctl -f - 的规则原文
}

func (l *cmdLog) add(s string)  { l.mu.Lock(); l.cmds = append(l.cmds, s); l.mu.Unlock() }
func (l *cmdLog) all() []string { l.mu.Lock(); defer l.mu.Unlock(); return slices.Clone(l.cmds) }

type recPF struct {
	R   pf.Runner
	log *cmdLog
}

func (r recPF) Run(ctx context.Context, args []string, stdin []byte) ([]byte, []byte, error) {
	r.log.add("/sbin/pfctl " + strings.Join(args, " "))
	if strings.HasSuffix(strings.Join(args, " "), " -f -") {
		r.log.mu.Lock()
		r.log.loads = append(r.log.loads, slices.Clone(stdin))
		r.log.mu.Unlock()
	}
	return r.R.Run(ctx, args, stdin)
}

type recNet struct {
	R   sysnet.Runner
	log *cmdLog
}

func (r recNet) Run(ctx context.Context, path string, args []string, stdin []byte) ([]byte, []byte, error) {
	s := strings.Join(append([]string{path}, args...), " ")
	if stdin != nil {
		s += " <<" + strings.ReplaceAll(strings.TrimSpace(string(stdin)), "\n", "; ")
	}
	r.log.add(strings.TrimSpace(s))
	return r.R.Run(ctx, path, args, stdin)
}

// namedDev 给 tun.Pipe 的一端起 utun<N> 的名字。
type namedDev struct {
	tun.Device
	name string
}

func (d namedDev) Name() string { return d.name }

const testMTU = 1400

type realKit struct {
	log  *cmdLog
	sim  *sysnet.SimRunner
	devs []tun.Device // 打开过的设备（测试里从外面关掉一个，模拟 utun 意外消失）
	mu   sync.Mutex
	seq  int
}

// realParams 是测试用的真实系统参数：模拟器 + FakePF + tun.Pipe；虚拟机等仍是假实现。
func (e *env) realParams(k *realKit) RealParams {
	k.sim = sysnet.NewSimRunner(e.sys.Net)
	return RealParams{
		PFRunner: recPF{R: e.sys.PF, log: k.log},
		Net:      recNet{R: k.sim, log: k.log},
		OpenTun: func() (tun.Device, error) {
			a, _ := tun.Pipe(testMTU)
			k.mu.Lock()
			k.seq++
			d := namedDev{Device: a, name: fmt.Sprintf("utun%d", 20+k.seq)}
			k.devs = append(k.devs, d)
			k.mu.Unlock()
			return d, nil
		},
		LookupUID: func(u string) (string, error) {
			if u == "_abroad" {
				return "250", nil
			}
			return "", errors.New("没有这个用户")
		},
		Ifaces: func() ([]sysnet.IfaceInfo, error) {
			if l := e.ifaces.Load(); l != nil { // 测试里换网络
				return *l, nil
			}
			return []sysnet.IfaceInfo{
				{Name: "en0", Up: true, Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.1.23/24")}},
				{Name: "bridge100", Up: true, Addrs: []netip.Prefix{netip.MustParsePrefix("192.168.64.1/24")}},
			}, nil
		},
		FirstHop: func() (string, error) { return "203.0.113.5", nil },
		// 设备关闭（进程退出或恢复时关掉）时，经它的路由随之消失（操作系统的行为）。
		OnTunClose: func(d tun.Device) { e.sys.Net.RemoveRoutesVia(d.Name()) },
		VM:         e.sys.VMs, Exit: e.sys.ExitCheck, Teardown: e.sys.Tear, Killer: e.sys.Kill,
		// 地区（M4-5）：Mac 一侧是真实实现 region.Mac，命令经模拟器；登录用户来自假系统。
		VMRegion: e.sys.VMRegion,
		Console: func() (string, int, error) {
			if c := e.sys.Net.Snapshot().Console; c != "" {
				return c, 501, nil
			}
			return "", 0, sysnet.ErrNoConsoleUser
		},
	}
}

func newRealEnv(t *testing.T) (*env, *realKit) {
	e := newEnv(t)
	k := &realKit{log: &cmdLog{}}
	sys, err := NewRealSystem(e.realParams(k))
	if err != nil {
		t.Fatal(err)
	}
	// 冲突检查用真实的检查器：它要的只读命令由模拟器按“没有冲突”的样本作答（M2-9b-2 第 0 步）。
	e.system = &sys
	return e, k
}

// bothSystems 让一组用例分别在假系统和真实系统（模拟器）上跑。
func bothSystems(t *testing.T, f func(*testing.T, *env)) {
	t.Run("fake", func(t *testing.T) { f(t, newEnv(t)) })
	t.Run("real", func(t *testing.T) {
		e, k := newRealEnv(t)
		f(t, e)
		if u := k.sim.Unknown(); len(u) > 0 {
			t.Fatalf("模拟器收到计划外的命令：%v", u)
		}
		auditAll(t, k.log.all())
	})
}

// c. 额外断言：ifconfig 的 MTU 等于设备 MTU；PrintMatcher 用注入的 uid；加载的规则里放行的是配置的用户。
func TestRealSystemWiring(t *testing.T) {
	e, k := newRealEnv(t)
	d := e.start(e.opts())
	post(t, d, "/v1/enable", 200)
	s := status(t, d)
	if !s.Protected || !strings.HasPrefix(s.Utun, "utun2") {
		t.Fatalf("%+v", s)
	}
	cfg := k.sim.Ifconfig(s.Utun)
	if len(cfg) != 7 || cfg[5] != strconv.Itoa(testMTU) || cfg[2] != "198.18.0.1" {
		t.Fatalf("ifconfig 参数：%v", cfg)
	}
	k.log.mu.Lock()
	loads := slices.Clone(k.log.loads)
	k.log.mu.Unlock()
	if len(loads) == 0 {
		t.Fatal("应加载过规则")
	}
	for _, l := range loads {
		if !strings.Contains(string(l), "user _abroad") {
			t.Fatalf("规则里放行的用户不对：\n%s", l)
		}
	}
	// PrintMatcher 用注入的 uid：uid 对不上时，加载后复核不一致，启用失败。
	e2 := newEnv(t)
	k2 := &realKit{log: &cmdLog{}}
	p := e2.realParams(k2)
	p.LookupUID = func(string) (string, error) { return "999", nil } // FakePF 把 _abroad 打成 250
	sys, err := NewRealSystem(p)
	if err != nil {
		t.Fatal(err)
	}
	sys.Preflight = allowAll{}
	e2.system = &sys
	d2 := e2.start(e2.opts())
	if code, _ := call(t, d2, "POST", "/v1/enable", nil); code == 200 {
		t.Fatal("uid 对不上时复核应失败")
	}
	e.noViolations()
}

// utun 意外消失（设备从外面被关掉）：Alive 为假，监护一轮内重建出新的 utun。
func TestRealTunDies(t *testing.T) {
	e, _ := newRealEnv(t)
	o := e.opts()
	o.SuperviseInterval = 30 * time.Millisecond
	d := e.start(o)
	post(t, d, "/v1/enable", 200)
	old := status(t, d).Utun
	// 设备意外消失：别处把它关掉了（trackedDev 记下），经它的路由随之消失。
	rt := d.tun.(*realTun)
	rt.mu.Lock()
	td := rt.dev
	rt.mu.Unlock()
	_ = td.Close()
	e.sys.Net.RemoveRoutesVia(old)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s := status(t, d); s.Utun != "" && s.Utun != old && s.GateOpen {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("没有重建 utun：%+v", status(t, d))
}

// ---- 命令审计（d）----

var (
	reUtun = regexp.MustCompile(`utun[0-9]+`)
	reTok  = regexp.MustCompile(`-X [0-9]+`)
)

// shape 把一条命令归成形态：utun 编号、token、网络服务名、MTU 换成占位符。
func shape(c string) string {
	c = reUtun.ReplaceAllString(c, "utun<N>")
	c = reTok.ReplaceAllString(c, "-X <数字>")
	if f := strings.Fields(c); len(f) >= 3 && f[0] == sysnet.PathNetworksetup && f[1] != "-listallnetworkservices" {
		c = f[0] + " " + f[1] + " <服务>"
		if f[1] == "-setv6manual" {
			c += " <地址> <前缀> <路由器>"
		}
	}
	c = regexp.MustCompile(`mtu [0-9]+`).ReplaceAllString(c, "mtu <MTU>")
	// 地区（M4-5）：具体的时区、语言、地区格式、用户名、uid 归一化。
	c = regexp.MustCompile(`-settimezone \S+`).ReplaceAllString(c, "-settimezone <时区>")
	c = regexp.MustCompile(`asuser [0-9]+ /usr/bin/sudo -u \S+`).ReplaceAllString(c, "asuser <uid> /usr/bin/sudo -u <用户>")
	c = regexp.MustCompile(`-array( \S+)+$`).ReplaceAllString(c, "-array <语言…>")
	c = regexp.MustCompile(`-string \S+$`).ReplaceAllString(c, "-string <地区格式>")
	return c
}

var pfAllowed = []*regexp.Regexp{
	regexp.MustCompile(`^/sbin/pfctl -s info$`),
	regexp.MustCompile(`^/sbin/pfctl -s rules$`),
	regexp.MustCompile(`^/sbin/pfctl -a com\.apple/250\.abroad -s rules$`),
	regexp.MustCompile(`^/sbin/pfctl -a com\.apple/250\.abroad -t abroad_lan -T show$`),
	regexp.MustCompile(`^/sbin/pfctl -o none -a com\.apple/250\.abroad -f -$`),
	regexp.MustCompile(`^/sbin/pfctl -a com\.apple/250\.abroad -F all$`),
	regexp.MustCompile(`^/sbin/pfctl -F states$`),
	regexp.MustCompile(`^/sbin/pfctl -E$`),
	regexp.MustCompile(`^/sbin/pfctl -X [0-9]+$`),
}

var netAllowed = []*regexp.Regexp{
	regexp.MustCompile(`^/sbin/route -n (add|change) -net (0\.0\.0\.0/1|128\.0\.0\.0/1) -interface utun[0-9]+$`),
	regexp.MustCompile(`^/sbin/route -n delete -net (0\.0\.0\.0/1|128\.0\.0\.0/1)$`),
	regexp.MustCompile(`^/sbin/route -n get [0-9.]+$`),
	regexp.MustCompile(`^/usr/sbin/netstat -rn -f inet$`),
	// 冲突检查的只读命令（M2-8 的只读白名单）。
	regexp.MustCompile(`^/usr/sbin/scutil --(proxy|nwi)$`),
	// 启用前读本机 DNS（M3-7，只读：scutil --dns 不带 stdin，只打印配置）。
	regexp.MustCompile(`^/usr/sbin/scutil --dns$`),
	regexp.MustCompile(`^/bin/ps -axo comm$`),
	regexp.MustCompile(`^/sbin/route -n get default$`),
	regexp.MustCompile(`^/usr/sbin/networksetup -listallnetworkservices$`),
	regexp.MustCompile(`^/usr/sbin/networksetup -(getinfo|setv6off|setv6automatic|setv6LinkLocal) .+$`),
	regexp.MustCompile(`^/usr/sbin/networksetup -setv6manual .+ \S+ [0-9]+ \S+$`),
	regexp.MustCompile(`^/usr/sbin/scutil <<show State:/Network/Service/claude-abroad/DNS; quit$`),
	regexp.MustCompile(`^/usr/sbin/scutil <<remove State:/Network/Service/claude-abroad/DNS; quit$`),
	regexp.MustCompile(`^/usr/sbin/scutil <<d\.init; d\.add ServerAddresses \* 198\.18\.0\.2; d\.add SupplementalMatchDomains \* ""; set State:/Network/Service/claude-abroad/DNS; quit$`),
	regexp.MustCompile(`^/sbin/ifconfig utun[0-9]+ inet 198\.18\.0\.1 198\.18\.0\.1 mtu [0-9]+ up$`),
	// 地区（M4-5，m4-plan v2.4 决定 7）：时区、自动时区、系统级的语言、当前登录用户的语言和地区格式。
	regexp.MustCompile(`^/usr/sbin/systemsetup -gettimezone$`),
	regexp.MustCompile(`^/usr/sbin/systemsetup -settimezone [A-Za-z][A-Za-z0-9_+/\-]*$`),
	regexp.MustCompile(`^/usr/bin/defaults (read|delete) /Library/Preferences/com\.apple\.timezone\.auto Active$`),
	regexp.MustCompile(`^/usr/bin/defaults write /Library/Preferences/com\.apple\.timezone\.auto Active -bool (true|false)$`),
	regexp.MustCompile(`^/usr/bin/defaults (read|delete) /Library/Preferences/\.GlobalPreferences AppleLanguages$`),
	regexp.MustCompile(`^/usr/bin/defaults write /Library/Preferences/\.GlobalPreferences AppleLanguages -array( [A-Za-z0-9][A-Za-z0-9_@=;.+\-]*)+$`),
	regexp.MustCompile(`^/bin/launchctl asuser [0-9]+ /usr/bin/sudo -u [a-z_][a-z0-9_.\-]* /usr/bin/defaults (read|delete) -g (AppleLanguages|AppleLocale)$`),
	regexp.MustCompile(`^/bin/launchctl asuser [0-9]+ /usr/bin/sudo -u [a-z_][a-z0-9_.\-]* /usr/bin/defaults write -g AppleLanguages -array( [A-Za-z0-9][A-Za-z0-9_@=;.+\-]*)+$`),
	regexp.MustCompile(`^/bin/launchctl asuser [0-9]+ /usr/bin/sudo -u [a-z_][a-z0-9_.\-]* /usr/bin/defaults write -g AppleLocale -string [A-Za-z0-9][A-Za-z0-9_@=;.+\-]*$`),
}

// auditAll 逐条检查命令（d）：路径、形态都在允许的范围内。
func auditAll(t *testing.T, cmds []string) {
	t.Helper()
	for _, c := range cmds {
		ok := false
		for _, re := range append(slices.Clone(pfAllowed), netAllowed...) {
			if re.MatchString(c) {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("命令形态不在允许的范围内：%q", c)
		}
		for _, bad := range []string{"pfctl -d", "pfctl -e"} {
			if strings.HasSuffix(c, bad) || strings.Contains(c, bad+" ") {
				t.Errorf("出现了 %s：%q", bad, c)
			}
		}
	}
}

// d. 全流程（启用、停用、再启用、监护修锚点、被杀后重启、恢复）的命令形态与 golden 文件一致。
// 有新形态出现时测试失败，人看过之后用 UPDATE_GOLDEN=1 重新生成。
func TestCommandAudit(t *testing.T) {
	e, k := newRealEnv(t)
	o := e.opts()
	o.SuperviseInterval = 20 * time.Millisecond
	d := e.start(o)
	// 地区（M4-5）：先手动指定目标，启用时同步、恢复时写回（命令经模拟器，进 golden）。
	if code, b := call(t, d, "PUT", "/v1/region", []byte(`{"mode":"manual","timezone":"Asia/Tokyo","lang":"ja-JP"}`)); code != 200 {
		t.Fatalf("切换地区：%d %s", code, b)
	}
	post(t, d, "/v1/enable", 200)
	post(t, d, "/v1/disable", 200)
	post(t, d, "/v1/enable", 200)
	e.sys.PF.ClearAnchor()
	deadline := time.Now().Add(3 * time.Second)
	for !e.sys.PF.Active(pf.DefaultAnchor) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// 别的软件把 0.0.0.0/1 改到 utun9：监护用 route change 改回（让 change 出现在命令清单里，M2-9b-2 第 0 步）。
	n01 := netip.MustParsePrefix("0.0.0.0/1")
	ours := status(t, d).Utun
	e.sys.Net.SetForeignRoute(n01, "utun9")
	deadline = time.Now().Add(3 * time.Second)
	for e.sys.Net.Snapshot().Routes[n01] != ours && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if e.sys.Net.Snapshot().Routes[n01] != ours {
		t.Fatal("监护没有把 0.0.0.0/1 改回")
	}
	d.abandon()
	e.d = nil
	e.cur.Store(nil)
	d = e.start(o)
	if s := waitGateOpen(t, d); s.State != "ENABLED" || !s.GateOpen {
		t.Fatalf("%+v", s)
	}
	post(t, d, "/v1/restore", 200)
	if !e.sys.Net.Snapshot().Equal(e.init) {
		t.Fatal("恢复后应与初始值相等")
	}
	cmds := k.log.all()
	auditAll(t, cmds)
	set := map[string]bool{}
	for _, c := range cmds {
		set[shape(c)] = true
	}
	got := strings.Join(slices.Sorted(func(yield func(string) bool) {
		for s := range set {
			if !yield(s) {
				return
			}
		}
	}), "\n") + "\n"
	golden := filepath.Join("testdata", "commands.golden")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("没有 golden 文件（UPDATE_GOLDEN=1 生成）：%v", err)
	}
	if string(want) != got {
		t.Fatalf("命令形态和 golden 不一致（新形态需要人看过后再更新）：\n--- 期望\n%s\n--- 实际\n%s", want, got)
	}
	if u := k.sim.Unknown(); len(u) > 0 {
		t.Fatalf("模拟器收到计划外的命令：%v", u)
	}
	e.noViolations()
}

// e. 查不到放行用户：返回 usererror，命令数为 0。
func TestRealSystemNoUser(t *testing.T) {
	e := newEnv(t)
	k := &realKit{log: &cmdLog{}}
	p := e.realParams(k)
	p.User = "_nobody_here"
	_, err := NewRealSystem(p)
	if !errors.Is(err, ErrNoAllowUser) || !strings.Contains(err.Error(), "失败：") {
		t.Fatalf("%v", err)
	}
	if c := k.log.all(); len(c) != 0 {
		t.Fatalf("不应发出命令：%v", c)
	}
}

// e. 没有主代理时启用被拒绝，提示先应用主代理，系统没有改动。
func TestRealSystemNoMainProxy(t *testing.T) {
	e := newEnv(t)
	k := &realKit{log: &cmdLog{}}
	p := e.realParams(k)
	p.FirstHop = nil
	sys, err := NewRealSystem(p)
	if err != nil {
		t.Fatal(err)
	}
	e.system = &sys
	d := e.start(e.opts())
	code, b := callOp(t, d, "/v1/enable")
	if code == 200 || !strings.Contains(string(b), "前提：主代理已应用") {
		t.Fatalf("%d %s", code, b)
	}
	if _, err := os.Stat(filepath.Join(e.dir, state.FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("不应写状态文件")
	}
	for _, c := range k.log.all() {
		if !strings.Contains(c, " -s ") && !strings.Contains(c, " -t ") {
			t.Fatalf("不应有改动类命令：%q", c)
		}
	}
	if !e.sys.Net.Snapshot().Equal(e.init) || e.sys.PF.Active(pf.DefaultAnchor) {
		t.Fatal("系统不应有改动")
	}
}

// f（9b-1）、i（9b-2）. 生产执行器：非 root 时返回错误；全仓库对 ProductionRunners 的调用恰好一处，在 cmd/abroadd/real.go。
func TestProductionRunners(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("以 root 运行时不测")
	}
	p, n, err := ProductionRunners()
	if !errors.Is(err, ErrNotRoot) || p != nil || n != nil {
		t.Fatalf("%v %v %v", p, n, err)
	}
	allowed, _ := filepath.Abs(filepath.Join("..", "..", "cmd", "abroadd", "real.go"))
	calls := 0
	filepath.WalkDir(filepath.Join("..", ".."), func(path string, de os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if de.IsDir() {
			if path != filepath.Join("..", "..") && (de.Name() == "third_party" || de.Name() == "out" || strings.HasPrefix(de.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return nil
		}
		abs, _ := filepath.Abs(path)
		ast.Inspect(f, func(n ast.Node) bool {
			if c, ok := n.(*ast.CallExpr); ok {
				name := ""
				switch fn := c.Fun.(type) {
				case *ast.Ident:
					name = fn.Name
				case *ast.SelectorExpr:
					name = fn.Sel.Name
				}
				if name == "ProductionRunners" {
					if abs != allowed {
						t.Errorf("%s 调用了 ProductionRunners（只允许 cmd/abroadd/real.go）", path)
					}
					calls++
				}
			}
			return true
		})
		return nil
	})
	if calls != 1 {
		t.Fatalf("对 ProductionRunners 的调用应恰好一处，实际 %d 处", calls)
	}
}

// a. 没有主代理存储（空的 profile 目录）时启用被拒绝，提示先应用主代理，系统没有改动。
func TestRealSystemEmptyMainProxyStore(t *testing.T) {
	e := newEnv(t)
	k := &realKit{log: &cmdLog{}}
	p := e.realParams(k)
	p.FirstHop = MainProxyFirstHop(t.TempDir())
	if _, err := p.FirstHop(); !errors.Is(err, ErrNoMainProxy) {
		t.Fatalf("空存储应返回 ErrNoMainProxy：%v", err)
	}
	sys, err := NewRealSystem(p)
	if err != nil {
		t.Fatal(err)
	}
	e.system = &sys
	d := e.start(e.opts())
	code, b := callOp(t, d, "/v1/enable")
	if code == 200 || !strings.Contains(string(b), "前提：主代理已应用") {
		t.Fatalf("%d %s", code, b)
	}
	if !e.sys.Net.Snapshot().Equal(e.init) || e.sys.PF.Active(pf.DefaultAnchor) {
		t.Fatal("系统不应有改动")
	}
}
