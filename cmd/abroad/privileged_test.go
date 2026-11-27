package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claude-abroad/internal/api"
	"claude-abroad/internal/daemon"
)

// privilegedNotProvided 是命令表里敏感、或按本次调用决定的命令中，应用里不经系统密码框提供的（写明理由）。
var privilegedNotProvided = map[string]string{
	"uninstall": "阶段 B：应用里的卸载走已安装的、属主 root 的 uninstall.sh，不走这份白名单",
}

// B0（v0.71b）：应用里经系统密码框以 root 运行的子命令白名单（app/Tests/AbroadKitTests/testdata/privileged-subcommands.txt，
// 和 Swift 的 PrivilegedSubcommand 一致）与命令表、路由表双向核对：
//   - 白名单每项在命令表里有、路由 ID 和文件里写的一致、路由级别是敏感或按本次调用决定；
//   - 命令表里路由级别是敏感或按本次调用决定的每个命令，要么在白名单里，要么在 privilegedNotProvided 里写了理由。
func TestPrivilegedWhitelistMatchesRoutes(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "app", "Tests", "AbroadKitTests", "testdata", "privileged-subcommands.txt"))
	if err != nil {
		t.Fatal(err)
	}
	level := map[string]api.Level{}
	for _, r := range daemon.RouteTable() {
		level[r.ID] = r.Level
	}
	cmdRoute := map[string]string{}
	for _, c := range commands {
		if c.later == "" {
			cmdRoute[strings.Join(c.path, " ")] = c.route
		}
	}
	white := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		words, route, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("格式不对：%q", line)
		}
		white[words] = true
		got, ok := cmdRoute[words]
		if !ok {
			t.Errorf("白名单里的 %q 不在命令表里", words)
			continue
		}
		if got != route {
			t.Errorf("%q 的路由是 %s，白名单里写的是 %s", words, got, route)
		}
		if l := level[got]; l != api.Sensitive && l != api.PerCall {
			t.Errorf("%q 的路由 %s 级别是 %s：只有敏感或按本次调用决定的才放进白名单", words, got, l)
		}
	}
	if len(white) < 20 {
		t.Fatalf("白名单只有 %d 项", len(white))
	}
	for words, route := range cmdRoute {
		l := level[route]
		if l != api.Sensitive && l != api.PerCall {
			continue
		}
		if !white[words] && privilegedNotProvided[words] == "" {
			t.Errorf("命令 %q（路由 %s，级别 %s）既不在白名单里，也没有写明为什么不提供", words, route, l)
		}
	}
	for words := range privilegedNotProvided {
		if white[words] {
			t.Errorf("%q 同时在白名单和“不提供”表里", words)
		}
		if l := level[cmdRoute[words]]; l != api.Sensitive && l != api.PerCall {
			t.Errorf("“不提供”表里的 %q 不是敏感级别（%s）：普通级别的命令应用直接调接口，不用列", words, l)
		}
	}
	for _, w := range []string{"claude-proxy on", "claude-proxy off"} {
		if !white[w] {
			t.Errorf("第四道的开关 %q 应在白名单里（顾问 2026-11-27 裁定）", w)
		}
	}
}
