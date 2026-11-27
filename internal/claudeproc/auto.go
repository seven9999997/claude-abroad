package claudeproc

import (
	"container/list"
	"path"
	"strings"
	"sync"
	"time"

	"claude-abroad/internal/procinfo"
)

// 启动管控用的“自动结束”判定（设计 10.2 第三层，M6-4a，m6-plan v2.6）。和回国用的判定（Scanner.Scan，有用户在预览里
// 确认）分开：这里没有人确认，所以只认签名，结果只有“可以自动结束”和“不能”两种，没有“可能是”。
//   - 签名看的是正在运行的那个进程（codesign 查进程号），不只是磁盘上的文件：进程启动后文件被替换、路径相同内容不同，
//     都不能靠路径蒙混；
//   - 查询结果按“进程号加启动时间”缓存（之后每 200 毫秒扫一次）：进程号被复用时不会用到旧结果；查询失败不缓存，
//     一律是“不能”；缓存有容量上限，进程表里已经没有的条目清掉；
//   - 本文件不结束任何进程、没有定时器和扫描循环（那些在 M6-4b）。

// Kind 是安装方式。
type Kind string

const (
	KindDesktop  Kind = "desktop"  // Claude 桌面版（含 Helper）
	KindNative   Kind = "native"   // 原生安装的 Claude Code（Anthropic 签名的 claude 可执行文件；IDE 插件带的也归这一类）
	KindNPM      Kind = "npm"      // npm 安装（node、bun 跑包里的 JS；IDE 插件用 node 起的也归这一类）
	KindHomebrew Kind = "homebrew" // Homebrew 安装
	KindOther    Kind = ""         // 不是 Claude（或认不出）
)

// AutoResult 是一个进程的自动结束判定。
type AutoResult struct {
	Kill   bool   // 可以自动结束
	Kind   Kind   // 认出的安装方式（不能结束时也给出，给状态和日志用）
	Reason string // 为什么（不含路径）
}

// ProcSigner 查正在运行的进程的签名 Team ID（codesign 查进程号）。没有签名时返回空串和 nil；
// 查询失败、超时、进程已经没了都返回错误。
type ProcSigner interface {
	ProcTeamID(pid int) (string, error)
}

// DefaultSignCacheSize 是签名结果缓存的默认容量。
const DefaultSignCacheSize = 512

type procKey struct {
	pid   int
	start time.Time
}

type cacheEntry struct {
	key  procKey
	team string
}

// AutoJudge 是自动结束用的判定（并发安全）。
type AutoJudge struct {
	Signer ProcSigner
	Bundle procinfo.BundleReader // .app 目录 → Bundle ID（桌面版那一类）
	Max    int                   // 缓存容量；0 用 DefaultSignCacheSize
	// FakeDir：开发构建只认这个目录下的进程（真实判定不用；见 fakedir.go）。
	FakeDir string

	mu    sync.Mutex
	m     map[procKey]*list.Element
	order *list.List // 最近用过的在前
}

func (j *AutoJudge) max() int {
	if j.Max <= 0 {
		return DefaultSignCacheSize
	}
	return j.Max
}

// cachedTeam 取进程的签名 Team ID：缓存命中就不查；查询失败不缓存。
func (j *AutoJudge) cachedTeam(p procinfo.Proc) (string, bool) {
	k := procKey{pid: p.PID, start: p.Start}
	j.mu.Lock()
	if j.m == nil {
		j.m, j.order = map[procKey]*list.Element{}, list.New()
	}
	if el, ok := j.m[k]; ok {
		j.order.MoveToFront(el)
		team := el.Value.(*cacheEntry).team
		j.mu.Unlock()
		return team, true
	}
	j.mu.Unlock()
	if j.Signer == nil {
		return "", false
	}
	team, err := j.Signer.ProcTeamID(p.PID)
	if err != nil {
		return "", false // 失败、超时、进程没了：不缓存，一律“不能”
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if el, ok := j.m[k]; ok { // 并发时别的调用已经放进去了
		j.order.MoveToFront(el)
		return team, true
	}
	j.m[k] = j.order.PushFront(&cacheEntry{key: k, team: team})
	for j.order.Len() > j.max() { // 超过容量：淘汰最久没用的
		old := j.order.Back()
		j.order.Remove(old)
		delete(j.m, old.Value.(*cacheEntry).key)
	}
	return team, true
}

// Prune 清掉进程表里已经没有的条目（进程号加启动时间都要对上；M6-4b 每轮扫描后调用）。
func (j *AutoJudge) Prune(alive []procinfo.Proc) {
	keep := make(map[procKey]bool, len(alive))
	for _, p := range alive {
		keep[procKey{pid: p.PID, start: p.Start}] = true
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	for k, el := range j.m {
		if !keep[k] {
			j.order.Remove(el)
			delete(j.m, k)
		}
	}
}

// CacheLen 是缓存里的条目数（测试和诊断用）。
func (j *AutoJudge) CacheLen() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.m)
}

// markHomebrew 只用来给出安装方式的名字（Kind），不作为“能不能结束”的依据。
const markHomebrew = "/Caskroom/claude-code/"

// underInstallDir 判断路径在不在 Claude Code 的安装位置之下（和回国用的判定共用 installMarkers 这一份清单）。
func underInstallDir(p string) bool {
	for _, m := range installMarkers {
		if strings.Contains(p, m) {
			return true
		}
	}
	return false
}

