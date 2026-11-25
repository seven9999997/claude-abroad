package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"claude-abroad/internal/claudeproc"
	"claude-abroad/internal/procinfo"
)

const kbSafari = "/Applications/Safari.app/Contents/MacOS/Safari"

// restoreWith 两步回国：第一步拿确认码（预览里要列出浏览器），第二步带上确认码和 kill_browsers。
func restoreWith(t *testing.T, d *Daemon, killBrowsers bool) ConfirmNeeded {
	t.Helper()
	b1, _ := json.Marshal(ConfirmRequest{KillBrowsers: killBrowsers})
	code, raw := call(t, d, "POST", "/v1/restore", b1)
	var cn ConfirmNeeded
	if code != 409 || json.Unmarshal(raw, &cn) != nil || cn.Confirm == "" {
		t.Fatalf("第一步：%d %s", code, raw)
	}
	b2, _ := json.Marshal(ConfirmRequest{Confirm: cn.Confirm, KillBrowsers: killBrowsers})
	if code, raw := call(t, d, "POST", "/v1/restore", b2); code != 200 {
		t.Fatalf("第二步：%d %s", code, raw)
	}
	return cn
}

// M6-4b-3：回国预览总是列出浏览器；不选时不结束，选了才结束确认过的。
func TestRestoreKillBrowsers(t *testing.T) {
	for _, kill := range []bool{false, true} {
		e := newEnv(t)
		e.sys.Claude.Browsers = &claudeproc.Browsers{Procs: e.sys.Procs, Bundle: func(app string) (string, error) {
			if strings.HasSuffix(app, "/Safari.app") {
				return "com.apple.Safari", nil
			}
			return "com.example.other", nil
		}}
		d := e.start(e.opts())
		post(t, d, "/v1/enable", 200)
		e.sys.Procs.Add(procinfo.Proc{PID: 4242, PPID: 1, Path: kbSafari}, claudeproc.ExitOnTerm, 0)
		cn := restoreWith(t, d, kill)
		raw, _ := json.Marshal(cn.Preview)
		if !strings.Contains(string(raw), `"browsers"`) || !strings.Contains(string(raw), "Safari") {
			t.Fatalf("预览应列出浏览器：%s", raw)
		}
		if e.sys.Procs.Alive(4242) == kill {
			t.Fatalf("kill_browsers=%v：Safari 存活 %v", kill, e.sys.Procs.Alive(4242))
		}
	}
}
