package daemon

import (
	"go/ast"
	"go/token"
	"strconv"
	"testing"

	"claude-abroad/internal/srccheck"
)

// j. 不准直接拨号（m3-plan M3-7）：直连只经 directdial 的客户端（在 dataplane 里经 stack），数据通道只经 unix。
// 白名单只有 unix socket：daemon.go 监听本地接口 api.sock；vmext.go 连 abroad-vm 的 ctl.sock、host.sock。
// 这两个文件里所有 DialContext、net.Listen 的网络类型都必须是字面量 "unix"。
func TestDaemonNoDial(t *testing.T) {
	srccheck.NoDial(t, ".", map[string][]string{
		"daemon.go": {"net.Listen"},
		"vmext.go":  {"net.Dialer", "http.Client", "http.Transport"},
		// M6-3b：第四层的本机代理端口只监听回环上的 TCP（地址在 startClaudeProxy 里校验必须是回环），不拨号：
		// 转发全部经网络栈的 dispatch（数据通道、直连拨号模块）。
		"claudeproxy.go": {"net.Listen"},
	})
	for _, f := range []string{"daemon.go", "vmext.go"} {
		af := srccheck.Parse(t, f)
		ast.Inspect(af, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := c.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "DialContext" && sel.Sel.Name != "Listen") || len(c.Args) < 2 {
				return true
			}
			arg := c.Args[0]
			if sel.Sel.Name == "DialContext" {
				arg = c.Args[1]
			}
			lit, ok := arg.(*ast.BasicLit)
			if v, _ := strconv.Unquote(litValue(lit, ok)); !ok || lit.Kind != token.STRING || v != "unix" {
				t.Errorf("%s 的 %s 网络类型应是字面量 \"unix\"", f, sel.Sel.Name)
			}
			return true
		})
	}
}

func litValue(l *ast.BasicLit, ok bool) string {
	if !ok {
		return ""
	}
	return l.Value
}
