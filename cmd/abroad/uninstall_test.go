package main

import (
	"strings"
	"testing"

	"claude-abroad/internal/claudeproc"
	"claude-abroad/internal/daemon"
	"claude-abroad/internal/procinfo"
)

// M6-5a 补审返工：命令行的卸载。
// 不是 root 时本地就拒绝、不发请求；确认词要 uninstall；步骤清单的输出；退出码；卸载后启用被拒（按代码显示）。
func TestCLIUninstall(t *testing.T) {
	r := newRig(t, rigOpt{claude: true})
	// 不是 root（控制台用户）：本地拒绝，不发请求
	r.asConsole()
	if x := r.run("uninstall"); x.code != ExitDenied || x.reqs != 0 {
		t.Fatalf("非 root 应本地拒绝：%+v", x)
	}
	r.asOwner()
	// 不是终端又没有 --yes：不执行
	if x := r.run("uninstall"); x.code != ExitConfirm || r.d.Status().State != "RESTORED" || strings.Contains(x.out, "✓") {
		t.Fatalf("没有确认不应执行：%+v", x)
	}
	// 终端里输入的不是 uninstall：不执行
	if x := r.runWith(in{tty: true, stdin: "yes\n"}, "uninstall"); x.code == ExitOK || strings.Contains(x.out, "✓") {
		t.Fatalf("确认词不对不应执行：%+v", x)
	}
	// 输入 uninstall：执行；步骤清单 ✓ 和 …（安装器那一步还没轮到）
	x := r.runWith(in{tty: true, stdin: "uninstall\n"}, "uninstall")
	if x.code != ExitOK || !strings.Contains(x.out, "✓ 结束 Claude 并恢复本机网络") || !strings.Contains(x.out, "✓ 解除 Claude 启动管控") ||
		!strings.Contains(x.out, "… 注销后台服务") || !strings.Contains(x.out, "还有步骤要由安装器完成") || strings.Contains(x.out, "✗") {
		t.Fatalf("卸载输出：%+v", x)
	}
	// 卸载后启用：按代码显示“已经一键卸载”，退出码是被拒
	if x := r.run("--yes", "up"); x.code != ExitDenied || !strings.Contains(x.err, "已经一键卸载") {
		t.Fatalf("卸载后启用应被拒：%+v", x)
	}
	// 再卸载一次：成功
	if x := r.run("--yes", "uninstall"); x.code != ExitOK {
		t.Fatalf("再卸载：%+v", x)
	}
}

// 卸载失败（结束 Claude 失败）：退出码非 0，步骤清单里失败的那一步是 ✗，和“还没轮到”的 … 分开。
func TestCLIUninstallFailureSteps(t *testing.T) {
	r := newRig(t, rigOpt{claude: true})
	mustOK(t, r.run("--yes", "up"), "启用")
	r.sys.Kill.SetFail(daemon.ErrFakeKill)
	x := r.run("--yes", "uninstall")
	if x.code == ExitOK || !strings.Contains(x.out, "✗ 结束 Claude 并恢复本机网络") || !strings.Contains(x.out, "卸载停在打 ✗ 的那一步") {
		t.Fatalf("失败时应列出步骤：%+v", x)
	}
	r.sys.Kill.SetFail(nil)
	if x := r.run("--yes", "uninstall"); x.code != ExitOK || !strings.Contains(x.out, "✓ 结束 Claude 并恢复本机网络") {
		t.Fatalf("重试应完成：%+v", x)
	}
}

// M6-4b-3 补审返工：回国带 --kill-browsers、有浏览器还开着（冒名路径的 Safari，确认不了）：回国成功，输出写明。
func TestCLIRestoreBrowsersLeft(t *testing.T) {
	r := newRig(t, rigOpt{})
	r.sys.Claude.Browsers = &claudeproc.Browsers{Procs: r.sys.Procs, Bundle: func(app string) (string, error) {
		if strings.HasSuffix(app, "/Safari.app") {
			return "com.apple.Safari", nil
		}
		return "com.example.other", nil
	}}
	mustOK(t, r.run("--yes", "up"), "启用")
	r.sys.Procs.Add(procinfo.Proc{PID: 4343, PPID: 1, Path: "/Users/u/Downloads/Safari.app/Contents/MacOS/Safari"}, claudeproc.ExitOnTerm, 0)
	x := r.run("--yes", "restore", "--kill-browsers")
	if x.code != ExitOK || !strings.Contains(x.out, "这些浏览器还开着：Safari") {
		t.Fatalf("%+v", x)
	}
}

// M6-12：domains 列出记录（没有时说明“还没有记录”）、domains clear 清空；diagnostics 默认不带清单，--include-domains 才带。
func TestCLIDomains(t *testing.T) {
	r := newRig(t, rigOpt{})
	x := r.run("domains")
	if x.code != ExitOK || !strings.Contains(x.out, "还没有记录") || !strings.Contains(x.out, "只是线索") {
		t.Fatalf("%+v", x)
	}
	if x := r.run("domains", "clear"); x.code != ExitOK || !strings.Contains(x.out, "已清空") {
		t.Fatalf("%+v", x)
	}
	if x := r.run("diagnostics"); x.code != ExitOK || !strings.Contains(x.out, `"claude_domains"`) || strings.Contains(x.out, `"domains":`) {
		t.Fatalf("诊断包默认只有条数：%+v", x)
	}
	if x := r.run("diagnostics", "--include-domains"); x.code != ExitOK {
		t.Fatalf("%+v", x)
	}
	if x := r.run("diagnostics", "--bogus"); x.code != ExitUsage {
		t.Fatalf("不认识的参数应是参数错：%+v", x)
	}
}

// M6-11b：status 写出回国拦截（打开了首次准备时）；没打开时不写。
func TestCLIStatusReturnBlock(t *testing.T) {
	r := newRig(t, rigOpt{prep: true})
	x := r.run("status")
	if x.code != ExitOK || !strings.Contains(x.out, "回国拦截：生效中，拦 160.79.104.0/21、2607:6bc0::/32") {
		t.Fatalf("应写出回国拦截：%+v", x)
	}
	r2 := newRig(t, rigOpt{})
	if x := r2.run("status"); strings.Contains(x.out, "回国拦截") {
		t.Fatalf("没打开首次准备时不应写：%s", x.out)
	}
}
