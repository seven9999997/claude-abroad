// Package claudeproc 是回国前强制结束 Claude 进程（design 10.1，m4-plan M4-4 v2.3）：扫描、进程树、
// 结束流程，以及给确认框看的预览。
//
// 判断“要结束的进程”比分流时（procinfo 的 isClaudeProc）严：分流时多算只是多走代理，结束时多算会误杀别的程序。
//   - 桌面版：Bundle ID 相符，并且代码签名的 Team ID 是 Anthropic 的；
//   - Claude Code 可执行文件：签名的 Team ID 是 Anthropic 的，或者路径带安装位置特征；只凭文件名叫 claude 不算；
//   - node、bun：参数里有包名（npm 全局安装的 bin 链接，解析链接后落在包目录里也算）；
//   - 以上进程的全部后代。
//
// 签名查询失败时那一条按“不匹配”处理，在预览里标出“无法确认签名”。abroadd 自己和它的后代永远不在清单里。
package claudeproc

import (
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"claude-abroad/internal/procinfo"
)

const claudeCodePkg = "@anthropic-ai/claude-code"

// installMarkers 是 Claude Code 的安装位置特征（路径里含其中之一即算，不看签名）。
var installMarkers = []string{
	"/.local/share/claude/versions/",         // 原生安装
	"/.claude/local/",                        // 旧的本地安装
	"/node_modules/" + claudeCodePkg + "/",   // npm 包里自带的可执行文件
	"/Caskroom/claude-code/",                 // Homebrew
	"/extensions/anthropic.claude-code-",     // VS Code 一类 IDE 的插件
	"/plugins/claude-code-jetbrains-plugin/", // JetBrains 插件
}

// MaxCmd 是预览里命令行的最大长度（字符）。
const MaxCmd = 200

// Target 是一个要结束的进程。PID 加 Start 确定“还是同一个进程”（发信号前复核）。
type Target struct {
	PID    int       `json:"pid"`
	PPID   int       `json:"-"`
	Start  time.Time `json:"-"`
	App    string    `json:"app"`
	Cmd    string    `json:"cmd"`
	Note   string    `json:"note,omitempty"` // 只有 Unverified 里的有：为什么没算进来
	Reason string    `json:"-"`
}

// Report 是一次扫描的结果。Targets 要结束；Unverified 是看起来像 Claude 但没能确认、没有算进来的。
type Report struct {
	Targets    []Target
	Unverified []Target
}

// Signer 查代码签名的 Team ID。没有签名或没有 Team ID 时返回空串和 nil；查询失败、输出不认识时返回错误。
type Signer interface {
	TeamID(path string) (string, error)
}

// Scanner 扫描进程表。
type Scanner struct {
	Procs   procinfo.Lister
	Signer  Signer
	Bundle  procinfo.BundleReader        // .app 目录 → Bundle ID
	Resolve func(string) (string, error) // 解析符号链接（nil 用 filepath.EvalSymlinks）
	Self    int                          // abroadd 自己的进程号：它和它的后代不在清单里
	FakeDir string                       // 开发构建：只认这个目录下的进程（真实判定不用；见 fakedir.go）
}

const (
	noteUnsigned  = "无法确认签名"
	noteWrongSign = "Bundle ID 相符但签名不是 Anthropic 的"
	appDesktop    = "Claude 桌面版"
	appCode       = "Claude Code"
)

type verdict struct {
	match      bool
	app        string
	reason     string
	unverified string
}

// Scan 扫描一次：识别根进程，再沿进程树收全部后代。
func (s *Scanner) Scan() (Report, error) {
	procs, err := s.Procs.List()
	if err != nil {
		return Report{}, err
	}
	byPID := make(map[int]procinfo.Proc, len(procs))
	children := map[int][]int{}
	for _, p := range procs {
		byPID[p.PID] = p
		if p.PPID != p.PID {
			children[p.PPID] = append(children[p.PPID], p.PID)
		}
	}
	// abroadd 自己和它的后代：永远不动。
	excluded := map[int]bool{0: true, 1: true}
	if s.Self > 0 {
		walk(s.Self, children, excluded)
	}
	teams := map[string]teamResult{}
	var rep Report
	roots := map[int]string{} // 根进程 → 显示名
	pids := make([]int, 0, len(procs))
	for pid := range byPID {
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	for _, pid := range pids {
		if excluded[pid] {
			continue
		}
		p := byPID[pid]
		v := s.classify(p, teams)
		switch {
		case v.match:
			roots[pid] = v.app
		case v.unverified != "":
			rep.Unverified = append(rep.Unverified, s.target(p, v.app, v.unverified, ""))
		}
	}
	// 后代：广度优先，visited 防环。
	in := map[int]string{}
	queue := make([]int, 0, len(roots))
	for _, pid := range pids {
		if app, ok := roots[pid]; ok {
			in[pid] = app
			queue = append(queue, pid)
		}
	}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, c := range children[pid] {
			if _, seen := in[c]; seen || excluded[c] {
				continue
			}
			in[c] = in[pid] + " 的子进程"
			if _, isRoot := roots[c]; isRoot {
				in[c] = roots[c]
			}
			queue = append(queue, c)
		}
	}
	for _, pid := range pids {
		app, ok := in[pid]
		if !ok {
			continue
		}
		reason := "后代"
		if _, isRoot := roots[pid]; isRoot {
			reason = "识别"
		}
		rep.Targets = append(rep.Targets, s.target(byPID[pid], app, "", reason))
	}
	// 已经算进来的不再出现在“没能确认”里。
	kept := rep.Unverified[:0]
	for _, u := range rep.Unverified {
		if _, ok := in[u.PID]; !ok {
			kept = append(kept, u)
		}
	}
	rep.Unverified = kept
	return rep, nil
}

