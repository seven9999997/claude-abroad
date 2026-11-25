package claudeproc

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"claude-abroad/internal/procinfo"
)

// fakeProcSigner：按进程号给 Team ID；Err 的返回错误，Timeout 的返回超时，Gone 的返回“进程已经没了”。记查询次数。
type fakeProcSigner struct {
	mu      sync.Mutex
	teams   map[int]string
	err     map[int]bool
	timeout map[int]bool
	gone    map[int]bool
	calls   map[int]int
}

func newFakeProcSigner() *fakeProcSigner {
	return &fakeProcSigner{teams: map[int]string{}, err: map[int]bool{}, timeout: map[int]bool{}, gone: map[int]bool{}, calls: map[int]int{}}
}

func (f *fakeProcSigner) ProcTeamID(pid int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[pid]++
	switch {
	case f.err[pid]:
		return "", errors.New("假的签名查询失败")
	case f.timeout[pid]:
		return "", errors.New("查询签名超时")
	case f.gone[pid]:
		return "", errors.New("进程已经没了")
	}
	return f.teams[pid], nil
}

func (f *fakeProcSigner) n(pid int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[pid]
}

var t0 = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

func proc(pid int, p string, args ...string) procinfo.Proc {
	return procinfo.Proc{PID: pid, PPID: 1, Path: p, Args: append([]string{p}, args...), Start: t0, Name: p[strings.LastIndex(p, "/")+1:]}
}

const (
	desktopMain   = "/Applications/Claude.app/Contents/MacOS/Claude"
	desktopHelper = "/Applications/Claude.app/Contents/Frameworks/Claude Helper (Renderer).app/Contents/MacOS/Claude Helper (Renderer)"
	nativeCode    = "/Users/u/.local/share/claude/versions/2.1.319" // 真实形态：可执行文件名是版本号（~/.local/bin/claude 是指向它的链接）
	ideNative     = "/Users/u/.vscode/extensions/anthropic.claude-code-2.0.1/resources/native-binary/claude"
	brewCode      = "/opt/homebrew/Caskroom/claude-code/2.1.319/claude"
	nodeBin       = "/opt/homebrew/bin/node"
)

func newJudge() (*AutoJudge, *fakeProcSigner) {
	s := newFakeProcSigner()
	return &AutoJudge{Signer: s, Bundle: fakeBundles}, s
}

// a. 每种安装方式各一条。
func TestAutoJudgeKinds(t *testing.T) {
	j, s := newJudge()
	for pid := 100; pid < 120; pid++ {
		s.teams[pid] = AnthropicTeamID
	}
	cases := []struct {
		name string
		p    procinfo.Proc
		kill bool
		kind Kind
	}{
		{"桌面版主程序", proc(100, desktopMain), true, KindDesktop},
		{"桌面版的 Helper", proc(101, desktopHelper), true, KindDesktop},
		{"原生安装的 Claude Code", proc(102, nativeCode), true, KindNative},
		{"npm 装的（node 跑包）", proc(103, nodeBin, "/opt/homebrew/lib/node_modules/@anthropic-ai/claude-code/cli.js"), false, KindNPM},
		{"bun 跑包", proc(104, "/Users/u/.bun/bin/bun", "/Users/u/.bun/install/global/node_modules/@anthropic-ai/claude-code/cli.js"), false, KindNPM},
		{"Homebrew 装的、签名是 Anthropic 的：由签名决定，可以", proc(105, brewCode), true, KindHomebrew},
		{"IDE 插件带的原生可执行文件", proc(106, ideNative), true, KindNative},
		{"IDE 插件用 node 启动的", proc(107, nodeBin, "/Users/u/.vscode/extensions/anthropic.claude-code-2.0.1/node_modules/@anthropic-ai/claude-code/cli.js"), false, KindNPM},
		{"无关的 node", proc(108, nodeBin, "server.js"), false, KindOther},
	}
	for _, c := range cases {
		r := j.Judge(c.p)
		if r.Kill != c.kill || r.Kind != c.kind || r.Reason == "" && c.kind != KindOther {
			t.Errorf("%s：%+v，应 kill=%v kind=%q", c.name, r, c.kill, c.kind)
		}
	}
	// npm 不查签名（确认不了，查了也不用）
	if s.n(103) != 0 {
		t.Fatal("npm 不应查签名")
	}
	// 防回归：用例里至少有一个“可以结束”的候选，文件名不是 claude（原生安装的文件名是版本号）
	found := false
	for _, c := range cases {
		if c.kill && !strings.HasSuffix(c.p.Path, "/claude") && c.kind == KindNative {
			found = true
		}
	}
	if !found {
		t.Fatal("测试用例里应有文件名不是 claude 的原生安装候选")
	}
}

