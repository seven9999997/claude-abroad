package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"claude-abroad/internal/claudecfg"
	"claude-abroad/internal/daemon"
)

// v0.72 验收 j：sudo abroad restore [--kill-browsers] [--claude-proxy on|off]。

// countRestore 数这次运行里发了几次 POST /v1/restore。
func (r *rig) countRestore() func() int {
	var mu sync.Mutex
	n := 0
	r.trace = func(method, path string, _ http.Header) {
		if method == "POST" && path == "/v1/restore" {
			mu.Lock()
			n++
			mu.Unlock()
		}
	}
	return func() int { mu.Lock(); defer mu.Unlock(); return n }
}

func (r *rig) cpOn() bool {
	r.t.Helper()
	st := r.status()
	return st.ClaudeCode != nil && st.ClaudeCode.Enabled
}

func (r *rig) restoreReady(t *testing.T, startOn bool) {
	t.Helper()
	r.asOwner()
	if startOn {
		mustOK(t, r.run("--yes", "claude-proxy", "on"), "打开第四道")
	}
	mustOK(t, r.run("--yes", "up"), "启用")
}

func TestCLIRestoreClaudeProxy(t *testing.T) {
	t.Run("参数", func(t *testing.T) {
		r := newRig(t, rigOpt{cproxy: true})
		r.asOwner()
		for _, args := range [][]string{{"restore", "--claude-proxy", "maybe"}, {"restore", "--claude-proxy"}, {"restore", "--claude-proxy", "ON"}} {
			if x := r.run(args...); x.code != ExitUsage || x.reqs != 0 {
				t.Fatalf("%v 应是用法错误、不发请求：%+v", args, x)
			}
		}
	})
	t.Run("交互：现在关、回车保持", func(t *testing.T) {
		r := newRig(t, rigOpt{cproxy: true})
		r.restoreReady(t, false)
		n := r.countRestore()
		x := r.runWith(in{tty: true, stdin: "\nyes\n"}, "restore")
		if x.code != ExitOK || !strings.Contains(x.out, "回国后开启 Claude Code 兜底（第四道）？[y/N]") || !strings.Contains(x.out, daemon.ClaudeProxyRestoreKeepOff) {
			t.Fatalf("%+v", x)
		}
		if n() != 2 || r.cpOn() || r.status().State != "RESTORED" {
			t.Fatalf("应保持关、两次请求：%d", n())
		}
		if strings.Contains(x.out, "第四道已") {
			t.Fatal("没有切换时不应打印切换结果")
		}
	})
	t.Run("交互：现在关、答 y", func(t *testing.T) {
		r := newRig(t, rigOpt{cproxy: true})
		r.restoreReady(t, false)
		n := r.countRestore()
		x := r.runWith(in{tty: true, stdin: "y\nyes\n"}, "restore")
		if x.code != ExitOK || !strings.Contains(x.out, "第四道已打开") {
			t.Fatalf("%+v", x)
		}
		for _, c := range daemon.ClaudeProxyConsequences {
			if !strings.Contains(x.out, c) {
				t.Fatalf("答得和现状不同时应打印后果“%s”：%s", c, x.out)
			}
		}
		if strings.Count(x.out, "？[y/N]") != 1 {
			t.Fatal("重新预览之后不应再问")
		}
		if n() != 3 || !r.cpOn() || r.status().State != "RESTORED" {
			t.Fatalf("应重新预览一次再确认（3 次请求）、开关开：%d", n())
		}
	})
	t.Run("交互：现在开、答 n", func(t *testing.T) {
		r := newRig(t, rigOpt{cproxy: true})
		r.restoreReady(t, true)
		x := r.runWith(in{tty: true, stdin: "n\nyes\n"}, "restore")
		if x.code != ExitOK || !strings.Contains(x.out, "回国后保留 Claude Code 兜底（第四道）？[Y/n]") ||
			!strings.Contains(x.out, daemon.ClaudeProxyRestoreOffNote) || !strings.Contains(x.out, "第四道已关闭") {
			t.Fatalf("%+v", x)
		}
		if r.cpOn() {
			t.Fatal("应已关掉")
		}
	})
	t.Run("交互：现在开、回车保持", func(t *testing.T) {
		r := newRig(t, rigOpt{cproxy: true})
		r.restoreReady(t, true)
		if x := r.runWith(in{tty: true, stdin: "\nyes\n"}, "restore"); x.code != ExitOK || !strings.Contains(x.out, daemon.ClaudeProxyRestoreKeepOn) {
			t.Fatalf("%+v", x)
		}
		if !r.cpOn() {
			t.Fatal("回车应保持开")
		}
	})
	// 裁定②：答的不是 y、n、回车：什么都不改，退出码和取消确认相同
	t.Run("交互：答别的", func(t *testing.T) {
		r := newRig(t, rigOpt{cproxy: true})
		r.restoreReady(t, false)
		n := r.countRestore()
		x := r.runWith(in{tty: true, stdin: "maybe\nyes\n"}, "restore")
		cancel := r.runWith(in{tty: true, stdin: "\nno\n"}, "restore")
		if x.code != ExitConfirm || x.code != cancel.code || !strings.Contains(x.err, "没有确认，什么都没有改动") {
			t.Fatalf("应退出、退出码同取消：%+v / %+v", x, cancel)
		}
		if n() != 2 || r.status().State != "ENABLED" || r.cpOn() {
			t.Fatalf("什么都不应改（只有两次第一步）：%d", n())
		}
	})
	t.Run("--yes 不问", func(t *testing.T) {
		r := newRig(t, rigOpt{cproxy: true})
		r.restoreReady(t, false)
		x := r.runWith(in{tty: true}, "--yes", "restore")
		if x.code != ExitOK || strings.Contains(x.out, "[y/N]") || !strings.Contains(x.out, "第四道保持现状（关）；要改加 --claude-proxy on|off") || r.cpOn() {
			t.Fatalf("%+v", x)
		}
	})
	t.Run("不在终端", func(t *testing.T) {
		r := newRig(t, rigOpt{cproxy: true})
		r.restoreReady(t, false)
		x := r.run("restore")
		if x.code != ExitConfirm || strings.Contains(x.out, "[y/N]") || !strings.Contains(x.out, "要改加 --claude-proxy on|off") || r.status().State != "ENABLED" {
			t.Fatalf("%+v", x)
		}
	})
	t.Run("带参数不问", func(t *testing.T) {
		r := newRig(t, rigOpt{cproxy: true})
		r.restoreReady(t, false)
		x := r.runWith(in{tty: true, stdin: "yes\n"}, "restore", "--claude-proxy", "on")
		if x.code != ExitOK || strings.Contains(x.out, "[y/N]") || !strings.Contains(x.out, "第四道已打开") || !r.cpOn() {
			t.Fatalf("%+v", x)
		}
	})
	t.Run("已经回国时不问", func(t *testing.T) {
		r := newRig(t, rigOpt{cproxy: true})
		r.restoreReady(t, false)
		mustOK(t, r.run("--yes", "restore"), "回国")
		if x := r.runWith(in{tty: true, stdin: "y\n"}, "restore"); x.code != ExitOK || strings.Contains(x.out, "[y/N]") {
			t.Fatalf("%+v", x)
		}
		if x := r.run("--yes", "restore", "--claude-proxy", "on"); x.code != ExitFail || !strings.Contains(x.err, "sudo abroad claude-proxy on") || r.cpOn() {
			t.Fatalf("已回国又要切换应失败并提示命令：%+v", x)
		}
	})
	t.Run("预览时有冲突", func(t *testing.T) {
		r := newRig(t, rigOpt{cproxy: true})
		r.restoreReady(t, false)
		_ = os.MkdirAll(r.cs.Dir, 0o755)
		_ = os.WriteFile(filepath.Join(r.cs.Dir, "corp.json"), []byte(`{"env":{"HTTPS_PROXY":"http://p.example:1"}}`), 0o644)
		x := r.run("--yes", "restore", "--claude-proxy", "on")
		if x.code != ExitFail || !strings.Contains(x.err, "corp.json 的 HTTPS_PROXY") || r.status().State != "ENABLED" || r.cpOn() {
			t.Fatalf("%+v", x)
		}
		// --json：错误应答（带 conflicts）原样在标准输出（应用经系统密码框执行时只拿得到标准输出）
		x = r.run("--json", "--yes", "restore", "--claude-proxy", "on")
		var eb struct {
			Code      string               `json:"code"`
			Conflicts []claudecfg.Conflict `json:"conflicts"`
		}
		if x.code != ExitFail || json.Unmarshal([]byte(x.out), &eb) != nil || eb.Code != "invalid" || len(eb.Conflicts) != 1 || eb.Conflicts[0].File != "corp.json" {
			t.Fatalf("--json 时标准输出应是带冲突的错误应答：%+v", x)
		}
	})
	// 裁定④：回国成功、打开失败：退出码 0，标准错误写清（做什么失败了、原因、下一步命令）；--json 里 ok 为假
	t.Run("回国成功、打开失败", func(t *testing.T) {
		r := newRig(t, rigOpt{cproxy: true})
		r.restoreReady(t, false)
		_ = os.MkdirAll(filepath.Dir(r.cs.Dir), 0o755)
		_ = os.WriteFile(r.cs.Dir, []byte("x"), 0o644) // 分片目录的位置被普通文件占着：写分片失败
		x := r.run("--json", "--yes", "restore", "--claude-proxy", "on")
		if x.code != ExitOK || !strings.Contains(x.err, "回国已完成，但切换第四道失败：") || !strings.Contains(x.err, "托管设置") ||
			!strings.HasSuffix(strings.TrimSpace(x.err), daemon.ClaudeProxyRestoreNextOn+"。") {
			t.Fatalf("%+v", x)
		}
		var st daemon.Status
		if json.Unmarshal([]byte(x.out), &st) != nil || st.State != "RESTORED" || st.ClaudeProxyChange == nil || st.ClaudeProxyChange.OK {
			t.Fatalf("--json 应带 ok 为假的 claude_proxy_change：%s", x.out)
		}
		if _, err := os.Stat(filepath.Join(r.cs.Dir, claudecfg.FileName)); err == nil || r.cpOn() {
			t.Fatal("应什么都没留下")
		}
	})
	t.Run("没打开管控", func(t *testing.T) {
		r := newRig(t, rigOpt{})
		mustOK(t, r.run("--yes", "up"), "启用")
		if x := r.runWith(in{tty: true, stdin: "yes\n"}, "restore"); x.code != ExitOK || strings.Contains(x.out, "第四道") {
			t.Fatalf("没有这一块时不提第四道：%+v", x)
		}
	})
}

// 读终端共用一个缓冲：连续两次读同一个输入流，各读到一行。
func TestCLIReadLineShared(t *testing.T) {
	a := &app{stdin: strings.NewReader("first\nsecond\n")}
	if l := a.readLine(); l != "first\n" {
		t.Fatalf("%q", l)
	}
	if l := a.readLine(); l != "second\n" {
		t.Fatalf("%q", l)
	}
}