func walk(pid int, children map[int][]int, set map[int]bool) {
	stack := []int{pid}
	for len(stack) > 0 {
		p := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if set[p] && p != pid {
			continue
		}
		set[p] = true
		for _, c := range children[p] {
			if !set[c] {
				stack = append(stack, c)
			}
		}
	}
}

type teamResult struct {
	id  string
	err error
}

func (s *Scanner) team(p string, cache map[string]teamResult) (string, error) {
	if r, ok := cache[p]; ok {
		return r.id, r.err
	}
	var r teamResult
	if s.Signer == nil {
		r.err = errNoSigner
	} else {
		r.id, r.err = s.Signer.TeamID(p)
	}
	cache[p] = r
	return r.id, r.err
}

func (s *Scanner) classify(p procinfo.Proc, teams map[string]teamResult) verdict {
	if s.FakeDir != "" {
		if underDir(p.Path, s.FakeDir) {
			return verdict{match: true, app: appCode, reason: fakeReason}
		}
		return verdict{}
	}
	if p.Path == "" { // 读不到路径：只凭短名没法确认
		if strings.EqualFold(p.Name, "claude") {
			return verdict{app: appCode, unverified: noteUnsigned}
		}
		return verdict{}
	}
	if s.Bundle != nil && IsDesktopBundle(string(procinfo.AppIDForPath(p.Path, s.Bundle))) {
		id, err := s.team(p.Path, teams)
		switch {
		case err != nil:
			return verdict{app: appDesktop, unverified: noteUnsigned}
		case IsAnthropicTeam(id):
			return verdict{match: true, app: appDesktop, reason: "Bundle ID 和签名"}
		default:
			return verdict{app: appDesktop, unverified: noteWrongSign}
		}
	}
	for _, m := range installMarkers {
		if strings.Contains(p.Path, m) {
			return verdict{match: true, app: appCode, reason: "安装位置"}
		}
	}
	switch base := path.Base(p.Path); base {
	case "claude":
		id, err := s.team(p.Path, teams)
		switch {
		case err != nil:
			return verdict{app: appCode, unverified: noteUnsigned}
		case IsAnthropicTeam(id):
			return verdict{match: true, app: appCode, reason: "签名"}
		}
		return verdict{} // 只是文件名叫 claude
	case "node", "bun":
		for i, a := range p.Args {
			if i == 0 {
				continue // argv[0] 是程序自己
			}
			if strings.Contains(a, claudeCodePkg) {
				return verdict{match: true, app: appCode, reason: "包名"}
			}
			if strings.HasPrefix(a, "/") && path.Base(a) == "claude" {
				if r, err := s.resolve(a); err == nil && strings.Contains(r, "/"+claudeCodePkg+"/") {
					return verdict{match: true, app: appCode, reason: "包名（链接）"}
				}
			}
		}
	}
	return verdict{}
}

func (s *Scanner) resolve(p string) (string, error) {
	if s.Resolve != nil {
		return s.Resolve(p)
	}
	return filepath.EvalSymlinks(p)
}

func (s *Scanner) target(p procinfo.Proc, app, note, reason string) Target {
	return Target{PID: p.PID, PPID: p.PPID, Start: p.Start, App: app, Cmd: DisplayCmd(p), Note: note, Reason: reason}
}

var homeRe = regexp.MustCompile(`/(?:Users|home)/[^/\s]+|/var/root`)

// DisplayCmd 是给确认框看的启动命令：参数用空格连起来（没有参数时用路径），用户主目录换成 ~，截断到 MaxCmd 个字符。
// 不含环境变量（procinfo 只读参数）。
func DisplayCmd(p procinfo.Proc) string {
	c := strings.Join(p.Args, " ")
	if strings.TrimSpace(c) == "" {
		c = p.Path
	}
	if c == "" {
		c = p.Name
	}
	c = homeRe.ReplaceAllString(c, "~")
	c = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, c)
	if utf8.RuneCountInString(c) > MaxCmd {
		r := []rune(c)
		c = string(r[:MaxCmd-1]) + "…"
	}
	return c
}

// PathVerdict 是 ClassifyPath 的结果（M4-9 的只读核对：R17 的安装位置特征、假 Claude 进程的路径）。
type PathVerdict struct {
	Match      bool   `json:"match"`                // 算作 Claude（会被列进清单）
	App        string `json:"app,omitempty"`        // Claude 桌面版 / Claude Code
	Reason     string `json:"reason,omitempty"`     // 依据：安装位置、签名、Bundle ID 和签名、包名
	Unverified string `json:"unverified,omitempty"` // 看起来像但没能确认的原因
}

// ClassifyPath 按扫描时同样的规则判断一个可执行文件路径（不带参数的进程）。只读：可能查一次代码签名。
func (s *Scanner) ClassifyPath(p string, args ...string) PathVerdict {
	v := s.classify(procinfo.Proc{Path: p, Name: path.Base(p), Args: append([]string{p}, args...)}, map[string]teamResult{})
	return PathVerdict{Match: v.match, App: v.app, Reason: v.reason, Unverified: v.unverified}
}

// InstallMarkers 返回 Claude Code 的安装位置特征（只读副本）。
func InstallMarkers() []string { return append([]string(nil), installMarkers...) }