// b. 反例都判“不能”。
func TestAutoJudgeNegatives(t *testing.T) {
	j, s := newJudge()
	s.teams[200] = ""              // 叫 claude 但没有签名
	s.teams[201] = "ABCDE12345"    // 路径和真的一样但 Team ID 不对
	s.teams[202] = AnthropicTeamID // Team ID 对但 Bundle ID 不在清单里（别的 .app）
	s.teams[207] = "ABCDE12345"
	s.err[208] = true
	s.err[203] = true
	s.timeout[204] = true
	s.gone[205] = true
	cases := []struct {
		name string
		p    procinfo.Proc
	}{
		{"叫 claude 但没有签名", proc(200, "/usr/local/bin/claude")},
		{"路径和真的一样（原生安装目录、文件名是版本号）但 Team ID 不对", proc(201, nativeCode)},
		{"Homebrew 路径但签名不对", proc(207, brewCode)},
		{"Homebrew 路径但签名查询失败", proc(208, brewCode)},
		{"Team ID 对但 Bundle ID 不在清单里", proc(202, "/Applications/NotClaude.app/Contents/MacOS/Claude")},
		{"签名查询出错", proc(203, nativeCode)},
		{"签名查询超时", proc(204, desktopMain)},
		{"进程在查询途中消失", proc(205, nativeCode)},
		{"读不到路径", procinfo.Proc{PID: 206, Name: "claude", Start: t0}},
	}
	for _, c := range cases {
		if r := j.Judge(c.p); r.Kill {
			t.Errorf("%s：不应判可以结束（%+v）", c.name, r)
		}
	}
}

// c. 缓存：同一进程第二次不查；进程号同启动时间不同重查；失败下次重查；超容量淘汰；进程表里没有的清掉。
func TestAutoJudgeCache(t *testing.T) {
	j, s := newJudge()
	s.teams[300] = AnthropicTeamID
	p := proc(300, nativeCode)
	if !j.Judge(p).Kill || !j.Judge(p).Kill || s.n(300) != 1 {
		t.Fatalf("同一进程第二次不应再查：查了 %d 次", s.n(300))
	}
	// 进程号被复用：启动时间不同，重新查，不沿用旧结果
	s.teams[300] = ""
	reused := p
	reused.Start = t0.Add(time.Minute)
	if j.Judge(reused).Kill || s.n(300) != 2 {
		t.Fatalf("进程号复用时应重查且不沿用：%d", s.n(300))
	}
	// 失败不缓存：下一次重查；失败期间一律“不能”
	s.err[301] = true
	q := proc(301, nativeCode)
	if j.Judge(q).Kill {
		t.Fatal("失败应判不能")
	}
	s.err[301] = false
	s.teams[301] = AnthropicTeamID
	if !j.Judge(q).Kill || s.n(301) != 2 {
		t.Fatalf("失败后应重查：%d", s.n(301))
	}
	// 容量：超出时淘汰最久没用的
	j2, s2 := newJudge()
	j2.Max = 3
	for pid := 400; pid < 405; pid++ {
		s2.teams[pid] = AnthropicTeamID
		j2.Judge(proc(pid, nativeCode))
	}
	if j2.CacheLen() != 3 {
		t.Fatalf("容量 3：%d", j2.CacheLen())
	}
	j2.Judge(proc(400, nativeCode)) // 最早的已被淘汰：重查
	if s2.n(400) != 2 {
		t.Fatalf("被淘汰的应重查：%d", s2.n(400))
	}
	// Prune：进程表里没有的清掉（进程号加启动时间都要对上）
	j2.Prune([]procinfo.Proc{proc(404, nativeCode)})
	if j2.CacheLen() != 1 {
		t.Fatalf("Prune 后应只剩 1 条：%d", j2.CacheLen())
	}
	moved := proc(404, nativeCode)
	moved.Start = t0.Add(time.Hour)
	j2.Prune([]procinfo.Proc{moved})
	if j2.CacheLen() != 0 {
		t.Fatal("启动时间不同的不算同一个进程，应清掉")
	}
}

