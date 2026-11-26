package rescue

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"claude-abroad/internal/claudeproc"
	"claude-abroad/internal/pf"
	"claude-abroad/internal/region"
	"claude-abroad/internal/state"
	"claude-abroad/internal/sysnet"
)

// rec 记录调用序列；fail 里的名字被调用时返回错误（注入失败）。
type rec struct {
	t     *testing.T
	store *state.Store
	seq   []string
	fail  map[string]bool
}

func (r *rec) do(name string) error {
	r.seq = append(r.seq, name)
	if r.fail[name] {
		return errors.New("注入的失败：" + name)
	}
	return nil
}

func (r *rec) net() int { // 网络相关的调用次数
	n := 0
	for _, s := range r.seq {
		for _, p := range []string{"pf.", "dns.", "route.", "ipv6.", "locale."} {
			if strings.HasPrefix(s, p) {
				n++
			}
		}
	}
	return n
}

type fClaude struct{ r *rec }

func (c fClaude) Preview(context.Context) (claudeproc.Preview, error) {
	return claudeproc.Preview{Processes: []claudeproc.Target{{PID: 42, App: "Claude"}}, WillKill: true}, c.r.do("claude.preview")
}
func (c fClaude) KillClaude(context.Context) error { return c.r.do("claude.kill") }

type fPF struct{ r *rec }

func (p fPF) Flush(context.Context) error {
	// 写成已回国在撤规则之前（验收 b）：清锚点时状态文件已经是 RESTORED。
	if f, err := p.r.store.Load(); err != nil || f.State != state.Restored {
		p.r.seq = append(p.r.seq, "!state-not-restored-before-flush")
	}
	return p.r.do("pf.flush")
}
func (p fPF) Release(_ context.Context, tok string) error { return p.r.do("pf.release " + tok) }
func (p fPF) Load(_ context.Context, rules []byte) error {
	want, _ := pf.Render(state.ChooseReturnBlock(p.r.store.Dir).Config()) // 和 abroadd、guard 同一个来源
	if !bytes.Equal(rules, want) {
		p.r.seq = append(p.r.seq, "!load-not-return-block")
	}
	return p.r.do("pf.load")
}

type fDNS struct{ r *rec }

func (d fDNS) Remove(context.Context) error { return d.r.do("dns.remove") }

type fRoutes struct{ r *rec }

func (x fRoutes) Delete(_ context.Context, p netip.Prefix) error {
	return x.r.do("route.delete " + p.String())
}

type fIPv6 struct{ r *rec }

func (x fIPv6) Restore(_ context.Context, svc string, s sysnet.IPv6Setting) error {
	return x.r.do("ipv6.restore " + svc + " " + string(s.Mode))
}

type fLocale struct {
	r   *rec
	cur sysnet.MacLocale
}

func (l *fLocale) Read(context.Context) (sysnet.MacLocale, error) { return l.cur, nil }
func (l *fLocale) SetTimezone(_ context.Context, tz string) error {
	if err := l.r.do("locale.tz " + tz); err != nil {
		return err
	}
	l.cur.Timezone = tz
	return nil
}
func (l *fLocale) SetAutoTZ(_ context.Context, v sysnet.OptStr) error {
	l.cur.AutoTZ = v
	return l.r.do("locale.autotz")
}
func (l *fLocale) SetUser(_ context.Context, u string, langs sysnet.OptList, loc sysnet.OptStr) error {
	l.cur.User, l.cur.UserLangs, l.cur.UserLocale = u, langs, loc
	return l.r.do("locale.user")
}
func (l *fLocale) SetSystem(_ context.Context, langs sysnet.OptList) error {
	l.cur.SysLangs = langs
	return l.r.do("locale.system")
}

var original = sysnet.MacLocale{Timezone: "Asia/Shanghai", AutoTZ: sysnet.Some("1"), User: "u", UserLangs: sysnet.SomeList("zh-Hans-CN"), UserLocale: sysnet.Some("zh_CN"), SysLangs: sysnet.SomeList("zh-Hans-CN")}
var target = sysnet.MacLocale{Timezone: "America/Los_Angeles", AutoTZ: sysnet.Some("0"), User: "u", UserLangs: sysnet.SomeList("en-US"), UserLocale: sysnet.Some("en_US"), SysLangs: sysnet.SomeList("en-US")}