// Judge 判定一个进程能不能自动结束。只认签名（m6-plan v2.6，architect 判 M6-4a 复审）：签名是 Anthropic 的就可以，
// 签名不对或查不到就不行；路径只用来决定要不要去查、给出安装方式的名字，不用来直接否决。去查签名的候选：
//   - 所在 .app 的 Bundle ID 在清单里（桌面版，含 Helper）；
//   - 路径在 Claude Code 的安装位置之下，不管文件名是什么（原生安装的可执行文件名是版本号，例如 versions/2.1.319）；
//   - 文件名是 claude（旧的本地安装、IDE 插件带的、Homebrew 装的等）。
//
// node、bun 跑包里的 JS（npm 安装，含 IDE 插件用 node 起的）确认不了，不能。其余不是 Claude。
func (j *AutoJudge) Judge(p procinfo.Proc) AutoResult {
	if j.FakeDir != "" {
		if underDir(p.Path, j.FakeDir) {
			return AutoResult{Kill: true, Kind: KindNative, Reason: fakeReason}
		}
		return AutoResult{}
	}
	if p.Path == "" {
		return AutoResult{Reason: "读不到可执行文件"}
	}
	if j.Bundle != nil && IsDesktopBundle(string(procinfo.AppIDForPath(p.Path, j.Bundle))) {
		return j.bySignature(p, KindDesktop, "Bundle ID 和运行中进程的签名", "Bundle ID 相符但签名不是 Anthropic 的")
	}
	switch path.Base(p.Path) {
	case "node", "bun":
		for i, a := range p.Args {
			if i > 0 && strings.Contains(a, claudeCodePkg) {
				return AutoResult{Kind: KindNPM, Reason: coverageReason(KindNPM)}
			}
		}
		return AutoResult{}
	}
	if path.Base(p.Path) == "claude" || underInstallDir(p.Path) {
		kind := KindNative
		if strings.Contains(p.Path, markHomebrew) {
			kind = KindHomebrew
		}
		r := j.bySignature(p, kind, "运行中进程的签名", "签名不是 Anthropic 的")
		if !r.Kill && !underInstallDir(p.Path) && r.Reason == "签名不是 Anthropic 的" {
			return AutoResult{Reason: "只是文件名叫 claude"} // 不在安装位置、签名也不对：不是 Claude
		}
		return r
	}
	return AutoResult{}
}

// bySignature 查运行中进程的签名：是 Anthropic 的才判“可以”；查询失败一律“不能”。
func (j *AutoJudge) bySignature(p procinfo.Proc, kind Kind, okReason, badReason string) AutoResult {
	team, ok := j.cachedTeam(p)
	switch {
	case !ok:
		return AutoResult{Kind: kind, Reason: "签名查询失败"}
	case IsAnthropicTeam(team):
		return AutoResult{Kill: true, Kind: kind, Reason: okReason}
	}
	return AutoResult{Kind: kind, Reason: badReason}
}

// CoverageRow 是一种安装方式的覆盖情况。
type CoverageRow struct {
	Kind   Kind           `json:"kind"`
	Name   string         `json:"name"`
	Status CoverageStatus `json:"status"` // 第三层（发现即结束）覆盖不覆盖：界面按取值显示，不解析 Reason
	Reason string         `json:"reason"`
}

// CoverageStatus 是第三层对一种安装方式的覆盖：覆盖、不覆盖、视签名而定。
type CoverageStatus string

const (
	CoverageCovered     CoverageStatus = "covered"
	CoverageNotCovered  CoverageStatus = "not_covered"
	CoverageBySignature CoverageStatus = "by_signature"
)

// coverage 是覆盖范围的唯一来源（文字只写在这里）。fallback 为真的行（第三层不覆盖或不一定覆盖）后面接上
// FallbackText 按第四道的开关生成的兜底说明。
var coverage = []struct {
	CoverageRow
	fallback bool
}{
	{CoverageRow{KindDesktop, "Claude 桌面版", CoverageCovered, "按 Bundle ID 和运行中进程的签名确认，可以自动结束"}, false},
	{CoverageRow{KindNative, "Claude Code（原生安装，含 IDE 插件带的）", CoverageCovered, "按运行中进程的签名确认，可以自动结束"}, false},
	{CoverageRow{KindNPM, "Claude Code（npm 安装，含 IDE 插件用 node 启动的）", CoverageNotCovered, "签名确认不了，第三层不覆盖"}, true},
	{CoverageRow{KindHomebrew, "Claude Code（Homebrew 安装）", CoverageBySignature, "运行中进程的签名是 Anthropic 的可以自动结束；否则不覆盖"}, true},
}

// FallbackText 是第三层没覆盖时由哪几层兜着（v0.68：第四道可选、默认关）。文字只在这里生成：
// 第四道打开时写第一层和第四层；关着时只写第一层和第五层，并注明第四层未开启。
func FallbackText(layer4 bool) string {
	if layer4 {
		return "由第一层（解析锁）和第四层（本机代理）兜着"
	}
	return "由第一层（解析锁）和第五层（回国拦截）兜着（第四层未开启）"
}

// Coverage 返回每种安装方式第三层覆盖不覆盖、原因（给状态接口和界面用）。layer4 是第四道现在是否打开（并且真的管着
// 本机的 Claude Code：开发构建的测试模式不算）。
func Coverage(layer4 bool) []CoverageRow {
	out := make([]CoverageRow, 0, len(coverage))
	for _, c := range coverage {
		r := c.CoverageRow
		if c.fallback {
			r.Reason += "，" + FallbackText(layer4)
		}
		out = append(out, r)
	}
	return out
}

func coverageReason(k Kind) string {
	for _, r := range coverage {
		if r.Kind == k {
			return r.Reason
		}
	}
	return ""
}