// c. 并发（带 -race 跑）：多个协程同时判定、清理，结果一致，同一进程最终只缓存一份。
func TestAutoJudgeConcurrent(t *testing.T) {
	j, s := newJudge()
	for pid := 500; pid < 520; pid++ {
		s.teams[pid] = AnthropicTeamID
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				pid := 500 + (i+g)%20
				if !j.Judge(proc(pid, nativeCode)).Kill {
					t.Errorf("pid %d 应可以结束", pid)
					return
				}
				if i%50 == 0 {
					j.Prune([]procinfo.Proc{proc(pid, nativeCode)})
				}
			}
		}(g)
	}
	wg.Wait()
	if j.CacheLen() > 20 {
		t.Fatalf("缓存条目 %d 超过进程数", j.CacheLen())
	}
}

// d、e. 清单一致：回国和自动结束用的是同一份 Team ID 和 Bundle ID；回国判定没有自己写死的另一份。
func TestIDListShared(t *testing.T) {
	if len(TeamIDs) == 0 || TeamIDs[0] != AnthropicTeamID || len(DesktopBundleIDs) == 0 || DesktopBundleIDs[0] != DesktopBundleID {
		t.Fatal("清单应包含原有的常量")
	}
	for _, f := range []string{"scan.go", "auto.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, bad := range []string{"== AnthropicTeamID", "== DesktopBundleID", `"` + AnthropicTeamID + `"`, `"` + DesktopBundleID + `"`} {
			if strings.Contains(src, bad) {
				t.Errorf("%s 里直接比较或写死了 %s：应经 IsAnthropicTeam、IsDesktopBundle", f, bad)
			}
		}
		if !strings.Contains(src, "IsAnthropicTeam(") {
			t.Errorf("%s 没有经 IsAnthropicTeam 判签名", f)
		}
	}
}

// f. 覆盖范围：每种安装方式一行，齐全；npm、Homebrew 写明由第一层和第四层兜着；文字只在一处。
func TestCoverage(t *testing.T) {
	rows := Coverage()
	want := map[Kind]bool{KindDesktop: true, KindNative: true, KindNPM: false, KindHomebrew: false}
	if len(rows) != len(want) {
		t.Fatalf("%v", rows)
	}
	for _, r := range rows {
		cov, ok := want[r.Kind]
		if !ok || r.Covered != cov || r.Name == "" || r.Reason == "" {
			t.Fatalf("%+v", r)
		}
		if !cov && (!strings.Contains(r.Reason, "第一层（解析锁）") || !strings.Contains(r.Reason, "第四层（本机代理）")) {
			t.Fatalf("不覆盖的应写明兜底：%+v", r)
		}
	}
	// 判定给出的原因和覆盖表同一来源
	j, _ := newJudge()
	if r := j.Judge(proc(600, nodeBin, "/x/node_modules/@anthropic-ai/claude-code/cli.js")); r.Reason != rows[2].Reason {
		t.Fatalf("原因应来自覆盖表：%q", r.Reason)
	}
	if !strings.Contains(rows[3].Reason, "视签名而定") {
		t.Fatalf("Homebrew 应视签名而定：%q", rows[3].Reason)
	}
	// 文字只在 auto.go 的覆盖表里写一次
	b, _ := os.ReadFile("auto.go")
	if n := strings.Count(string(b), "由第一层（解析锁）和第四层（本机代理）兜着"); n != 2 { // npm、Homebrew 两行
		t.Fatalf("兜底那句应只在覆盖表里出现（%d 次）", n)
	}
}

// g. 源码检查：自动结束判定的代码里没有定时器、扫描循环，也没有结束进程的调用。
func TestAutoNoTimersNoKill(t *testing.T) {
	for _, f := range []string{"auto.go", "idlist.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"time.NewTicker", "time.Tick(", "time.AfterFunc", "time.Sleep", "go func", ".Signal(", "Kill(", "syscall."} {
			if strings.Contains(string(b), bad) {
				t.Errorf("%s 里有 %s（这次只有判定、缓存、覆盖范围）", f, bad)
			}
		}
	}
	_ = fmt.Sprint()
}
