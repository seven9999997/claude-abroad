package envcheck

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"claude-abroad/internal/exitpolicy"
	"claude-abroad/internal/procinfo"
	"claude-abroad/internal/region"
	"claude-abroad/internal/sysnet"
)

func skipped(d string) Outcome { return Outcome{Result: Skipped, Detail: d} }

// ---- 04 时区、05 语言（m4-plan v2.7 决定 3）----

// ErrBusy 是改回地区时拿不到操作锁（本轮记无法判断）。
var ErrBusy = errors.New("有操作在进行")

// RegionDeps 是 04、05 的依赖。Target 返回 false（带 --dev-skip-region、没有目标）时这两项“已跳过”。
type RegionDeps struct {
	Read    func(ctx context.Context) (sysnet.MacLocale, error)
	Target  func() (region.Target, bool)
	Reapply func(ctx context.Context) error // 状态机的“改回地区”：只写不同的项，不结束 Claude；忙时返回包着 ErrBusy 的错误
}

// RegionFixer 记连续改回的次数（同一项连续 3 轮都要靠改回时提示“可能有别的软件在改”）。
type RegionFixer struct {
	D  RegionDeps
	mu sync.Mutex
	n  map[string]int
	// Fixed 记每一项累计改回的次数（测试和诊断用）。
	Fixed map[string]int
}

// SuspectAfter 是连续改回多少轮后提示。
const SuspectAfter = 3

func (f *RegionFixer) check(ctx context.Context, id string, ok func(sysnet.MacLocale, region.Target) bool, what, fix string) Outcome {
	t, on := (region.Target{}), false
	if f.D.Target != nil {
		t, on = f.D.Target()
	}
	if !on {
		return skipped("已跳过：没有同步地区")
	}
	cur, err := f.D.Read(ctx)
	if err != nil {
		return unk("无法判断：读不出 Mac 当前的设置")
	}
	f.mu.Lock()
	if f.n == nil {
		f.n, f.Fixed = map[string]int{}, map[string]int{}
	}
	f.mu.Unlock()
	if ok(cur, t) {
		f.mu.Lock()
		f.n[id] = 0
		f.mu.Unlock()
		return pass(what + "和目标一致")
	}
	if err := f.D.Reapply(ctx); err != nil {
		if errors.Is(err, ErrBusy) {
			return unk("无法判断：有别的操作在进行，没能改回")
		}
	}
	cur, err = f.D.Read(ctx)
	if err != nil {
		return unk("无法判断：读不出 Mac 当前的设置")
	}
	if !ok(cur, t) {
		return fail(what+"和目标不一致，自动改回也没成功", fix)
	}
	f.mu.Lock()
	f.n[id]++
	f.Fixed[id]++
	n := f.n[id]
	f.mu.Unlock()
	d := what + "被改过，已自动改回"
	if n >= SuspectAfter {
		d += "；已经连续 " + itoa(n) + " 轮要改回，可能有别的软件在改"
	}
	return pass(d)
}

// Timezone 是 04 时区（硬性，只看本机状态）：时区等于目标、自动时区是关的。
func Timezone(f *RegionFixer) Item {
	return Item{ID: "04", Name: "时区", Level: Hard, Run: func(ctx context.Context) Outcome {
		return f.check(ctx, "04", func(c sysnet.MacLocale, t region.Target) bool {
			return c.Timezone == t.Timezone && c.AutoTZ.Set && c.AutoTZ.V == "0"
		}, "时区和“自动设置时区”", "检查有没有别的软件在改时区；在“系统设置 → 通用 → 日期与时间”里关掉“自动设置时区”。")
	}}
}

