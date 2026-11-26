package main

import (
	"strings"
	"testing"

	"claude-abroad/internal/daemon"
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