type env struct {
	r    *rec
	dir  string
	loc  *fLocale
	out  bytes.Buffer
	d    Deps
	conf int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{dir: t.TempDir()}
	st := state.NewStore(filepath.Join(e.dir, "state"))
	e.r = &rec{t: t, store: st, fail: map[string]bool{}}
	e.loc = &fLocale{r: e.r, cur: target}
	e.d = Deps{
		Store: st, Claude: fClaude{e.r}, PF: fPF{e.r}, DNS: fDNS{e.r}, Routes: fRoutes{e.r}, IPv6: fIPv6{e.r}, Locale: e.loc,
		GuardTokens: func() ([]string, error) { return []string{"77"}, nil },
		DaemonAlive: func(context.Context) bool { return false },
		IsRoot:      func() bool { return true },
		Confirm:     func(claudeproc.Preview) bool { e.conf++; return true },
		Out:         &e.out,
	}
	return e
}

// enabled 写一份已启用的状态文件：两个启用引用、一个网络服务的 IPv6 原值（另一个原来就关着）、地区原值和目标。
func (e *env) enabled(t *testing.T) {
	t.Helper()
	tg := region.Target{Timezone: "America/Los_Angeles", Lang: "en-US", Locale: "en_US", Source: region.SourceExit}
	o := original
	if err := e.d.Store.Save(state.File{Version: state.Version, State: state.Enabled, Step: 9, PFTokens: []string{"11", "12"},
		IPv6Original: map[string]sysnet.IPv6Setting{"Wi-Fi": {Mode: sysnet.IPv6Automatic}, "Thunderbolt": {Mode: sysnet.IPv6Off}},
		Region:       state.RegionFile{Original: &o, Target: &tg}}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) run() int { e.r.seq = nil; e.out.Reset(); return Run(context.Background(), e.d) }

func (e *env) final(t *testing.T) state.File {
	t.Helper()
	f, err := e.d.Store.Load()
	if err != nil {
		t.Fatalf("最后的状态文件读不出：%v", err)
	}
	return f
}

// a. 前提不满足：什么都不改（调用次数为 0）。
func TestRefusals(t *testing.T) {
	cases := map[string]func(e *env) func(){
		"不是 root": func(e *env) func() { e.d.IsRoot = func() bool { return false }; return func() {} },
		"abroadd 持有锁": func(e *env) func() {
			other := state.NewStore(e.d.Store.Dir)
			rel, err := other.Lock()
			if err != nil {
				t.Fatal(err)
			}
			return rel
		},
		"abroadd 连得上": func(e *env) func() { e.d.DaemonAlive = func(context.Context) bool { return true }; return func() {} },
		"你没有确认":       func(e *env) func() { e.d.Confirm = func(claudeproc.Preview) bool { return false }; return func() {} },
	}
	for name, prep := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.enabled(t)
			before, _ := os.ReadFile(filepath.Join(e.d.Store.Dir, state.FileName))
			done := prep(e)
			defer done()
			if code := e.run(); code != ExitRefused {
				t.Fatalf("退出码 %d：%s", code, e.out.String())
			}
			for _, s := range e.r.seq {
				if s != "claude.preview" { // 确认之前只列清单
					t.Fatalf("不应有任何改动：%v", e.r.seq)
				}
			}
			after, _ := os.ReadFile(filepath.Join(e.d.Store.Dir, state.FileName))
			if !bytes.Equal(before, after) {
				t.Fatal("状态文件不应改动")
			}
			if !strings.Contains(e.out.String(), "什么都没改") {
				t.Fatalf("输出应说明什么都没改：%s", e.out.String())
			}
		})
	}
}