// Language 是 05 系统语言与地区格式（硬性，只看本机状态）：当前登录用户的首选语言、地区格式等于目标。没有人登录时已跳过。
func Language(f *RegionFixer) Item {
	return Item{ID: "05", Name: "系统语言与地区格式", Level: Hard, Run: func(ctx context.Context) Outcome {
		return f.check(ctx, "05", func(c sysnet.MacLocale, t region.Target) bool {
			if c.User == "" {
				return true // 没有人登录：用户级不适用
			}
			first := ""
			if len(c.UserLangs.V) > 0 {
				first = c.UserLangs.V[0]
			}
			return first == t.Lang && c.UserLocale.Set && c.UserLocale.V == t.Locale
		}, "首选语言和地区格式", "检查有没有别的软件在改语言；在“系统设置 → 通用 → 语言与地区”里把目标语言放到第一位。")
	}}
}

// ---- 15 Claude 客户端语言（R10）、16 浏览器语言 ----

// HomeDeps 是 15、16 的依赖：当前登录用户的主目录、锁定的目标。
type HomeDeps struct {
	Home   func() (string, error)
	Target func() (region.Target, bool)
}

// ClaudeDesktopConfig 是桌面版保存界面语言的文件（相对用户主目录；R10 的调查结论：顶层 "locale"，形如 xx-XX）。
const ClaudeDesktopConfig = "Library/Application Support/Claude/config.json"

func sameLang(a, b string) bool {
	norm := func(s string) string { return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "_", "-")) }
	return norm(a) == norm(b)
}

func readJSON(path string, max int64, v any) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > max {
		return errors.New("文件不对或太大")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// ClaudeClientLang 是 15 Claude 客户端语言（警告，只看本机状态）。
func ClaudeClientLang(d HomeDeps) Item {
	return Item{ID: "15", Name: "Claude 客户端语言", Level: Warn, Run: func(ctx context.Context) Outcome {
		t, ok := d.Target()
		if !ok {
			return skipped("已跳过：没有同步地区")
		}
		home, err := d.Home()
		if err != nil {
			return unk("无法判断：找不到当前登录用户的主目录")
		}
		var cfg struct {
			Locale string `json:"locale"`
		}
		err = readJSON(filepath.Join(home, ClaudeDesktopConfig), 64<<10, &cfg)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return skipped("已跳过：没有找到 Claude 桌面版的配置（可能没装）")
		case err != nil:
			return unk("无法判断：Claude 桌面版的配置读不出来")
		case cfg.Locale == "":
			return pass("Claude 桌面版跟随系统语言")
		case !sameLang(cfg.Locale, t.Lang):
			return fail("Claude 桌面版的界面语言和目标语言不一致", "在 Claude 桌面版的设置里把界面语言改成目标语言（或改成跟随系统）。")
		}
		return pass("Claude 桌面版的界面语言和目标一致")
	}}
}

// browserPrefs 是要看的浏览器默认配置（相对用户主目录）。Safari 跟随系统，不单独查。
var browserPrefs = []struct{ name, path string }{
	{"Chrome", "Library/Application Support/Google/Chrome/Default/Preferences"},
	{"Edge", "Library/Application Support/Microsoft Edge/Default/Preferences"},
}

// BrowserLang 是 16 浏览器语言（警告，只看本机状态）：Chrome、Edge 默认配置里的首选语言。
func BrowserLang(d HomeDeps) Item {
	return Item{ID: "16", Name: "浏览器语言", Level: Warn, Run: func(ctx context.Context) Outcome {
		t, ok := d.Target()
		if !ok {
			return skipped("已跳过：没有同步地区")
		}
		home, err := d.Home()
		if err != nil {
			return unk("无法判断：找不到当前登录用户的主目录")
		}
		found := 0
		var bad []string
		for _, b := range browserPrefs {
			var p struct {
				Intl struct {
					AcceptLanguages string `json:"accept_languages"`
				} `json:"intl"`
			}
			err := readJSON(filepath.Join(home, b.path), 32<<20, &p)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return unk("无法判断：" + b.name + " 的配置读不出来")
			}
			found++
			first, _, _ := strings.Cut(p.Intl.AcceptLanguages, ",")
			if first != "" && !sameLang(first, t.Lang) && !sameLang(first, strings.SplitN(t.Lang, "-", 2)[0]) {
				bad = append(bad, b.name)
			}
		}
		switch {
		case found == 0:
			return skipped("已跳过：没有找到 Chrome、Edge 的配置")
		case len(bad) > 0:
			return fail(strings.Join(bad, "、")+" 的首选语言和目标语言不一致",
				"在浏览器的“设置 → 语言”里把目标语言移到第一位（Safari 跟随系统，不用改）。")
		}
		return pass("浏览器的首选语言和目标一致")
	}}
}

