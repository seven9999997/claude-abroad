package claudeproc

import (
	"context"
	"strings"
	"testing"

	"claude-abroad/internal/procinfo"
)

const (
	chromeMain   = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	chromeHelper = "/Applications/Google Chrome.app/Contents/Frameworks/Google Chrome Framework.framework/Helpers/Google Chrome Helper.app/Contents/MacOS/Google Chrome Helper"
	firefoxMain  = "/Applications/Firefox.app/Contents/MacOS/firefox"
	safariMain   = "/Applications/Safari.app/Contents/MacOS/Safari"
	fakeSafari   = "/Users/u/Downloads/Safari.app/Contents/MacOS/Safari"
)

func browserBundles(app string) (string, error) {
	switch {
	case strings.HasSuffix(app, "/Google Chrome.app"):
		return "com.google.Chrome", nil
	case strings.HasSuffix(app, "/Firefox.app"):
		return "org.mozilla.firefox", nil
	case strings.HasSuffix(app, "/Safari.app"):
		return "com.apple.Safari", nil
	}
	return fakeBundles(app)
}

func browserWorld(t *testing.T) (*FakeWorld, *fakeProcSigner, *Browsers) {
	t.Helper()
	w := NewFakeWorld()
	s := newFakeProcSigner()
	w.Add(procinfo.Proc{PID: 500, PPID: 1, Path: chromeMain}, ExitOnKill, 0) // 不理 TERM
	w.Add(procinfo.Proc{PID: 501, PPID: 500, Path: chromeHelper}, ExitOnTerm, 0)
	w.Add(procinfo.Proc{PID: 510, PPID: 1, Path: firefoxMain}, ExitOnTerm, 0) // 签名不对
	w.Add(procinfo.Proc{PID: 520, PPID: 1, Path: safariMain}, ExitOnTerm, 0)
	w.Add(procinfo.Proc{PID: 530, PPID: 1, Path: fakeSafari}, ExitOnTerm, 0) // 冒名的 Safari（不在系统保护的路径）
	s.teams[500], s.teams[501] = "EQHXZ8M8AV", "EQHXZ8M8AV"
	s.teams[510] = "ABCDE12345"
	return w, s, &Browsers{Procs: w, Bundle: browserBundles, Signer: s}
}

// 预览：每个浏览器一行（Helper 合到主进程那一行），确认不了的标出来。
func TestBrowsersPreview(t *testing.T) {
	_, _, b := browserWorld(t)
	got, err := b.Preview()
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]Target{}
	for _, x := range got {
		m[x.App] = x
	}
	if len(got) != 3 || m["Google Chrome"].PID != 500 || m["Google Chrome"].Note != "" {
		t.Fatalf("%+v", got)
	}
	if m["Firefox"].Note == "" {
		t.Fatal("签名不对的应标出确认不了")
	}
	if m["Safari"].Note == "" {
		t.Fatal("有一个 Safari 是冒名的：这一行应标出确认不了")
	}
}

// 结束：只结束确认过的（Chrome 及其 Helper、系统 Safari），先 TERM 后 KILL；签名不对的、冒名的不碰。
func TestBrowsersKill(t *testing.T) {
	w, _, b := browserWorld(t)
	k := &Killer{Scanner: &Scanner{}, Info: w.Info, Sig: w, Clock: w}
	left, err := b.Kill(context.Background(), k)
	if err != nil || len(left) != 0 {
		t.Fatalf("%v %v", left, err)
	}
	for _, pid := range []int{500, 501, 520} {
		if w.Alive(pid) {
			t.Fatalf("%d 应被结束", pid)
		}
	}
	for _, pid := range []int{510, 530} {
		if !w.Alive(pid) {
			t.Fatalf("%d 确认不了，不应结束", pid)
		}
	}
	log := sigLog(w)
	if !strings.Contains(log, "TERM 500") || !strings.Contains(log, "KILL 500") || strings.Index(log, "TERM 500") > strings.Index(log, "KILL 500") {
		t.Fatalf("应先 TERM 后 KILL：%s", log)
	}
}

