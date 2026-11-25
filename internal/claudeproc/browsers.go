package claudeproc

import (
	"context"
	"strings"
	"syscall"

	"claude-abroad/internal/procinfo"
)

// 回国时的浏览器（设计 10.1，M6-4b-3，m6-plan 的 --kill-browsers）：网页标签没法单独结束。
//   - 默认：回国预览里总是列出正在运行的浏览器，提示“其中打开的 Claude 页面请自行关闭”，不结束；
//   - 用户显式选择（命令行 --kill-browsers，或确认框里默认不勾的选项）时：结束确认过的浏览器——应用标识在下面的
//     清单里，并且运行中进程的签名 Team ID 是清单里那个浏览器的（Safari 是系统自带、没有 Team ID，按系统保护的
//     固定路径确认）；确认不了的不结束；先 TERM，宽限期后 KILL；预览里写明“未保存的表单内容会丢失”。
// 各浏览器的 Team ID 在 M6-9 的真机抽查里核对（R17 一并）；不对的那个浏览器确认不了，不会被结束。

type browserApp struct {
	bundle string
	name   string
	teams  []string
	paths  []string // 没有 Team ID 的系统应用：只认这些系统保护的路径前缀
}

var browserApps = []browserApp{
	{bundle: "com.apple.safari", name: "Safari", paths: []string{"/Applications/Safari.app/", "/System/Volumes/Preboot/Cryptexes/App/System/Applications/Safari.app/", "/System/Cryptexes/App/System/Applications/Safari.app/"}},
	{bundle: "com.google.chrome", name: "Google Chrome", teams: []string{"EQHXZ8M8AV"}},
	{bundle: "org.mozilla.firefox", name: "Firefox", teams: []string{"43AQ936H96"}},
	{bundle: "com.microsoft.edgemac", name: "Microsoft Edge", teams: []string{"UBF8T346G9"}},
	{bundle: "com.brave.browser", name: "Brave", teams: []string{"KL8N8XSYF4"}},
}

// BrowserNote 是预览里浏览器那一段的说明。
const BrowserNote = "浏览器里打开的 Claude 页面请自行关闭；选了“同时强制退出浏览器”时会结束下面确认过的浏览器，未保存的表单内容会丢失"

const noteBrowserUnverified = "签名确认不了，不会结束"

func browserFor(bundle string) *browserApp {
	for i := range browserApps {
		if strings.EqualFold(browserApps[i].bundle, bundle) {
			return &browserApps[i]
		}
	}
	return nil
}

// Browsers 识别正在运行的浏览器进程（含 Helper：按外层 .app 取应用标识）。
type Browsers struct {
	Procs  procinfo.Lister
	Bundle procinfo.BundleReader
	Signer ProcSigner
	Self   int
}

type browserProc struct {
	p        procinfo.Proc
	app      *browserApp
	verified bool
}

func (b *Browsers) scan() ([]browserProc, error) {
	if b == nil || b.Procs == nil || b.Bundle == nil {
		return nil, nil
	}
	procs, err := b.Procs.List()
	if err != nil {
		return nil, err
	}
	var out []browserProc
	for _, p := range procs {
		if p.PID <= 1 || p.PID == b.Self || p.Path == "" {
			continue
		}
		app := browserFor(string(procinfo.AppIDForPath(p.Path, b.Bundle)))
		if app == nil {
			continue
		}
		out = append(out, browserProc{p: p, app: app, verified: b.verify(p, app)})
	}
	return out, nil
}

func (b *Browsers) verify(p procinfo.Proc, app *browserApp) bool {
	if len(app.teams) == 0 {
		for _, pre := range app.paths {
			if strings.HasPrefix(p.Path, pre) {
				return true
			}
		}
		return false
	}
	if b.Signer == nil {
		return false
	}
	team, err := b.Signer.ProcTeamID(p.PID)
	if err != nil {
		return false
	}
	for _, t := range app.teams {
		if team == t {
			return true
		}
	}
	return false
}

// Preview 列出正在运行的浏览器（每个浏览器一行，用它的主进程；确认不了的标出来）。只读。
func (b *Browsers) Preview() ([]Target, error) {
	ps, err := b.scan()
	if err != nil {
		return nil, err
	}
	seen := map[string]int{} // 浏览器名 → 在 out 里的位置
	var out []Target
	for _, bp := range ps {
		t := Target{PID: bp.p.PID, PPID: bp.p.PPID, Start: bp.p.Start, App: bp.app.name}
		if !bp.verified {
			t.Note = noteBrowserUnverified
		}
		if i, ok := seen[bp.app.name]; ok {
			// 已有一行：留进程号最小的那个（通常是主进程）；有任何一个进程确认不了就标出来
			if t.Note != "" {
				out[i].Note = t.Note
			}
			if t.PID < out[i].PID {
				note := out[i].Note
				out[i] = t
				out[i].Note = note
			}
			continue
		}
		seen[bp.app.name] = len(out)
		out = append(out, t)
	}
	return out, nil
}

// Kill 结束确认过的浏览器进程：先 TERM，等宽限期，还在的 KILL。返回结束不了的。确认不了的不碰。
func (b *Browsers) Kill(ctx context.Context, k *Killer) ([]Target, error) {
	ps, err := b.scan()
	if err != nil {
		return nil, err
	}
	var ts []Target
	for _, bp := range ps {
		if bp.verified {
			ts = append(ts, Target{PID: bp.p.PID, PPID: bp.p.PPID, Start: bp.p.Start, App: bp.app.name})
		}
	}
	if len(ts) == 0 {
		return nil, nil
	}
	for _, t := range ts {
		k.send(t, syscall.SIGTERM)
	}
	left, err := k.wait(ctx, ts, dur(k.Grace, DefaultGrace))
	if err != nil {
		return left, err
	}
	for _, t := range left {
		k.send(t, syscall.SIGKILL)
	}
	return k.wait(ctx, left, DefaultKillWait)
}

type killBrowsersKey struct{}

// WithKillBrowsers 标记这一次回国要同时结束确认过的浏览器（用户显式选择）。
func WithKillBrowsers(ctx context.Context) context.Context {
	return context.WithValue(ctx, killBrowsersKey{}, true)
}

// KillBrowsersRequested 报告这一次回国是否要同时结束浏览器。
func KillBrowsersRequested(ctx context.Context) bool {
	v, _ := ctx.Value(killBrowsersKey{}).(bool)
	return v
}