// ---- 12 Claude 进程环境、17 终端语言 ----

// ProcEnv 是一个 Claude 根进程的环境（只有白名单里的部分）。
type ProcEnv struct {
	App string // “Claude 桌面版”“Claude Code”
	Env procinfo.EnvInfo
}

// ProcEnvs 列出正在运行的 Claude 根进程的环境（daemon：claudeproc 扫描 + procinfo 读环境）。
type ProcEnvs func(ctx context.Context) ([]ProcEnv, error)

// ClaudeProcEnv 是 12 Claude 进程环境（硬性，只看本机状态）：只报有没有代理变量和变量名，不读出值。
func ClaudeProcEnv(list ProcEnvs) Item {
	return Item{ID: "12", Name: "Claude 进程环境", Level: Hard, Run: func(ctx context.Context) Outcome {
		ps, err := list(ctx)
		if err != nil {
			return unk("无法判断：读不出 Claude 进程的环境")
		}
		for _, p := range ps {
			if len(p.Env.ProxyVars) > 0 {
				return fail(p.App+" 的环境变量里有代理设置（"+strings.Join(p.Env.ProxyVars, "、")+"）",
					"退出 Claude，在终端或 shell 配置里去掉这些变量后重新打开（Claude 出国已经接管了网络，不需要它们）。")
			}
		}
		if len(ps) == 0 {
			return pass("没有在运行的 Claude 进程")
		}
		return pass("Claude 进程的环境变量里没有代理设置")
	}}
}

// TerminalLang 是 17 终端语言环境（警告，只看本机状态）：Claude Code 进程的 LANG、LC_ALL 与目标语言一致。
func TerminalLang(list ProcEnvs, target func() (region.Target, bool)) Item {
	return Item{ID: "17", Name: "终端语言环境", Level: Warn, Run: func(ctx context.Context) Outcome {
		t, ok := target()
		if !ok {
			return skipped("已跳过：没有同步地区")
		}
		ps, err := list(ctx)
		if err != nil {
			return unk("无法判断：读不出 Claude 进程的环境")
		}
		want := langCode(t.Lang) // ja-JP → ja；zh-Hant-TW → zh
		for _, p := range ps {
			if p.App != "Claude Code" {
				continue
			}
			v := p.Env.LcAll // LC_ALL 优先于 LANG
			if v == "" {
				v = p.Env.Lang
			}
			if v == "" || v == "C" || v == "POSIX" || strings.HasPrefix(v, "C.") {
				continue
			}
			if langCode(v) != want {
				return fail("Claude Code 的 LANG 或 LC_ALL 和目标语言不一致",
					"重开终端（终端是切换地区前打开的），或检查 .zshrc 里有没有写死 LANG。")
			}
		}
		return pass("Claude Code 的语言环境和目标一致（或没有在运行的 Claude Code）")
	}}
}

// ---- 07 的后一半：解析器的位置 ----

// EchoResolver 经我们的 DNS 查一条会回显解析器地址的记录（返回解析器的出口地址）。
type EchoResolver func(ctx context.Context) (netip.Addr, error)

func resolverMainland(ctx context.Context, echo EchoResolver) (bad bool, known bool) {
	if echo == nil {
		return false, true
	}
	a, err := echo(ctx)
	if err != nil || !a.IsValid() {
		return false, false
	}
	ml, err := exitpolicy.InMainland(a.Unmap())
	if err != nil {
		return false, false
	}
	return ml, true
}

// langCode 取语言代码（小写）：ja_JP.UTF-8 → ja，zh-Hant-TW → zh。
func langCode(s string) string {
	s = strings.ToLower(s)
	if i := strings.IndexAny(s, "_-."); i >= 0 {
		s = s[:i]
	}
	return s
}