// 回国：不带选项时浏览器只列出不结束；带上时才结束确认过的。
func TestServiceKillBrowsersOption(t *testing.T) {
	for _, kill := range []bool{false, true} {
		w, _, b := browserWorld(t)
		svc := NewFakeService(w, &FakeSigner{}, 999, false, nil)
		svc.Browsers = b
		pv, err := svc.Preview(context.Background())
		if err != nil || len(pv.Browsers) != 3 || pv.BrowserNote == "" {
			t.Fatalf("预览应总是列出浏览器：%+v %v", pv, err)
		}
		ctx := context.Background()
		if kill {
			ctx = WithKillBrowsers(ctx)
		}
		if err := svc.KillClaude(ctx); err != nil {
			t.Fatal(err)
		}
		if w.Alive(520) == kill {
			t.Fatalf("kill=%v：Safari 存活 %v", kill, w.Alive(520))
		}
		if !w.Alive(510) {
			t.Fatal("确认不了的任何时候都不结束")
		}
	}
}

// M6-4b-3 补审返工：选了结束浏览器，结束后报告还在运行的——一个怎么都不退（Chrome）、一个签名不对（Firefox）、
// 冒名的 Safari（确认不了）、只列出的 Arc；全部结束时为空。
func TestBrowsersKillReport(t *testing.T) {
	w, s, b := browserWorld(t)
	w.Remove(500)
	w.Add(procinfo.Proc{PID: 500, PPID: 1, Path: chromeMain}, Unkillable, 0) // 怎么都不退
	s.teams[500] = "EQHXZ8M8AV"
	const arc = "/Applications/Arc.app/Contents/MacOS/Arc"
	w.Add(procinfo.Proc{PID: 540, PPID: 1, Path: arc}, ExitOnTerm, 0)
	b.Bundle = func(app string) (string, error) {
		if strings.HasSuffix(app, "/Arc.app") {
			return "company.thebrowser.Browser", nil
		}
		return browserBundles(app)
	}
	k := &Killer{Scanner: &Scanner{}, Info: w.Info, Sig: w, Clock: w}
	_, names, _ := b.KillReport(context.Background(), k)
	if strings.Join(names, ",") != "Arc,Firefox,Google Chrome,Safari" {
		t.Fatalf("还在运行的：%v", names)
	}
	if !w.Alive(540) {
		t.Fatal("只列出的 Arc 不应被结束")
	}
	pv, _ := b.Preview()
	var arcNote string
	for _, p := range pv {
		if p.App == "Arc" {
			arcNote = p.Note
		}
	}
	if arcNote != noteBrowserListOnly {
		t.Fatalf("预览里 Arc 应标“只列出”：%+v", pv)
	}
	// 全部结束：为空
	w2, _, b2 := browserWorld(t)
	w2.Remove(510)
	w2.Remove(530)
	k2 := &Killer{Scanner: &Scanner{}, Info: w2.Info, Sig: w2, Clock: w2}
	if _, names, _ := b2.KillReport(context.Background(), k2); len(names) != 0 {
		t.Fatalf("全部结束时应为空：%v", names)
	}
}

// 回国服务记下“还在运行的浏览器”；没选结束浏览器时为空。
func TestServiceBrowsersLeft(t *testing.T) {
	for _, kill := range []bool{false, true} {
		w, _, b := browserWorld(t)
		svc := NewFakeService(w, &FakeSigner{}, 999, false, nil)
		svc.Browsers = b
		svc.Killer.Clock = w
		ctx := context.Background()
		if kill {
			ctx = WithKillBrowsers(ctx)
		}
		if err := svc.KillClaude(ctx); err != nil {
			t.Fatal(err)
		}
		got := strings.Join(svc.BrowsersLeft(), ",")
		if kill && got != "Firefox,Safari" || !kill && got != "" {
			t.Fatalf("kill=%v：%q", kill, got)
		}
	}
}
