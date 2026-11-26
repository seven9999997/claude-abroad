package claudeproc

import (
	"context"
	"testing"

	"claude-abroad/internal/procinfo"
)

const fakeDir = "/Library/Application Support/claude-abroad/fake-claude"

// M6-4b-1 必须改 2：带开发参数（只针对指定目录）时，签名完全正确的“真 Claude”不被第三层结束，目录下的假进程被结束；
// 回国的预览、结束也只有目录下的；浏览器一律不列、不结束。
func TestFakeDirGuard(t *testing.T) {
	r := newGuardRig(t)
	r.addClaude(100, ExitOnTerm) // 签名是 Anthropic 的原生 Claude Code
	r.w.Add(procinfo.Proc{PID: 200, PPID: 1, Path: fakeDir + "/claude-fake", Name: "claude-fake"}, ExitOnTerm, 0)
	r.w.Add(procinfo.Proc{PID: 201, PPID: 1, Path: fakeDir + "-other/x", Name: "x"}, ExitOnTerm, 0) // 前缀相同但不在目录下
	RestrictToDir(nil, r.g, fakeDir)
	r.setPolicy(GuardPolicy{CoverAll: true})
	got := r.g.Tick()
	if len(got) != 1 || got[0].PID != 200 {
		t.Fatalf("只应拦下目录下的假进程：%+v", got)
	}
	if r.w.Alive(200) || !r.w.Alive(100) || !r.w.Alive(201) {
		t.Fatalf("真 Claude 和目录外的不应被结束：%s", sigLog(r.w))
	}
	if r.g.Judge.CacheLen() != 0 {
		t.Fatal("只针对目录时不应查签名")
	}
}

func TestFakeDirService(t *testing.T) {
	w := NewFakeWorld()
	w.Add(procinfo.Proc{PID: 100, PPID: 1, Path: "/Applications/Claude.app/Contents/MacOS/Claude", Name: "Claude"}, ExitOnTerm, 0)
	w.Add(procinfo.Proc{PID: 101, PPID: 1, Path: "/usr/local/bin/claude", Name: "claude"}, ExitOnTerm, 0)
	w.Add(procinfo.Proc{PID: 200, PPID: 1, Path: fakeDir + "/claude-fake", Name: "claude-fake"}, ExitOnTerm, 0)
	w.Add(procinfo.Proc{PID: 300, PPID: 1, Path: "/Applications/Safari.app/Contents/MacOS/Safari", Name: "Safari"}, ExitOnTerm, 0)
	sig := &FakeSigner{Teams: map[string]string{"/Applications/Claude.app/Contents/MacOS/Claude": AnthropicTeamID, "/usr/local/bin/claude": AnthropicTeamID}}
	sc := &Scanner{Procs: w, Signer: sig, Bundle: fakeBundles}
	s := &Service{Scanner: sc, Killer: &Killer{Scanner: sc, Info: w.Info, Sig: w, Clock: w},
		Browsers: &Browsers{Procs: w, Bundle: fakeBundles, Signer: newFakeProcSigner()}}
	RestrictToDir(s, nil, fakeDir)
	pv, err := s.Preview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.Processes) != 1 || pv.Processes[0].PID != 200 || len(pv.Unverified) != 0 || len(pv.Browsers) != 0 {
		t.Fatalf("预览里只应有目录下的假进程：%+v", pv)
	}
	if err := s.KillClaude(WithKillBrowsers(context.Background())); err != nil {
		t.Fatal(err)
	}
	if w.Alive(200) || !w.Alive(100) || !w.Alive(101) || !w.Alive(300) {
		t.Fatalf("只应结束目录下的假进程：%s", sigLog(w))
	}
}

func TestUnderDir(t *testing.T) {
	for _, c := range []struct {
		p, d string
		ok   bool
	}{
		{"/a/b/c", "/a/b", true}, {"/a/b", "/a/b", false}, {"/a/bc/x", "/a/b", false},
		{"/a/b/../c/x", "/a/b", false}, {"rel/x", "rel", false}, {"", "/a", false}, {"/a/x", "", false},
	} {
		if underDir(c.p, c.d) != c.ok {
			t.Errorf("%q 在 %q 下：应为 %v", c.p, c.d, c.ok)
		}
	}
}

// M6-5a 补审返工：预览里浏览器的说明按“同时强制退出浏览器”这个选项区分；没有浏览器时不加说明。
func TestPreviewBrowserChoice(t *testing.T) {
	p := Preview{Browsers: []Target{{PID: 1, App: "Safari"}}, BrowserNote: BrowserNote}
	if p.WithBrowserChoice(false).BrowserNote != BrowserNote || p.WithBrowserChoice(true).BrowserNote != BrowserKillNote {
		t.Fatal("两种选项的说明应不同")
	}
	if BrowserNote == BrowserKillNote {
		t.Fatal("文字应不同")
	}
	if (Preview{}).WithBrowserChoice(true).BrowserNote != "" {
		t.Fatal("没有浏览器时不加说明")
	}
}