// b. 顺序固定：结束 Claude 在任何网络操作之前；写已回国在撤规则之前；六步按序。
func TestOrder(t *testing.T) {
	e := newEnv(t)
	e.enabled(t)
	if code := e.run(); code != ExitOK {
		t.Fatalf("退出码 %d：%s", code, e.out.String())
	}
	want := []string{"claude.preview", "claude.kill", "pf.flush", "pf.release 11", "pf.release 12", "pf.release 77",
		"dns.remove", "route.delete 0.0.0.0/1", "route.delete 128.0.0.0/1", "ipv6.restore Wi-Fi automatic",
		"locale.system", "locale.user", "locale.autotz", "locale.tz Asia/Shanghai"}
	if strings.Join(e.r.seq, "|") != strings.Join(want, "|") {
		t.Fatalf("调用序列：\n%v\n应为：\n%v", e.r.seq, want)
	}
	if e.conf != 1 {
		t.Fatal("应确认一次")
	}
	f := e.final(t)
	if f.State != state.Restored || leftovers(f, false) {
		t.Fatalf("最后应是干净的已回国：%+v", f)
	}
	if !e.loc.cur.Equal(original) {
		t.Fatalf("地区应写回原值：%+v", e.loc.cur)
	}
	for _, w := range []string{"第 1 步", "第 2 步", "第 3 步", "第 4 步", "第 5 步", "第 6 步", "应急恢复完成"} {
		if !strings.Contains(e.out.String(), w) {
			t.Fatalf("输出里应有“%s”：%s", w, e.out.String())
		}
	}
	// 再执行一次：已经是干净的已回国，什么都不做
	if code := e.run(); code != ExitOK || len(e.r.seq) != 0 {
		t.Fatalf("再执行一次不应有调用：%d %v", code, e.r.seq)
	}
	// --yes：不问确认，清单照样打印
	e2 := newEnv(t)
	e2.enabled(t)
	e2.d.Yes = true
	if code := e2.run(); code != ExitOK || e2.conf != 0 || !strings.Contains(e2.out.String(), "42  Claude") {
		t.Fatalf("--yes：%d conf=%d %s", code, e2.conf, e2.out.String())
	}
}

// c. 结束 Claude 失败：中止，网络调用 0 次，状态文件不变，退出码非 0，写明原因和下一步。
func TestKillFails(t *testing.T) {
	e := newEnv(t)
	e.enabled(t)
	e.r.fail["claude.kill"] = true
	before, _ := os.ReadFile(filepath.Join(e.d.Store.Dir, state.FileName))
	if code := e.run(); code != ExitFailed {
		t.Fatalf("退出码 %d", code)
	}
	if e.r.net() != 0 {
		t.Fatalf("不应碰网络：%v", e.r.seq)
	}
	after, _ := os.ReadFile(filepath.Join(e.d.Store.Dir, state.FileName))
	if !bytes.Equal(before, after) {
		t.Fatal("状态文件不应改动")
	}
	if !strings.Contains(e.out.String(), "网络没有改动") || !strings.Contains(e.out.String(), "手动退出 Claude") {
		t.Fatalf("应写明原因和下一步：%s", e.out.String())
	}
}

// d. 每一步注入失败：停在那一步、退出码非 0；去掉失败再执行一次能从头走完，结果相同。
func TestFailEachStepThenRetry(t *testing.T) {
	for _, step := range []string{"pf.flush", "dns.remove", "route.delete 128.0.0.0/1", "ipv6.restore Wi-Fi automatic"} {
		t.Run(step, func(t *testing.T) {
			e := newEnv(t)
			e.enabled(t)
			e.r.fail[step] = true
			if code := e.run(); code != ExitFailed {
				t.Fatalf("退出码 %d", code)
			}
			if last := e.r.seq[len(e.r.seq)-1]; last != step {
				t.Fatalf("应停在 %s：%v", step, e.r.seq)
			}
			if f := e.final(t); f.State != state.Restored {
				t.Fatalf("第 3 步之后的失败，状态文件已是已回国：%s", f.State)
			}
			if !strings.Contains(e.out.String(), "停在这一步") {
				t.Fatal("应说明停在哪一步、怎么办")
			}
			delete(e.r.fail, step)
			if code := e.run(); code != ExitOK {
				t.Fatalf("重试退出码 %d：%s", code, e.out.String())
			}
			if e.r.seq[1] != "claude.kill" || !strings.Contains(strings.Join(e.r.seq, "|"), "dns.remove") {
				t.Fatalf("重试应从头走：%v", e.r.seq)
			}
			if f := e.final(t); f.State != state.Restored || leftovers(f, false) || !e.loc.cur.Equal(original) {
				t.Fatalf("重试后结果应相同：%+v %+v", f, e.loc.cur)
			}
		})
	}
}

// e. 状态文件四种情况：都能走完，最后是已回国。
func TestStateFileCases(t *testing.T) {
	cases := map[string]func(e *env, p string){
		"已启用": func(e *env, _ string) { e.enabled(t) },
		"已停用": func(e *env, _ string) {
			if err := e.d.Store.Save(state.File{Version: state.Version, State: state.Disabled, PFTokens: []string{"5"}}); err != nil {
				t.Fatal(err)
			}
		},
		"截断": func(e *env, p string) {
			e.enabled(t)
			b, _ := os.ReadFile(p)
			_ = os.WriteFile(p, b[:len(b)/2], 0o600)
		},
		"版本不认识": func(e *env, p string) {
			_ = os.MkdirAll(filepath.Dir(p), 0o700)
			_ = os.WriteFile(p, []byte(`{"version":99,"state":"ENABLED"}`), 0o600)
		},
	}
	for name, prep := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			p := filepath.Join(e.d.Store.Dir, state.FileName)
			prep(e, p)
			if code := e.run(); code != ExitOK {
				t.Fatalf("退出码 %d：%s", code, e.out.String())
			}
			if f := e.final(t); f.State != state.Restored {
				t.Fatalf("最后应是已回国：%s", f.State)
			}
			if !strings.Contains(strings.Join(e.r.seq, "|"), "pf.flush") || !strings.Contains(strings.Join(e.r.seq, "|"), "dns.remove") {
				t.Fatalf("网络各步应完成：%v", e.r.seq)
			}
		})
	}
}

// f. 待还原记录读不出来：网络各步完成，地区没有动，输出里有明确的提示。
func TestRecordsUnreadable(t *testing.T) {
	e := newEnv(t)
	e.enabled(t)
	p := filepath.Join(e.d.Store.Dir, state.FileName)
	_ = os.WriteFile(p, []byte("{"), 0o600)
	if code := e.run(); code != ExitOK {
		t.Fatalf("退出码 %d：%s", code, e.out.String())
	}
	for _, s := range e.r.seq {
		if strings.HasPrefix(s, "locale.") || strings.HasPrefix(s, "ipv6.") {
			t.Fatalf("记录读不出来时不应动地区和 IPv6：%v", e.r.seq)
		}
	}
	if !strings.Contains(strings.Join(e.r.seq, "|"), "route.delete 128.0.0.0/1") || !e.loc.cur.Equal(target) {
		t.Fatalf("网络各步应完成、地区不动：%v", e.r.seq)
	}
	if !strings.Contains(e.out.String(), "请手动核对") || !strings.Contains(e.out.String(), "emergency.md") {
		t.Fatalf("应明确提示手动核对和应急文档：%s", e.out.String())
	}
}

// g. 不碰解析锁和托管设置：前后目录快照相同（应急程序根本拿不到它们的路径；这里再核对一遍）。
func TestLeavesLaunchControlAlone(t *testing.T) {
	e := newEnv(t)
	e.enabled(t)
	res := filepath.Join(e.dir, "resolver")
	cc := filepath.Join(e.dir, "managed-settings.d")
	for _, d := range []string{res, cc} {
		_ = os.MkdirAll(d, 0o755)
	}
	_ = os.WriteFile(filepath.Join(res, "claude.ai"), []byte("nameserver 198.18.0.2\n"), 0o644)
	_ = os.WriteFile(filepath.Join(cc, "claude-abroad.json"), []byte("{}\n"), 0o644)
	snap := func() string {
		var b strings.Builder
		for _, d := range []string{res, cc} {
			ents, _ := os.ReadDir(d)
			for _, x := range ents {
				c, _ := os.ReadFile(filepath.Join(d, x.Name()))
				b.WriteString(x.Name() + "=" + string(c))
			}
		}
		return b.String()
	}
	before := snap()
	if code := e.run(); code != ExitOK {
		t.Fatal(code)
	}
	if snap() != before {
		t.Fatal("解析锁和托管设置不应改动")
	}
	if !strings.Contains(e.out.String(), "解析锁和 Claude Code 的托管设置没有动") {
		t.Fatal("输出应说明没有动启动管控")
	}
}

// 第 6 步地区有几项没能写回：记进待还原文件，abroadd 下次启动后重试；状态仍是已回国。
func TestRegionPartial(t *testing.T) {
	e := newEnv(t)
	e.enabled(t)
	e.r.fail["locale.tz Asia/Shanghai"] = true
	if code := e.run(); code != ExitOK {
		t.Fatalf("退出码 %d：%s", code, e.out.String())
	}
	p, ok, err := e.d.Store.LoadPending()
	if err != nil || !ok || len(p.Items) != 1 || p.Timezone != "Asia/Shanghai" {
		t.Fatalf("应记下没还原的时区：%+v %v %v", p, ok, err)
	}
	if !strings.Contains(e.out.String(), "没能还原") {
		t.Fatal("应说明哪几项没能还原")
	}
	// 再执行一次：按待还原文件写回，删掉文件
	delete(e.r.fail, "locale.tz Asia/Shanghai")
	if code := e.run(); code != ExitOK {
		t.Fatal(code)
	}
	if _, ok, _ := e.d.Store.LoadPending(); ok || e.loc.cur.Timezone != "Asia/Shanghai" {
		t.Fatalf("重试后应写回并删掉待还原文件：%+v", e.loc.cur)
	}
}

// i. abroadd 持有锁时应急程序退出；abroadd 被强制结束（进程没了，锁随文件描述符一起释放）之后应急程序能拿到锁、走完。
// 用一个真的子进程（本测试程序自己，带环境变量）模拟 abroadd 持锁。
func TestLockHolderKilled(t *testing.T) {
	if dir := os.Getenv("RESCUE_TEST_HOLD_LOCK"); dir != "" {
		if _, err := state.NewStore(dir).Lock(); err != nil {
			os.Exit(3)
		}
		os.Stdout.WriteString("locked\n")
		select {} // 一直持锁，直到被结束
	}
	e := newEnv(t)
	e.enabled(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHolderKilled$")
	cmd.Env = append(os.Environ(), "RESCUE_TEST_HOLD_LOCK="+e.d.Store.Dir)
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	if line != "locked\n" {
		_ = cmd.Process.Kill()
		t.Fatalf("子进程没拿到锁：%q", line)
	}
	if code := e.run(); code != ExitRefused || len(e.r.seq) != 0 {
		_ = cmd.Process.Kill()
		t.Fatalf("持锁时应拒绝、什么都不做：%d %v", code, e.r.seq)
	}
	_ = cmd.Process.Kill() // 强制结束（SIGKILL）
	_ = cmd.Wait()
	if code := e.run(); code != ExitOK {
		t.Fatalf("持锁的进程没了之后应能走完：%d %s", code, e.out.String())
	}
}

// M6-11b 验收 c：首次准备做过、没卸载 → 第 4 步换成回国拦截那一组（没有清空）、留最新的一个启用引用（开机 guard 的），
// 其余释放，状态文件里记着它；否则清空并全部释放（TestOrder）。
func TestReturnBlockKept(t *testing.T) {
	for _, uninst := range []bool{false, true} {
		e := newEnv(t)
		e.enabled(t)
		_ = os.WriteFile(filepath.Join(e.d.Store.Dir, state.PreparedFile), []byte("x\n"), 0o600)
		_ = state.WriteReturnBlockSet(e.d.Store.Dir, pf.SetReal)
		if uninst {
			_ = os.WriteFile(filepath.Join(e.d.Store.Dir, state.UninstalledFile), []byte("x\n"), 0o600)
		}
		if code := e.run(); code != ExitOK {
			t.Fatalf("退出码 %d：%s", code, e.out.String())
		}
		seq := strings.Join(e.r.seq, "|")
		f := e.final(t)
		if uninst {
			if !strings.Contains(seq, "pf.flush|pf.release 11|pf.release 12|pf.release 77") || strings.Contains(seq, "pf.load") || len(f.PFTokens) != 0 {
				t.Fatalf("卸载过：应清空并全部释放：%s %v", seq, f.PFTokens)
			}
			continue
		}
		if !strings.Contains(seq, "claude.kill|pf.load|pf.release 11|pf.release 12|dns.remove") || strings.Contains(seq, "pf.flush") || strings.Contains(seq, "!") {
			t.Fatalf("应换组、留 77、释放其余：%s", seq)
		}
		if f.State != state.Restored || len(f.PFTokens) != 1 || f.PFTokens[0] != "77" || leftovers(f, true) {
			t.Fatalf("状态文件应记着留下的那一个：%+v", f)
		}
		if !strings.Contains(e.out.String(), "回国拦截") {
			t.Fatalf("输出应说明换成了回国拦截：%s", e.out.String())
		}
		// 再执行一次：没有要恢复的（留一个引用是正常的）
		if code := e.run(); code != ExitOK || len(e.r.seq) != 0 {
			t.Fatalf("第二次应什么都不做：%d %v", code, e.r.seq)
		}
	}
}
